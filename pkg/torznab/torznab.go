// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

// Package torznab exposes scraped tracker results over a
// Torznab/Newznab-compatible HTTP API.
package torznab

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/torrplay/jacklet/pkg/database"
	"github.com/torrplay/jacklet/pkg/scraper"
)

// ScrapeTimeout bounds how long a search request may spend re-scraping a
// tracker before responding to the client.
const ScrapeTimeout = 45 * time.Second

// Torznab serves one Torznab/Newznab-compatible API endpoint per indexer,
// mirroring how Jackett exposes each configured indexer as its own Torznab
// endpoint, plus the aggregate endpoint under AggregateID that answers
// from every configured indexer at once.
type Torznab struct {
	apiKey        string
	baseURL       string
	contactEmail  string
	logger        *slog.Logger
	scrapeTimeout time.Duration
	scrpr         *scraper.Scraper
	store         *database.Store
	trackers      scraper.TrackerSource
}

// Options configures behavior that is deployment-specific rather than part
// of a tracker definition.
type Options struct {
	// BaseURL is the externally reachable origin used in generated links.
	// Empty derives the origin from the incoming request.
	BaseURL string
	// ContactEmail is the operator address advertised as the caps
	// server email and the feed's webMaster, which some Torznab clients
	// show to the end user. Empty omits both, which is preferable to
	// advertising an address nobody reads.
	ContactEmail string
	// ScrapeTimeout overrides ScrapeTimeout. It is primarily useful to an
	// embedding program with a shorter request budget.
	ScrapeTimeout time.Duration
}

// New creates a new Torznab handler. If apiKey is non-empty,
// requests must supply a matching "apikey" query parameter. scrpr is
// shared across every request so its HTTP client, cookie jar, and
// FlareSolverr session persist across searches instead of being rebuilt
// per request. Definitions come from trackers, which is any
// scraper.TrackerSource: pass a scraper.DefinitionStore to read them from
// a directory of YAML files, or supply another implementation to serve
// definitions from somewhere else.
func New(store *database.Store, scrpr *scraper.Scraper, trackers scraper.TrackerSource, apiKey string, logger *slog.Logger) *Torznab {
	return NewWithOptions(store, scrpr, trackers, apiKey, logger, Options{})
}

// NewWithOptions creates a Torznab handler with deployment options.
func NewWithOptions(store *database.Store, scrpr *scraper.Scraper, trackers scraper.TrackerSource, apiKey string, logger *slog.Logger, options Options) *Torznab {
	scrapeTimeout := options.ScrapeTimeout
	if scrapeTimeout <= 0 {
		scrapeTimeout = ScrapeTimeout
	}
	return &Torznab{
		apiKey:        apiKey,
		baseURL:       strings.TrimRight(options.BaseURL, "/"),
		contactEmail:  options.ContactEmail,
		logger:        logger,
		scrapeTimeout: scrapeTimeout,
		scrpr:         scrpr,
		store:         store,
		trackers:      trackers,
	}
}

func (t *Torznab) getBaseURL(r *http.Request) string {
	if t.baseURL != "" {
		return t.baseURL
	}
	proto := "http"
	if r.TLS != nil {
		proto = "https"
	}
	return fmt.Sprintf("%s://%s", proto, r.Host)
}

func (t *Torznab) authorized(r *http.Request) bool {
	if t.apiKey == "" {
		return true
	}
	provided := r.URL.Query().Get("apikey")
	return subtle.ConstantTimeCompare([]byte(provided), []byte(t.apiKey)) == 1
}

// authorizedIndexer checks the apikey and resolves {id} to the definitions
// the request is answered from: one tracker, every configured tracker
// under AggregateID, or the ones a filter expression covers. It writes an
// error response and returns ok=false if either step fails. Shared by the
// search-shaped endpoints (ServeHTTP, Results).
func (t *Torznab) authorizedIndexer(w http.ResponseWriter, r *http.Request, fail errorWriter) (idx *indexer, ok bool) {
	if !t.authorized(r) {
		fail(w, http.StatusUnauthorized, errInvalidAPIKey, "Invalid API Key")
		return nil, false
	}

	id := r.PathValue("id")
	if id == AggregateID || isIndexerFilter(id) {
		// A definition that failed to parse is reported where it was
		// read: DefinitionStore logs the file and the error. A meta
		// indexer answers from the ones that loaded rather than refusing
		// every search because one file is broken.
		defs, _, err := t.trackers.Trackers()
		if err != nil {
			t.logger.Error("failed to load tracker definitions", "error", err)
			fail(w, http.StatusInternalServerError, errUnknown, "Failed to load tracker definitions")
			return nil, false
		}
		if id == AggregateID {
			return aggregateIndexer(defs), true
		}

		match, err := parseIndexerFilter(id)
		if err != nil {
			fail(w, http.StatusNotFound, errIndexerNotSupported, err.Error())
			return nil, false
		}
		return t.filterIndexer(defs, id, match), true
	}

	def, ok := t.findAuthorizedTracker(w, r, fail)
	if !ok {
		return nil, false
	}
	return singleIndexer(def), true
}

