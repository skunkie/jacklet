// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

// Package database owns the SQLite store that holds scraped torrent
// listings. Every read and write of the torrents table goes through Store,
// so the table's shape, its identity rule and its sort order are stated
// once rather than restated by each package that touches it.
package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // register the sqlite driver
)

// busyTimeout is how long a blocked writer waits for a competing write to
// finish before giving up with SQLITE_BUSY.
const busyTimeout = 5 * time.Second

// ErrNotFound is returned by Find when no stored torrent matches.
var ErrNotFound = errors.New("torrent not found")

// Torrent is one scraped listing as it is stored. It is the single shape
// the store reads and writes; a caller renders it into whatever its own
// wire format needs.
type Torrent struct {
	// Album, Artist, Label and Track describe a music release, and Author,
	// BookTitle and Publisher a book: the metadata Lidarr and Readarr match
	// on, which a definition for a music or ebook tracker scrapes.
	Album  string
	Artist string
	Author string
	// BookTitle is the work's title, as distinct from Name, which is the
	// release's own title on the tracker.
	BookTitle string
	// Category is the standard Torznab category id the definition's
	// caps.categorymappings resolved the row to.
	Category    int
	Description string
	// DetailsURL is the release's page on the tracker, absolute, when the
	// definition scrapes one.
	DetailsURL string
	DoubanID   string
	// DownloadURL is what to fetch to get the torrent: a magnet URI, or an
	// absolute HTTP link to a .torrent file.
	DownloadURL          string
	DownloadVolumeFactor float64
	Files                int
	// Genres is the release's genres as one comma-separated list, already
	// in the form the Torznab "genre" attribute takes.
	Genres string
	Grabs  int
	// ID is the store's own row id, assigned on first insert and stable
	// across the refreshes of later scrapes.
	ID       int64
	IMDBID   string
	InfoHash string
	Label    string
	Leechers int
	// Magnet is the release's magnet URI when the definition scrapes one,
	// alongside rather than instead of DownloadURL: a tracker commonly
	// offers both, and they are fetched differently.
	Magnet          string
	MinimumRatio    float64
	MinimumSeedTime int
	Name            string
	// Poster is an absolute URL to the release's cover image, served as the
	// Torznab "coverurl" attribute.
	Poster string
	// Published is the release date as RFC 3339 in UTC, or empty when the
	// definition yielded no parseable date. Empty means "unknown", not
	// "the zero time", which would otherwise look infinitely old.
	Published string
	Publisher string
	RageID    string
	Seeders   int
	Size      int64
	TMDBID    string
	TVDBID    string
	TVMazeID  string
	Track     string
	// Tracker is the tracker id (see scraper.TrackerID) the row was
	// scraped from.
	Tracker            string
	TraktID            string
	UploadVolumeFactor float64
	Year               int
}

// Store is the torrents table. It is safe for concurrent use.
type Store struct {
	db *sql.DB
}

