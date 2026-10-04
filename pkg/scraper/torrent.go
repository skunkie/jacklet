// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

// Torrent is one scraped listing: the single shape a scrape produces and a
// store reads and writes, from which a caller renders whatever its own wire
// format needs.
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
	// Categories are the Torznab category ids the definition's
	// caps.categorymappings resolved the row to: standard ids, each
	// followed by the tracker's own custom category (100000 and up) when
	// its mapping has a description. A row that maps to none has none,
	// which a Torznab feed leaves out (see Query.NeedsCategory).
	Categories  []int
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
	// ID is a store's own row id, assigned on first insert and stable across
	// the refreshes of later scrapes. A scrape leaves it zero.
	ID int64
	// IMDBID, like every external id a row holds (DoubanID, RageID,
	// TMDBID, TraktID, TVDBID and TVMazeID), is the id's bare digits,
	// "0123456" rather than "tt0123456", or empty when none was scraped.
	// A search by id matches it as a number, and the feed renders it in
	// each id's own form.
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