// findAuthorizedTracker checks the apikey and resolves {id} to a single
// tracker definition, writing an error response and returning ok=false if
// either step fails. Used by the endpoints that act on exactly one tracker
// (Download), and by authorizedIndexer for the non-meta case.
func (t *Torznab) findAuthorizedTracker(w http.ResponseWriter, r *http.Request, fail errorWriter) (def *scraper.Tracker, ok bool) {
	if !t.authorized(r) {
		fail(w, http.StatusUnauthorized, errInvalidAPIKey, "Invalid API Key")
		return nil, false
	}

	def, err := t.trackers.Find(r.PathValue("id"))
	if err != nil {
		if errors.Is(err, scraper.ErrTrackerNotFound) {
			fail(w, http.StatusNotFound, errIndexerNotSupported, err.Error())
			return nil, false
		}
		t.logger.Error("failed to load tracker definitions", "error", err)
		fail(w, http.StatusInternalServerError, errUnknown, "Failed to load tracker definitions")
		return nil, false
	}

	return def, true
}

// ServeHTTP handles requests to "/api/v2.0/indexers/{id}/results/torznab",
// where {id} is a tracker definition's own "id" field, AggregateID to
// query every configured indexer at once, or a filter expression (see
// parseIndexerFilter) to query the ones it covers.
func (t *Torznab) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	idx, ok := t.authorizedIndexer(w, r, t.writeTorznabError)
	if !ok {
		return
	}

	switch r.URL.Query().Get("t") {
	case "caps":
		t.handleCaps(w, r, idx)
	// The indexers behind a meta indexer -- the aggregate, or a filter
	// expression. Jackett serves this from the same endpoint, so a client
	// that discovers one such endpoint can enumerate what is behind it.
	case "indexers":
		t.handleIndexerList(w, r, idx)
	// The Newznab/Torznab spec's actual "t" values are unhyphenated
	// ("tvsearch", "movie", "music", "book"), unlike the hyphenated element
	// names ("tv-search", "movie-search") used in the caps XML response
	// below — a real client (Sonarr sends t=tvsearch, Radarr t=movie,
	// Lidarr t=music, Readarr t=book) would never match a hyphenated case
	// here. Every form is accepted anyway, since real-world
	// Newznab-compatible clients aren't perfectly consistent about it.
	// Every mode handleCaps can advertise as available must appear here,
	// or a client takes Jacklet up on an offer it then rejects.
	case "search", "tvsearch", "tv-search", "movie", "moviesearch", "movie-search",
		"music", "musicsearch", "music-search", "audio", "audiosearch", "audio-search",
		"book", "booksearch", "book-search":
		t.handleSearch(w, r, idx)
	default:
		t.writeTorznabError(w, http.StatusBadRequest, errNoSuchFunction, "Invalid 't' parameter")
	}
}

func (t *Torznab) handleCaps(w http.ResponseWriter, r *http.Request, idx *indexer) {
	t.writeXML(w, t.capsFor(idx, t.getBaseURL(r)))
}

// capsFor builds an indexer's capabilities document. It is what "t=caps"
// answers with, and also what "t=indexers" nests inside each entry, which
// is why it is separate from the handler: the two must describe an
// indexer identically.
func (t *Torznab) capsFor(idx *indexer, baseURL string) Caps {
	supported := idx.categories()
	categories := make([]Category, len(supported))
	for i, c := range supported {
		categories[i] = Category{ID: c.ID, Name: c.Name}
	}

	return Caps{
		Categories: &Categories{Category: categories},
		Limits: &Limits{
			Default: defaultLimit,
			Max:     maxLimit,
		},
		Searching: searchingCaps(idx.searchModes()),
		Server: &Server{
			Email: t.contactEmail,
			Title: idx.Name,
			URL:   baseURL,
		},
	}
}

