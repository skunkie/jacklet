// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

// Package storetest is a conformance suite for admin.Store. It holds an
// implementation to the Catalog contract the Torznab handlers rely on, which
// catalogtest checks, and to what the admin panel asks of it on top of it:
// the per-tracker figures Stats reports, and ClearTracker's deletion of a
// tracker's torrents.
package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/torrplay/jacklet/pkg/admin"
	"github.com/torrplay/jacklet/pkg/scraper"
	"github.com/torrplay/jacklet/pkg/torznab/catalogtest"
)

// Fixture is a store under test together with the two ways the suite fills
// it, since the interface itself only reads.
type Fixture struct {
	// Add stores torrents as generic cache entries, with no search
	// provenance.
	Add func(ctx context.Context, torrents []scraper.Torrent) error
	// AddForSearch stores torrents as scraper-managed rows produced for the
	// search identified by searchKey, which is what a scraper's Sink does.
	AddForSearch func(ctx context.Context, torrents []scraper.Torrent, searchKey string) error
	// Store is the implementation under test.
	Store admin.Store
}

// Run checks a Store against the contract its documentation states: every
// case catalogtest.Run holds its Catalog to, and the Stats and ClearTracker
// cases below.
// newFixture is called once per subtest and must return an empty store each
// time.
func Run(t *testing.T, newFixture func(t *testing.T) Fixture) {
	t.Helper()

	catalogtest.Run(t, func(t *testing.T) catalogtest.Fixture {
		t.Helper()
		fixture := newFixture(t)
		return catalogtest.Fixture{Add: fixture.Add, AddForSearch: fixture.AddForSearch, Catalog: fixture.Store}
	})

	ctx := context.Background()
	release := func(tracker, name, published string) scraper.Torrent {
		return scraper.Torrent{Name: name, Published: published, Tracker: tracker}
	}

	t.Run("stats of an empty store", func(t *testing.T) {
		stats, err := newFixture(t).Store.Stats(ctx)
		require.NoError(t, err)
		require.Empty(t, stats, "an empty store reported a tracker")
	})

	t.Run("stats count and date each tracker", func(t *testing.T) {
		fixture := newFixture(t)
		require.NoError(t, fixture.Add(ctx, []scraper.Torrent{
			release("alpha", "Sample One", "2024-01-01T00:00:00Z"),
			release("alpha", "Sample Two", "2024-03-01T00:00:00Z"),
			release("beta", "Sample Three", "2024-02-01T00:00:00Z"),
		}))
		require.NoError(t, fixture.AddForSearch(ctx, []scraper.Torrent{release("alpha", "Sample Four", "2024-02-01T00:00:00Z")}, "a-search"))

		stats, err := fixture.Store.Stats(ctx)
		require.NoError(t, err)
		require.Len(t, stats, 2, "a tracker with nothing stored, or one stored twice, had an entry")

		require.Equal(t, 3, stats["alpha"].Torrents, "rows stored for a search were not counted with the rest")
		require.Equal(t, "alpha", stats["alpha"].Tracker, "an entry did not name its own tracker")
		require.True(t, stats["alpha"].Newest.Equal(time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)),
			"the newest release was %v, not the latest of the tracker's dates", stats["alpha"].Newest)

		require.Equal(t, 1, stats["beta"].Torrents)
		require.True(t, stats["beta"].Newest.Equal(time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)))

		_, ok := stats["gamma"]
		require.False(t, ok, "a tracker never scraped had an entry")
	})

	t.Run("stats leave the newest date zero when none parses", func(t *testing.T) {
		fixture := newFixture(t)
		require.NoError(t, fixture.Add(ctx, []scraper.Torrent{release("alpha", "Sample Undated", "")}))

		stats, err := fixture.Store.Stats(ctx)
		require.NoError(t, err)
		require.Equal(t, 1, stats["alpha"].Torrents)
		require.True(t, stats["alpha"].Newest.IsZero(), "a tracker with no dated release reported %v as its newest", stats["alpha"].Newest)
	})

	t.Run("clearing a tracker deletes only its torrents", func(t *testing.T) {
		fixture := newFixture(t)
		require.NoError(t, fixture.Add(ctx, []scraper.Torrent{
			release("alpha", "Sample One", "2024-01-01T00:00:00Z"),
			release("beta", "Sample Two", "2024-02-01T00:00:00Z"),
		}))
		require.NoError(t, fixture.AddForSearch(ctx, []scraper.Torrent{release("alpha", "Sample Three", "")}, "a-search"))

		removed, err := fixture.Store.ClearTracker(ctx, "alpha")
		require.NoError(t, err)
		require.Equal(t, int64(2), removed, "the count did not include every torrent of the tracker")

		stats, err := fixture.Store.Stats(ctx)
		require.NoError(t, err)
		_, ok := stats["alpha"]
		require.False(t, ok, "the cleared tracker still has torrents")
		require.Equal(t, 1, stats["beta"].Torrents, "another tracker's torrents were deleted")

		found, _, err := fixture.Store.Search(ctx, scraper.Query{Limit: 10, QueryKey: "a-search", Trackers: []string{"alpha"}})
		require.NoError(t, err)
		require.Empty(t, found, "the stored fallback for a search still answers with a cleared torrent")
	})

	t.Run("clearing a tracker with nothing stored", func(t *testing.T) {
		fixture := newFixture(t)
		require.NoError(t, fixture.Add(ctx, []scraper.Torrent{release("beta", "Sample One", "")}))

		removed, err := fixture.Store.ClearTracker(ctx, "alpha")
		require.NoError(t, err)
		require.Zero(t, removed)

		stats, err := fixture.Store.Stats(ctx)
		require.NoError(t, err)
		require.Equal(t, 1, stats["beta"].Torrents)
	})
}
