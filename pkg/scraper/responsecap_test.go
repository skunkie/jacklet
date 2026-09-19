// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torrplay/jacklet/pkg/database"
)

// Nothing else bounds how large a response may be: the HTTP client's
// timeout bounds how long one takes, and the whole body is read into
// memory and then given a document tree of its own. The aggregate indexer
// makes that concurrent across several trackers at once.

// newSizedTracker serves a listing page padded to bodyBytes, and returns a
// scraper whose response cap is maxBytes.
func newSizedTracker(t *testing.T, bodyBytes int, maxBytes int64) (*Scraper, *Tracker) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		page := `<table><tr class="row"><td><a href="/d/1">Padded Release</a></td></tr></table>`
		fmt.Fprint(w, page)
		// Padding inside a comment, so the page stays parseable at any
		// size. The delimiters count toward the total, or a body meant to
		// sit exactly on the cap lands just past it.
		if pad := bodyBytes - len(page) - len("<!---->"); pad > 0 {
			fmt.Fprint(w, "<!--"+strings.Repeat("x", pad)+"-->")
		}
	}))
	t.Cleanup(server.Close)

	def := fmt.Sprintf(`
id: sized
name: sized
links:
  - %s/
search:
  paths:
    - path: /
  rows:
    selector: .row
  fields:
    title:
      selector: a
    download:
      selector: a
      attribute: href
`, server.URL)

	tracker := loadTestTracker(t, t.TempDir(), "sized", def)
	store, err := database.Open(context.Background(), ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { store.Close() })

	scrpr := NewWithOptions(store, NewConfigStore(""), "", testLogger(),
		Options{MaxResponseBytes: maxBytes})
	return scrpr, tracker
}

// TestScraper_ResponseUnderTheCapIsScraped checks the cap does not disturb
// a response that fits, including one right at the limit.
func TestScraper_ResponseUnderTheCapIsScraped(t *testing.T) {
	const limit = 4096
	for _, size := range []int{100, limit - 1, limit} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			scrpr, def := newSizedTracker(t, size, limit)
			require.NoError(t, scrpr.ScrapeIndexer(context.Background(), def,
				SearchParams{Query: "padded", Type: "search"}))

			stored := storedTorrents(t, scrpr, def)
			require.Len(t, stored, 1)
			require.Equal(t, "Padded Release", stored[0].Name)
		})
	}
}

// TestScraper_ResponseOverTheCapFails covers the other side: a scrape that
// would buffer more than the cap fails rather than truncating. Half a
// document parses into plausible-looking nonsense, which is worse than an
// error saying what happened.
func TestScraper_ResponseOverTheCapFails(t *testing.T) {
	scrpr, def := newSizedTracker(t, 8192, 4096)

	err := scrpr.ScrapeIndexer(context.Background(), def,
		SearchParams{Query: "padded", Type: "search"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "larger than 4096 bytes")
	require.Empty(t, storedTorrents(t, scrpr, def), "a truncated page must not be stored")
}

// TestScraper_FlareSolverrResponseOverTheCapFails covers the other
// unbounded read: FlareSolverr returns the whole rendered page as JSON, so
// it is the same exposure by a different route.
func TestScraper_FlareSolverrResponseOverTheCapFails(t *testing.T) {
	flare := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["cmd"] == "sessions.create" {
			fmt.Fprint(w, `{"status":"ok","session":"s1"}`)
			return
		}
		fmt.Fprint(w, `{"status":"ok","solution":{"response":"`+strings.Repeat("x", 8192)+`"}}`)
	}))
	t.Cleanup(flare.Close)

	scrpr, def := newSizedTracker(t, 100, 4096)
	scrpr.flareSolverrURL = flare.URL

	err := scrpr.ScrapeIndexer(context.Background(), def,
		SearchParams{Query: "padded", Type: "search"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "larger than 4096 bytes")
}

func TestNew_DefaultsTheResponseCap(t *testing.T) {
	require.Equal(t, int64(defaultMaxResponseBytes),
		New(nil, NewConfigStore(""), "", testLogger()).maxResponseBytes)
}