// searchingCaps renders declared search modes (caps.modes) as the Torznab
// caps "searching" element, so a tracker that serves music or ebooks is
// advertised as able to, instead of every indexer reporting the same fixed
// set. modes comes from indexer.searchModes, which is one definition's
// modes or the union across every definition for the aggregate.
func searchingCaps(modes map[string][]string) *Searching {
	mode := func(name string) *Search {
		params, ok := modes[name]
		if !ok {
			return &Search{Available: "no"}
		}
		return &Search{Available: "yes", SupportedParams: strings.Join(params, ",")}
	}

	searching := &Searching{
		AudioSearch: mode("music-search"),
		BookSearch:  mode("book-search"),
		MovieSearch: mode("movie-search"),
		Search:      mode("search"),
		TVSearch:    mode("tv-search"),
	}
	// Cardigann definitions spell the audio mode either way.
	if searching.AudioSearch.Available == "no" {
		searching.AudioSearch = mode("audio-search")
	}
	return searching
}

// defaultLimit and maxLimit match the values Jacklet's own caps response
// advertises (Limits above) — keep them in sync.
const (
	defaultLimit = 50
	maxLimit     = 100
)

// jsonResultLimit bounds a page of the JSON results endpoint. Jackett's
// JSON endpoint has no limit or offset of its own and answers with the
// whole result set, so a client sends no "limit" and expects everything
// its query matched; applying the Torznab feed's default would silently
// truncate a search that found more. The cap is here only so a request
// cannot ask the store for an unbounded page.
const jsonResultLimit = 10000

// limits bounds one endpoint's page size: the size used when the client
// asks for none, and the largest it may ask for.
type limits struct {
	Default int
	Max     int
}

// torznabLimits are the Torznab feed's, which its caps document
// advertises. jsonLimits are the JSON results endpoint's, which is
// Jackett's own address and answers unpaged.
var (
	jsonLimits    = limits{Default: jsonResultLimit, Max: jsonResultLimit}
	torznabLimits = limits{Default: defaultLimit, Max: maxLimit}
)

// searchQuery indexes a request's query parameters so one can be looked
// up under more than one name, without regard to case, and with the
// bracketed array form a browser client sends.
//
// Torznab's own parameters are lowercase ("q", "cat"), but Jackett's JSON
// results endpoint binds an ApiSearch model through ASP.NET, which matches
// its "Query", "Category" and "Tracker" properties case-insensitively and
// accepts "Category[]" for the array. A client written against Jackett
// sends those spellings to this address, so both vocabularies are read
// here rather than only Torznab's.
type searchQuery map[string][]string

// newSearchQuery indexes the parameters of one request.
func newSearchQuery(q url.Values) searchQuery {
	indexed := make(searchQuery, len(q))
	for key, values := range q {
		name := strings.ToLower(strings.TrimSuffix(key, "[]"))
		indexed[name] = append(indexed[name], values...)
	}
	return indexed
}

// get returns the first non-empty value under any of names, which are
// lowercase. Later names are the aliases of the first.
func (q searchQuery) get(names ...string) string {
	for _, name := range names {
		for _, value := range q[name] {
			if value != "" {
				return value
			}
		}
	}
	return ""
}

// list returns every value under any of names, splitting each on commas.
// A repeated parameter and a single comma-separated one are the two
// spellings of a list, and Torznab's own "cat" is defined as the second.
func (q searchQuery) list(names ...string) []string {
	var values []string
	for _, name := range names {
		for _, value := range q[name] {
			for part := range strings.SplitSeq(value, ",") {
				if part = strings.TrimSpace(part); part != "" {
					values = append(values, part)
				}
			}
		}
	}
	return values
}

// number returns the integer under name, or fallback when it is absent or
// unparseable.
func (q searchQuery) number(name string, fallback int) int {
	if n, err := strconv.Atoi(q.get(name)); err == nil && n > 0 {
		return n
	}
	return fallback
}

