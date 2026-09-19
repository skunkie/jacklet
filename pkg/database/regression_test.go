// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package database

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestStoreCRUDStatsAndRecent(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "crud.db"))
	require.NoError(t, err)
	defer store.Close()

	require.NoError(t, store.Ping(ctx))

	rows := []Torrent{
		{Category: 2000, Name: "Example Older", Published: "2024-01-01T00:00:00Z", Tracker: "alpha"},
		{Category: 5000, Name: "Example Newer", Published: "2024-01-02T00:00:00Z", Tracker: "alpha"},
		{Name: "Example Other", Published: "2024-01-03T00:00:00Z", Tracker: "beta"},
	}
	count, err := store.UpsertAll(ctx, rows)
	require.NoError(t, err)
	require.Equal(t, len(rows), count)

	count, err = store.UpsertAll(ctx, nil)
	require.NoError(t, err, "an empty UpsertAll")
	require.Zero(t, count, "an empty UpsertAll stored rows")

	recent, err := store.Recent(ctx, "alpha", 1)
	require.NoError(t, err)
	require.Len(t, recent, 1)
	require.Equal(t, "Example Newer", recent[0].Name, "Recent did not return the newest alpha row")

	got, err := store.Find(ctx, "alpha", recent[0].ID)
	require.NoError(t, err)
	require.Equal(t, rows[1].Name, got.Name)
	require.Equal(t, rows[1].Category, got.Category)

	_, err = store.Find(ctx, "beta", recent[0].ID)
	require.ErrorIs(t, err, ErrNotFound, "a row was found under the wrong tracker")

	stats, err := store.Stats(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, stats["alpha"].Torrents)
	require.True(t, stats["alpha"].Newest.Equal(time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)),
		"alpha newest = %v", stats["alpha"].Newest)

	total, err := store.Total(ctx)
	require.NoError(t, err)
	require.Equal(t, 3, total)
}

// SQLite serializes writers, so a concurrent server needs WAL journaling
// and a busy timeout; without them competing scrapes fail with
// SQLITE_BUSY and silently lose rows.
func TestInitDB_ConcurrentWrites(t *testing.T) {
	store, err := Open(context.Background(), filepath.Join(t.TempDir(), "concurrent.db"))
	require.NoError(t, err, "failed to initialize database")
	defer store.Close()

	var journalMode string
	require.NoError(t, store.db.QueryRow("PRAGMA journal_mode").Scan(&journalMode))
	require.Equal(t, "wal", journalMode, "journal_mode")

	var busyTimeoutMS int
	require.NoError(t, store.db.QueryRow("PRAGMA busy_timeout").Scan(&busyTimeoutMS))
	require.NotZero(t, busyTimeoutMS, "busy_timeout is unset; a blocked writer would fail immediately")

	const writers, perWriter = 16, 40
	var wg sync.WaitGroup
	errCh := make(chan error, writers*perWriter)
	for i := range writers {
		wg.Go(func() {
			for j := range perWriter {
				if _, err := store.db.Exec(
					`INSERT INTO torrents (name, tracker) VALUES (?, ?)`,
					fmt.Sprintf("torrent-%d-%d", i, j), "tracker",
				); err != nil {
					errCh <- err
				}
			}
		})
	}
	wg.Wait()
	close(errCh)

	failures := 0
	var first error
	for err := range errCh {
		if first == nil {
			first = err
		}
		failures++
	}
	require.Zero(t, failures, "%d/%d concurrent writes failed, first: %v", failures, writers*perWriter, first)
}

// Queries are scoped to one tracker and ordered newest-release-first, an
// access pattern the UNIQUE(tracker, identity) index cannot serve.
func TestInitDB_IndexesTrackerLookups(t *testing.T) {
	store, err := Open(context.Background(), filepath.Join(t.TempDir(), "indexed.db"))
	require.NoError(t, err, "failed to initialize database")
	defer store.Close()

	var plan string
	row := store.db.QueryRow(`EXPLAIN QUERY PLAN SELECT name FROM torrents WHERE tracker = ? ORDER BY `+newestFirst, "t")
	var id, parent, notUsed int
	require.NoError(t, row.Scan(&id, &parent, &notUsed, &plan), "failed to explain query")
	require.Contains(t, plan, "idx_torrents_tracker_published",
		"per-tracker query does not use the tracker index")
}

