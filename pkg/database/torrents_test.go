// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package database

import (
	"path/filepath"
	"strings"
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

// A backlog larger than one batch is deleted over several statements, each
// committed on its own, and every expired row goes, from both tables.
func TestStore_Prune_DeletesInBatches(t *testing.T) {
	store, err := Open(t.Context(), filepath.Join(t.TempDir(), "prune-batches.db"))
	require.NoError(t, err, "failed to initialize database")
	defer store.Close()

	now := time.Now().UTC()
	expired := now.Add(-90 * 24 * time.Hour).Format(time.RFC3339)
	recent := now.Add(-time.Hour).Format(time.RFC3339)
	for _, row := range []struct{ name, published string }{
		{"expired one", expired},
		{"expired three", expired},
		{"expired two", expired},
		{"recent", recent},
	} {
		_, err := store.db.Exec(`INSERT INTO torrents (name, tracker, published) VALUES (?, ?, ?)`,
			row.name, "tracker", row.published)
		require.NoError(t, err, "failed to insert %s", row.name)
	}
	var recentID int64
	require.NoError(t, store.db.QueryRow(`SELECT id FROM torrents WHERE name = 'recent'`).Scan(&recentID))
	for _, search := range []struct{ key, lastSeen string }{
		{"expired a", expired},
		{"expired b", expired},
		{"expired c", expired},
		{"recent", recent},
	} {
		_, err := store.db.Exec(
			`INSERT INTO torrent_searches (tracker, search_key, torrent_id, last_seen) VALUES (?, ?, ?, ?)`,
			"tracker", search.key, recentID, search.lastSeen)
		require.NoError(t, err, "failed to insert search %s", search.key)
	}

	removed, err := store.prune(t.Context(), 30*24*time.Hour, 2)
	require.NoError(t, err)
	require.Equal(t, int64(3), removed, "the count must add up every batch")

	var names []string
	rows, err := store.db.Query(`SELECT name FROM torrents`)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		names = append(names, name)
	}
	require.NoError(t, rows.Err())
	require.Equal(t, []string{"recent"}, names)

	var keys []string
	searches, err := store.db.Query(`SELECT search_key FROM torrent_searches`)
	require.NoError(t, err)
	defer searches.Close()
	for searches.Next() {
		var key string
		require.NoError(t, searches.Scan(&key))
		keys = append(keys, key)
	}
	require.NoError(t, searches.Err())
	require.Equal(t, []string{"recent"}, keys, "expired searches must go even when they outnumber a batch")
}

// Each batch reads only the rows it deletes, through an index on the column
// it selects them by, rather than walking the table; a batch that scanned
// would make a large backlog cost one full scan per batch.
func TestBatchedDeletesReadAnIndex(t *testing.T) {
	store, err := Open(t.Context(), filepath.Join(t.TempDir(), "prune-plan.db"))
	require.NoError(t, err, "failed to initialize database")
	defer store.Close()

	for _, tc := range []struct{ name, statement, wantIndex string }{
		{name: "a tracker's torrents", statement: clearTrackerBatch, wantIndex: "idx_torrents_tracker_published"},
		{name: "dated torrents", statement: pruneDatedBatch, wantIndex: "idx_torrents_dated_published"},
		{name: "searches", statement: pruneSearchesBatch, wantIndex: "idx_torrent_searches_last_seen"},
		{name: "undated torrents", statement: pruneUndatedBatch, wantIndex: "idx_torrents_undated_last_seen"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := store.db.QueryContext(t.Context(), `EXPLAIN QUERY PLAN `+tc.statement, "value", 1, 1)
			require.NoError(t, err)
			defer rows.Close()
			var plan []string
			for rows.Next() {
				var id, parent, unused int
				var detail string
				require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
				plan = append(plan, detail)
			}
			require.NoError(t, rows.Err())
			require.Contains(t, strings.Join(plan, "\n"), tc.wantIndex, "the batch does not read its index")
		})
	}
}

// A row stored after a sweep began is left for the next one, however old
// its release, so scrapes storing old releases cannot keep a sweep going.
func TestStore_PruneThrough_LeavesRowsPastTheBound(t *testing.T) {
	store, err := Open(t.Context(), filepath.Join(t.TempDir(), "prune-bound.db"))
	require.NoError(t, err, "failed to initialize database")
	defer store.Close()

	expired := time.Now().UTC().Add(-90 * 24 * time.Hour).Format(time.RFC3339)
	names := []string{"before the sweep", "during the sweep"}
	ids := make([]int64, 0, len(names))
	for _, name := range names {
		res, err := store.db.Exec(`INSERT INTO torrents (name, tracker, published) VALUES (?, ?, ?)`,
			name, "tracker", expired)
		require.NoError(t, err, "failed to insert %s", name)
		id, err := res.LastInsertId()
		require.NoError(t, err)
		ids = append(ids, id)
	}

	cutoff := time.Now().UTC().Add(-30 * 24 * time.Hour).Format(time.RFC3339)
	removed, err := store.pruneThrough(t.Context(), cutoff, 0, ids[0], 1)
	require.NoError(t, err)
	require.Equal(t, int64(1), removed)

	var name string
	require.NoError(t, store.db.QueryRow(`SELECT name FROM torrents`).Scan(&name))
	require.Equal(t, "during the sweep", name, "a row past the bound was deleted")
}

// A row stored after a clear began is kept, so a scrape storing while it
// runs cannot keep a clear going.
func TestStore_ClearThrough_LeavesRowsPastTheBound(t *testing.T) {
	store, err := Open(t.Context(), filepath.Join(t.TempDir(), "clear-bound.db"))
	require.NoError(t, err, "failed to initialize database")
	defer store.Close()

	names := []string{"before the clear", "during the clear"}
	ids := make([]int64, 0, len(names))
	for _, name := range names {
		res, err := store.db.Exec(`INSERT INTO torrents (name, tracker) VALUES (?, ?)`, name, "tracker")
		require.NoError(t, err, "failed to insert %s", name)
		id, err := res.LastInsertId()
		require.NoError(t, err)
		ids = append(ids, id)
	}

	removed, err := store.clearThrough(t.Context(), "tracker", ids[0])
	require.NoError(t, err)
	require.Equal(t, int64(1), removed)

	var name string
	require.NoError(t, store.db.QueryRow(`SELECT name FROM torrents`).Scan(&name))
	require.Equal(t, "during the clear", name, "a row past the bound was deleted")
}

func TestStore_Prune_RefusesABatchSizeBelowOne(t *testing.T) {
	store, err := Open(t.Context(), filepath.Join(t.TempDir(), "prune-size.db"))
	require.NoError(t, err, "failed to initialize database")
	defer store.Close()

	for _, size := range []int{0, -1} {
		_, err := store.prune(t.Context(), time.Hour, size)
		require.ErrorContains(t, err, "invalid prune batch size", "a batch size of %d would never finish", size)
	}
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
