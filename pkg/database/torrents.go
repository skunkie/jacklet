// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/torrplay/jacklet/pkg/scraper"
)

// newestFirst is the ORDER BY clause every user-facing listing uses.
//
// The sort is on the release date scraped from the tracker, not on the row
// id. Upsert refreshes a row in place and leaves its id alone, so ordering
// by id is first-seen order — which would rank a year-old release scraped
// today above everything stored after it. Rows whose date could not be
// parsed hold an empty string, which sorts last; the id breaks ties,
// including for a tracker that yields no dates at all.
const newestFirst = "published DESC, id DESC"

// torrentColumns is the column list every read shares, in the order
// scanTorrent expects.
const torrentColumns = `id, name, tracker, details_url, download_url, magnet, seeders, leechers, size,
	published, category, description, infohash, grabs, files, imdbid, tvdbid,
	download_volume_factor, upload_volume_factor, minimum_ratio, minimum_seed_time,
	tmdbid, tvmazeid, traktid, doubanid, rageid, genres, year, poster,
	author, booktitle, publisher, artist, album, label, track`

// torrentInternalColumns are the columns of "torrents" the store
// maintains for itself rather than returning to a caller: what
// distinguishes one torrent from another within a tracker, when a row was
// last seen, and whether a scraper search produced it. They are listed
// separately because torrentColumns is what a read selects, and together
// the two are every column this build touches -- which is what
// verifySchema checks the database actually has.
//
// "identity" earns its place here even though no query selects it: it is
// the conflict target every upsert resolves against, so a table without
// it accepts a read and fails the first write.
const torrentInternalColumns = `identity, last_seen, search_managed`

// searchColumns is every column of "torrent_searches", which records
// which rows a given scraper search produced.
const searchColumns = `tracker, search_key, torrent_id, last_seen`

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanTorrent reads one row selected with torrentColumns.
func scanTorrent(row rowScanner) (scraper.Torrent, error) {
	var t scraper.Torrent
	err := row.Scan(
		&t.ID, &t.Name, &t.Tracker, &t.DetailsURL, &t.DownloadURL, &t.Magnet, &t.Seeders, &t.Leechers, &t.Size,
		&t.Published, &t.Category, &t.Description, &t.InfoHash, &t.Grabs, &t.Files, &t.IMDBID, &t.TVDBID,
		&t.DownloadVolumeFactor, &t.UploadVolumeFactor, &t.MinimumRatio, &t.MinimumSeedTime,
		&t.TMDBID, &t.TVMazeID, &t.TraktID, &t.DoubanID, &t.RageID, &t.Genres, &t.Year, &t.Poster,
		&t.Author, &t.BookTitle, &t.Publisher, &t.Artist, &t.Album, &t.Label, &t.Track,
	)
	return t, err
}

// upsertSQL stores a scraped torrent, refreshing one already seen for its
// tracker rather than skipping it: swarm counts, size and category all
// drift between scrapes, so keeping the first-seen values would defeat
// re-scraping. A published date is overwritten only when the new scrape
// actually parsed one.
//
// Which rows count as the same torrent is the table's own identity rule,
// not the caller's (see createSchema).
const upsertSQL = `
	INSERT INTO torrents (
		name, tracker, details_url, download_url, magnet, seeders, leechers, size, published, category,
		description, infohash, grabs, files, imdbid, tvdbid,
		download_volume_factor, upload_volume_factor, minimum_ratio, minimum_seed_time, last_seen, search_managed,
		tmdbid, tvmazeid, traktid, doubanid, rageid, genres, year, poster,
	author, booktitle, publisher, artist, album, label, track)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
		?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(tracker, identity) DO UPDATE SET
		name = excluded.name,
		details_url = excluded.details_url,
		download_url = excluded.download_url,
		magnet = excluded.magnet,
		seeders = excluded.seeders,
		leechers = excluded.leechers,
		size = excluded.size,
		published = CASE WHEN excluded.published != '' THEN excluded.published ELSE torrents.published END,
		category = excluded.category,
		description = excluded.description,
		infohash = excluded.infohash,
		grabs = excluded.grabs,
		files = excluded.files,
		imdbid = excluded.imdbid,
		tvdbid = excluded.tvdbid,
		download_volume_factor = excluded.download_volume_factor,
		upload_volume_factor = excluded.upload_volume_factor,
		minimum_ratio = excluded.minimum_ratio,
		minimum_seed_time = excluded.minimum_seed_time,
		last_seen = excluded.last_seen,
		search_managed = CASE WHEN excluded.search_managed = 1 THEN 1 ELSE torrents.search_managed END,
		tmdbid = excluded.tmdbid,
		tvmazeid = excluded.tvmazeid,
		traktid = excluded.traktid,
		doubanid = excluded.doubanid,
		rageid = excluded.rageid,
		genres = excluded.genres,
		year = excluded.year,
		poster = excluded.poster,
		author = excluded.author,
		booktitle = excluded.booktitle,
		publisher = excluded.publisher,
		artist = excluded.artist,
		album = excluded.album,
		label = excluded.label,
		track = excluded.track`