func TestPrune(t *testing.T) {
	store, err := Open(context.Background(), filepath.Join(t.TempDir(), "prune.db"))
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

	removed, err := store.Prune(context.Background(), 30*24*time.Hour)
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

// The schema is authoritative: there is no migration path, so every column
// carries its own default and NULL is not representable.
func TestInitDB_SchemaRejectsNulls(t *testing.T) {
	store, err := Open(context.Background(), filepath.Join(t.TempDir(), "notnull.db"))
	require.NoError(t, err, "failed to initialize database")
	defer store.Close()

	t.Run("a minimal row takes the defaults", func(t *testing.T) {
		_, err := store.db.Exec(`INSERT INTO torrents (name, tracker) VALUES (?, ?)`, "Example", "t")
		require.NoError(t, err, "minimal insert failed")

		var seeders, size, category, grabs int
		var downloadFactor, uploadFactor float64
		var published, url string
		require.NoError(t, store.db.QueryRow(
			`SELECT seeders, size, category, grabs, download_volume_factor, upload_volume_factor, published, details_url
			 FROM torrents WHERE name = ?`, "Example").
			Scan(&seeders, &size, &category, &grabs, &downloadFactor, &uploadFactor, &published, &url),
			"scanning defaults failed")

		require.Zero(t, seeders, "seeders")
		require.Zero(t, size, "size")
		require.Zero(t, category, "category")
		require.Zero(t, grabs, "grabs")
		require.Equal(t, float64(1), downloadFactor, "download volume factor")
		require.Equal(t, float64(1), uploadFactor, "upload volume factor")
		require.Empty(t, published, "published")
		require.Empty(t, url, "details_url")
	})

	t.Run("an explicit NULL is refused", func(t *testing.T) {
		_, err := store.db.Exec(
			`INSERT INTO torrents (name, tracker, seeders) VALUES (?, ?, NULL)`, "Nulled", "t")
		require.Error(t, err, "inserting NULL into a NOT NULL column succeeded")
	})
}

// Every store operation takes a context so a caller that goes away — a
// disconnected HTTP client, a shutdown — releases the single connection
// the pool allows instead of holding it until the query finishes.
func TestStoreOperationsHonorContextCancellation(t *testing.T) {
	store, err := Open(context.Background(), filepath.Join(t.TempDir(), "ctx.db"))
	require.NoError(t, err, "failed to initialize database")
	defer store.Close()

	_, err = store.db.Exec(`INSERT INTO torrents (name, tracker) VALUES (?, ?)`, "Example", "t")
	require.NoError(t, err)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	t.Run("Stats", func(t *testing.T) {
		_, err := store.Stats(cancelled)
		require.ErrorIs(t, err, context.Canceled)
	})

	t.Run("Total", func(t *testing.T) {
		_, err := store.Total(cancelled)
		require.ErrorIs(t, err, context.Canceled)
	})

	t.Run("Prune", func(t *testing.T) {
		_, err := store.Prune(cancelled, time.Hour)
		require.ErrorIs(t, err, context.Canceled)
	})

	t.Run("a live context still works", func(t *testing.T) {
		total, err := store.Total(context.Background())
		require.NoError(t, err)
		require.Equal(t, 1, total)
	})
}

// Two different releases that happen to share a title must stay two rows.
// Keying identity on the title collapsed them into one, whose size and
// swarm counts were whichever release was scraped last.
func TestTorrentIdentity_DistinguishesSameTitledReleases(t *testing.T) {
	store, err := Open(context.Background(), filepath.Join(t.TempDir(), "identity.db"))
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
func TestNewestFirst_OrdersByReleaseDate(t *testing.T) {
	store, err := Open(context.Background(), filepath.Join(t.TempDir(), "order.db"))
	require.NoError(t, err, "failed to initialize database")
	defer store.Close()

	for _, row := range []struct{ name, published string }{
		{"Recent", "2024-06-01T00:00:00Z"},
		{"Undated", ""},
		{"Ancient", "2001-01-01T00:00:00Z"}, // stored last, released first
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

// TestOpenRejectsACacheMissingAColumn covers the file a schema change
// leaves behind: CREATE TABLE IF NOT EXISTS keeps an existing table as it
// found it, so without a check the mismatch surfaces as whatever SQLite
// says about the first missing column, on the first search rather than at
// startup.
func TestOpenRejectsACacheMissingAColumn(t *testing.T) {
	for _, tc := range []struct {
		name   string
		schema string
		table  string
	}{
		{
			name:  "a torrents table from a narrower schema",
			table: "torrents",
			schema: `CREATE TABLE torrents (
				id INTEGER PRIMARY KEY,
				name TEXT NOT NULL,
				tracker TEXT NOT NULL)`,
		},
		{
			// The check covers every column the queries name, including
			// the ones the store keeps for itself and never returns.
			name:   "a torrents table missing only an internal column",
			table:  "torrents",
			schema: `CREATE TABLE torrents (` + torrentColumns + `, last_seen TEXT NOT NULL DEFAULT '')`,
		},
		{
			// identity is generated and never selected, but it is the
			// conflict target of every upsert: a table without it reads
			// fine and fails the first write, which is the cryptic
			// failure this check exists to replace.
			name:  "a torrents table missing the generated identity column",
			table: "torrents",
			schema: `CREATE TABLE torrents (` + torrentColumns +
				`, last_seen TEXT NOT NULL DEFAULT '', search_managed INTEGER NOT NULL DEFAULT 0)`,
		},
		{
			name:  "a torrent_searches table from a narrower schema",
			table: "torrent_searches",
			schema: `CREATE TABLE torrent_searches (
				tracker TEXT NOT NULL,
				search_key TEXT NOT NULL)`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "stale.db")
			db, err := sql.Open("sqlite", path)
			require.NoError(t, err)
			_, err = db.Exec(tc.schema)
			require.NoError(t, err)
			require.NoError(t, db.Close())

			store, err := Open(context.Background(), path)
			if store != nil {
				store.Close()
			}
			require.ErrorContains(t, err, "delete the cache database",
				"the failure must name the recovery")
			require.ErrorContains(t, err, tc.table, "the failure must name the table")
		})
	}
}

// TestOpenAcceptsItsOwnSchema is the other half: the check must pass on a
// database this build created, or every start fails.
func TestOpenAcceptsItsOwnSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh.db")

	store, err := Open(context.Background(), path)
	require.NoError(t, err)
	require.NoError(t, store.Close())

	// Reopening reaches the check with the tables already present, which
	// is the path a restart takes.
	store, err = Open(context.Background(), path)
	require.NoError(t, err, "a database this build created must reopen")
	require.NoError(t, store.Close())
}
