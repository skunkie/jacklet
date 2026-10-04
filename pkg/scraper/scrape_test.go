// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const testListingPage = `<div class="row"><a href="/dl/1">Sample Release One</a></div>
<div class="row"><a href="/dl/2">Sample Release Two</a></div>`

// newListingTracker serves testListingPage from a test server and returns a
// definition that scrapes it, along with the count of requests the server
// has answered.
func newListingTracker(t *testing.T, handler http.HandlerFunc) (*Tracker, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		handler(w, r)
	}))
	t.Cleanup(site.Close)

	def := loadTestTracker(t, t.TempDir(), "listing", fmt.Sprintf(`
id: listing
name: Listing
links:
  - %s/
search:
  paths:
    - path: "/"
      method: get
  rows:
    selector: "div.row"
  fields:
    title:
      selector: "a"
    download:
      selector: "a"
      attribute: "href"
`, site.URL))
	return def, &hits
}

// followerCount is how many searches are following the scrape in flight for
// one search of a tracker, and zero when none is in flight.
func (s *Scraper) followerCount(trackerID, queryKey string) int {
	s.scrapeStateMu.Lock()
	defer s.scrapeStateMu.Unlock()
	if state := s.scrapeState[trackerID]; state != nil {
		if flight := state.inFlight[queryKey]; flight != nil {
			return flight.followers
		}
	}
	return 0
}

func servePage(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, testListingPage) }

// failingSink is a Sink that keeps nothing and fails every call, counting
// them, so a test can reach the code that handles a store that is down and
// see that it was asked.
type failingSink struct{ calls atomic.Int64 }

func (s *failingSink) UpsertAllForSearch(context.Context, []Torrent, string) (int, error) {
	s.calls.Add(1)
	return 0, errors.New("sink unavailable")
}

// A Scraper with no Sink is a complete scraper: what it found comes back to
// the caller, tagged with the tracker, and nothing needs a store.
func TestScrapeReturnsTorrentsWithoutASink(t *testing.T) {
	def, _ := newListingTracker(t, servePage)

	found, err := New(NewConfigStore(""), "", slog.New(slog.DiscardHandler)).Scrape(t.Context(), def, SearchParams{})
	require.NoError(t, err)
	require.Len(t, found, 2)
	require.Equal(t, "Sample Release One", found[0].Name)
	require.Equal(t, "Sample Release Two", found[1].Name)
	require.Equal(t, "listing", found[0].Tracker)
	require.Zero(t, found[0].ID, "a scrape assigns no store id")
}

// The Sink gets each page a scrape found, so what it stored is what Scrape
// returned.
func TestScrapePassesEachPageToTheSink(t *testing.T) {
	def, _ := newListingTracker(t, servePage)
	sink := &fakeStore{}

	found, err := NewWithOptions(NewConfigStore(""), "", slog.New(slog.DiscardHandler), Options{Sink: sink}).Scrape(t.Context(), def, SearchParams{Query: "sample"})
	require.NoError(t, err)

	stored, err := sink.Recent(t.Context(), "listing", 10)
	require.NoError(t, err)
	require.Len(t, stored, len(found), "the sink and the caller saw different results")
}

// A store that is down costs the results their persistence, not the caller
// the scrape: what the tracker served is still returned.
func TestScrapeReturnsTorrentsWhenTheSinkFails(t *testing.T) {
	def, _ := newListingTracker(t, servePage)
	sink := &failingSink{}

	found, err := NewWithOptions(NewConfigStore(""), "", slog.New(slog.DiscardHandler), Options{Sink: sink}).Scrape(t.Context(), def, SearchParams{})
	require.NoError(t, err, "a failing sink was reported as a failed scrape")
	require.Len(t, found, 2)
	require.EqualValues(t, 1, sink.calls.Load())
}

// A repeat inside the de-duplication window fetches nothing, and says so
// with ErrThrottled instead of returning an empty result that reads as "the
// tracker has nothing".
func TestScrapeReportsARepeatAsThrottled(t *testing.T) {
	def, hits := newListingTracker(t, servePage)
	scrpr := New(NewConfigStore(""), "", slog.New(slog.DiscardHandler))

	_, err := scrpr.Scrape(t.Context(), def, SearchParams{Query: "same"})
	require.NoError(t, err)

	found, err := scrpr.Scrape(t.Context(), def, SearchParams{Query: "same"})
	require.ErrorIs(t, err, ErrThrottled)
	require.Empty(t, found)
	require.EqualValues(t, 1, hits.Load(), "the repeat reached the tracker")
}

// A tracker inside its failure backoff is skipped the same way.
func TestScrapeReportsABackedOffTrackerAsThrottled(t *testing.T) {
	def, _ := newListingTracker(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	scrpr := New(NewConfigStore(""), "", slog.New(slog.DiscardHandler))

	found, err := scrpr.Scrape(t.Context(), def, SearchParams{Query: "first"})
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrThrottled)
	require.Empty(t, found, "a failed scrape returned torrents")

	_, err = scrpr.Scrape(t.Context(), def, SearchParams{Query: "second"})
	require.ErrorIs(t, err, ErrThrottled)
}

// Identical searches made together share one scrape, and every caller gets
// its torrents, not only the one that led it.
func TestScrapeFollowersReceiveTheLeadersTorrents(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseLeader := func() { releaseOnce.Do(func() { close(release) }) }
	def, hits := newListingTracker(t, func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		servePage(w, r)
	})
	// A leader still held when the test fails would block the test server's
	// shutdown, so it is let go however the test ends.
	t.Cleanup(releaseLeader)
	sink := &fakeStore{}
	scrpr := NewWithOptions(NewConfigStore(""), "", slog.New(slog.DiscardHandler), Options{Sink: sink})

	type outcome struct {
		err   error
		found []Torrent
	}
	results := make(chan outcome, 2)
	scrape := func() {
		found, err := scrpr.Scrape(t.Context(), def, SearchParams{Query: "same"})
		results <- outcome{err: err, found: found}
	}
	go scrape()
	<-entered
	go scrape()
	// The leader is held until the follower has joined its flight, so the
	// follower cannot arrive after the leader has finished and be skipped.
	require.Eventually(t, func() bool {
		return scrpr.followerCount(TrackerID(def), SearchParams{Query: "same"}.cacheKey()) == 1
	}, 5*time.Second, time.Millisecond, "the second search never joined the first")
	releaseLeader()

	for range 2 {
		got := <-results
		require.NoError(t, got.err)
		require.Len(t, got.found, 2, "a caller was handed no torrents")
	}
	require.EqualValues(t, 1, hits.Load())
	require.Len(t, sink.all(), 2, "the followers stored the rows again")
}

// A Scraper reports whether it has a Sink, so a handler built over a store
// can tell that no scrape will ever reach it.
func TestScraper_HasSink(t *testing.T) {
	require.False(t, New(NewConfigStore(""), "", slog.New(slog.DiscardHandler)).HasSink())
	require.True(t, NewWithOptions(NewConfigStore(""), "", slog.New(slog.DiscardHandler), Options{Sink: &fakeStore{}}).HasSink())

	var missing *Scraper
	require.False(t, missing.HasSink(), "a nil Scraper has no sink")
}

// stubConfig is a ConfigSource that answers every tracker with the same
// overrides, or with an error.
type stubConfig struct {
	err       error
	overrides map[string]any
}

func (c *stubConfig) Overrides(string) (map[string]any, error) { return c.overrides, c.err }

// The settings a Scraper layers over a definition's defaults come from
// whatever ConfigSource it is given, not from files: an override the source
// supplies reaches the request, and a source that fails leaves the
// definition's own defaults in force rather than failing the scrape.
func TestScrapeReadsSettingsFromAConfigSource(t *testing.T) {
	var query atomic.Value
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query.Store(r.URL.Query().Get("sort"))
		servePage(w, r)
	}))
	t.Cleanup(site.Close)
	def := loadTestTracker(t, t.TempDir(), "configured", fmt.Sprintf(`
