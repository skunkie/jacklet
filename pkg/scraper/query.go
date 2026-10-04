// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"errors"
	"time"
)

// ErrNotFound is what a store's Find returns, wrapped, when no stored
// torrent matches.
var ErrNotFound = errors.New("torrent not found")

// Query describes a listing of stored torrents, across one or more trackers,
// for a store to answer.
type Query struct {
	// Categories restricts the results to rows filed under any of these
	// Torznab category ids, a tracker's custom ones (100000 and up)
	// included, matched whole, so each must be written as a plain decimal
	// integer, as Torznab parsing writes it: "02000" matches nothing. A row
	// filed under no category matches any list, as Jackett keeps it. Empty
	// matches every category. Pass them already expanded from any parent
	// categories the client asked for.
	Categories []string
	// IDs restricts the rows no search produced, which QueryKey leaves
	// unrestricted, to those stored with one of these ids and with no
	// other value for any of them, each compared as a number, so
	// "tt0123456" matches a row holding "123456". A search by id has no
	// terms to match their names on. The zero value restricts nothing.
	IDs ExternalIDs
	// Limit caps how many rows are returned; zero or less returns none.
	Limit int
	// NeedsCategory leaves out rows filed under no category, as Jackett
	// leaves them out of everything but an interactive search, which is
	// its JSON results endpoint.
	NeedsCategory bool
	// Offset skips that many matches before the first returned row.
	Offset int
	// QueryKey restricts scraper-managed results to rows previously produced
	// by that exact search. A tracker populated only through Upsert remains a
	// generic library-managed cache without provenance. Empty omits query
	// provenance entirely.
	QueryKey string
	// Terms are the words a torrent's name must all contain,
	// case-insensitively. Empty matches every name, which is the
	// RSS-style "latest releases" case.
	Terms []string
	// Trackers scopes the search to these tracker ids. It is a list rather
	// than a single id so the aggregate indexer can answer from one query
	// over every configured tracker, which is what makes its sort order,
	// paging and total counts global instead of stitched together from
	// per-tracker pages. Empty matches every tracker in the store,
	// including one whose definition is no longer configured, so a caller
	// serving a fixed set of indexers should name them.
	Trackers []string
	// UnsearchedTerms are further words, matched as Terms are, that only
	// the name of a row no search produced must contain. A search's
	// episode goes here: the tracker matched it for the rows it returned,
	// which may write it some other way.
	UnsearchedTerms []string
}

// ExternalIDs are the ids a release has in other catalogs, as a search
// names them and a scrape stores them. Empty means not named.
type ExternalIDs struct {
	DoubanID string
	IMDBID   string
	RageID   string
	TMDBID   string
	TVDBID   string
	TVMazeID string
	TraktID  string
}

// TrackerStats summarizes what a store currently holds for one tracker, as
// distinct from IndexerStatus, which reports how its last scrape went.
type TrackerStats struct {
	// Newest is the most recent release date among the stored torrents,
	// zero when none of them carries a parseable date.
	Newest time.Time
	// Torrents is how many rows are stored for the tracker.
	Torrents int
	// Tracker is the tracker id the stats describe.
	Tracker string
}