// upsertArgs binds a torrent to upsertSQL's placeholders.
func upsertArgs(t *scraper.Torrent, lastSeen string, searchManaged bool) []any {
	return []any{
		t.Name, t.Tracker, t.DetailsURL, t.DownloadURL, t.Magnet, t.Seeders, t.Leechers, t.Size, t.Published, t.Category,
		t.Description, t.InfoHash, t.Grabs, t.Files, t.IMDBID, t.TVDBID,
		t.DownloadVolumeFactor, t.UploadVolumeFactor, t.MinimumRatio, t.MinimumSeedTime, lastSeen, searchManaged,
		t.TMDBID, t.TVMazeID, t.TraktID, t.DoubanID, t.RageID, t.Genres, t.Year, t.Poster,
		t.Author, t.BookTitle, t.Publisher, t.Artist, t.Album, t.Label, t.Track,
	}
}

// Upsert stores one scraped torrent. See upsertSQL for what "already
// stored" means and what a repeat scrape refreshes.
func (s *Store) Upsert(ctx context.Context, t scraper.Torrent) error {
	_, err := s.db.ExecContext(ctx, upsertSQL, upsertArgs(&t, nowRFC3339(), false)...)
	return err
}

// UpsertAll stores every torrent in one transaction, and reports how many
// rows it wrote. A scrape yields a page of rows at once and the pool is
// capped at a single connection, so committing each row on its own would
// cost a separate transaction per row, each serializing against every
// concurrent reader.
func (s *Store) UpsertAll(ctx context.Context, torrents []scraper.Torrent) (int, error) {
	return s.upsertAll(ctx, torrents, "")
}

// UpsertAllForSearch stores every torrent as scraper-managed and records that
// each was returned for searchKey. Search can then answer a failed scrape only
// from rows that a matching request actually produced, including parameters
// that are not represented as torrent columns.
func (s *Store) UpsertAllForSearch(ctx context.Context, torrents []scraper.Torrent, searchKey string) (int, error) {
	return s.upsertAll(ctx, torrents, searchKey)
}

