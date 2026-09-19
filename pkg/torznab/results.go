// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package torznab

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/torrplay/jacklet/pkg/database"
	"github.com/torrplay/jacklet/pkg/scraper"
)

// This file serves Jackett's JSON results endpoint. Torznab is XML-only
// and defines no JSON variant, so there is no spec to follow here: the
// shape below is Jackett's ManualSearchResult, key for key, because a
// client reaching this address is one written against Jackett and reads
// those keys literally. The keys are PascalCase and the Go fields carry
// them as tags for the same reason the Cardigann structs carry theirs --
// the document belongs to another program.

// Status values of one indexer in a search result, as Jackett numbers
// them. Jacklet has no third state to report: a tracker was refreshed or
// it failed, and Unknown is carried only so the numbering matches.
const (
	indexerStatusUnknown = 0
	indexerStatusError   = 1
	indexerStatusOK      = 2
)

// searchResults is Jackett's ManualSearchResult: the releases, and a
// per-indexer report of how the search went on each one.
type searchResults struct {
	Indexers []searchIndexer `json:"Indexers"`
	Results  []jackettResult `json:"Results"`
}

// searchIndexer is one indexer's part in a search: how long it took, how
// many releases it contributed, and why it contributed none when it
// failed. It is what lets a client distinguish "this tracker is down"
// from "this tracker has nothing", which a bare list of releases cannot.
type searchIndexer struct {
	ElapsedTime int64  `json:"ElapsedTime"`
	Error       string `json:"Error"`
	ID          string `json:"ID"`
	Name        string `json:"Name"`
	Results     int    `json:"Results"`
	Status      int    `json:"Status"`
}

// jackettResult is one release, in the shape Jackett's TrackerCacheResult
// serializes.
//
// Two of Jackett's own fields are absent. FirstSeen is when a release
// entered Jackett's in-memory result cache, which is a property of that
// cache rather than of the release, and the store here keeps no such
// column. BlackholeLink addresses a blackhole directory Jacklet does not
// configure.
//
// Jackett leaves most of the numeric fields nullable and sends null for
// one a definition did not scrape. The store holds plain numbers, where
// an unscraped field is zero, so zero is what absent looks like and those
// fields are null here when it is -- the same reading the Torznab feed
// already takes, which leaves such a field out of a release's attributes
// rather than emitting it as 0.
//
// Seeders, Peers and the two volume factors stay plain numbers, because
// for them zero is an answer rather than a gap. A dead release genuinely
// has no seeders and no leechers, and a download factor of zero is what
// marks a freeleech -- the scraper defaults both factors to Cardigann's
// 1, so neither is ever unset. Reporting any of those as null would lose
// the common case to describe the rare one.
type jackettResult struct {
	Album     string `json:"Album"`
	Artist    string `json:"Artist"`
	Author    string `json:"Author"`
	BookTitle string `json:"BookTitle"`
	// Category is a list because a release may map to more than one
	// standard category. A definition resolves a row to exactly one, so
	// the list holds that one.
	Category     []int  `json:"Category"`
	CategoryDesc string `json:"CategoryDesc"`
	// Comments is the release's page on the tracker, repeated under the
	// older name for it. Jackett's own result carries the page only as
	// Details; this is the one key here that Jackett does not send, kept
	// because a client written against an older Jackett looks for it and
	// an extra key costs a reader nothing.
	Comments             string  `json:"Comments"`
	Description          string  `json:"Description"`
	Details              string  `json:"Details"`
	DoubanID             *int64  `json:"DoubanId"`
	DownloadVolumeFactor float64 `json:"DownloadVolumeFactor"`
	Files                *int    `json:"Files"`
	GUID                 string  `json:"Guid"`
	// Gain is Jackett's sort key for how much a release has moved: its
	// size in gibibytes times its seeder count.
	Gain   *float64 `json:"Gain"`
	Genres []string `json:"Genres"`
	Grabs  *int     `json:"Grabs"`
	// IMDB is the bare number rather than the "tt"-prefixed form the
	// Torznab attribute uses, which is how Jackett reports it here.
	IMDB     *int64 `json:"Imdb"`
	InfoHash string `json:"InfoHash"`
	Label    string `json:"Label"`
	// Languages and Subs are the release's audio and subtitle languages.
	// A Cardigann definition declares no field for either, so they are
	// always empty -- emitted rather than dropped, since a client reading
	// them would otherwise find null where Jackett gives it an array.
	Languages []string `json:"Languages"`
	Link      string   `json:"Link"`
	// MagnetURI is the release's magnet when the tracker offers one
	// beside the torrent file that Link points at.
	MagnetURI       string   `json:"MagnetUri"`
	MinimumRatio    *float64 `json:"MinimumRatio"`
	MinimumSeedTime *int     `json:"MinimumSeedTime"`
	// Peers is the leecher count, not the swarm size: Jackett's
	// ReleaseInfo subtracts the seeders before serializing it, and a
	// client reading this field expects what Jackett puts in it.
	Peers       int      `json:"Peers"`
	Poster      string   `json:"Poster"`
	PublishDate string   `json:"PublishDate"`
	Publisher   string   `json:"Publisher"`
	RageID      *int64   `json:"RageID"`
	Seeders     int      `json:"Seeders"`
	Size        *int64   `json:"Size"`
	Subs        []string `json:"Subs"`
	TMDBID      *int64   `json:"TMDb"`
	TVDBID      *int64   `json:"TVDBId"`
	TVMazeID    *int64   `json:"TVMazeId"`
	Title       string   `json:"Title"`
	Track       string   `json:"Track"`
	Tracker     string   `json:"Tracker"`
	// TrackerID is the indexer the release came from, which under a meta
	// indexer is not the id the request named.
	TrackerID          string  `json:"TrackerId"`
	TrackerType        string  `json:"TrackerType"`
	TraktID            *int64  `json:"TraktId"`
	UploadVolumeFactor float64 `json:"UploadVolumeFactor"`
	Year               *int    `json:"Year"`
}