// searchParamsFromQuery reads the Torznab search parameters (q, cat,
// season, ep, the external ids, year, genre and the music/book terms)
// plus pagination (limit, offset) shared by every search-shaped endpoint
// (the Torznab XML search and the JSON results endpoint). pageLimits are
// the calling endpoint's, since the two page differently.
//
// limit and offset are returned separately as well as carried on params:
// the pair here bounds what the store returns, while the copies on params
// go to the tracker, for a definition that pages server-side.
func searchParamsFromQuery(values url.Values, pageLimits limits) (params scraper.SearchParams, limit, offset int) {
	q := newSearchQuery(values)

	limit = min(q.number("limit", pageLimits.Default), pageLimits.Max)
	offset = q.number("offset", 0)

	params = scraper.SearchParams{
		Album:      q.get("album"),
		Artist:     q.get("artist"),
		Author:     q.get("author"),
		Categories: q.list("cat", "category"),
		DoubanID:   q.get("doubanid"),
		Ep:         q.get("ep"),
		Extended:   q.get("extended"),
		Genre:      q.get("genre"),
		IMDBID:     q.get("imdbid"),
		Label:      q.get("label"),
		Limit:      limit,
		Offset:     offset,
		Publisher:  q.get("publisher"),
		Query:      q.get("q", "query"),
		Season:     q.get("season"),
		TMDBID:     q.get("tmdbid"),
		TVDBID:     q.get("tvdbid"),
		TVMazeID:   q.get("tvmazeid"),
		TVRageID:   q.get("rid", "rageid"),
		Title:      q.get("title"),
		Track:      q.get("track"),
		TraktID:    q.get("traktid"),
		Type:       canonicalSearchType(q.get("t")),
		Year:       q.get("year"),
	}

	return params, limit, offset
}

// canonicalSearchType normalizes the "t" parameter to the unhyphenated
// form the Newznab/Torznab spec defines, which is what a definition
// branching on ".Query.Type" compares against. The hyphenated spellings
// exist only as caps element names, but real clients send them anyway, and
// ServeHTTP accepts every form — so the definitions must not have to.
//
// An absent "t" means the JSON results endpoint, which has no "t" of its
// own and runs a plain search.
func canonicalSearchType(t string) string {
	switch t {
	case "tvsearch", "tv-search":
		return "tvsearch"
	case "movie", "moviesearch", "movie-search":
		return "movie"
	case "music", "musicsearch", "music-search", "audio", "audiosearch", "audio-search":
		return "music"
	case "book", "booksearch", "book-search":
		return "book"
	case "":
		return "search"
	default:
		return t
	}
}

// searchTerms is the list of words a stored title must contain to be
// considered a match. Every term the client supplied is required, so a
// tvsearch for "Some Show" season 2 doesn't return every release the
// tracker happened to have in the store. An empty list matches everything,
// which is the RSS-style "latest releases" case.
func searchTerms(params scraper.SearchParams) []string {
	fields := []string{params.Query, params.Artist, params.Album, params.Track, params.Author, params.Title}
	terms := make([]string, 0, len(fields))
	for _, field := range fields {
		terms = append(terms, strings.Fields(field)...)
	}
	return terms
}

// searchResult is what one search produced: the page of stored torrents
// to render, the total number of matches across every page, and what
// refreshing each covered tracker produced.
type searchResult struct {
	Offset   int
	Outcomes []scrapeOutcome
	Query    string
	// ScrapeErr is set when no tracker this indexer covers could be
	// refreshed at all. Whatever was already stored is returned with it:
	// whether an unrefreshed answer beats no answer is the endpoint's
	// call, since the Torznab feed has nowhere to report a failed indexer
	// and the JSON one does.
	ScrapeErr error
	Torrents  []database.Torrent
	Total     int
}

