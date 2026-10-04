// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

// Package catalogtest is a conformance suite for torznab.Catalog. The
// Torznab handlers lean on what Search answers -- ordering, paging totals,
// case folding, filters and search provenance -- so an implementation
// written for an embedding program runs Run against itself instead of
// discovering a difference through a client.
package catalogtest

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/torrplay/jacklet/pkg/scraper"
	"github.com/torrplay/jacklet/pkg/torznab"
)

// Fixture is a catalog under test together with the two ways the suite
// fills it, since the Catalog interface itself only reads.
type Fixture struct {
	// Add stores torrents as generic cache entries, with no search
	// provenance: what a program writing straight into its storage produces.
	Add func(ctx context.Context, torrents []scraper.Torrent) error
	// AddForSearch stores torrents as scraper-managed rows produced for the
	// search identified by searchKey, which is what a scraper's Sink does.
	AddForSearch func(ctx context.Context, torrents []scraper.Torrent, searchKey string) error
	// Catalog is the implementation under test.
	Catalog torznab.Catalog
}

// Run checks a Catalog against the contract its documentation states.
// newFixture is called once per subtest and must return an empty catalog
// each time, so no case sees another's rows.
func Run(t *testing.T, newFixture func(t *testing.T) Fixture) {
	t.Helper()
	ctx := context.Background()

	fresh := func(t *testing.T, torrents ...scraper.Torrent) Fixture {
		t.Helper()
		fixture := newFixture(t)
		require.NoError(t, fixture.Add(ctx, torrents), "the fixture could not store its rows")
		return fixture
	}
	names := func(torrents []scraper.Torrent) []string {
		out := make([]string, len(torrents))
		for i := range torrents {
			out[i] = torrents[i].Name
		}
		return out
	}
	search := func(t *testing.T, fixture Fixture, query scraper.Query) ([]string, int) {
		t.Helper()
		found, total, err := fixture.Catalog.Search(ctx, query)
		require.NoError(t, err)
		return names(found), total
	}
	release := func(tracker, name, published string, category int) scraper.Torrent {
		return scraper.Torrent{Categories: []int{category}, Name: name, Published: published, Tracker: tracker}
	}

	t.Run("newest release first", func(t *testing.T) {
		fixture := fresh(t,
			release("alpha", "Sample Middle", "2024-02-01T00:00:00Z", 2000),
			release("alpha", "Sample Newest", "2024-03-01T00:00:00Z", 2000),
			release("beta", "Sample Oldest", "2024-01-01T00:00:00Z", 2000),
		)
		got, total := search(t, fixture, scraper.Query{Limit: 10})
		require.Equal(t, []string{"Sample Newest", "Sample Middle", "Sample Oldest"}, got,
			"results across trackers were not ordered by release date, newest first")
		require.Equal(t, 3, total)
	})

	t.Run("paging counts every match", func(t *testing.T) {
		rows := make([]scraper.Torrent, 0, 5)
		for _, day := range []string{"1", "2", "3", "4", "5"} {
			rows = append(rows, release("alpha", "Sample "+day, "2024-01-0"+day+"T00:00:00Z", 2000))
		}
		fixture := fresh(t, rows...)

		first, total := search(t, fixture, scraper.Query{Limit: 2})
		require.Equal(t, []string{"Sample 5", "Sample 4"}, first)
		require.Equal(t, 5, total, "the total counted only the page")

		second, total := search(t, fixture, scraper.Query{Limit: 2, Offset: 2})
		require.Equal(t, []string{"Sample 3", "Sample 2"}, second)
		require.Equal(t, 5, total)

		past, total := search(t, fixture, scraper.Query{Limit: 2, Offset: 10})
		require.Empty(t, past, "a page past the last result returned rows")
		require.Equal(t, 5, total, "a page past the last result lost the total")

		none, _ := search(t, fixture, scraper.Query{Limit: 0})
		require.Empty(t, none, "a zero limit returned rows")
	})

	t.Run("terms all match, ignoring case", func(t *testing.T) {
		fixture := fresh(t,
			release("alpha", "Sample Show S01E02 1080p", "2024-01-03T00:00:00Z", 5000),
			release("alpha", "Sample Show S02E01 720p", "2024-01-02T00:00:00Z", 5000),
			release("alpha", "Тестовый Релиз 2024", "2024-01-01T00:00:00Z", 2000),
		)
		got, total := search(t, fixture, scraper.Query{Limit: 10, Terms: []string{"SAMPLE", "s01e02"}})
		require.Equal(t, []string{"Sample Show S01E02 1080p"}, got, "a term was matched case-sensitively or not required")
		require.Equal(t, 1, total)

		got, _ = search(t, fixture, scraper.Query{Limit: 10, Terms: []string{"тестовый", "РЕЛИЗ"}})
		require.Equal(t, []string{"Тестовый Релиз 2024"}, got, "non-ASCII terms were not matched case-insensitively")

		got, _ = search(t, fixture, scraper.Query{Limit: 10})
		require.Len(t, got, 3, "no terms must match every name")
	})

	t.Run("category list filters", func(t *testing.T) {
		fixture := fresh(t,
			release("alpha", "Sample Movie", "2024-01-02T00:00:00Z", 2000),
			release("alpha", "Sample Show", "2024-01-01T00:00:00Z", 5000),
		)
		got, total := search(t, fixture, scraper.Query{Categories: []string{"5000"}, Limit: 10})
		require.Equal(t, []string{"Sample Show"}, got)
		require.Equal(t, 1, total)

		got, _ = search(t, fixture, scraper.Query{Categories: []string{"2000", "5000"}, Limit: 10})
		require.Len(t, got, 2)

		got, _ = search(t, fixture, scraper.Query{Limit: 10})
		require.Len(t, got, 2, "an empty category list must match every category")
	})

	t.Run("a row matches any of its categories, each whole", func(t *testing.T) {
		fixture := fresh(t,
			scraper.Torrent{Categories: []int{2040, 100011}, Name: "Sample Both", Published: "2024-01-02T00:00:00Z", Tracker: "alpha"},
			release("alpha", "Sample Other", "2024-01-01T00:00:00Z", 5000),
		)
		for _, category := range []string{"2040", "100011"} {
			got, _ := search(t, fixture, scraper.Query{Categories: []string{category}, Limit: 10})
			require.Equal(t, []string{"Sample Both"}, got, "a search for %s", category)
		}
		for _, category := range []string{"204", "1000", "040"} {
			got, _ := search(t, fixture, scraper.Query{Categories: []string{category}, Limit: 10})
			require.Empty(t, got, "a search for %s matched part of a category", category)
		}
	})

	t.Run("a row filed under no category", func(t *testing.T) {
		fixture := fresh(t,
			scraper.Torrent{Name: "Sample Unfiled", Published: "2024-01-02T00:00:00Z", Tracker: "alpha"},
			release("alpha", "Sample Movie", "2024-01-01T00:00:00Z", 2000),
		)
		got, _ := search(t, fixture, scraper.Query{Categories: []string{"5000"}, Limit: 10})
		require.Equal(t, []string{"Sample Unfiled"}, got, "a category filter left out a row with no category, which Jackett keeps")

		got, total := search(t, fixture, scraper.Query{Limit: 10, NeedsCategory: true})
		require.Equal(t, []string{"Sample Movie"}, got, "NeedsCategory kept a row with no category")
		require.Equal(t, 1, total, "the total counted a row NeedsCategory leaves out")
	})

	t.Run("tracker list scopes", func(t *testing.T) {
		fixture := fresh(t,
			release("alpha", "Sample One", "2024-01-03T00:00:00Z", 2000),
			release("beta", "Sample Two", "2024-01-02T00:00:00Z", 2000),
			release("gamma", "Sample Three", "2024-01-01T00:00:00Z", 2000),
		)
		got, total := search(t, fixture, scraper.Query{Limit: 10, Trackers: []string{"alpha", "gamma"}})
		require.Equal(t, []string{"Sample One", "Sample Three"}, got)
		require.Equal(t, 2, total)

		got, total = search(t, fixture, scraper.Query{Limit: 10})
		require.Len(t, got, 3, "an empty tracker list must match every tracker")
		require.Equal(t, 3, total)

		got, _ = search(t, fixture, scraper.Query{Limit: 10, Trackers: []string{"unknown"}})
		require.Empty(t, got)
	})

	t.Run("query key limits scraper-managed rows to their search", func(t *testing.T) {
		fixture := fresh(t, release("alpha", "Sample Unmanaged", "2024-01-01T00:00:00Z", 2000))
		require.NoError(t, fixture.AddForSearch(ctx, []scraper.Torrent{release("alpha", "Sample For First", "2024-01-03T00:00:00Z", 2000)}, "first"))
		require.NoError(t, fixture.AddForSearch(ctx, []scraper.Torrent{release("alpha", "Sample For Second", "2024-01-02T00:00:00Z", 2000)}, "second"))

		got, _ := search(t, fixture, scraper.Query{Limit: 10, QueryKey: "first"})
		require.Equal(t, []string{"Sample For First", "Sample Unmanaged"}, got,
			"a row produced for another search answered this one, or a row with no provenance was withheld")

		got, _ = search(t, fixture, scraper.Query{Limit: 10, QueryKey: "unknown"})
		require.Equal(t, []string{"Sample Unmanaged"}, got)

		got, _ = search(t, fixture, scraper.Query{Limit: 10})
		require.Len(t, got, 3, "an empty key must not filter on provenance")
	})

	t.Run("ids limit rows with no provenance to those holding one", func(t *testing.T) {
		withIMDB := release("alpha", "Sample With Imdb", "2024-01-01T00:00:00Z", 2000)
		withIMDB.IMDBID = "123456"
		withTVDB := release("alpha", "Sample With Tvdb", "2024-01-02T00:00:00Z", 2000)
		withTVDB.TVDBID = "77"
		withOther := release("alpha", "Sample With Another Tvdb", "2024-01-03T00:00:00Z", 2000)
		withOther.TVDBID = "78"
		contradicting := release("alpha", "Sample With Tvdb And Another Imdb", "2024-01-03T12:00:00Z", 2000)
		contradicting.IMDBID = "9999999"
		contradicting.TVDBID = "77"
		fixture := fresh(t, withIMDB, withTVDB, withOther, contradicting, release("alpha", "Sample Without Id", "2024-01-04T00:00:00Z", 2000))
		require.NoError(t, fixture.AddForSearch(ctx, []scraper.Torrent{release("alpha", "Sample From Search", "2024-01-05T00:00:00Z", 2000)}, "lookup"))

		got, _ := search(t, fixture, scraper.Query{IDs: scraper.ExternalIDs{IMDBID: "tt0123456"}, Limit: 10, QueryKey: "lookup"})
		require.Equal(t, []string{"Sample From Search", "Sample With Imdb"}, got,
			"a row with no provenance answered an IMDb id it does not hold, or the search's own row was withheld")

		got, _ = search(t, fixture, scraper.Query{IDs: scraper.ExternalIDs{IMDBID: "tt0123456", TVDBID: "077"}, Limit: 10, QueryKey: "lookup"})
		require.Equal(t, []string{"Sample From Search", "Sample With Tvdb", "Sample With Imdb"}, got,
			"a row with no provenance holding one of the ids was withheld, or one holding none, or one contradicting an id, answered")

		got, _ = search(t, fixture, scraper.Query{IDs: scraper.ExternalIDs{TVDBID: "77"}, Limit: 10, QueryKey: "lookup"})
		require.Equal(t, []string{"Sample From Search", "Sample With Tvdb And Another Imdb", "Sample With Tvdb"}, got,
			"an id the search does not name kept a row out")
	})

	t.Run("unsearched terms narrow only rows with no provenance", func(t *testing.T) {
		fixture := fresh(t,
			release("alpha", "Sample Show S01E02", "2024-01-01T00:00:00Z", 5000),
			release("alpha", "Sample Show S01E03", "2024-01-02T00:00:00Z", 5000))
		require.NoError(t, fixture.AddForSearch(ctx, []scraper.Torrent{release("alpha", "Sample Show 1x02", "2024-01-03T00:00:00Z", 5000)}, "episode"))

		got, _ := search(t, fixture, scraper.Query{Limit: 10, QueryKey: "episode", UnsearchedTerms: []string{"s01e02"}})
		require.Equal(t, []string{"Sample Show 1x02", "Sample Show S01E02"}, got,
			"a row with no provenance naming another episode answered, or the search's own row was held to the episode")
	})

	t.Run("find is scoped to its tracker", func(t *testing.T) {
		fixture := fresh(t,
			release("alpha", "Sample One", "2024-01-02T00:00:00Z", 2000),
			release("beta", "Sample Two", "2024-01-01T00:00:00Z", 2000),
		)
		stored, _, err := fixture.Catalog.Search(ctx, scraper.Query{Limit: 10, Trackers: []string{"alpha"}})
		require.NoError(t, err)
		require.Len(t, stored, 1)

		found, err := fixture.Catalog.Find(ctx, "alpha", stored[0].ID)
		require.NoError(t, err)
		require.Equal(t, "Sample One", found.Name)

		_, err = fixture.Catalog.Find(ctx, "beta", stored[0].ID)
		require.ErrorIs(t, err, scraper.ErrNotFound, "a row was found under another tracker")

		_, err = fixture.Catalog.Find(ctx, "alpha", stored[0].ID+1000)
		require.ErrorIs(t, err, scraper.ErrNotFound, "an unknown row was not reported as not found")
	})

	t.Run("safe for concurrent use", func(t *testing.T) {
		fixture := fresh(t, release("alpha", "Sample One", "2024-01-01T00:00:00Z", 2000))
		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() {
				found, total, err := fixture.Catalog.Search(ctx, scraper.Query{Limit: 10})
				assert.NoError(t, err)
				assert.Equal(t, 1, total)
				assert.Len(t, found, 1)
			})
		}
		wg.Wait()
	})
}
