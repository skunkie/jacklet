// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package torznab

import (
	"errors"
	"log/slog"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torrplay/jacklet/pkg/scraper"
)

// TestScrapeGuarded covers the aggregate's per-tracker panic containment.
// Those scrapes run in their own goroutines, which net/http's recovery
// does not reach, so a panic anywhere in the extraction pipeline over a
// tracker's externally-served response must come back as that tracker's
// error rather than killing the process.
func TestScrapeGuarded(t *testing.T) {
	tz := &Torznab{logger: slog.New(slog.DiscardHandler)}
	def := &scraper.Tracker{ID: "example", Name: "Example"}

	t.Run("panic becomes an error", func(t *testing.T) {
		err := tz.scrapeGuarded(def, func() error { panic("boom") })
		require.ErrorContains(t, err, "panic: boom")
	})

	t.Run("a returned error passes through unwrapped", func(t *testing.T) {
		want := errors.New("unreachable")
		err := tz.scrapeGuarded(def, func() error { return want })
		require.ErrorIs(t, err, want)
	})

	t.Run("success passes through", func(t *testing.T) {
		require.NoError(t, tz.scrapeGuarded(def, func() error { return nil }))
	})

	// The real call site is a goroutine, where an escaping panic is fatal
	// rather than a failed subtest.
	t.Run("contains a panic raised in its own goroutine", func(t *testing.T) {
		var wg sync.WaitGroup
		var err error
		wg.Go(func() { err = tz.scrapeGuarded(def, func() error { panic("boom") }) })
		wg.Wait()
		require.ErrorContains(t, err, "panic: boom")
	})
}

// TestScrapeContainsPanicPerTracker runs the real aggregate fan-out over a
// panic, so the guard is exercised where it is installed rather than only
// on its own.
//
// A nil scraper panics on entry to Scrape — before that function
// registers the defers that recover its own panics, which is the case
// scrapeGuarded documents as its reason to exist. Two definitions keep the
// single-def fast path out of it, since that path is the request
// goroutine's and net/http recovers it.
func TestScrapeContainsPanicPerTracker(t *testing.T) {
	tz := &Torznab{logger: slog.New(slog.DiscardHandler)}
	idx := &indexer{
		Defs: []*scraper.Tracker{{ID: "a", Name: "A"}, {ID: "b", Name: "B"}},
		ID:   AggregateID,
	}

	outcomes, err := tz.scrape(t.Context(), idx, scraper.SearchParams{})

	// Every tracker panicked, so the search has nothing to answer from and
	// says so — rather than the process ending mid-request.
	require.ErrorContains(t, err, "every indexer failed")
	require.ErrorContains(t, err, "a: panic:")
	require.ErrorContains(t, err, "b: panic:")

	// Each tracker's own failure is reported against it, which is what the
	// JSON results endpoint renders per indexer.
	require.Len(t, outcomes, 2)
	require.Equal(t, "a", outcomes[0].TrackerID)
	require.ErrorContains(t, outcomes[0].Err, "panic:")
	require.Equal(t, "b", outcomes[1].TrackerID)
	require.ErrorContains(t, outcomes[1].Err, "panic:")
}

// One indexer lists its tracker's custom categories after its standard
// ones, as Jackett's caps do, while a meta indexer lists none, since one
// tracker's custom number names another tracker's category elsewhere; and
// a result's description names only standard categories, as Jackett's
// does.
func TestIndexer_Categories(t *testing.T) {
	def := &scraper.Tracker{Caps: scraper.Caps{CategoryMappings: []scraper.CategoryMapping{
		{Cat: "TV", Desc: "Sample Series", ID: "2"},
		{Cat: "Movies/HD", ID: "1"},
	}}, ID: "example"}
	standard := []scraper.CategoryInfo{{ID: 2040, Name: "Movies/HD"}, {ID: 5000, Name: "TV"}}

	require.Equal(t, []scraper.CategoryInfo{
		{ID: 2040, Name: "Movies/HD"}, {ID: 5000, Name: "TV"}, {ID: 100002, Name: "Sample Series"},
	}, (&indexer{Defs: []*scraper.Tracker{def}}).categories())
	require.Equal(t, standard, (&indexer{Defs: []*scraper.Tracker{def}, IsMeta: true}).categories(),
		"a meta indexer listed a tracker's custom category")
	require.Equal(t, map[int]string{2040: "Movies/HD", 5000: "TV"}, (&indexer{Defs: []*scraper.Tracker{def}}).categoryNames(),
		"a custom category was named in a result's description")
}

func TestHandles(t *testing.T) {
	plain := map[string][]string{"search": {"q"}}
	full := map[string][]string{
		"book-search":  {"q"},
		"movie-search": {"q", "imdbid"},
		"music-search": {"q"},
		"search":       {"q"},
		"tv-search":    {"q", "season", "ep", "tvdbid", "rid"},
	}
	for _, tc := range []struct {
		name   string
		caps   scraper.Caps
		params scraper.SearchParams
		want   bool
	}{
		{name: "a plain search is always available", caps: scraper.Caps{Modes: plain}, params: scraper.SearchParams{Type: "search"}, want: true},
		{name: "a mode the caps advertise", caps: scraper.Caps{Modes: full}, params: scraper.SearchParams{Type: "tvsearch"}, want: true},
		{name: "a mode the caps do not advertise", caps: scraper.Caps{Modes: plain}, params: scraper.SearchParams{Type: "tvsearch"}},
		{name: "music in either spelling", caps: scraper.Caps{Modes: map[string][]string{"audio-search": {"q"}, "search": {"q"}}}, params: scraper.SearchParams{Type: "music"}, want: true},
		{name: "a book search without the mode", caps: scraper.Caps{Modes: plain}, params: scraper.SearchParams{Type: "book"}},
		{name: "an imdb id with a movie search taking one", caps: scraper.Caps{Modes: full}, params: scraper.SearchParams{IMDBID: "tt0123456", Type: "search"}, want: true},
		{name: "an imdb id without a movie search taking one", caps: scraper.Caps{Modes: plain}, params: scraper.SearchParams{IMDBID: "tt0123456", Type: "search"}},
		{
			name:   "an imdb tv search the definition allows",
			caps:   scraper.Caps{AllowTVSearchIMDB: true, Modes: map[string][]string{"search": {"q"}, "tv-search": {"q"}}},
			params: scraper.SearchParams{IMDBID: "tt0123456", Type: "tvsearch"},
			want:   true,
		},
		{
			name:   "an imdb movie search on tv-only imdb support",
			caps:   scraper.Caps{AllowTVSearchIMDB: true, Modes: map[string][]string{"search": {"q"}, "tv-search": {"q"}}},
			params: scraper.SearchParams{IMDBID: "tt0123456", Type: "movie"},
		},
		{
			name:   "an imdb lookup with no mode",
			caps:   scraper.Caps{Modes: full},
			params: scraper.SearchParams{IMDBID: "tt0123456"},
			want:   true,
		},
		{
			// Jackett lets a TVDB id through on the TV search taking it,
			// whatever mode the request names.
			name:   "a tvdb id the tv search takes, in another mode",
			caps:   scraper.Caps{Modes: map[string][]string{"search": {"q"}, "tv-search": {"q", "tvdbid"}}},
			params: scraper.SearchParams{TVDBID: "77", Type: "movie"},
			want:   true,
		},
		{
			name:   "no declared modes advertise the defaults",
			params: scraper.SearchParams{IMDBID: "tt0123456", Type: "movie"},
			want:   true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, handles(&scraper.Tracker{Caps: tc.caps}, tc.params))
		})
	}
}