func (s *Store) upsertAll(ctx context.Context, torrents []scraper.Torrent, searchKey string) (int, error) {
	if len(torrents) == 0 {
		return 0, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	// A rollback after a successful commit is a no-op, so this needs no
	// condition; its error is the one Commit already reported.
	defer tx.Rollback() //nolint:errcheck // see above
	statement := upsertSQL
	if searchKey != "" {
		statement += " RETURNING id"
	}
	stmt, err := tx.PrepareContext(ctx, statement)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	lastSeen := nowRFC3339()
	for i := range torrents {
		if searchKey == "" {
			if _, err := stmt.ExecContext(ctx, upsertArgs(&torrents[i], lastSeen, false)...); err != nil {
				return 0, fmt.Errorf("storing %q: %w", torrents[i].Name, err)
			}
			continue
		}

		var torrentID int64
		if err := stmt.QueryRowContext(ctx, upsertArgs(&torrents[i], lastSeen, true)...).Scan(&torrentID); err != nil {
			return 0, fmt.Errorf("storing %q: %w", torrents[i].Name, err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO torrent_searches (tracker, search_key, torrent_id, last_seen)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(tracker, search_key, torrent_id) DO UPDATE SET last_seen = excluded.last_seen`,
			torrents[i].Tracker, searchKey, torrentID, lastSeen); err != nil {
			return 0, fmt.Errorf("recording search for %q: %w", torrents[i].Name, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(torrents), nil
}

func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// Find returns one stored torrent by its row id, scoped to a tracker so
// one indexer's endpoint cannot reach another's rows. It returns
// scraper.ErrNotFound when there is no such row.
func (s *Store) Find(ctx context.Context, tracker string, id int64) (scraper.Torrent, error) {
	t, err := scanTorrent(s.db.QueryRowContext(ctx,
		`SELECT `+torrentColumns+` FROM torrents WHERE id = ? AND tracker = ?`, id, tracker))
	if errors.Is(err, sql.ErrNoRows) {
		return scraper.Torrent{}, fmt.Errorf("%w: %s/%d", scraper.ErrNotFound, tracker, id)
	}
	return t, err
}

// Search returns a page of stored torrents, newest release first, along
// with the total number of matches across every page.
func (s *Store) Search(ctx context.Context, q scraper.Query) (torrents []scraper.Torrent, total int, err error) {
	whereSQL, args := whereClause(q)

	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM torrents WHERE `+whereSQL, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	//nolint:gosec // G202: torrentColumns, newestFirst and whereSQL are fixed strings and "?" placeholders; every value is bound
	pageSQL := `SELECT ` + torrentColumns + ` FROM torrents WHERE ` + whereSQL +
		` ORDER BY ` + newestFirst + ` LIMIT ? OFFSET ?`
	rows, err := s.db.QueryContext(ctx, pageSQL, append(append([]any{}, args...), q.Limit, q.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	for rows.Next() {
		t, err := scanTorrent(rows)
		if err != nil {
			return nil, 0, err
		}
		torrents = append(torrents, t)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	return torrents, total, nil
}

// whereClause builds the shared "WHERE ..." fragment and its bound arguments.
// Only fixed strings and "?" placeholders are concatenated into the SQL
// text; every value the caller supplied is bound.
func whereClause(q scraper.Query) (string, []any) {
	var clause strings.Builder
	args := []any{}

	switch len(q.Trackers) {
	case 0:
		// No tracker restriction; the name and category clauses below are
		// appended with " AND ", so the WHERE needs a term to hang off.
		clause.WriteString("1 = 1")
	case 1:
		clause.WriteString("tracker = ?")
		args = append(args, q.Trackers[0])
	default:
		// The count comes from len, and the values themselves are bound.
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(q.Trackers)), ",")
		clause.WriteString("tracker IN (")
		clause.WriteString(placeholders)
		clause.WriteString(")")
		for _, tracker := range q.Trackers {
			args = append(args, tracker)
		}
	}

	nameClause, nameArgs := nameMatchClause(q.Terms)
	clause.WriteString(nameClause)
	args = append(args, nameArgs...)

	if len(q.Categories) > 0 {
		// The count comes from len, and the values themselves are bound.
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(q.Categories)), ",")
		clause.WriteString(" AND category IN (")
		clause.WriteString(placeholders)
		clause.WriteString(")")
		for _, cat := range q.Categories {
			args = append(args, cat)
		}
	}

	if q.QueryKey != "" {
		clause.WriteString(` AND (
			search_managed = 0
			OR EXISTS (
				SELECT 1 FROM torrent_searches AS matching_search
				WHERE matching_search.torrent_id = torrents.id
					AND matching_search.tracker = torrents.tracker
					AND matching_search.search_key = ?))`)
		args = append(args, q.QueryKey)
	}

	return clause.String(), args
}

// Prune deletes torrents released longer ago than maxAge, keeping the
// store from growing without bound as trackers are re-scraped. The cutoff
// is the release date scraped from the tracker, not when the row was
// stored, so a tracker listing an old release does not keep it. Rows whose
// published date is missing or unparseable age from the last scrape that
// observed them, so they cannot accumulate forever. It returns the number
// of rows removed.
func (s *Store) Prune(ctx context.Context, maxAge time.Duration) (int64, error) {
	cutoff := time.Now().UTC().Add(-maxAge).Format(time.RFC3339)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck // a rollback after Commit is a no-op

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM torrent_searches WHERE last_seen != '' AND last_seen < ?`, cutoff); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx,
		`DELETE FROM torrents
		 WHERE (published != '' AND published < ?)
			OR (published = '' AND last_seen != '' AND last_seen < ?)`, cutoff, cutoff)
	if err != nil {
		return 0, err
	}
	removed, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return removed, nil
}