// Results serves "GET /api/v2.0/indexers/{id}/results": the same
// scrape-then-search pipeline as the Torznab XML endpoint, and the same
// query parameters, returned as JSON. This is Jackett's own address and
// Jackett's own idea -- a convenience it added on top of the Torznab
// spec, which is XML-only.
//
// Unlike the XML feed it answers 200 for a tracker that could not be
// reached, with the failure recorded against that indexer. The shape has
// somewhere to put it, and a client that asked several indexers at once
// should not lose the ones that answered because one did not.
func (t *Torznab) Results(w http.ResponseWriter, r *http.Request) {
	idx, ok := t.authorizedIndexer(w, r, t.writeJSONError)
	if !ok {
		return
	}

	// Jackett's "Tracker[]" re-runs a search over a subset of what the
	// addressed indexer covers, so a client can narrow one without
	// addressing each indexer separately. Naming none of the covered
	// indexers leaves an empty result rather than an error -- the client
	// chose the set -- which search answers as it answers any indexer
	// covering no definitions.
	if requested := newSearchQuery(r.URL.Query()).list("tracker"); len(requested) > 0 {
		idx = idx.restrictedTo(requested)
	}

	result, ok := t.search(w, r, idx, t.writeJSONError, jsonLimits)
	if !ok {
		return
	}

	baseURL := t.getBaseURL(r)
	names := idx.names()
	types := idx.types()
	categories := idx.categoryNames()

	results := make([]jackettResult, len(result.Torrents))
	counts := make(map[string]int, len(idx.Defs))
	for i := range result.Torrents {
		row := &result.Torrents[i]
		owner := rowIndexerID(row, idx.ID)
		counts[owner]++
		results[i] = toJackettResult(row, owner, names, types, categories,
			downloadLink(row, baseURL, owner, t.apiKey))
	}

	t.writeResults(w, searchResults{
		Indexers: searchIndexers(idx, result.Outcomes, counts),
		Results:  results,
	})
}

// writeResults encodes a search result as the endpoint's response.
func (t *Torznab) writeResults(w http.ResponseWriter, results searchResults) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(results); err != nil {
		t.logger.Error("failed to encode results", "error", err)
	}
}

// restrictedTo narrows an indexer to the trackers a request named, for
// Jackett's "Tracker[]" parameter. An id it names that this indexer does
// not cover is ignored rather than refused, since the parameter selects
// from what was addressed rather than adding to it.
func (idx *indexer) restrictedTo(ids []string) *indexer {
	wanted := make(map[string]bool, len(ids))
	for _, id := range ids {
		wanted[id] = true
	}

	narrowed := *idx
	narrowed.Defs = nil
	for _, def := range idx.Defs {
		if wanted[scraper.TrackerID(def)] {
			narrowed.Defs = append(narrowed.Defs, def)
		}
	}
	return &narrowed
}

// searchIndexers reports what each covered tracker contributed, pairing
// its scrape outcome with how many of the returned releases came from it.
func searchIndexers(idx *indexer, outcomes []scrapeOutcome, counts map[string]int) []searchIndexer {
	names := idx.names()
	indexers := make([]searchIndexer, len(outcomes))
	for i, outcome := range outcomes {
		entry := searchIndexer{
			ElapsedTime: outcome.Elapsed.Milliseconds(),
			ID:          outcome.TrackerID,
			Name:        trackerName(names, outcome.TrackerID),
			Results:     counts[outcome.TrackerID],
			Status:      indexerStatusOK,
		}
		if outcome.Err != nil {
			entry.Error = outcome.Err.Error()
			entry.Status = indexerStatusError
		}
		indexers[i] = entry
	}
	return indexers
}

