// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A definition's requestDelay is measured from the tracker's answer to one
// request to the sending of the next, as Jackett measures it, so a slow
// answer does not use up the gap. It covers every request the definition
// makes: the pages of a search, and a download after it.
func TestScraperWaitsTheDefinitionsRequestDelay(t *testing.T) {
	const gap = 300 * time.Millisecond
	for _, tc := range []struct {
		name           string
		paths          string
		shouldDownload bool
	}{
		{
			name:  "between a search's pages",
			paths: "    - path: /first\n    - path: /second\n",
		},
		{
			name:           "before a download",
			paths:          "    - path: /first\n",
			shouldDownload: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var answered, sent []time.Time
			site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				sent = append(sent, time.Now())
				mu.Unlock()
				// An answer taking a while, shorter than the gap.
				time.Sleep(gap / 3)
				if r.URL.Path == "/download/1" {
					w.Header().Set("Content-Type", "application/x-bittorrent")
					fmt.Fprint(w, "d4:infode")
				} else {
					fmt.Fprint(w, `<div class="row"><a href="/download/1">Example Release</a></div>`)
				}
				mu.Lock()
				answered = append(answered, time.Now())
				mu.Unlock()
			}))
			defer site.Close()

			def := loadTestTracker(t, t.TempDir(), "paced", `
id: paced
name: Paced
links:
  - `+site.URL+`/
requestDelay: `+fmt.Sprint(gap.Seconds())+`
search:
  paths:
`+tc.paths+`  rows:
    selector: div.row
  fields:
    title:
      selector: a
    download:
      selector: a
      attribute: href
`)
			scrpr := NewWithOptions(NewConfigStore(""), "", slog.New(slog.DiscardHandler), Options{Sink: &fakeStore{}})
			require.NoError(t, scrpr.scrapeIndexer(t.Context(), def, SearchParams{Query: "example"}))
			if tc.shouldDownload {
				download, err := scrpr.Download(t.Context(), def, site.URL+"/download/1")
				require.NoError(t, err)
				download.Body.Close()
			}

			mu.Lock()
			defer mu.Unlock()
			require.Len(t, sent, 2)
			require.GreaterOrEqual(t, sent[1].Sub(answered[0]), gap, "the second request did not wait out the delay after the first was answered")
		})
	}
}

// Requests made at the same time take turns, each reserving the slot after
// the one before it, rather than all going out once the delay has passed.
func TestScraper_PaceRequest_SpacesConcurrentRequests(t *testing.T) {
	const gap = 100 * time.Millisecond
	scrpr := New(NewConfigStore(""), "", slog.New(slog.DiscardHandler))
	def := &Tracker{ID: "paced", RequestDelay: gap.Seconds()}

	var mu sync.Mutex
	var started []time.Time
	var wg sync.WaitGroup
	// The first slot opens no earlier than this, so measuring from it
	// does not depend on when the first goroutine records its start.
	begin := time.Now()
	for range 3 {
		wg.Go(func() {
			answered, err := scrpr.paceRequest(t.Context(), def)
			if err != nil {
				return
			}
			mu.Lock()
			started = append(started, time.Now())
			mu.Unlock()
			answered()
		})
	}
	wg.Wait()

	require.Len(t, started, 3)
	require.GreaterOrEqual(t, slices.MaxFunc(started, time.Time.Compare).Sub(begin), 2*gap, "three concurrent requests were not spaced by the delay")
}

// A request waiting its turn gives up when its caller does, and gives its
// turn back, so the next request does not wait for one never sent.
func TestScraper_PaceRequest_Canceled(t *testing.T) {
	scrpr := New(NewConfigStore(""), "", slog.New(slog.DiscardHandler))
	def := &Tracker{ID: "paced", RequestDelay: 60}

	answered, err := scrpr.paceRequest(t.Context(), def)
	require.NoError(t, err, "the first request has nothing to wait for")
	answered()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	_, err = scrpr.paceRequest(ctx, def)
	require.ErrorIs(t, err, errRequestQueue)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Empty(t, scrpr.requestTurnFor("paced").turn, "the canceled request kept its turn")

	// A caller that has already given up gets no turn, even one with
	// nothing to wait for.
	canceled, cancelNow := context.WithCancel(t.Context())
	cancelNow()
	_, err = scrpr.paceRequest(canceled, &Tracker{ID: "idle", RequestDelay: 1})
	require.ErrorIs(t, err, errRequestQueue)
}