// Open opens the store at dataSourceName and creates its schema.
//
// The schema is created fresh and never migrated: the torrents table is a
// cache of scraped results that any search refills, so a schema change is
// answered by deleting the database file rather than by carrying migration
// code for every past shape.
//
// SQLite allows only one writer at a time, so the underlying pool is
// configured for a concurrent HTTP server: WAL journaling lets readers run
// alongside the writer, a busy timeout makes a blocked writer wait instead
// of failing immediately, and the pool is capped at a single connection so
// concurrent scrapes queue rather than collide.
func Open(ctx context.Context, dataSourceName string) (*Store, error) {
	// The folding function must be registered before any connection is
	// opened, since the driver only applies it to connections made after.
	if err := registerFold(); err != nil {
		return nil, fmt.Errorf("failed to register the case-folding function: %w", err)
	}

	db, err := sql.Open("sqlite", dataSourceName+pragmaSuffix(dataSourceName))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)

	s := &Store{db: db}
	if err := s.createSchema(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the store's database handle.
func (s *Store) Close() error { return s.db.Close() }

// Ping reports whether the database is reachable, for a health probe.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func (s *Store) createSchema(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS torrents (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			tracker TEXT NOT NULL,
			details_url TEXT NOT NULL DEFAULT '',
			download_url TEXT NOT NULL DEFAULT '',
			seeders INTEGER NOT NULL DEFAULT 0,
			leechers INTEGER NOT NULL DEFAULT 0,
			magnet TEXT NOT NULL DEFAULT '',
			size INTEGER NOT NULL DEFAULT 0,
			published TEXT NOT NULL DEFAULT '',
			category INTEGER NOT NULL DEFAULT 0,
			description TEXT NOT NULL DEFAULT '',
			infohash TEXT NOT NULL DEFAULT '',
			grabs INTEGER NOT NULL DEFAULT 0,
			files INTEGER NOT NULL DEFAULT 0,
			imdbid TEXT NOT NULL DEFAULT '',
			tvdbid TEXT NOT NULL DEFAULT '',
			tmdbid TEXT NOT NULL DEFAULT '',
			tvmazeid TEXT NOT NULL DEFAULT '',
			traktid TEXT NOT NULL DEFAULT '',
			doubanid TEXT NOT NULL DEFAULT '',
			rageid TEXT NOT NULL DEFAULT '',
			genres TEXT NOT NULL DEFAULT '',
			year INTEGER NOT NULL DEFAULT 0,
			poster TEXT NOT NULL DEFAULT '',
			author TEXT NOT NULL DEFAULT '',
			booktitle TEXT NOT NULL DEFAULT '',
			publisher TEXT NOT NULL DEFAULT '',
			artist TEXT NOT NULL DEFAULT '',
			album TEXT NOT NULL DEFAULT '',
			label TEXT NOT NULL DEFAULT '',
			track TEXT NOT NULL DEFAULT '',
			download_volume_factor REAL NOT NULL DEFAULT 1,
			upload_volume_factor REAL NOT NULL DEFAULT 1,
			minimum_ratio REAL NOT NULL DEFAULT 0,
			minimum_seed_time INTEGER NOT NULL DEFAULT 0,
			last_seen TEXT NOT NULL DEFAULT '',
			search_managed INTEGER NOT NULL DEFAULT 0,
			-- What distinguishes one torrent from another within a tracker.
			-- The info hash comes first because it identifies the torrent
			-- itself, then the details page because it is the tracker's own
			-- permalink for the listing, and only then the title.
			--
			-- The title is the last resort rather than the key: two different
			-- releases on one tracker routinely share a title — a re-upload,
			-- or one film listed at several qualities under a cleaned name —
			-- and keying on it collapsed them into a single row whose size and
			-- swarm counts were whichever one was scraped last.
			--
			-- It is derived here rather than at each insert so that every
			-- writer agrees on it. lower() folds ASCII only, which is exactly
			-- right for a hex info hash.
			identity TEXT NOT NULL GENERATED ALWAYS AS (
				CASE
					WHEN infohash != '' THEN 'btih:' || lower(infohash)
					WHEN details_url != '' THEN 'url:' || details_url
					ELSE 'title:' || name
				END
			) VIRTUAL,
			UNIQUE(tracker, identity)
		)
	`); err != nil {
		return err
	}

	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS torrent_searches (
			tracker TEXT NOT NULL,
			search_key TEXT NOT NULL,
			torrent_id INTEGER NOT NULL REFERENCES torrents(id) ON DELETE CASCADE,
			last_seen TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (tracker, search_key, torrent_id)
		)
	`); err != nil {
		return err
	}

	if err := s.verifySchema(ctx); err != nil {
		return err
	}

	// Every listing is scoped to one tracker and ordered by descending
	// release date, so the UNIQUE(tracker, identity) index doesn't serve
	// the sort. Index the access pattern the queries actually use.
	_, err := s.db.ExecContext(ctx,
		`CREATE INDEX IF NOT EXISTS idx_torrents_tracker_published ON torrents(tracker, published DESC, id DESC)`)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`CREATE INDEX IF NOT EXISTS idx_torrent_searches_last_seen ON torrent_searches(last_seen)`)
	return err
}

// pragmaSuffix builds the driver's PRAGMA query-string parameters, taking
// care not to mangle a DSN that already carries its own query string or
// that names an in-memory database.
func pragmaSuffix(dataSourceName string) string {
	sep := "?"
	if strings.Contains(dataSourceName, "?") {
		sep = "&"
	}
	return fmt.Sprintf("%s_pragma=journal_mode(WAL)&_pragma=busy_timeout(%d)&_pragma=foreign_keys(1)",
		sep, busyTimeout.Milliseconds())
}

// verifySchema checks that the tables this build reads and writes have the
// columns it expects, and reports how to recover when they do not.
//
// CREATE TABLE IF NOT EXISTS leaves an existing table exactly as it found
// it, so a database file written by a build with a different schema opens
// without complaint and then fails on the first query, with whatever
// SQLite says about the first column that happens to be missing. The
// schema is never migrated -- the torrents table is a cache any search
// refills -- so the recovery is to delete the file, and saying that at
// startup is worth more than the SQL error later.
//
// The column lists are the ones the queries themselves are built from, so
// this check widens on its own when a column is added and cannot fall
// behind the schema it guards.
//
// It checks that the columns resolve, and nothing more: a type, a
// constraint or an index that differs is not detected. Those are not
// shapes a build of this program produces, where adding or renaming a
// column is the ordinary change, and every index is created with IF NOT
// EXISTS and so repairs itself.
func (s *Store) verifySchema(ctx context.Context) error {
	for _, table := range []struct {
		columns string
		name    string
	}{
		{columns: torrentColumns + ", " + torrentInternalColumns, name: "torrents"},
		{columns: searchColumns, name: "torrent_searches"},
	} {
		// LIMIT 0 asks SQLite to resolve the column names without reading
		// a row, so this costs nothing on a large cache.
		//nolint:gosec // G202: both halves are fixed strings from the table above; the statement takes no values
		if _, err := s.db.ExecContext(ctx,
			`SELECT `+table.columns+` FROM `+table.name+` LIMIT 0`); err != nil {
			return fmt.Errorf(
				"the cache database has a %s table this build does not recognize; "+
					"delete the cache database and restart, and its contents are scraped again: %w",
				table.name, err)
		}
	}
	return nil
}