// search runs the pipeline both search-shaped endpoints share: re-scrape
// the indexer, then answer from the store. pageLimits are the calling
// endpoint's, since the two page differently.
//
// It writes its own error response and reports ok=false when the request
// cannot be answered at all. A failed scrape is not such a case: the store
// exists precisely so a transient outage degrades to stale results rather
// than to none, and the failure is reported on the result for the endpoint
// to render.
func (t *Torznab) search(w http.ResponseWriter, r *http.Request, idx *indexer, fail errorWriter, pageLimits limits) (searchResult, bool) {
	// No definition loaded at all, so nothing is configured to search,
	// which an empty feed would misreport as "no releases match".
	if idx.IsUnconfigured {
		fail(w, http.StatusServiceUnavailable, errUnknown, "No indexer definitions are configured")
		return searchResult{}, false
	}

	params, limit, offset := searchParamsFromQuery(r.URL.Query(), pageLimits)

	// An indexer covering no definitions -- a filter that matched none of
	// them, or a "Tracker[]" naming none of the ones addressed -- has an
	// empty answer, and is answered without reaching the store: an empty
	// tracker list there scopes the query to every tracker rather than to
	// none, which would return the whole store instead of nothing.
	//
	// The request is still read first, so the page this answers is the
	// page that was asked for: the feed echoes the offset back, and one
	// reported as zero because nothing matched would describe a different
	// request than the client made.
	if len(idx.Defs) == 0 {
		return searchResult{Offset: offset, Query: params.Query}, true
	}

	// Refresh results by re-scraping this indexer, bounded so a slow or
	// unreachable tracker cannot stall the response indefinitely. An empty
	// query still triggers a scrape (RSS-style: latest releases), matching
	// Torznab's "t=search" with no "q" convention.
	scrapeCtx, cancel := context.WithTimeout(r.Context(), t.scrapeTimeout)
	outcomes, scrapeErr := t.scrape(scrapeCtx, idx, params)
	cancel()

	// One query over every tracker the indexer covers, rather than a query
	// each: the sort order, the page and the total are then global, which
	// merging per-tracker pages afterwards could not reproduce.
	torrents, total, err := t.store.Search(r.Context(), database.Search{
		Categories: scraper.ExpandCategoryIDs(params.Categories),
		Limit:      limit,
		Offset:     offset,
		QueryKey:   params.ResultKey(),
		Terms:      searchTerms(params),
		Trackers:   idx.ids(),
	})
	if err != nil {
		t.logger.Error("failed to query items", "error", err)
		fail(w, http.StatusInternalServerError, errUnknown, fmt.Sprintf("Failed to query items: %v", err))
		return searchResult{}, false
	}

	if scrapeErr != nil {
		t.logger.Error("failed to scrape indexer", "indexer", idx.ID, "error", scrapeErr)
		if total > 0 {
			t.logger.Warn("serving stored results after a failed scrape", "indexer", idx.ID, "results", len(torrents))
		}
	}

	return searchResult{
		Offset:    offset,
		Outcomes:  outcomes,
		Query:     params.Query,
		ScrapeErr: scrapeErr,
		Torrents:  torrents,
		Total:     total,
	}, true
}

// rowIndexerID is the indexer a stored row's download link must point at:
// the tracker that actually produced it, which under the aggregate is not
// the indexer the client asked. Rows Jacklet stores always name their
// tracker; the request's own id is the fallback for a store filled by
// another program through this importable package.
func rowIndexerID(row *database.Torrent, requested string) string {
	if row.Tracker != "" {
		return row.Tracker
	}
	return requested
}

func (t *Torznab) handleSearch(w http.ResponseWriter, r *http.Request, idx *indexer) {
	result, ok := t.search(w, r, idx, t.writeTorznabError, torznabLimits)
	if !ok {
		return
	}

	// An RSS feed has nowhere to record which indexer failed, so a search
	// that refreshed nothing and found nothing stored is reported as the
	// failure it is rather than as an empty feed.
	if result.ScrapeErr != nil && result.Total == 0 {
		t.writeTorznabError(w, http.StatusBadGateway, errUnknown,
			fmt.Sprintf("Indexer %s is unreachable: %v", idx.ID, result.ScrapeErr))
		return
	}

	baseURL := t.getBaseURL(r)
	items := make([]Item, len(result.Torrents))
	for i := range result.Torrents {
		row := &result.Torrents[i]
		items[i] = toTorznabItem(row, downloadLink(row, baseURL, rowIndexerID(row, idx.ID), t.apiKey))
	}

	feed := &Feed{
		// Channel takes the wire-order exemption from the alphabetical
		// field rule, so this literal follows the declaration rather than
		// the alphabet.
		Channel: &Channel{
			Title:       idx.Name,
			Link:        baseURL,
			Description: idx.Description,
			Language:    idx.Language,
			Category:    "movies",
			WebMaster:   t.contactEmail,
			Image: &Image{
				Link:  baseURL,
				Title: idx.Name,
				URL:   baseURL + "/favicon.svg",
			},
			Response: &Response{
				Offset: result.Offset,
				Total:  result.Total,
			},
			Items: items,
		},
		TorznabXMLNS: "http://torznab.com/schemas/2015/feed",
		Version:      "2.0",
		XMLName:      xml.Name{Local: "rss", Space: "http://www.newznab.com/DTD/2010/feeds/attributes/"},
	}

	t.writeXML(w, feed)
}

// writeJSONError is the errorWriter for the JSON endpoints. The Torznab
// code is dropped rather than added to the body: the JSON shape is
// Jackett's, which carries no such field.
func (t *Torznab) writeJSONError(w http.ResponseWriter, status, _ int, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(map[string]string{"error": message}); err != nil {
		t.logger.Error("failed to encode error response", "error", err)
	}
}