// The gap follows the previous answer even when that answer takes longer
// than the gap, rather than following the previous request's start.
func TestScraper_PaceRequest_WaitsForASlowAnswer(t *testing.T) {
	const gap = 100 * time.Millisecond
	scrpr := New(NewConfigStore(""), "", slog.New(slog.DiscardHandler))
	def := &Tracker{ID: "paced", RequestDelay: gap.Seconds()}

	answered, err := scrpr.paceRequest(t.Context(), def)
	require.NoError(t, err)
	next := make(chan time.Time, 1)
	go func() {
		answered, err := scrpr.paceRequest(t.Context(), def)
		if err != nil {
			next <- time.Time{}
			return
		}
		next <- time.Now()
		answered()
	}()
	time.Sleep(3 * gap)
	answeredAt := time.Now()
	answered()

	require.GreaterOrEqual(t, (<-next).Sub(answeredAt), gap, "the next request went out before the delay after the slow answer")
}

// A search that ends while it waits its turn asked the tracker nothing, so
// the tracker is not counted as failing or backed off.
func TestScraperDoesNotBlameTheTrackerForItsRequestDelay(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<div class="row"><a href="/download/1">Example Release</a></div>`)
	}))
	defer site.Close()
	def := loadTestTracker(t, t.TempDir(), "paced", `
id: paced
name: Paced
links:
  - `+site.URL+`/
requestDelay: 60
search:
  paths:
    - path: /
  rows:
    selector: div.row
  fields:
    title:
      selector: a
    download:
      selector: a
      attribute: href
`)
	scrpr := NewWithOptions(NewConfigStore(""), "", slog.New(slog.DiscardHandler), Options{Sink: &fakeStore{}})
	answered, err := scrpr.paceRequest(t.Context(), def)
	require.NoError(t, err)
	defer answered()

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	err = scrpr.scrapeIndexer(ctx, def, SearchParams{Query: "example"})
	require.ErrorIs(t, err, errRequestQueue)

	state, ok := scrpr.scrapeStateSnapshot("paced")
	require.True(t, ok)
	require.Zero(t, state.failures, "a wait for a turn was recorded as a tracker failure")
}

// Pacing a request, as a download is paced, does not count as searching
// the tracker.
func TestScraper_PaceRequest_LeavesTheTrackerUnsearched(t *testing.T) {
	scrpr := New(NewConfigStore(""), "", slog.New(slog.DiscardHandler))
	answered, err := scrpr.paceRequest(t.Context(), &Tracker{ID: "paced", RequestDelay: 1})
	require.NoError(t, err)
	answered()
	require.False(t, scrpr.Status("paced").Scraped)
}

func TestTracker_RequestGap(t *testing.T) {
	for _, tc := range []struct {
		name  string
		delay float64
		want  time.Duration
	}{
		{name: "unset", want: 0},
		{name: "fractional", delay: 2.5, want: 2500 * time.Millisecond},
		// 4.1 seconds is 4099999999.9999995 nanoseconds as a float.
		{name: "rounded", delay: 4.1, want: 4100 * time.Millisecond},
		{name: "negative", delay: -1, want: 0},
		{name: "not a number", delay: math.NaN(), want: 0},
		{name: "beyond the cap", delay: 1e10, want: maxRequestGap},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, (&Tracker{RequestDelay: tc.delay}).requestGap())
		})
	}
}

// A tracker that hangs is still counted as failing when the scrape then
// gives up waiting its turn, on a failover to its next mirror, on that
// mirror's login or on its next search path: a queue wait does not replace
// the failure the tracker caused.
func TestScraperBlamesTheTrackerBehindALaterQueueWait(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/hang") {
			<-r.Context().Done()
			return
		}
		fmt.Fprint(w, `<div class="row"><a href="/download/1">Example Release</a></div>`)
	}))
	defer site.Close()

	for _, tc := range []struct {
		name, links, login, paths string
	}{
		{
			name:  "a later mirror",
			links: "  - " + site.URL + "/hang/\n  - " + site.URL + "/answer/\n",
			paths: "    - path: search\n",
		},
		{
			name:  "a later mirror's login",
			links: "  - " + site.URL + "/hang/\n  - " + site.URL + "/answer/\n",
			login: "login:\n  method: get\n  path: login\n",
			paths: "    - path: search\n",
		},
		{
			name:  "a later search path",
			links: "  - " + site.URL + "/\n",
			paths: "    - path: hang\n    - path: answer\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			def := loadTestTracker(t, t.TempDir(), "paced", `
id: paced
name: Paced
links:
`+tc.links+tc.login+`requestDelay: 1
search:
  paths:
`+tc.paths+`  rows:
    selector: div.row
  fields:
    title:
      selector: a
    download:
      selector: a
      attribute: href
`)
			scrpr := NewWithOptions(NewConfigStore(""), "", slog.New(slog.DiscardHandler), Options{Sink: &fakeStore{}})

			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			err := scrpr.scrapeIndexer(ctx, def, SearchParams{Query: "example"})
			require.Error(t, err)
			require.NotErrorIs(t, err, errRequestQueue, "the queue wait replaced the tracker's own failure")

			state, ok := scrpr.scrapeStateSnapshot("paced")
			require.True(t, ok)
			require.Equal(t, 1, state.failures, "the hung tracker was not counted as failing")
		})
	}
}
