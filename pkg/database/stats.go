// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package database

import (
	"context"
	"database/sql"
	"time"

	"github.com/torrplay/jacklet/pkg/scraper"
)

// Stats returns the stored-torrent summary for every tracker present in
// the store, keyed by tracker id. A tracker that has never been scraped
// has no entry.
func (s *Store) Stats(ctx context.Context) (map[string]scraper.TrackerStats, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT tracker, COUNT(*), COALESCE(MAX(NULLIF(published, '')), '')
		FROM torrents
		GROUP BY tracker
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	stats := make(map[string]scraper.TrackerStats)
	for rows.Next() {
		var (
			tracker sql.NullString
			newest  string
			count   int
		)
		if err := rows.Scan(&tracker, &count, &newest); err != nil {
			return nil, err
		}

		entry := scraper.TrackerStats{Torrents: count, Tracker: tracker.String}
		if newest != "" {
			// Dates are stored as UTC RFC 3339, so MAX over the text column
			// is the chronological maximum.
			if t, err := time.Parse(time.RFC3339, newest); err == nil {
				entry.Newest = t
			}
		}
		stats[tracker.String] = entry
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return stats, nil
}