// trackerName is the display name for a tracker id, falling back to the
// id itself for a stored row whose definition is no longer configured.
func trackerName(names map[string]string, id string) string {
	if name, ok := names[id]; ok {
		return name
	}
	return id
}

func (t *Torznab) writeXML(w http.ResponseWriter, data any) {
	t.writeXMLStatus(w, http.StatusOK, data)
}

// writeXMLStatus writes an XML document under a given status code. A
// Torznab error is a document in its own right rather than a bare status,
// so it cannot go through http.Error.
//
// A failure part-way through is logged rather than reported to the
// client. The status and the opening bytes are already on the wire by
// then, so there is no status left to replace and nothing useful to say
// in the body: appending a plain-text message would leave the client
// holding a half-written document that no longer parses.
func (t *Torznab) writeXMLStatus(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(status)
	if _, err := w.Write([]byte(xml.Header)); err != nil {
		t.logger.Error("failed to write the XML header", "error", err)
		return
	}

	encoder := xml.NewEncoder(w)
	encoder.Indent("", "  ")
	if err := encoder.Encode(data); err != nil {
		t.logger.Error("failed to encode an XML response", "error", err)
		return
	}
}

// XML types

type Caps struct {
	Categories *Categories `xml:"categories"`
	Limits     *Limits     `xml:"limits"`
	Searching  *Searching  `xml:"searching"`
	Server     *Server     `xml:"server"`
	XMLName    xml.Name    `xml:"caps"`
}

type Server struct {
	Email string `xml:"email,attr,omitempty"`
	Title string `xml:"title,attr"`
	URL   string `xml:"url,attr"`
}

type Limits struct {
	Default int `xml:"default,attr"`
	Max     int `xml:"max,attr"`
}

type Searching struct {
	AudioSearch *Search `xml:"audio-search"`
	BookSearch  *Search `xml:"book-search"`
	MovieSearch *Search `xml:"movie-search"`
	Search      *Search `xml:"search"`
	TVSearch    *Search `xml:"tv-search"`
}

type Search struct {
	Available       string `xml:"available,attr"`
	SupportedParams string `xml:"supportedParams,attr,omitempty"`
}

type Categories struct {
	Category []Category `xml:"category"`
}

type Category struct {
	ID   int    `xml:"id,attr"`
	Name string `xml:"name,attr"`
}

type Feed struct {
	Channel      *Channel `xml:"channel"`
	TorznabXMLNS string   `xml:"xmlns:torznab,attr"`
	Version      string   `xml:"version,attr"`
	XMLName      xml.Name `xml:"rss"`
}

// Channel is the RSS <channel>. Its fields are declared in the
// order RSS 2.0 requires them to be serialized — title/link/description
// first, items last — rather than alphabetically, because encoding/xml
// emits elements in struct declaration order and offers no tag to
// reorder them.
type Channel struct {
	Title       string    `xml:"title"`
	Link        string    `xml:"link"`
	Description string    `xml:"description"`
	Language    string    `xml:"language"`
	Category    string    `xml:"category"`
	WebMaster   string    `xml:"webMaster,omitempty"`
	Image       *Image    `xml:"image"`
	Response    *Response `xml:"response"`
	Items       []Item    `xml:"item"`
}

type Image struct {
	Link  string `xml:"link"`
	Title string `xml:"title"`
	URL   string `xml:"url"`
}

type Response struct {
	Offset int `xml:"offset,attr"`
	Total  int `xml:"total,attr"`
}

// Item is one RSS <item>. As with Channel, its fields are
// declared in RSS serialization order rather than alphabetically.
type Item struct {
	Title       string      `xml:"title"`
	GUID        *GUID       `xml:"guid"`
	Link        string      `xml:"link"`
	Comments    string      `xml:"comments,omitempty"`
	PubDate     string      `xml:"pubDate"`
	Category    string      `xml:"category"`
	Description string      `xml:"description"`
	Enclosure   *Enclosure  `xml:"enclosure"`
	Attributes  []Attribute `xml:"torznab:attr"`
}

// GUID is an RSS <guid>. isPermaLink is always stated explicitly:
// RSS defaults it to true, which would invite a client to treat a
// non-URL identifier as a link.
type GUID struct {
	IsPermaLink string `xml:"isPermaLink,attr"`
	Value       string `xml:",chardata"`
}

type Enclosure struct {
	Length int64  `xml:"length,attr"`
	Type   string `xml:"type,attr"`
	URL    string `xml:"url,attr"`
}

type Attribute struct {
	Name  string `xml:"name,attr"`
	Value string `xml:"value,attr"`
}