id: configured
name: Configured
links:
  - %s/
settings:
  - name: sort
    type: text
    default: added
search:
  paths:
    - path: "/"
      method: get
  inputs:
    sort: "{{ .Config.sort }}"
  rows:
    selector: "div.row"
  fields:
    title:
      selector: "a"
`, site.URL))

	scrape := func(source ConfigSource, query string) {
		_, err := New(source, "", slog.New(slog.DiscardHandler)).Scrape(t.Context(), def, SearchParams{Query: query})
		require.NoError(t, err)
	}

	scrape(&stubConfig{overrides: map[string]any{"sort": "seeders"}}, "first")
	require.Equal(t, "seeders", query.Load(), "an override from the source did not reach the request")

	scrape(&stubConfig{err: errors.New("settings unavailable")}, "second")
	require.Equal(t, "added", query.Load(), "a failing source did not leave the definition's default")
}

// A torrent seen again on a later scrape must have its swarm counts
// refreshed rather than keeping the values it was first stored with.
func TestScraperRefreshesExistingTorrent(t *testing.T) {
	seeders := "10"
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<div class="row"><a href="/download/1">Same Release</a><span class="s">`+seeders+`</span></div>`)
	}))
	defer testServer.Close()

	dir := t.TempDir()
	def := loadTestTracker(t, dir, "refresh", `
id: refresh
name: Refresh
links:
  - `+testServer.URL+`/
search:
  paths:
    - path: "/"
      method: get
  rows:
    selector: "div.row"
  fields:
    title:
      selector: "a"
    download:
      selector: "a"
      attribute: "href"
    seeders:
      selector: "span.s"
`)

	store := &fakeStore{}

	scrpr := NewWithOptions(NewConfigStore(""), "", slog.New(slog.DiscardHandler), Options{Sink: store})
	require.NoError(t, scrpr.scrapeIndexer(t.Context(), def, SearchParams{}))

	require.Equal(t, 10, storedTorrent(t, scrpr, def, "Same Release").Seeders)

	// Re-scrape with a different swarm count, bypassing the rate limiter.
	seeders = "42"
	scrpr.clearScrapeState()
	require.NoError(t, scrpr.scrapeIndexer(t.Context(), def, SearchParams{}))

	require.Equal(t, 42, storedTorrent(t, scrpr, def, "Same Release").Seeders)
	require.Len(t, storedTorrents(t, scrpr, def), 1, "refreshing must not duplicate the row")
}

