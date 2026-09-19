// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/torrplay/jacklet/pkg/database"
)

// TestScraper_RecoveredScrape covers the containment itself: extraction
// runs over responses Jacklet does not control, and the aggregate indexer
// scrapes in goroutines net/http's recovery does not reach, so a panic has
// to come back as this tracker's error rather than end the process.
func TestScraper_RecoveredScrape(t *testing.T) {
	store, err := database.Open(context.Background(), ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { store.Close() })

	s := New(store, NewConfigStore(""), "", testLogger())
	def := &Tracker{ID: "example", Name: "Example"}

	panicking := func() (err error) {
		defer func() { err = s.recoveredScrape(recover(), def, err) }()
		panic("boom")
	}
	require.ErrorContains(t, panicking(), "panic: boom")

	// Nothing recovered leaves the scrape's own outcome alone, in either
	// direction.
	failing := errors.New("unreachable")
	require.ErrorIs(t, s.recoveredScrape(nil, def, failing), failing)
	require.NoError(t, s.recoveredScrape(nil, def, nil))
}

// TestScraper_RecordsAPanickingScrapeAsAFailure runs the real
// ScrapeIndexer over a panic, so the defer pair that matters is the one in
// production rather than a replica of it.
//
// The seam is a Scraper built with no ConfigStore: Resolve dereferences
// the nil store, and it does so after both defers are registered, which is
// exactly where a panic has to leave the tracker's bookkeeping intact.
// The assertion on "panic:" keeps the test honest — an ordinary scrape
// failure would also count a failure, and would pass an assertion that
// only looked at the count.
func TestScraper_RecordsAPanickingScrapeAsAFailure(t *testing.T) {
	store, err := database.Open(context.Background(), ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { store.Close() })

	s := New(store, nil, "", testLogger())
	tracker := loadTestTracker(t, t.TempDir(), "panic-tracker", `
id: panic-tracker
name: panic-tracker
links:
  - https://tracker.invalid/
search:
  paths:
    - path: /search
  rows:
    selector: ".row"
  fields:
    title:
      selector: "a"
`)

	err = s.ScrapeIndexer(context.Background(), tracker, SearchParams{})
	require.ErrorContains(t, err, "panic:")

	// finishScrape ran with the recovered error in place, so the tracker
	// backs off instead of being filed as a success and retried at full
	// rate on the next search.
	status := s.Status(TrackerID(tracker))
	require.Equal(t, 1, status.Failures)
	require.True(t, status.NextAllowed.After(time.Now()), "a panicking scrape must back off")

	// The flight was settled too, so a concurrent search of the same query
	// was released with that failure rather than left waiting out the
	// request's scrape timeout for a scrape that is never coming back.
	plan := s.planScrape(TrackerID(tracker), SearchParams{}.cacheKey())
	require.False(t, plan.isFollower, "the panicking flight must not be left in flight")
}
