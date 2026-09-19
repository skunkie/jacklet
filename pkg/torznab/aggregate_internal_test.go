// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package torznab

import (
	"context"
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
// A nil scraper panics on entry to ScrapeIndexer — before that function
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

	outcomes, err := tz.scrape(context.Background(), idx, scraper.SearchParams{})

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