// The published column is compared lexicographically when pruning and
// parsed back when serving a feed, so it must be stored as UTC RFC3339 —
// and left empty, not zero-valued, when no date could be parsed.
func TestScraperStoresNormalizedPublishedDate(t *testing.T) {
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `
			<div class="row"><a href="/d/1">Dated</a><span class="p">2024-03-05 14:30:00</span></div>
			<div class="row"><a href="/d/2">Undated</a><span class="p">not a date</span></div>`)
	}))
	defer testServer.Close()

	dir := t.TempDir()
	def := loadTestTracker(t, dir, "dates", `
id: dates
name: Dates
links:
  - `+testServer.URL+`/
search:
  paths:
    - path: "/"
      method: get
  rows:
    selector: "div.row"
  fields:
    title:
      selector: "a"
    download:
      selector: "a"
      attribute: "href"
    date:
      selector: "span.p"
`)

	store := &fakeStore{}

	scrpr := NewWithOptions(NewConfigStore(""), "", slog.New(slog.DiscardHandler), Options{Sink: store})
	require.NoError(t, scrpr.scrapeIndexer(t.Context(), def, SearchParams{}))

	dated := storedTorrent(t, scrpr, def, "Dated").Published
	undated := storedTorrent(t, scrpr, def, "Undated").Published

	parsed, err := time.Parse(time.RFC3339, dated)
	require.NoError(t, err)
	// A tracker printing no offset means its own wall clock, which is read
	// as local time; the stored value is that instant normalized to UTC.
	// Asserting against a fixed "...Z" would only hold on a UTC host.
	require.Equal(t,
		time.Date(2024, time.March, 5, 14, 30, 0, 0, time.Local).UTC(), //nolint:gosmopolitan // asserting the local-time behavior under test, on whatever host runs it
		parsed.UTC(),
	)
	require.Empty(t, undated, "an unparseable date must not be stored as the zero time")
}

// Concurrent duplicates of one search must collapse onto a single scrape
// rather than each reserving its own.
func TestScraperConcurrentDuplicateSearchesScrapeOnce(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var hits atomic.Int64
	var enteredOnce sync.Once
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		enteredOnce.Do(func() { close(entered) })
		<-release
		fmt.Fprint(w, `<div class="row"><a>Release</a></div>`)
	}))
	defer testServer.Close()

	dir := t.TempDir()
	def := loadTestTracker(t, dir, "concurrent", `
id: concurrent
name: Concurrent
links:
  - `+testServer.URL+`/
search:
  paths:
    - path: "/"
      method: get
  rows:
    selector: "div.row"
  fields:
    title:
      selector: "a"
`)

	store := &fakeStore{}

	scrpr := NewWithOptions(NewConfigStore(""), "", slog.New(slog.DiscardHandler), Options{Sink: store})

	results := make(chan error, 2)
	go func() {
		results <- scrpr.scrapeIndexer(t.Context(), def, SearchParams{Query: "same"})
	}()
	<-entered
	go func() {
		results <- scrpr.scrapeIndexer(t.Context(), def, SearchParams{Query: "same"})
	}()

	select {
	case err := <-results:
		require.FailNowf(t, "a duplicate search did not wait",
			"it returned before the shared scrape completed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	require.NoError(t, <-results)
	require.NoError(t, <-results)

	require.Equal(t, int64(1), hits.Load())
	stored, err := store.Recent(t.Context(), "concurrent", 10)
	require.NoError(t, err)
	require.Len(t, stored, 1)
}