// toJackettResult renders one stored row. link is what the client should
// fetch, from downloadLink.
func toJackettResult(row *database.Torrent, owner string, names, types map[string]string, categories map[int]string, link string) jackettResult {
	return jackettResult{
		Album:                row.Album,
		Artist:               row.Artist,
		Author:               row.Author,
		BookTitle:            row.BookTitle,
		Category:             resultCategories(row.Category),
		CategoryDesc:         categories[row.Category],
		Comments:             row.DetailsURL,
		Description:          summary(row),
		Details:              row.DetailsURL,
		DoubanID:             externalID(row.DoubanID),
		DownloadVolumeFactor: row.DownloadVolumeFactor,
		Files:                optional(row.Files),
		GUID:                 guid(row),
		Gain:                 gain(row),
		Genres:               resultGenres(row.Genres),
		Grabs:                optional(row.Grabs),
		IMDB:                 externalID(row.IMDBID),
		InfoHash:             row.InfoHash,
		Label:                row.Label,
		Languages:            []string{},
		Link:                 link,
		MagnetURI:            row.Magnet,
		MinimumRatio:         optional(row.MinimumRatio),
		MinimumSeedTime:      optional(row.MinimumSeedTime),
		Peers:                row.Leechers,
		Poster:               row.Poster,
		PublishDate:          row.Published,
		Publisher:            row.Publisher,
		RageID:               externalID(row.RageID),
		Seeders:              row.Seeders,
		Size:                 optional(row.Size),
		Subs:                 []string{},
		TMDBID:               externalID(row.TMDBID),
		TVDBID:               externalID(row.TVDBID),
		TVMazeID:             externalID(row.TVMazeID),
		Title:                row.Name,
		Track:                row.Track,
		Tracker:              trackerName(names, owner),
		TrackerID:            owner,
		TrackerType:          types[owner],
		TraktID:              externalID(row.TraktID),
		UploadVolumeFactor:   row.UploadVolumeFactor,
		Year:                 optional(row.Year),
	}
}

// bytesPerGibibyte is the divisor Jackett uses for Gain, which counts in
// gibibytes rather than gigabytes.
const bytesPerGibibyte = 1024 * 1024 * 1024

// gain is how much a release has moved: its size in gibibytes times its
// seeder count, which is Jackett's own sort key for the column. Jackett
// derives it from two nullable fields and leaves it null when either is
// missing, so a release whose size was never scraped has no gain rather
// than a gain of zero.
func gain(row *database.Torrent) *float64 {
	if row.Size == 0 {
		return nil
	}
	value := float64(row.Seeders) * float64(row.Size) / bytesPerGibibyte
	return &value
}

// optional renders a number Jackett leaves nullable, reading zero as the
// absence of a scraped value. It is only for the fields where zero cannot
// be an answer in its own right; jackettResult says which those are.
func optional[T int | int64 | float64](value T) *T {
	if value == 0 {
		return nil
	}
	return &value
}

// resultCategories renders a row's standard category as the list Jackett
// serves. A row a definition could not map to a standard category has
// none rather than category zero, which a client would try to look up.
func resultCategories(category int) []int {
	if category == 0 {
		return []int{}
	}
	return []int{category}
}

// resultGenres splits the stored comma-separated genre list into the
// array Jackett serves. It is never null, so a client can iterate it
// without a guard.
func resultGenres(genres string) []string {
	split := []string{}
	for genre := range strings.SplitSeq(genres, ",") {
		if genre = strings.TrimSpace(genre); genre != "" {
			split = append(split, genre)
		}
	}
	return split
}

// externalID renders a stored external id -- which is bare digits, as
// every one of them is stored -- as the number Jackett serves here,
// rather than as the string the Torznab attributes carry. An id that was
// never scraped, or that is not a number at all, is null: a client
// matching on it must be able to tell an absent id from a real one.
func externalID(digits string) *int64 {
	value, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return nil
	}
	return &value
}

// types maps each covered tracker's id to its definition's type
// ("public", "private", "semi-private"), which Jackett reports against
// every release.
func (idx *indexer) types() map[string]string {
	types := make(map[string]string, len(idx.Defs))
	for _, def := range idx.Defs {
		types[scraper.TrackerID(def)] = def.Type
	}
	return types
}

// categoryNames maps the standard category ids this indexer can serve to
// their names, for the description Jackett reports beside the id.
func (idx *indexer) categoryNames() map[int]string {
	names := make(map[int]string)
	for _, category := range idx.categories() {
		names[category.ID] = category.Name
	}
	return names
}
