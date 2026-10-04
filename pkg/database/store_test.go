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
	"github.com/torrplay/jacklet/pkg/scraper"
)

// SQLite serializes writers, so a concurrent server needs WAL journaling
// and a busy timeout; without them competing scrapes fail with
// SQLITE_BUSY and silently lose rows.
func TestInitDBConcurrentWrites(t *testing.T) {
	store, err := Open(t.Context(), filepath.Join(t.TempDir(), "concurrent.db"))
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
func TestInitDBIndexesTrackerLookups(t *testing.T) {
	store, err := Open(t.Context(), filepath.Join(t.TempDir(), "indexed.db"))
	require.NoError(t, err, "failed to initialize database")
	defer store.Close()

	var plan string
	row := store.db.QueryRow(`EXPLAIN QUERY PLAN SELECT name FROM torrents WHERE tracker = ? ORDER BY `+newestFirst, "t")
	var id, parent, notUsed int
	require.NoError(t, row.Scan(&id, &parent, &notUsed, &plan), "failed to explain query")
	require.Contains(t, plan, "idx_torrents_tracker_published",
		"per-tracker query does not use the tracker index")
}

// The schema is authoritative: there is no migration path, so every column
// carries its own default and NULL is not representable.
func TestInitDBSchemaRejectsNulls(t *testing.T) {
	store, err := Open(t.Context(), filepath.Join(t.TempDir(), "notnull.db"))
	require.NoError(t, err, "failed to initialize database")
	defer store.Close()

	t.Run("a minimal row takes the defaults", func(t *testing.T) {
		_, err := store.db.Exec(`INSERT INTO torrents (name, tracker) VALUES (?, ?)`, "Example", "t")
		require.NoError(t, err, "minimal insert failed")

		var seeders, size, grabs int
		var downloadFactor, uploadFactor float64
		var categories, published, url string
		require.NoError(t, store.db.QueryRow(
			`SELECT seeders, size, categories, grabs, download_volume_factor, upload_volume_factor, published, details_url
			 FROM torrents WHERE name = ?`, "Example").
			Scan(&seeders, &size, &categories, &grabs, &downloadFactor, &uploadFactor, &published, &url),
			"scanning defaults failed")

		require.Zero(t, seeders, "seeders")
		require.Zero(t, size, "size")
		require.Empty(t, categories, "categories")
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
	store, err := Open(t.Context(), filepath.Join(t.TempDir(), "ctx.db"))
	require.NoError(t, err, "failed to initialize database")
	defer store.Close()

	_, err = store.db.Exec(`INSERT INTO torrents (name, tracker) VALUES (?, ?)`, "Example", "t")
	require.NoError(t, err)

	canceled, cancel := context.WithCancel(t.Context())
	cancel()

	t.Run("Stats", func(t *testing.T) {
		_, err := store.Stats(canceled)
		require.ErrorIs(t, err, context.Canceled)
	})

	t.Run("Search", func(t *testing.T) {
		_, _, err := store.Search(canceled, scraper.Query{Limit: 1})
		require.ErrorIs(t, err, context.Canceled)
	})

	t.Run("Find", func(t *testing.T) {
		_, err := store.Find(canceled, "t", 1)
		require.ErrorIs(t, err, context.Canceled)
	})

	t.Run("Prune", func(t *testing.T) {
		_, err := store.Prune(canceled, time.Hour)
		require.ErrorIs(t, err, context.Canceled)
	})

	t.Run("a live context still works", func(t *testing.T) {
		stats, err := store.Stats(t.Context())
		require.NoError(t, err)
		require.Equal(t, 1, stats["t"].Torrents)
	})
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

			store, err := Open(t.Context(), path)
			if store != nil {
				store.Close()
			}
			require.ErrorContains(t, err, "delete the cache database",
				"the failure must name the recovery")
			require.ErrorContains(t, err, tc.table, "the failure must name the table")
		})
	}
}

