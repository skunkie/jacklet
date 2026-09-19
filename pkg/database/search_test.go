// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package database

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// SQLite's own LIKE and lower() fold case for ASCII only, so a Cyrillic
// search term would never match a stored Cyrillic title.
func TestNameMatchClause_FoldsNonASCIICase(t *testing.T) {
	store, err := Open(context.Background(), filepath.Join(t.TempDir(), "search.db"))
	require.NoError(t, err)
	defer store.Close()

	titles := []string{
		"Тестовый Релиз (2009) BDRip",
		"Тёмный Образец (1977)",
		"Sample Show (1977) BDRip",
		"ВЕРХНИЙ РЕГИСТР",
	}
	for _, title := range titles {
		_, err := store.db.Exec(`INSERT INTO torrents (name, tracker) VALUES (?, ?)`, title, "t")
		require.NoError(t, err)
	}

	count := func(t *testing.T, terms ...string) int {
		t.Helper()
		clause, args := nameMatchClause(terms)
		query := `SELECT COUNT(*) FROM torrents WHERE tracker = ?` + clause
		var n int
		require.NoError(t, store.db.QueryRow(query, append([]any{"t"}, args...)...).Scan(&n))
		return n
	}

	tests := []struct {
		name  string
		terms []string
		want  int
	}{
		{name: "cyrillic lowercase matches a capitalized title", terms: []string{"тестовый"}, want: 1},
		{name: "cyrillic capitalized matches too", terms: []string{"Тестовый"}, want: 1},
		{name: "cyrillic uppercase title found by lowercase query", terms: []string{"верхний"}, want: 1},
		{name: "cyrillic with ё", terms: []string{"тёмный"}, want: 1},
		{name: "ascii still folds", terms: []string{"sample show"}, want: 1},
		{name: "every term must match", terms: []string{"sample", "show"}, want: 1},
		{name: "a term that matches nothing excludes the row", terms: []string{"sample", "absent"}, want: 0},
		{name: "no terms matches everything", terms: nil, want: len(titles)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, count(t, tc.terms...), "matched the wrong number of rows")
		})
	}
}

// A term's LIKE metacharacters must match literally.
func TestNameMatchClause_EscapesWildcards(t *testing.T) {
	store, err := Open(context.Background(), filepath.Join(t.TempDir(), "wildcards.db"))
	require.NoError(t, err)
	defer store.Close()

	for _, title := range []string{"100% Example", "Other Example"} {
		_, err := store.db.Exec(`INSERT INTO torrents (name, tracker) VALUES (?, ?)`, title, "t")
		require.NoError(t, err)
	}

	clause, args := nameMatchClause([]string{"%"})
	var n int
	require.NoError(t, store.db.QueryRow(`SELECT COUNT(*) FROM torrents WHERE tracker = ?`+clause,
		append([]any{"t"}, args...)...).Scan(&n))
	require.Equal(t, 1, n, "a literal %% matched the wrong number of rows")
}

func TestLikePattern(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{in: "Тестовый", want: "%тестовый%"},
		{in: "Sample", want: "%sample%"},
		{in: "50%", want: `%50\%%`},
		{in: "a_b", want: `%a\_b%`},
	}
	for _, tc := range tests {
		require.Equal(t, tc.want, likePattern(tc.in), "likePattern(%q)", tc.in)
	}
}

// TestSearch_ScopesToTrackers covers Search.Trackers, which the aggregate
// indexer relies on to answer from several trackers in one query: the sort
// order and the total are then global rather than stitched together from
// per-tracker pages.
func TestSearch_ScopesToTrackers(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "scope.db"))
	require.NoError(t, err)
	defer store.Close()

	// Interleaved release dates, so a correct merge cannot be produced by
	// returning one tracker's rows after the other's.
	for _, row := range []struct{ name, published, tracker string }{
		{"Sample One", "2024-03-04T00:00:00Z", "alpha"},
		{"Sample Two", "2024-03-03T00:00:00Z", "beta"},
		{"Sample Three", "2024-03-02T00:00:00Z", "alpha"},
		{"Sample Four", "2024-03-01T00:00:00Z", "gamma"},
	} {
		require.NoError(t, store.Upsert(ctx, Torrent{Name: row.name, Published: row.published, Tracker: row.tracker}),
			"failed to store %q", row.name)
	}

	names := func(q Search) ([]string, int) {
		t.Helper()
		q.Limit = 10
		torrents, total, err := store.Search(ctx, q)
		require.NoError(t, err, "search failed")
		got := make([]string, len(torrents))
		for i, torrent := range torrents {
			got[i] = torrent.Name
		}
		return got, total
	}

	got, total := names(Search{Trackers: []string{"alpha"}})
	require.Equal(t, []string{"Sample One", "Sample Three"}, got, "one tracker")
	require.Equal(t, 2, total, "one tracker")

	got, total = names(Search{Trackers: []string{"alpha", "beta"}})
	require.Equal(t, []string{"Sample One", "Sample Two", "Sample Three"}, got,
		"several trackers, merged newest-first")
	require.Equal(t, 3, total, "several trackers")

	// No restriction: every tracker in the store, including one no caller
	// named.
	got, total = names(Search{})
	require.Len(t, got, 4, "no tracker restriction")
	require.Equal(t, 4, total, "no tracker restriction")

	// The other clauses still apply on top of a multi-tracker scope.
	got, _ = names(Search{Terms: []string{"three"}, Trackers: []string{"alpha", "beta"}})
	require.Equal(t, []string{"Sample Three"}, got, "terms with several trackers")

	got, _ = names(Search{Trackers: []string{"alpha", "gamma"}})
	require.Equal(t, []string{"Sample One", "Sample Three", "Sample Four"}, got, "non-adjacent trackers")
}

func TestSearch_QueryKeyScopesScrapedRows(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "query-key.db"))
	require.NoError(t, err)
	defer store.Close()

	first := Torrent{Name: "First Example", Tracker: "demo"}
	second := Torrent{Name: "Second Example", Tracker: "demo"}
	_, err = store.UpsertAllForSearch(ctx, []Torrent{first}, "first-key")
	require.NoError(t, err)
	_, err = store.UpsertAllForSearch(ctx, []Torrent{second}, "second-key")
	require.NoError(t, err)

	rows, total, err := store.Search(ctx, Search{Limit: 10, QueryKey: "first-key", Trackers: []string{"demo"}})
	require.NoError(t, err)
	require.Equal(t, 1, total)
	require.Len(t, rows, 1)
	require.Equal(t, first.Name, rows[0].Name)

	rows, total, err = store.Search(ctx, Search{Limit: 10, QueryKey: "unknown-key", Trackers: []string{"demo"}})
	require.NoError(t, err)
	require.Zero(t, total, "an unknown search key matched rows")
	require.Empty(t, rows)
}
