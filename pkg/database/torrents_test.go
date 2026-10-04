// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package database

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/torrplay/jacklet/pkg/scraper"
)

func TestStoreCRUDAndStats(t *testing.T) {
	ctx := t.Context()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "crud.db"))
	require.NoError(t, err)
	defer store.Close()

	require.NoError(t, store.Ping(ctx))

	rows := []scraper.Torrent{
		{Categories: []int{2000}, Name: "Example Older", Published: "2024-01-01T00:00:00Z", Tracker: "alpha"},
		{Categories: []int{5000, 100022}, Name: "Example Newer", Published: "2024-01-02T00:00:00Z", Tracker: "alpha"},
		{Name: "Example Other", Published: "2024-01-03T00:00:00Z", Tracker: "beta"},
	}
	count, err := store.UpsertAll(ctx, rows)
	require.NoError(t, err)
	require.Equal(t, len(rows), count)

	count, err = store.UpsertAll(ctx, nil)
	require.NoError(t, err, "an empty UpsertAll")
	require.Zero(t, count, "an empty UpsertAll stored rows")

	recent, _, err := store.Search(ctx, scraper.Query{Limit: 1, Trackers: []string{"alpha"}})
	require.NoError(t, err)
	require.Len(t, recent, 1)
	require.Equal(t, "Example Newer", recent[0].Name, "Search did not return the newest alpha row")

	got, err := store.Find(ctx, "alpha", recent[0].ID)
	require.NoError(t, err)
	require.Equal(t, rows[1].Name, got.Name)
	require.Equal(t, rows[1].Categories, got.Categories, "a row's categories did not come back whole, in order")

	_, err = store.Find(ctx, "beta", recent[0].ID)
	require.ErrorIs(t, err, scraper.ErrNotFound, "a row was found under the wrong tracker")

	stats, err := store.Stats(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, stats["alpha"].Torrents)
	require.True(t, stats["alpha"].Newest.Equal(time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)),
		"alpha newest = %v", stats["alpha"].Newest)

	require.Equal(t, 1, stats["beta"].Torrents)
}

func TestPrune(t *testing.T) {
	store, err := Open(t.Context(), filepath.Join(t.TempDir(), "prune.db"))
	require.NoError(t, err, "failed to initialize database")
	defer store.Close()

	insert := func(name, published string) {
		t.Helper()
		_, err := store.db.Exec(
			`INSERT INTO torrents (name, tracker, published) VALUES (?, ?, ?)`,
			name, "tracker", published,
		)
		require.NoError(t, err, "failed to insert %s", name)
	}

	now := time.Now().UTC()
	insert("ancient", now.Add(-90*24*time.Hour).Format(time.RFC3339))
	insert("recent", now.Add(-1*time.Hour).Format(time.RFC3339))
	insert("undated ancient", "")
	insert("undated recent", "")
	_, err = store.db.Exec(`UPDATE torrents SET last_seen = ? WHERE name = ?`,
		now.Add(-90*24*time.Hour).Format(time.RFC3339), "undated ancient")
	require.NoError(t, err)
	_, err = store.db.Exec(`UPDATE torrents SET last_seen = ? WHERE name = ?`,
		now.Add(-1*time.Hour).Format(time.RFC3339), "undated recent")
	require.NoError(t, err)

	removed, err := store.Prune(t.Context(), 30*24*time.Hour)
	require.NoError(t, err)
	require.Equal(t, int64(2), removed, "Prune removed the wrong number of rows")

	rows, err := store.db.Query(`SELECT name FROM torrents ORDER BY name`)
	require.NoError(t, err, "failed to query remaining rows")
	defer rows.Close()

	var remaining []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		remaining = append(remaining, name)
	}
	require.NoError(t, rows.Err(), "row iteration failed")

	require.Equal(t, []string{"recent", "undated recent"}, remaining)
}

// Two different releases that happen to share a title must stay two rows.
// Keying identity on the title collapsed them into one, whose size and
// swarm counts were whichever release was scraped last.
func TestTorrentIdentityDistinguishesSameTitledReleases(t *testing.T) {
	store, err := Open(t.Context(), filepath.Join(t.TempDir(), "identity.db"))
	require.NoError(t, err, "failed to initialize database")
	defer store.Close()

	insert := func(name, infohash, url string, size int64) {
		t.Helper()
		_, err := store.db.Exec(
			`INSERT INTO torrents (name, tracker, infohash, details_url, size) VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT(tracker, identity) DO UPDATE SET size = excluded.size`,
			name, "t", infohash, url, size)
		require.NoError(t, err, "failed to insert %q", name)
	}

	// Same title, different torrents: both are kept.
	insert("Some Release", "aaaa", "", 1)
	insert("Some Release", "bbbb", "", 2)
	// Same info hash in different case is the same torrent, refreshed.
	insert("Some Release", "AAAA", "", 3)
	// No info hash: the details permalink separates them.
	insert("Other Release", "", "https://x/1", 4)
	insert("Other Release", "", "https://x/2", 5)
	// Neither: the title is the last resort, so this one refreshes.
	insert("Bare Release", "", "", 6)
	insert("Bare Release", "", "", 7)

	var rows int
	require.NoError(t, store.db.QueryRow(`SELECT COUNT(*) FROM torrents`).Scan(&rows), "failed to count rows")
	require.Equal(t, 5, rows, "stored the wrong number of rows")

	// The upsert refreshed the row first stored under the lowercase hash.
	var size int64
	require.NoError(t, store.db.QueryRow(`SELECT size FROM torrents WHERE infohash = ?`, "aaaa").Scan(&size),
		"failed to read the refreshed row")
	require.Equal(t, int64(3), size, "info hash differing only in case stored a second row")
}

// Listings order by the release date scraped from the tracker. Ordering by
// id is first-seen order, which ranks an old release scraped today above
// everything stored after it.
func TestNewestFirstOrdersByReleaseDate(t *testing.T) {
	store, err := Open(t.Context(), filepath.Join(t.TempDir(), "order.db"))
	require.NoError(t, err, "failed to initialize database")
	defer store.Close()

	for _, row := range []struct{ name, published string }{
		{"Recent", "2024-06-01T00:00:00Z"},
		{"Undated", ""},
		// Stored last, released first.
		{"Ancient", "2001-01-01T00:00:00Z"},
	} {
		_, err := store.db.Exec(
			`INSERT INTO torrents (name, tracker, published) VALUES (?, ?, ?)`,
			row.name, "t", row.published)
		require.NoError(t, err, "failed to insert %q", row.name)
	}

	rows, err := store.db.Query(`SELECT name FROM torrents WHERE tracker = ? ORDER BY `+newestFirst, "t")
	require.NoError(t, err)
	defer rows.Close()

	var got []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		got = append(got, name)
	}
	require.NoError(t, rows.Err(), "failed to read rows")

	// Undated rows sort last, since their age is unknown.
	require.Equal(t, []string{"Recent", "Ancient", "Undated"}, got)
}