// userVersion reads the schema version stamped on the database at path,
// straight from SQLite rather than through the store under test.
func userVersion(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer db.Close()
	var version int
	require.NoError(t, db.QueryRow(`PRAGMA user_version`).Scan(&version))
	return version
}

// setUserVersion stamps a database with an arbitrary schema version.
func setUserVersion(t *testing.T, path string, version int) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer db.Close()
	_, err = db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, version))
	require.NoError(t, err)
}

// TestOpen_StampsTheSchemaVersion covers a database whose columns resolve
// but which a different build stamped: opening it would succeed and
// quietly write rows that disagree with the ones already there, so it is
// refused with the same recovery as a missing column. A database no build
// has stamped is adopted, since the column check has already vetted it.
func TestOpen_StampsTheSchemaVersion(t *testing.T) {
	t.Run("a new database is stamped", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "fresh.db")
		store, err := Open(t.Context(), path)
		require.NoError(t, err)
		require.NoError(t, store.Close())

		require.Equal(t, schemaVersion, userVersion(t, path))
	})

	for _, version := range []int{schemaVersion + 1, schemaVersion + 100} {
		t.Run(fmt.Sprintf("version %d is refused", version), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "other.db")
			store, err := Open(t.Context(), path)
			require.NoError(t, err)
			require.NoError(t, store.Close())
			setUserVersion(t, path, version)

			store, err = Open(t.Context(), path)
			if store != nil {
				store.Close()
			}
			require.ErrorContains(t, err, "delete the cache database", "the failure must name the recovery")
			require.ErrorContains(t, err, fmt.Sprintf("schema version %d", version), "the failure must name the version found")
			require.Equal(t, version, userVersion(t, path), "a refused database must be left as it was")
		})
	}

	t.Run("an unstamped database with this schema is adopted and stamped", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "unstamped.db")
		store, err := Open(t.Context(), path)
		require.NoError(t, err)
		require.NoError(t, store.Close())
		setUserVersion(t, path, 0)

		store, err = Open(t.Context(), path)
		require.NoError(t, err, "a database written before stamping existed must still open")
		require.NoError(t, store.Close())
		require.Equal(t, schemaVersion, userVersion(t, path))
	})
}

// TestOpen_IndexesTheCascadeKey covers the foreign key from
// torrent_searches to torrents: deleting a torrent cascades by looking its
// searches up on torrent_id, and the primary key leads with other columns,
// so without an index that leads with torrent_id every deleted torrent
// scans the whole table and a prune grows quadratically while it holds the
// only connection. Asserting the index rather than a timing keeps the test
// deterministic.
func TestOpen_IndexesTheCascadeKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh.db")
	store, err := Open(t.Context(), path)
	require.NoError(t, err)
	require.NoError(t, store.Close())

	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer db.Close()

	var names []string
	func() {
		indexes, err := db.Query(`SELECT name FROM pragma_index_list('torrent_searches')`)
		require.NoError(t, err)
		defer indexes.Close()
		for indexes.Next() {
			var name string
			require.NoError(t, indexes.Scan(&name))
			names = append(names, name)
		}
		require.NoError(t, indexes.Err())
	}()

	for _, name := range names {
		var leading string
		require.NoError(t, db.QueryRow(
			`SELECT name FROM pragma_index_info(?) WHERE seqno = 0`, name).Scan(&leading))
		if leading == "torrent_id" {
			return
		}
	}
	require.Fail(t, "no index on torrent_searches leads with torrent_id", "indexes: %v", names)
}

// TestOpenAcceptsItsOwnSchema is the other half: the check must pass on a
// database this build created, or every start fails.
func TestOpenAcceptsItsOwnSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh.db")

	store, err := Open(t.Context(), path)
	require.NoError(t, err)
	require.NoError(t, store.Close())

	// Reopening reaches the check with the tables already present, which
	// is the path a restart takes.
	store, err = Open(t.Context(), path)
	require.NoError(t, err, "a database this build created must reopen")
	require.NoError(t, store.Close())
}
