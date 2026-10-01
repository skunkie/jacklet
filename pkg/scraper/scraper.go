// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

// Package scraper loads Cardigann-style YAML tracker definitions and
// scrapes their sites into torrent listings, applying each definition's
// field filters and category mappings along the way.
//
// A Scraper needs no storage: Scrape returns what it found. A program that
// wants the results kept passes a Sink in Options, which receives each page
// as it is scraped; the database package's Store is one.
package scraper

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/PuerkitoBio/goquery"
	"golang.org/x/net/html/charset"
)

// fetchTimeout bounds how long a single tracker's search request may take,
// so one slow or unreachable indexer cannot stall an entire scrape.
const fetchTimeout = 30 * time.Second

// flareSolverrTimeout bounds a FlareSolverr call. It is much larger than
// fetchTimeout because FlareSolverr drives a real browser through an
// anti-bot challenge, which routinely takes longer than a plain fetch.
const flareSolverrTimeout = 3 * time.Minute

// flareSolverrDestroyTimeout bounds destroying a replaced session. The
// destroy is sent on a context of its own, so a request that has already
// been cancelled cannot cause the session to be forgotten without it.
const flareSolverrDestroyTimeout = 10 * time.Second

// flareSolverrTimeoutMargin is how much sooner than the request's own
// deadline FlareSolverr is told to give up. FlareSolverr enforces its
// maxTimeout itself and answers with an ordinary error, so a slow challenge
// ends there instead of with Jacklet abandoning a request that is still
// running in the session's one tab.
const flareSolverrTimeoutMargin = 3 * time.Second

// flareSolverrMinTimeout is the least maxTimeout worth sending. FlareSolverr
// treats anything below a millisecond as unset and falls back to its own
// default, which is longer than the time that is left.
const flareSolverrMinTimeout = time.Second

// flareSessionMaxFailures is how many failed requests in a row a session
// survives. FlareSolverr keeps a session's browser after an error, so one
// that has crashed fails every request until it is destroyed, while a single
// failure is more often a challenge that did not solve in time, and
// destroying the session then would throw away the clearance it holds.
const flareSessionMaxFailures = 2

// flareSolverrBlockedMessage is what FlareSolverr says when the site has
// banned the address. A new browser cannot fix that, so it is not counted
// against the session.
const flareSolverrBlockedMessage = "Cloudflare has blocked this request"

// minScrapeInterval is how long one search's results are treated as
// current. A repeat of a search already run within this window is
// answered from the database instead of hitting the site again, which is
// what keeps rapid Torznab polling (e.g. Sonarr/Radarr RSS sync) from
// hammering a tracker.
//
// It deliberately does not suppress a *different* search of the same
// tracker: the store holds the previous search's results, so answering
// from it would return the wrong results rather than merely stale ones.
// Those are spaced by minScrapeSpacing instead.
const minScrapeInterval = 5 * time.Second

// minScrapeSpacing is the minimum gap between two *different* searches of
// one tracker. It is much shorter than minScrapeInterval because a client
// working through a library issues many distinct searches in a row: it is
// there to keep Jacklet from opening them all at once, not to slow the
// batch to a crawl.
const minScrapeSpacing = 1 * time.Second

// maxScrapeWait caps how long a search will wait its turn behind other
// searches of the same tracker. Beyond it, answering from the store beats
// making the client wait, since a Torznab client has its own timeout.
const maxScrapeWait = 15 * time.Second

// maxScrapeBackoff caps how long a persistently failing tracker is left
// alone, however many consecutive failures it has racked up.
const maxScrapeBackoff = 10 * time.Minute

// defaultMaxResponseBytes bounds how much of one response a scrape will
// buffer. A listing page is tens of kilobytes and the largest response any
// real definition produces is far below this, so the cap is invisible in
// normal use — it is there because nothing else bounds the size: the HTTP
// client's timeout bounds how *long* a response may take, not how large it
// may be, and the whole body is read into memory and then given a document
// tree of its own. The aggregate indexer makes that concurrent across as
// many trackers as maxConcurrentScrapes allows.
//
// Exceeding it fails the scrape rather than truncating: half a document
// parses into plausible-looking nonsense, which is worse than an error
// that says what happened.
const defaultMaxResponseBytes = 32 << 20 // 32 MiB

// DefaultMaxFlareSolverrSessions is how many FlareSolverr sessions are kept
// alive at once unless Options.MaxFlareSolverrSessions says otherwise. Each is
// a browser instance on the FlareSolverr host. It matches the aggregate's
// concurrency, which is how many can be in use at once, and not the number of
// trackers that use FlareSolverr: an aggregate search visits them in the same
// order every time, so when there are more of them than this each one finds
// its session just evicted for the one before it. It should cover them.
const DefaultMaxFlareSolverrSessions = 8

// scrapeState tracks one tracker's rate limiting and failure backoff.
//
// Backoff is per tracker — a site that is down is down for every search —
// while the de-duplication window is per search, since two different
// searches of one tracker yield different results and must not stand in
// for each other.
type scrapeState struct {
	failures int
	// hasWarnedFlareForm keeps the warning about a POST FlareSolverr cannot
	// carry faithfully to one line per tracker, rather than one per search.
	hasWarnedFlareForm bool
	// hasWarnedFlareHeaders keeps the warning about headers FlareSolverr
	// cannot carry to one line per tracker, rather than one per search.
	hasWarnedFlareHeaders bool
	inFlight              map[string]*scrapeFlight
	nextAllowed           time.Time
	recent                map[string]time.Time
}

// scrapeFlight is one scrape in progress, which every identical search
// that arrives meanwhile follows. followers counts them, and is guarded by
// scrapeStateMu like the rest of the scrape state. torrents is shared with
// those followers and must not be modified once published.
type scrapeFlight struct {
	done      chan struct{}
	err       error
	followers int
	torrents  []Torrent
}

// scrapePlan is what Scrape should do before hitting a tracker: skip and
// leave the answer to the caller's own copy, follow an identical scrape
// already in progress, or lead a new scrape after waiting out the
// rate-limit window.
type scrapePlan struct {
	flight *scrapeFlight
	// followers is how many searches, this one included, are following the
	// flight when this plan follows one.
	followers  int
	isFollower bool
	shouldSkip bool
	wait       time.Duration
}

// Scraper scrapes trackers for new torrents. A Scraper is safe for
// concurrent use and is meant to be constructed once and shared across
// requests, so its HTTP client, cookie jar, FlareSolverr sessions, and
// per-tracker rate limiting persist for the life of the process.
type Scraper struct {
	config      ConfigSource
	flareClient *http.Client
	// flareSessions holds one FlareSolverr session per tracker, keyed by
	// TrackerID. A session carries the browser's cookies, so giving each
	// tracker its own confines a lapsed login, a failed challenge or a
	// request that ran out of time to that tracker, and lets different
	// trackers be driven at the same time.
	flareSessions   map[string]*flareSession
	flareSessionsMu sync.Mutex
	flareSolverrURL string
	httpClient      *http.Client
	// isFlareClosed is set by Close, under flareSessionsMu, and stops a request
	// still running when the Scraper shuts down from creating a session
	// that nothing would destroy.
	isFlareClosed bool
	logger        *slog.Logger
	loginState    map[string]*loginState
	loginStateMu  sync.Mutex
	// magnetTrackers is Options.MagnetTrackers, copied so a caller
	// editing its slice afterwards cannot change what a running Scraper
	// puts in a magnet.
	magnetTrackers []string
	// maxDownloadBytes is Options.MaxDownloadBytes, or
	// defaultMaxDownloadBytes. It is a field rather than the constant so
	// that an embedding program can raise it and a test can lower it to a
	// size it can produce in a hurry.
	maxDownloadBytes int64
	// maxFlareSessions is Options.MaxFlareSolverrSessions, or
	// DefaultMaxFlareSolverrSessions.
	maxFlareSessions int
	// maxResponseBytes is Options.MaxResponseBytes, or
	// defaultMaxResponseBytes, and is a field for the same reasons as
	// maxDownloadBytes.
	maxResponseBytes int64
	// noRedirectClient is what a search fetch uses unless its path sets
	// followredirect. It shares httpClient's cookie jar, so a session
	// established through one is sent by the other.
	noRedirectClient *http.Client
	scrapeState      map[string]*scrapeState
	scrapeStateMu    sync.Mutex
	sink             Sink
}

// Options configures knobs that are deployment-specific rather than part
// of a tracker definition. A zero or negative field keeps the default, so
// the zero Options is what New builds.
//
// Neither byte cap can be turned off. A scrape reads a whole response into
// memory before parsing it, and a download is proxied to a client that has
// no way of its own to stop a tracker sending more, so "unlimited" would
// mean a tracker choosing how much memory or bandwidth an embedding
// program spends.
type Options struct {
	// MagnetTrackers are announce URLs (udp, http, https or wss) appended
	// as "tr" parameters to a magnet Jacklet builds from a bare info hash,
	// so a client reaches peers through them as well as through the DHT.
	// Empty, the default, builds a trackerless magnet. A magnet a tracker
	// supplied is passed on as it stands. The list is written into a row
	// when it is scraped, so a change reaches rows scraped afterwards and
	// leaves stored ones as they were until they are seen again. Magnets
	// are served to every client, so use only public announce URLs.
	MagnetTrackers []string
	// MaxDownloadBytes caps one proxied torrent file, 1 MiB by default.
	// Unlike a scrape this cannot fail cleanly: the body is already on its
	// way to the client when the cap is reached, so exceeding it breaks
	// the transfer rather than returning an error. A .torrent is a piece
	// hash list, well under a megabyte in practice, so this wants raising
	// only for a tracker that serves something unusual.
	MaxDownloadBytes int64
	// MaxFlareSolverrSessions is how many FlareSolverr sessions are kept
	// alive at once, DefaultMaxFlareSolverrSessions by default. Each session
	// is a browser instance on the FlareSolverr host, so this is a memory
	// decision on that host. It should still cover the trackers that use
	// FlareSolverr: a tracker whose session is evicted to stay within it
	// solves its challenge and logs in again on its next search, and with
	// more trackers than sessions that happens on every search. Sessions in
	// use are never evicted, so the count can exceed this by however many
	// requests are running at once.
	MaxFlareSolverrSessions int
	// MaxResponseBytes caps one scraped response, 32 MiB by default.
	// Raising it is a memory decision rather than a size one: the whole
	// body is buffered and given a document tree of its own, and an
	// aggregate search does that concurrently across trackers, so the cost
	// is the new value multiplied by that concurrency.
	MaxResponseBytes int64
	// Sink receives the torrents each scraped page produces, so a Scraper
	// can persist them. Nil, the default, persists nothing: Scrape still
	// returns what it found, which is all a program that keeps its own
	// results needs.
	Sink Sink
}

// Sink receives the torrents a scrape produced, one call per search page,
// so a program can keep them wherever it likes. It reports how many were
// stored. searchKey identifies the search (SearchParams.ResultKey), for a
// sink that remembers which search a row answered. An implementation must
// be safe for concurrent use, since trackers are scraped at the same time.
type Sink interface {
	UpsertAllForSearch(ctx context.Context, torrents []Torrent, searchKey string) (int, error)
}

// New creates a new Scraper with the default limits and no Sink, so it
// persists nothing and Scrape returns what it found. config supplies each
// tracker's setting overrides; pass NewConfigStore("") for a Scraper that
// uses only the defaults its definitions declare.
func New(config ConfigSource, flareSolverrURL string, logger *slog.Logger) *Scraper {
	return NewWithOptions(config, flareSolverrURL, logger, Options{})
}

// NewWithOptions creates a Scraper with deployment options, for an
// embedding program that needs limits other than the defaults.
func NewWithOptions(config ConfigSource, flareSolverrURL string, logger *slog.Logger, options Options) *Scraper {
	maxDownloadBytes := options.MaxDownloadBytes
	if maxDownloadBytes <= 0 {
		maxDownloadBytes = defaultMaxDownloadBytes
	}
	maxFlareSessions := options.MaxFlareSolverrSessions
	if maxFlareSessions <= 0 {
		maxFlareSessions = DefaultMaxFlareSolverrSessions
	}
	maxResponseBytes := options.MaxResponseBytes
	if maxResponseBytes <= 0 {
		maxResponseBytes = defaultMaxResponseBytes
	}

	// An address that is not usable is left as it was, and its requests fail
	// with an error that says so; a program that wants it refused up front
	// calls FlareSolverrEndpoint itself, as the server does at startup.
	if endpoint, err := FlareSolverrEndpoint(flareSolverrURL); err == nil {
		flareSolverrURL = endpoint
	}

	jar, _ := cookiejar.New(nil) // New never fails with nil options.
	return &Scraper{
		config:           config,
		flareClient:      &http.Client{Jar: jar, Timeout: flareSolverrTimeout},
		flareSessions:    make(map[string]*flareSession),
		flareSolverrURL:  flareSolverrURL,
		httpClient:       &http.Client{Jar: jar, Timeout: fetchTimeout},
		logger:           logger,
		loginState:       make(map[string]*loginState),
		magnetTrackers:   slices.Clone(options.MagnetTrackers),
		maxDownloadBytes: maxDownloadBytes,
		maxFlareSessions: maxFlareSessions,
		maxResponseBytes: maxResponseBytes,
		noRedirectClient: &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Jar:     jar,
			Timeout: fetchTimeout,
		},
		scrapeState: make(map[string]*scrapeState),
		sink:        options.Sink,
	}
}

// HasSink reports whether Options.Sink was set, that is, whether this
// Scraper hands what it finds to a store. One that has no Sink still returns
// every scrape's torrents, but nothing keeps them for a handler that answers
// searches from a store.
func (s *Scraper) HasSink() bool {
	return s != nil && s.sink != nil
}

// Close releases resources held by the Scraper across its lifetime — at
// present, destroying the FlareSolverr sessions so they don't linger on the
// FlareSolverr side after Jacklet shuts down. Safe to call even if no
// session was ever created.
func (s *Scraper) Close(ctx context.Context) error {
	s.flareSessionsMu.Lock()
	s.isFlareClosed = true
	var ids []string
	for _, session := range s.flareSessions {
		if session.id != "" {
			ids = append(ids, session.id)
			session.id = ""
		}
	}
	s.flareSessionsMu.Unlock()

	var errs []error
	for _, id := range ids {
		if _, err := s.flareSolverrRequest(ctx, map[string]any{"cmd": "sessions.destroy", "session": id}); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// SearchParams carries one Torznab search request's parameters down into a
// single indexer scrape.
type SearchParams struct {
	Album      string
	Artist     string
	Author     string
	Categories []string
	DoubanID   string
	Ep         string
	// Extended is the Newznab "extended" flag, passed through to
	// definitions that vary their request on it.
	Extended string
	Genre    string
	IMDBID   string
	Label    string
	// Limit and Offset are the client's paging, forwarded to definitions
	// that page on the tracker's side (Cardigann's ".Query.Limit" and
	// ".Query.Offset"). They do not bound what Jacklet stores: the store's
	// own paging is applied afterwards, over everything scraped.
	Limit     int
	Offset    int
	Publisher string
	Query     string
	Season    string
	TMDBID    string
	TVDBID    string
	TVMazeID  string
	TVRageID  string
	Title     string
	Track     string
	TraktID   string
	// Type is the Torznab "t" value the request arrived with, normalized
	// to its unhyphenated spec form ("search", "tvsearch", "movie",
	// "music", "book"), for definitions that branch on the search mode.
	Type string
	Year string
}

// keywords builds the effective search text sent to the tracker: the raw
// query, with season/episode folded in as "SxxEyy" when present. Cardigann
// definitions that lack dedicated season/episode inputs rely on this: they
// declare keywordsfilters that rewrite the "S01E02"-shaped text out of the
// query into whatever convention their site uses.
func (p SearchParams) keywords() string {
	kw := p.Query
	// A music or book client sends its terms in dedicated parameters and
	// leaves "q" empty; a definition with no matching input still needs
	// something to search for.
	for _, term := range []string{p.Artist, p.Album, p.Track, p.Author, p.Title} {
		if term != "" {
			kw = strings.TrimSpace(kw + " " + term)
		}
	}
	if p.Season != "" {
		kw = strings.TrimSpace(kw + " S" + padNumber(p.Season))
	}
	if p.Ep != "" {
		kw = strings.TrimSpace(kw + "E" + padNumber(p.Ep))
	}
	return kw
}

// cacheKey identifies the search these parameters describe, for the
// per-search de-duplication window in planScrape. Every parameter that
// can change what a tracker returns takes part, so two searches share a
// key only when they would produce the same results.
func (p SearchParams) cacheKey() string {
	categories := slices.Clone(p.Categories)
	slices.Sort(categories)

	// "\x00" cannot appear in a query parameter, so no combination of
	// field values can collide with a different combination.
	return strings.Join([]string{
		p.Album, p.Artist, p.Author, p.DoubanID, p.Ep, p.Extended, p.Genre,
		p.IMDBID, p.Label, p.Publisher, p.Query, p.Season, p.Title, p.TMDBID,
		p.Track, p.TraktID, p.TVDBID, p.TVMazeID, p.TVRageID, p.Type, p.Year,
		strings.Join(categories, ","),
	}, "\x00")
}

// ResultKey identifies the complete logical search independently of paging.
// Stores use it to associate scraped rows with the request that produced
// them, so a stale-result fallback cannot return rows from another search.
func (p SearchParams) ResultKey() string {
	return p.cacheKey()
}

// pagingKey is the part of a search's identity that only matters to a
// definition which pages on the tracker's side. It is appended to cacheKey
// for such a definition, so asking for the next page reaches the tracker
// instead of being answered from the previous page's scrape — and left off
// for every other definition, where two pages of one search are the same
// tracker request and re-issuing it would be a request the tracker did not
// need to serve.
func (p SearchParams) pagingKey() string {
	return fmt.Sprintf("\x00%d\x00%d", p.Limit, p.Offset)
}

// templateQuery renders these parameters as Cardigann's ".Query.*"
// template context.
func (p SearchParams) templateQuery() queryParams {
	flag := func(on bool) string {
		if on {
			return "True"
		}
		return ""
	}

	episode := ""
	if p.Season != "" {
		episode = "S" + padNumber(p.Season)
	}
	if p.Ep != "" {
		episode += "E" + padNumber(p.Ep)
	}

	idSearch := p.IMDBID != "" || p.TMDBID != "" || p.TVDBID != "" ||
		p.TVMazeID != "" || p.TVRageID != "" || p.TraktID != "" || p.DoubanID != ""

	q := queryParams{
		Album:         p.Album,
		Artist:        p.Artist,
		Author:        p.Author,
		Categories:    p.Categories,
		DoubanID:      p.DoubanID,
		Ep:            p.Ep,
		Episode:       episode,
		Extended:      p.Extended,
		Genre:         p.Genre,
		IMDBID:        p.IMDBID,
		IMDBIDShort:   strings.TrimPrefix(p.IMDBID, "tt"),
		IsBookSearch:  flag(p.Type == "book"),
		IsDoubanQuery: flag(p.DoubanID != ""),
		IsGenreQuery:  flag(p.Genre != ""),
		IsIdSearch:    flag(idSearch),
		IsImdbQuery:   flag(p.IMDBID != ""),
		IsMovieSearch: flag(p.Type == "movie"),
		IsMusicSearch: flag(p.Type == "music"),
		// An RSS-style request is one with nothing to search for: the
		// client is asking for the tracker's latest releases.
		IsRssSearch:   flag(p.keywords() == "" && !idSearch),
		IsSearch:      flag(p.Type == "search"),
		IsTVRageQuery: flag(p.TVRageID != ""),
		IsTVSearch:    flag(p.Type == "tvsearch"),
		Keywords:      p.keywords(),
		Label:         p.Label,
		Publisher:     p.Publisher,
		Q:             p.Query,
		Season:        p.Season,
		TMDBID:        p.TMDBID,
		TVDBID:        p.TVDBID,
		TVMazeID:      p.TVMazeID,
		TVRageID:      p.TVRageID,
		Title:         p.Title,
		Track:         p.Track,
		TraktID:       p.TraktID,
		Type:          p.Type,
		Year:          p.Year,
	}
	// Paging is rendered only when the client asked for a page, so a
	// definition's "{{ if .Query.Offset }}" stays false for a plain search
	// rather than reading as page zero.
	if p.Limit > 0 {
		q.Limit = strconv.Itoa(p.Limit)
	}
	if p.Offset > 0 {
		q.Offset = strconv.Itoa(p.Offset)
	}
	return q
}

// pagesServerSide reports whether a definition builds its request out of
// the client's paging, which is the only case where two pages of one
// search are two different tracker requests. Cardigann has no page loop of
// its own: a definition that pages does it by putting ".Query.Offset" or
// ".Query.Limit" into a path or an input, exactly as it does in Jackett.
func pagesServerSide(def *Tracker) bool {
	uses := func(s string) bool {
		return strings.Contains(s, ".Query.Offset") || strings.Contains(s, ".Query.Limit")
	}
	for _, input := range def.Search.Inputs {
		if uses(input) {
			return true
		}
	}
	for _, path := range def.Search.Paths {
		if uses(path.Path) {
			return true
		}
		for _, input := range path.Inputs {
			if uses(input) {
				return true
			}
		}
	}
	return false
}

// padNumber zero-pads a numeric string to 2 digits (e.g. "1" -> "01"); a
// non-numeric or already-wider value is returned unchanged.
func padNumber(s string) string {
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 || n > 99 {
		return s
	}
	return fmt.Sprintf("%02d", n)
}

// planScrape decides whether a search of id should hit the tracker, and
// reserves its slot when it should, so concurrent requests queue instead
// of all deciding to go at once. queryKey identifies the search itself
// (see SearchParams.cacheKey).
//
// A tracker inside its failure backoff is skipped outright. A repeat of a
// search run within minScrapeInterval is skipped too, since the store
// already holds that search's results. Anything else waits for the
// tracker's next free slot (minScrapeSpacing after the previous scrape),
// up to maxScrapeWait.
//
// An identical scrape already in progress is followed to completion rather
// than treated as a cached result, since its rows are not in the store yet.
func (s *Scraper) planScrape(id, queryKey string) scrapePlan {
	s.scrapeStateMu.Lock()
	defer s.scrapeStateMu.Unlock()

	now := time.Now()

	st, ok := s.scrapeState[id]
	if !ok {
		st = &scrapeState{
			inFlight: make(map[string]*scrapeFlight),
			recent:   make(map[string]time.Time),
		}
		s.scrapeState[id] = st
	}
	if st.inFlight == nil {
		st.inFlight = make(map[string]*scrapeFlight)
	}
	if st.recent == nil {
		st.recent = make(map[string]time.Time)
	}

	if flight, ok := st.inFlight[queryKey]; ok {
		flight.followers++
		return scrapePlan{flight: flight, followers: flight.followers, isFollower: true}
	}

	// A failing tracker is left alone entirely: waiting out a backoff of
	// up to maxScrapeBackoff would strand the request.
	if st.failures > 0 && now.Before(st.nextAllowed) {
		return scrapePlan{shouldSkip: true}
	}

	// A completed copy of this search is already in the store.
	if last, ok := st.recent[queryKey]; ok && now.Sub(last) < minScrapeInterval {
		return scrapePlan{shouldSkip: true}
	}

	start := now
	if st.nextAllowed.After(start) {
		start = st.nextAllowed
	}
	wait := start.Sub(now)
	if wait > maxScrapeWait {
		return scrapePlan{shouldSkip: true}
	}

	st.nextAllowed = start.Add(minScrapeSpacing)
	pruneRecentLocked(st, now)
	flight := &scrapeFlight{done: make(chan struct{})}
	st.inFlight[queryKey] = flight

	return scrapePlan{flight: flight, wait: wait}
}

// pruneRecentLocked drops de-duplication entries that can no longer
// suppress a scrape, so the map does not grow with every distinct search
// ever made. The caller holds scrapeStateMu.
func pruneRecentLocked(st *scrapeState, now time.Time) {
	for key, at := range st.recent {
		if now.Sub(at) >= minScrapeInterval {
			delete(st.recent, key)
		}
	}
}

// finishScrape publishes the leader's torrents and error to every follower,
// starts the completed-search de-duplication window on success, and updates
// failure backoff for the tracker.
func (s *Scraper) finishScrape(id, queryKey string, flight *scrapeFlight, torrents []Torrent, err error) {
	s.scrapeStateMu.Lock()
	defer s.scrapeStateMu.Unlock()

	st := s.scrapeState[id]
	if st == nil {
		flight.err = err
		flight.torrents = torrents
		close(flight.done)
		return
	}
	flight.err = err
	flight.torrents = torrents
	if st.inFlight[queryKey] == flight {
		delete(st.inFlight, queryKey)
	}
	if err == nil {
		st.failures = 0
		st.recent[queryKey] = time.Now()
		close(flight.done)
		return
	}

	if errors.Is(err, errFlareSolverrQueue) {
		close(flight.done)
		return
	}

	st.failures++
	backoff := min(minScrapeInterval*time.Duration(1<<min(st.failures, 8)), maxScrapeBackoff)
	if next := time.Now().Add(backoff); next.After(st.nextAllowed) {
		st.nextAllowed = next
	}
	close(flight.done)
}

// recoveredScrape reports what a scrape failed with, given whatever
// recover returned and the error the scrape itself was returning: a panic
// becomes an ordinary error, so the tracker fails the way an unreachable
// one does instead of taking the process down with it.
//
// Extraction runs over live, externally-served responses Jacklet does not
// control, and the aggregate indexer scrapes trackers in goroutines of its
// own, which net/http's per-request recovery does not reach. Recovering in
// Scrape rather than in that caller is what lets the failure reach
// the scrape's own bookkeeping, which has already been deferred by the
// time a panic starts unwinding.
//
// The caller passes recover() directly, since recover only stops a panic
// when the deferred function itself calls it.
func (s *Scraper) recoveredScrape(recovered any, def *Tracker, err error) error {
	if recovered == nil {
		return err
	}
	s.logger.Error("panic while scraping a tracker",
		"tracker", def.Name, "panic", recovered, "stack", string(debug.Stack()))
	return fmt.Errorf("tracker %s: panic: %v", def.Name, recovered)
}

// ErrThrottled is what Scrape returns when it fetched nothing: the tracker
// is inside its failure backoff, or the search repeats one that completed
// within minScrapeInterval, so the caller's own copy of that search's
// results is the answer. It is not a failure of the tracker.
var ErrThrottled = errors.New("search skipped: the tracker was scraped too recently")

// Scrape searches a single tracker definition and returns what it found,
// also passing each page to the Sink when one is configured. The context
// bounds the tracker's network requests. It returns an error only when
// every one of the tracker's links was unreachable — callers can surface
// that as a per-indexer error rather than a silent empty result — and
// ErrThrottled when nothing was fetched (see there). An identical search
// already in progress is followed rather than repeated, and its torrents
// are the ones returned; they are shared with the other followers, so a
// caller must not modify them. A different search of a recently scraped
// tracker waits out the window rather than being skipped (see planScrape).
func (s *Scraper) Scrape(ctx context.Context, def *Tracker, params SearchParams) (torrents []Torrent, err error) {
	logger := s.logger.With("tracker", def.Name)

	if len(def.Search.Paths) == 0 {
		return nil, fmt.Errorf("tracker %s: no search paths defined", def.Name)
	}

	baseURLs := candidateBaseURLs(def)
	if len(baseURLs) == 0 {
		return nil, fmt.Errorf("tracker %s: no valid links defined", def.Name)
	}

	trackerID := TrackerID(def)
	queryKey := params.cacheKey()
	if pagesServerSide(def) {
		queryKey += params.pagingKey()
	}
	plan := s.planScrape(trackerID, queryKey)
	if plan.shouldSkip {
		logger.Debug("skipping live scrape: rate limited", "query", params.Query)
		return nil, ErrThrottled
	}
	if plan.isFollower {
		logger.Debug("waiting for an identical scrape", "query", params.Query, "followers", plan.followers)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-plan.flight.done:
			return plan.flight.torrents, plan.flight.err
		}
	}
	defer func() { s.finishScrape(trackerID, queryKey, plan.flight, torrents, err) }()
	// Registered after finishScrape's defer, so it runs before it: the
	// recovered failure has to be in err by the time finishScrape reads
	// it. Otherwise a panicking scrape is recorded as a success — the
	// tracker's failure backoff reset, the de-duplication window armed for
	// results that were never stored, and followers of the same search
	// handed a nil error for rows they will not find.
	defer func() {
		err = s.recoveredScrape(recover(), def, err)
		if err != nil {
			torrents = nil
		}
	}()
	if plan.wait > 0 {
		logger.Debug("waiting for the rate-limit window", "query", params.Query, "wait", plan.wait)
		timer := time.NewTimer(plan.wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	logger.Info("scraping for torrents", "query", params.Query)

	cfg, overrides, err := resolveConfig(s.config, def)
	if err != nil {
		logger.Warn("failed to load config overrides", "error", err)
	}
	ctx = withFlareSolverrChoice(ctx, FlareSolverrOptedIn(overrides))

	categories := siteCategoryIDs(def, params.Categories)
	if len(categories) == 0 {
		// A search naming no category still has to reach the paths that
		// declare one, so Jackett falls back to the categories the
		// definition marked "default: true". Most definitions mark none,
		// and then this stays empty, which is what a definition's
		// "{{ if .Categories }}...{{ else }}" branch is written for.
		categories = DefaultCategoryIDs(def)
	}
	td := templateData{
		Categories: categories,
		False:      cardigannFalse,
		Query:      params.templateQuery(),
		Today:      newTodayVars(time.Now()),
		True:       cardigannTrue,
		encoding:   def.Encoding,
	}

	var lastErr error
	reachable := false
	for _, baseURL := range baseURLs {
		// ".Config.sitelink" names the mirror actually in use, not the
		// definition's first link: after a failover the first one is the
		// dead host, and a definition builds absolute URLs out of this.
		mirrorCfg := withSiteLink(cfg, baseURL)
		if err := s.ensureLoggedIn(ctx, def, baseURL, mirrorCfg); err != nil {
			logger.Warn("failed to authenticate", "base", baseURL.String(), "error", err)
			lastErr = err
			continue
		}

		td.Config = mirrorCfg
		td.Keywords = applyFilters(params.keywords(), def.Search.KeywordsFilters, td, logger)

		found, ok, err := s.scrapeMirror(ctx, def, baseURL, params.ResultKey(), td, logger)
		torrents = append(torrents, found...)
		if err != nil {
			lastErr = err
		}
		if ok {
			// This mirror is reachable; don't fail over to another one.
			reachable = true
			break
		}
	}

	if !reachable {
		return nil, fmt.Errorf("tracker %s: all links unreachable: %w", def.Name, lastErr)
	}

	logger.Info("scraping complete", "query", params.Query, "found", len(torrents))
	return torrents, nil
}

// scrapeMirror searches every path of one mirror and hands what it finds to
// the Sink. It returns the torrents found, whether the mirror answered
// at all (ok), and the last fetch error seen. A mirror that answers but
// matches nothing is still reachable — "no results" must not be read as
// "try the next mirror".
func (s *Scraper) scrapeMirror(ctx context.Context, def *Tracker, baseURL *url.URL, resultKey string, td templateData, logger *slog.Logger) (found []Torrent, ok bool, lastErr error) {
	pathsOK, skipped := 0, 0
	for i := range def.Search.Paths {
		searchPath := &def.Search.Paths[i]

		// A path may declare the categories it serves, so a definition can
		// keep movies on one page and music on another without fetching
		// both for every search. Its own ".Categories" narrows to the ones
		// it and the search have in common.
		pathTD := td
		categories, run := searchPath.selectCategories(td.Categories)
		if !run {
			logger.Debug("skipping search path: no category in common",
				"path", searchPath.Path, "categories", td.Categories)
			skipped++
			continue
		}
		pathTD.Categories = categories

		searchURL := baseURL.ResolveReference(&url.URL{Path: searchPath.Path})

		rows, err := s.fetchRows(ctx, def, searchPath, searchURL, pathForm(def, searchPath, pathTD, logger))
		if err != nil {
			logger.Warn("failed to scrape tracker", "base", baseURL.String(), "path", searchPath.Path, "error", err)
			lastErr = err
			// The session may have lapsed server-side; make the next scrape
			// re-authenticate rather than trusting the cached login until
			// it expires. A request that never got its turn asked the
			// tracker nothing, so the login stands.
			if !errors.Is(err, errFlareSolverrQueue) {
				s.invalidateLogin(TrackerID(def))
			}
			continue
		}
		pathsOK++
		found = append(found, s.storeRows(ctx, def, searchURL, resultKey, rows, pathTD, logger)...)
	}

	// A mirror whose every path was skipped was never asked anything, so
	// it is not unreachable — failing over to the next one would ask it
	// the same question and skip the same paths, and reporting the tracker
	// as down would turn "this search matches no page of this site" into
	// an error the client sees.
	if pathsOK == 0 && skipped == len(def.Search.Paths) {
		return found, true, nil
	}
	return found, pathsOK > 0, lastErr
}

// storeRows extracts each scraped row and passes the page to the Sink in
// one call, carrying a date from a preceding header row down to the rows
// beneath it. It returns the rows it extracted, whether or not the Sink
// kept them: a failed store is logged and does not discard a scrape.
//
// pageURL is the search page the rows came from, which is what their
// relative links resolve against.
func (s *Scraper) storeRows(ctx context.Context, def *Tracker, pageURL *url.URL, resultKey string, rows []resultRow, td templateData, logger *slog.Logger) []Torrent {
	torrents := make([]Torrent, 0, len(rows))
	currentDate := ""

	for _, row := range rows {
		if header, ok := dateHeader(def, row); ok {
			currentDate = applyFilters(header, def.Search.Rows.DateHeaders.Filters, td, logger)
			continue
		}

		data, ok := extractFields(def, row, td, logger)
		if !ok {
			continue
		}
		// A date carried by a preceding header row applies to every row
		// under it that has no date of its own.
		if currentDate != "" && data["date"] == "" {
			data["date"] = currentDate
		}
		// Row filters judge the finished row, so they run once every field
		// has been extracted and filtered.
		if skipRow(def, row, data["title"], td, logger) {
			continue
		}
		if torrent, ok := torrentFrom(def, pageURL, s.magnetTrackers, data); ok {
			torrents = append(torrents, torrent)
		}
	}

	if s.sink == nil {
		return torrents
	}
	// One call for the page rather than one per row: a store with a single
	// connection would otherwise serialize a separate write against every
	// concurrent reader.
	if _, err := s.sink.UpsertAllForSearch(ctx, torrents, resultKey); err != nil {
		logger.Error("failed to store scraped torrents", "rows", len(torrents), "error", err)
	}
	return torrents
}

// dateHeader reports whether a row is one of the date headers a definition
// declares, rather than a result.
func dateHeader(def *Tracker, row resultRow) (string, bool) {
	if def.Search.Rows.DateHeaders == nil {
		return "", false
	}
	header, ok := row.lookup(*def.Search.Rows.DateHeaders)
	if !ok || strings.TrimSpace(header) == "" {
		return "", false
	}
	return header, true
}

// pathForm renders the form a single search path submits: the shared
// search.inputs block, with the path's own inputs layered on top. A path
// that declares "inheritinputs: false" submits only its own.
func pathForm(def *Tracker, searchPath *SearchPath, td templateData, logger *slog.Logger) url.Values {
	form := url.Values{}

	render := func(inputs map[string]string) {
		for key, value := range inputs {
			rendered := renderTemplate(value, td, logger)
			if key == "$raw" {
				mergeRawFragment(form, rendered)
				continue
			}
			form.Set(key, rendered)
		}
	}

	if searchPath.InheritsInputs() {
		render(def.Search.Inputs)
	}
	render(searchPath.Inputs)
	return form
}

// candidateBaseURLs returns def's own links followed by its legacy links,
// parsed and skipping any that fail to parse, for use as fallback mirrors
// when the primary link is unreachable.
func candidateBaseURLs(def *Tracker) []*url.URL {
	links := make([]string, 0, len(def.Links)+len(def.LegacyLinks))
	links = append(links, def.Links...)
	links = append(links, def.LegacyLinks...)

	urls := make([]*url.URL, 0, len(links))
	for _, link := range links {
		if u, err := url.Parse(link); err == nil {
			urls = append(urls, u)
		}
	}
	return urls
}

// mergeRawFragment merges a rendered "$raw" input (a literal
// "key=value&key=value&..." querystring fragment, as produced by
// definitions like "{{ range .Categories }}f[]={{.}}&{{end}}") into form.
func mergeRawFragment(form url.Values, raw string) {
	raw = strings.TrimSuffix(raw, "&")
	if raw == "" {
		return
	}
	for pair := range strings.SplitSeq(raw, "&") {
		if pair == "" {
			continue
		}
		key, value, _ := strings.Cut(pair, "=")
		if k, err := url.QueryUnescape(key); err == nil {
			key = k
		}
		if v, err := url.QueryUnescape(value); err == nil {
			value = v
		}
		form.Add(key, value)
	}
}

// fetchRows retrieves a search page and selects its result rows, honoring
// the response format the search path declares (HTML by default, JSON when
// "response.type" says so).
func (s *Scraper) fetchRows(ctx context.Context, def *Tracker, searchPath *SearchPath, searchURL *url.URL, form url.Values) ([]resultRow, error) {
	body, err := s.fetch(ctx, def, searchPath, searchURL, form)
	if err != nil {
		return nil, err
	}

	// Cardigann's preprocessing filters run over the raw response before it
	// is parsed, for sites that need the payload repaired or unwrapped
	// first.
	if len(def.Search.PreprocessingFilters) > 0 {
		body = applyFilters(body, def.Search.PreprocessingFilters, templateData{encoding: def.Encoding}, s.logger)
	}

	if searchPath.IsJSON() {
		var document any
		if err := json.Unmarshal([]byte(body), &document); err != nil {
			return nil, fmt.Errorf("tracker %s: invalid JSON response: %w", def.Name, err)
		}
		if msg := searchPath.Response.NoResultsMessage; msg != "" && strings.Contains(body, msg) {
			return nil, nil
		}
		return applyRowLimits(jsonRows(document, def.Search.Rows.Selector), def.Search.Rows), nil
	}

	if searchPath.IsXML() {
		root, err := parseXML(body)
		if err != nil {
			return nil, fmt.Errorf("tracker %s: invalid XML response: %w", def.Name, err)
		}
		if msg := searchPath.Response.NoResultsMessage; msg != "" && strings.Contains(body, msg) {
			return nil, nil
		}
		// A definition's search.error blocks select against an HTML error
		// page, so they are not applied here — as in Jackett, which builds
		// its error check only for the HTML branch.
		return applyRowLimits(xmlRows(root, def.Search.Rows.Selector), def.Search.Rows), nil
	}

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(body))
	if err != nil {
		return nil, err
	}

	if err := checkSearchError(def, doc, s.logger); err != nil {
		return nil, err
	}

	selection := doc.Find(def.Search.Rows.Selector)
	if def.Search.Rows.Remove != "" {
		selection = selection.Not(def.Search.Rows.Remove)
	}

	rows := make([]resultRow, 0, selection.Length())
	selection.Each(func(_ int, sel *goquery.Selection) {
		rows = append(rows, htmlRow{sel: sel})
	})
	return applyRowLimits(rows, def.Search.Rows), nil
}

// applyRowLimits drops the leading rows a definition declares are headers
// rather than results.
func applyRowLimits(rows []resultRow, spec Rows) []resultRow {
	if spec.After > 0 && len(rows) > spec.After {
		return rows[spec.After:]
	}
	if spec.After > 0 {
		return nil
	}
	return rows
}

// checkSearchError surfaces an error the site reported inside an otherwise
// successful response — a "no permission" or "flood wait" page — so it is
// reported as a failed scrape instead of being read as zero results.
func checkSearchError(def *Tracker, doc *goquery.Document, logger *slog.Logger) error {
	return checkErrorBlocks(def, doc, def.Search.Error, logger)
}

// fetch retrieves the raw body of a tracker's search response, via
// FlareSolverr when configured, or directly otherwise. It honors the HTTP
// method declared by the search path.
func (s *Scraper) fetch(ctx context.Context, def *Tracker, searchPath *SearchPath, searchURL *url.URL, form url.Values) (string, error) {
	method := http.MethodPost
	if strings.EqualFold(searchPath.Method, "get") {
		method = http.MethodGet
	}

	// The tracker's declared character set applies to what is submitted as
	// well as to what comes back.
	encoded, err := encodeForm(form, def.Encoding)
	if err != nil {
		return "", err
	}

	if s.usesFlareSolverr(ctx, def) {
		s.warnUncarriedHeaders(def)
		s.warnFlareSolverrForm(def, method, form)
		page, err := s.scrapeWithFlareSolverr(ctx, TrackerID(def), method, searchURL, encoded)
		if err != nil {
			return "", err
		}
		return unwrapBrowserDocument(page, searchPath), nil
	}

	reqURL := searchURL.String()
	var body io.Reader
	if method == http.MethodGet {
		u := *searchURL
		u.RawQuery = encoded
		reqURL = u.String()
	} else {
		body = strings.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, reqURL, body)
	if err != nil {
		return "", err
	}
	if method == http.MethodPost {
		contentType := "application/x-www-form-urlencoded"
		if def.Encoding != "" {
			contentType += "; charset=" + def.Encoding
		}
		req.Header.Set("Content-Type", contentType)
	}
	// Set before the definition's own headers, so a definition that
	// declares a User-Agent still wins.
	req.Header.Set("User-Agent", defaultUserAgent)
	for key, value := range def.Search.Headers {
		req.Header.Set(key, headerValue(value))
	}

	// Cardigann does not follow a redirect from a search unless the path
	// asks it to, and a tracker that answers a search by redirecting to
	// its login page is why: following one scrapes that page as if it were
	// results, turning a lapsed session into a silent "no matches".
	// Whatever the redirect response carries is parsed instead, which for
	// a bare 302 is nothing — the same as Jackett.
	client := s.noRedirectClient
	if searchPath.FollowRedirect {
		client = s.httpClient
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	// A redirect left unfollowed yields whatever the 3xx carried, which is
	// usually nothing, so the scrape reports no results with no other clue
	// as to why.
	if !searchPath.FollowRedirect && isRedirect(resp.StatusCode) {
		s.logger.Debug("not following a redirect from a search path",
			"path", searchPath.Path, "status", resp.StatusCode,
			"hint", "add followredirect: true to this search path if the results are behind it")
	}

	if err := checkStatus(resp.StatusCode, searchURL); err != nil {
		return "", err
	}

	contentType := resp.Header.Get("Content-Type")
	if def.Encoding != "" {
		contentType = "text/html; charset=" + def.Encoding
	}

	// LimitedReader rather than io.LimitReader, so that N reaching zero
	// afterwards says the body ran past the cap rather than merely
	// reaching it.
	limited := &io.LimitedReader{N: s.maxResponseBytes + 1, R: resp.Body}
	reader, err := charset.NewReader(limited, contentType)
	if errors.Is(err, io.EOF) {
		// An empty body is an answer, not a failure: a redirect left
		// unfollowed carries none, and some trackers answer a search that
		// matches nothing with nothing at all. charset.NewReader reports
		// the empty read as io.EOF, and passing that on would have the
		// scrape report the tracker as unreachable — failing over to the
		// next mirror and, with none left, answering the client 502.
		return "", nil
	}
	if err != nil {
		return "", err
	}

	decoded, err := io.ReadAll(reader)
	if err != nil {
		return "", err
	}
	if limited.N == 0 {
		return "", fmt.Errorf("tracker %s: response is larger than %d bytes", def.Name, s.maxResponseBytes)
	}
	return string(decoded), nil
}

// defaultUserAgent is sent unless a definition declares its own. Go's
// default ("Go-http-client/1.1") is refused outright by a large share of
// trackers, and a refusal reads as an empty result rather than as an
// error, so the default is the difference between a definition working
// and appearing to match nothing.
const defaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36"

// warnUncarriedHeaders says once per tracker that a definition's
// search.headers are not reaching the site.
//
// A FlareSolverr request is made by a real browser, which supplies its own
// headers; FlareSolverr v3 has no option to override them, so there
// is nothing to forward and forwarding them anyway would only look like
// support. A definition relying on a header — an Accept, a Referer, an API
// key — will be answered differently or not at all, and the silence is the
// problem: this at least names the cause. Downloads are fetched directly
// and do apply the headers, so the two paths genuinely differ.
func (s *Scraper) warnUncarriedHeaders(def *Tracker) {
	if len(def.Search.Headers) == 0 {
		return
	}

	id := TrackerID(def)
	if !s.isFirstFlareWarning(id, func(st *scrapeState) *bool { return &st.hasWarnedFlareHeaders }) {
		return
	}
	names := make([]string, 0, len(def.Search.Headers))
	for name := range def.Search.Headers {
		names = append(names, name)
	}
	slices.Sort(names)
	s.logger.Warn("search headers are not sent when FlareSolverr is in use; the browser supplies its own",
		"tracker", id, "headers", strings.Join(names, ", "),
		"hint", "unset JACKLET_FLARESOLVERR_URL for this deployment if this indexer needs them")
}

// warnFlareSolverrForm says once per tracker when a POST sent through
// FlareSolverr cannot arrive as the definition wrote it. FlareSolverr does not
// forward the form: it reads it as UTF-8, replacing every byte that is not,
// and has the browser submit a new UTF-8 form, so a tracker whose encoding is
// not UTF-8 receives replacement characters where the search terms were and
// answers with its default listing. It also drops a field named "submit".
// Jacklet cannot change what the browser sends, so it names the cause.
func (s *Scraper) warnFlareSolverrForm(def *Tracker, method string, form url.Values) {
	if method != http.MethodPost {
		return
	}
	_, isLegacyEncoding, _ := formEncoding(def.Encoding)
	_, hasSubmit := form["submit"]
	if !isLegacyEncoding && !hasSubmit {
		return
	}

	id := TrackerID(def)
	if !s.isFirstFlareWarning(id, func(st *scrapeState) *bool { return &st.hasWarnedFlareForm }) {
		return
	}
	var causes []string
	if isLegacyEncoding {
		causes = append(causes, "the tracker's encoding is "+strings.TrimSpace(def.Encoding)+", and FlareSolverr submits the form as UTF-8, so the search terms arrive as replacement characters")
	}
	if hasSubmit {
		causes = append(causes, "FlareSolverr drops a form field named submit")
	}
	s.logger.Warn("this POST does not reach the tracker as the definition wrote it when FlareSolverr is in use",
		"tracker", id, "causes", strings.Join(causes, "; "),
		"hint", "turn off the flaresolverr setting for this indexer if it does not need the challenge solved")
}

// isFirstFlareWarning reports whether the flag warned selects, in the
// tracker's state, was still unset, and sets it, so a warning is one line per
// tracker and not one per search.
func (s *Scraper) isFirstFlareWarning(trackerID string, warned func(*scrapeState) *bool) bool {
	s.scrapeStateMu.Lock()
	defer s.scrapeStateMu.Unlock()

	st, ok := s.scrapeState[trackerID]
	if !ok {
		st = &scrapeState{recent: make(map[string]time.Time)}
		s.scrapeState[trackerID] = st
	}
	flag := warned(st)
	isFirst := !*flag
	*flag = true
	return isFirst
}

// isRedirect reports whether a status is one of the 3xx redirects.
func isRedirect(statusCode int) bool {
	return statusCode >= 300 && statusCode < 400
}

// checkStatus turns an HTTP error status into a scrape error. Without it a
// 403 anti-bot page, a 503, or a "you are banned" page is parsed as if it
// were a results page: it matches no rows, the mirror is recorded as
// reachable, and the scrape reports success with zero results — which
// hides the real cause from the operator and keeps the failure backoff
// from ever engaging.
//
// The query string is left out of the message because it carries the
// search terms and, for some definitions, a passkey.
func checkStatus(statusCode int, requestURL *url.URL) error {
	if statusCode < 400 {
		return nil
	}

	return fmt.Errorf("tracker responded %s%s", http.StatusText(statusCode), redactedTarget(requestURL))
}

// redactedTarget names a request's address for an error message, as " for
// <address>", without credentials or the query string. A tracker's query
// string carries the passkey on many sites, and these messages reach logs
// and the admin panel. Saying that it was dropped keeps the remains from
// reading as the whole URL, which makes a refused request look like a
// malformed one. A nil address names nothing.
func redactedTarget(requestURL *url.URL) string {
	if requestURL == nil {
		return ""
	}
	redacted := *requestURL
	redacted.User = nil
	hadQuery := redacted.RawQuery != ""
	redacted.RawQuery = ""
	target := " for " + redacted.String()
	if hadQuery {
		target += " (query redacted)"
	}
	return target
}

// headerValue renders a definition's header value, which YAML may decode
// as a bare scalar or as a single-element list.
func headerValue(value any) string {
	if list, ok := value.([]any); ok {
		parts := make([]string, 0, len(list))
		for _, item := range list {
			parts = append(parts, fmt.Sprintf("%v", item))
		}
		return strings.Join(parts, ", ")
	}
	return fmt.Sprintf("%v", value)
}

// extractFields resolves every field defined for a tracker against one
// scraped row, in the order the definition declared them. Declaration
// order matters: a field's Text or a filter's args may reference an
// earlier field via "{{ .Result.<name> }}", and FieldList preserves the
// YAML order so such dependencies resolve correctly.
//
// It reports ok=false when a required field's selector matched nothing,
// which marks the row as unusable rather than storing it with a hole in
// it. A field declared "optional" yields an empty value instead.
func extractFields(def *Tracker, row resultRow, td templateData, logger *slog.Logger) (map[string]string, bool) {
	rowData := make(map[string]string, len(def.Search.Fields))
	td.Result = rowData

	for i := range def.Search.Fields {
		nf := &def.Search.Fields[i]
		field := nf.Field

		var value string
		switch {
		case len(field.Case) > 0:
			resolved, matched := resolveCase(row, field)
			if !matched && !field.Optional {
				logger.Debug("skipping row: no case arm matched", "field", nf.Name)
				return nil, false
			}
			value = resolved

		case field.Selector != "":
			resolved, matched := row.lookup(field)
			if !matched {
				if !field.Optional {
					logger.Debug("skipping row: required field not found", "field", nf.Name, "selector", field.Selector)
					return nil, false
				}
				resolved = ""
			}
			value = resolved

		case field.Text != "":
			value = renderTemplate(field.Text, td, logger)
		}

		rowData[nf.Name] = applyFilters(value, field.Filters, td, logger)
	}

	return rowData, true
}

// resolveCase evaluates a field's "case" arms in declaration order and
// returns the value of the first one whose selector matches the row. The
// "*" arm matches anything and is the conventional default.
func resolveCase(row resultRow, field Field) (string, bool) {
	scope := row
	if field.Selector != "" {
		if _, matched := row.lookup(Field{Selector: field.Selector}); !matched {
			return "", false
		}
		scope = scopedRow{row: row, selector: field.Selector}
	}

	for _, arm := range field.Case {
		if scope.matches(arm.Selector) {
			return arm.Value, true
		}
	}
	return "", false
}

// scopedRow narrows a row to the subtree a field's selector picked out, so
// that field's case arms are evaluated relative to it.
type scopedRow struct {
	row      resultRow
	selector string
}

// debugString is the whole row's, not the narrowed subtree's: a scoped row
// exists only to resolve one field's case arms.
func (r scopedRow) debugString() string { return r.row.debugString() }

func (r scopedRow) matches(selector string) bool {
	if selector == "*" || selector == "" {
		return true
	}
	return r.row.matches(r.selector + " " + selector)
}

func (r scopedRow) lookup(field Field) (string, bool) {
	field.Selector = strings.TrimSpace(r.selector + " " + field.Selector)
	return r.row.lookup(field)
}

// torrentFrom turns one row's extracted fields into the torrent to store,
// reporting ok=false for a row with no title, which is not a usable
// result.
//
// pageURL is the search page the row was scraped from, and its relative
// links resolve against that, as they would in a browser: a definition
// whose search path sits in a subdirectory ("forum/tracker.php") carries
// links relative to that directory, so resolving them against the site
// root instead would address a page that does not exist.
//
// magnetTrackers are appended to a magnet built from an info hash; a
// magnet the tracker supplied is left as it is.
func torrentFrom(def *Tracker, pageURL *url.URL, magnetTrackers []string, data map[string]string) (Torrent, bool) {
	title := data["title"]
	if title == "" {
		return Torrent{}, false
	}

	infoHash := strings.TrimSpace(data["infohash"])

	// A tracker commonly offers both a torrent file and a magnet, and a
	// definition declares them as separate fields, so both are kept: one
	// is fetched through the tracker's session, the other is handed to a
	// client as it stands.
	magnet := magnetURI(data["magnet"])
	downloadURL := data["download"]
	if isMagnetURI(downloadURL) {
		magnet, downloadURL = strings.TrimSpace(downloadURL), ""
	}
	if downloadURL != "" {
		downloadURL = resolveReference(pageURL, downloadURL)
	}
	if magnet == "" && infoHash != "" && isPubliclyShared(def) {
		// Without magnetTrackers an info hash alone yields a magnet with
		// no trackers in it, so a client can only find peers through the
		// DHT. That works for a tracker whose torrents are on the DHT, and
		// a private tracker's are deliberately not, where such a link would
		// also leave the passkey behind.
		var built strings.Builder
		built.WriteString("magnet:?xt=")
		built.WriteString(btihURN)
		built.WriteString(infoHash)
		built.WriteString("&dn=")
		built.WriteString(url.QueryEscape(title))
		for _, tracker := range magnetTrackers {
			built.WriteString("&tr=")
			built.WriteString(url.QueryEscape(tracker))
		}
		magnet = built.String()
	}
	if infoHash == "" && magnet != "" {
		// The reverse: a definition that scrapes only a magnet still
		// names its info hash, inside the magnet. This needs no check on
		// the tracker's type, unlike building a magnet above -- reading a
		// hash out of a link already in hand discloses nothing the link
		// did not, where minting a trackerless magnet for a private
		// tracker would put its torrent on the DHT.
		infoHash = magnetInfoHash(magnet)
	}

	size, _ := parseSize(data["size"])

	// Counts go through Jackett's lenient coercion rather than a strict
	// parse: a tracker rendering "1,234" seeders must not read as zero.
	seeders := coercePeerCount(data["seeders"])
	leechers := coercePeerCount(data["leechers"])
	grabs := int(coerceLong(data["grabs"]))
	files := int(coerceLong(data["files"]))
	minimumSeedTime := int(coerceLong(data["minimumseedtime"]))
	minimumRatio := coerceDouble(data["minimumratio"])

	// Cardigann's volume factors describe a normal, fully-counted torrent
	// unless a definition scrapes something else (0 download = freeleech).
	// An absent field keeps the default; a present one is coerced, so a
	// tracker writing "0,5" is understood.
	downloadVolumeFactor := 1.0
	if raw, ok := data["downloadvolumefactor"]; ok && keepNumeric(raw) != "" {
		downloadVolumeFactor = coerceDouble(raw)
	}
	uploadVolumeFactor := 1.0
	if raw, ok := data["uploadvolumefactor"]; ok && keepNumeric(raw) != "" {
		uploadVolumeFactor = coerceDouble(raw)
	}

	// Store a normalized, timezone-independent timestamp: the column is
	// compared lexicographically when pruning and parsed back when serving
	// a feed's pubDate, and an empty string means "date unknown" rather
	// than "the zero time", which would otherwise look infinitely old.
	// The date field goes through the same resolution as the "fuzzytime"
	// filter, which is what Jackett applies here: a definition is not
	// required to declare a date filter for a tracker that prints a Unix
	// timestamp or "5 min ago", and without this such a row loses its date
	// entirely.
	published := ""
	if t, err := parseUnknownTimeValue(data["date"]); err == nil && !t.IsZero() {
		published = t.UTC().Format(time.RFC3339)
	}

	categoryID := defaultCategoryID
	if raw, ok := data["category_id"]; ok {
		categoryID = mapCategory(def, raw)
	} else if raw, ok := data["category"]; ok {
		categoryID = mapCategory(def, raw)
	}

	return Torrent{
		Category:             categoryID,
		Description:          data["description"],
		DetailsURL:           resolveReference(pageURL, data["details"]),
		DownloadURL:          downloadURL,
		DownloadVolumeFactor: downloadVolumeFactor,
		Files:                files,
		Grabs:                grabs,
		IMDBID:               extractDigits(firstNonEmpty(data["imdb"], data["imdbid"])),
		InfoHash:             infoHash,
		Leechers:             leechers,
		Magnet:               magnet,
		MinimumRatio:         minimumRatio,
		MinimumSeedTime:      minimumSeedTime,
		Name:                 title,
		Published:            published,
		Seeders:              seeders,
		Size:                 size,
		TVDBID:               extractDigits(data["tvdbid"]),
		Tracker:              TrackerID(def),
		UploadVolumeFactor:   uploadVolumeFactor,

		// The remaining Cardigann fields. The external ids are reduced to
		// their digits, as Jackett stores them as numbers; the media
		// metadata is passed through as scraped.
		Album:     data["album"],
		Artist:    data["artist"],
		Author:    data["author"],
		BookTitle: data["booktitle"],
		DoubanID:  extractDigits(data["doubanid"]),
		Genres:    parseGenres(data["genre"]),
		Label:     data["label"],
		Poster:    resolveReference(pageURL, data["poster"]),
		Publisher: data["publisher"],
		RageID:    extractDigits(data["rageid"]),
		TMDBID:    extractDigits(data["tmdbid"]),
		TVMazeID:  extractDigits(data["tvmazeid"]),
		Track:     data["track"],
		TraktID:   extractDigits(data["traktid"]),
		Year:      int(coerceLong(data["year"])),
	}, true
}

// extractDigits is Jackett's ParseUtil.GetLongFromString: it skips any
// leading non-digits and takes the first run of digits, so an id field
// holding "tt1234567" or "tmdb/955" yields just the number -- which is
// what Jackett puts in the corresponding Torznab attribute.
func extractDigits(value string) string {
	start := -1
	for i, r := range value {
		if r >= '0' && r <= '9' {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 {
			return value[start:i]
		}
	}
	if start >= 0 {
		return value[start:]
	}
	return ""
}

// parseGenres normalizes a scraped genre list the way Jackett does: split
// on the same delimiters "validate" uses, turn an underscore into a space,
// drop repeats, and join with ", ", which is the form the Torznab "genre"
// attribute takes.
//
// Splitting on a space is Jackett's choice and is kept deliberately, even
// though it means "Science Fiction" arrives as two genres: a definition's
// author wrote their genre field against that behaviour.
func parseGenres(value string) string {
	fields := strings.FieldsFunc(value, func(r rune) bool {
		return strings.ContainsRune(validateDelimiters, r)
	})

	seen := make(map[string]bool, len(fields))
	genres := make([]string, 0, len(fields))
	for _, field := range fields {
		genre := strings.ReplaceAll(field, "_", " ")
		if seen[genre] {
			continue
		}
		seen[genre] = true
		genres = append(genres, genre)
	}
	return strings.Join(genres, ", ")
}

// firstNonEmpty returns the first non-empty value, for a field Cardigann
// lets a definition spell more than one way.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// isMagnetURI reports whether raw is a magnet link. The scheme is matched
// without regard to case, as a URI scheme is defined.
func isMagnetURI(raw string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(raw)), "magnet:")
}

// magnetURI returns raw when it is a magnet link, and "" otherwise. A
// definition's magnet field is whatever a third-party page put there, and
// a stored value that is not a magnet would later be handed to a client,
// or redirected to, as though it were one.
func magnetURI(raw string) string {
	if !isMagnetURI(raw) {
		return ""
	}
	return strings.TrimSpace(raw)
}

// btihURN names a BitTorrent v1 info hash in a magnet's "xt" parameter.
// The link built from a hash and the hash read back out of a link share
// it, so the two cannot drift apart.
const btihURN = "urn:btih:"

// Lengths of the two encodings a magnet may name an info hash in.
const (
	hexInfoHashLength    = 40
	base32InfoHashLength = 32
)

// magnetInfoHash returns the BitTorrent v1 info hash a magnet names, or
// "" when it names none.
//
// A magnet may carry more than one "xt": a hybrid torrent names both its
// v1 and its v2 hash, in either order. The "btih" one is picked out
// rather than whichever comes first, and a v2-only magnet yields nothing,
// since its "btmh" multihash is not an info hash and a client matching on
// one would never match it.
func magnetInfoHash(magnet string) string {
	parsed, err := url.Parse(magnet)
	if err != nil {
		return ""
	}

	for _, xt := range parsed.Query()["xt"] {
		if len(xt) <= len(btihURN) || !strings.EqualFold(xt[:len(btihURN)], btihURN) {
			continue
		}
		if hash := xt[len(btihURN):]; isInfoHash(hash) {
			return hash
		}
	}
	return ""
}

// isInfoHash reports whether s is a BitTorrent v1 info hash in one of the
// two encodings a magnet may use: 40 hexadecimal digits, or 32 base32
// characters. Anything else is something other than a hash, and storing
// it would advertise a value no client can match on.
func isInfoHash(s string) bool {
	var allowed func(rune) bool
	switch len(s) {
	case hexInfoHashLength:
		allowed = isHexDigit
	case base32InfoHashLength:
		allowed = isBase32Digit
	default:
		return false
	}

	for _, r := range s {
		if !allowed(r) {
			return false
		}
	}
	return true
}

func isHexDigit(r rune) bool {
	return r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F'
}

// isBase32Digit accepts RFC 4648's alphabet unpadded, which is how a
// magnet spells a hash in base32, and its lowercase form besides, since a
// tracker that lowercases the link is naming the same hash.
func isBase32Digit(r rune) bool {
	return r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '2' && r <= '7'
}

// isPubliclyShared reports whether a definition's torrents are on the
// public DHT, which is what makes a magnet built from an info hash alone
// resolvable. A definition states this as "public" or "semi-private";
// anything else, including a definition that states nothing, is treated as
// private, since guessing wrong hands out a link that cannot work.
func isPubliclyShared(def *Tracker) bool {
	switch strings.ToLower(strings.TrimSpace(def.Type)) {
	case "public", "semi-private":
		return true
	default:
		return false
	}
}

// resolveReference resolves a possibly-relative URL scraped from a page
// against the URL of that page. An absolute ref, an empty ref, or a ref
// that doesn't parse is returned unchanged.
func resolveReference(pageURL *url.URL, ref string) string {
	if ref == "" || pageURL == nil {
		return ref
	}
	parsed, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return pageURL.ResolveReference(parsed).String()
}

// sizeRe splits a size into its number and optional unit, tolerating a
// missing separator ("1.2GB"), a comma decimal separator ("1,5 GB"), and
// digit grouping, all of which appear in real tracker markup. The unit is
// optional because many definitions extract a raw byte count instead of a
// human-readable size.
// sizeUnitExponents maps a lowercased unit to its power of 1024. Both the
// binary ("KiB") and the ambiguous decimal ("KB") spellings are treated as
// powers of 1024, which is what trackers mean in practice, alongside the
// Cyrillic abbreviations used by Russian-language trackers.
//
// The Cyrillic entries are a deliberate improvement on Jackett rather than
// a match for it: Jackett tests its unit with Contains("mb") and friends,
// which no Cyrillic spelling can satisfy, so it silently reads "1.5 ГБ" as
// one byte. Longer keys are matched first so that "mb" cannot win inside a
// longer unit.
var sizeUnitExponents = map[string]int{
	"b": 0, "б": 0,
	"kb": 1, "kib": 1, "кб": 1,
	"mb": 2, "mib": 2, "мб": 2,
	"gb": 3, "gib": 3, "гб": 3,
	"tb": 4, "tib": 4, "тб": 4,
	"pb": 5, "pib": 5,
}

// keepNumeric keeps only digits and the two separator characters, which is
// the first thing every one of Jackett's ParseUtil number helpers does.
func keepNumeric(value string) string {
	var b strings.Builder
	for _, r := range value {
		if (r >= '0' && r <= '9') || r == '.' || r == ',' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// normalizeDecimal turns a run of digits and separators into a number Go
// can parse, following Jackett's ParseUtil.NormalizeNumber: a comma is a
// decimal point, and when several separators remain, all but the last are
// digit grouping. That makes "1.018,29" 1018.29 rather than 1.01829 -- the
// difference between a megabyte figure and one a thousand times too small.
func normalizeDecimal(digits string) string {
	digits = strings.ReplaceAll(digits, ",", ".")
	if strings.Count(digits, ".") > 1 {
		last := strings.LastIndex(digits, ".")
		digits = strings.ReplaceAll(digits[:last], ".", "") + digits[last:]
	}
	return digits
}

// coerceDouble is Jackett's ParseUtil.CoerceDouble: lenient, and 0 when
// there is nothing numeric to find.
func coerceDouble(value string) float64 {
	digits := keepNumeric(value)
	if digits == "" {
		return 0
	}
	parsed, err := strconv.ParseFloat(normalizeDecimal(digits), 64)
	if err != nil {
		return 0
	}
	return parsed
}

// coerceLong is Jackett's ParseUtil.CoerceLong: both separators are digit
// grouping, and anything after a decimal point is dropped.
//
// Leniency is the point. Trackers render counts as "1,234", "1 234" or
// "12 seeders", and parsing those strictly yields 0 -- which reads
// downstream as a release with no seeders and gets it discarded.
func coerceLong(value string) int64 {
	digits := keepNumeric(value)
	if digits == "" {
		return 0
	}
	// With both separators present, "," groups digits and "." is the
	// decimal point, so the integer is everything before it. With only one
	// present, Jackett treats it as digit grouping whichever it is -- which
	// is why "1.234" seeders is 1234, not 1.
	if strings.Contains(digits, ",") && strings.Contains(digits, ".") {
		digits = digits[:strings.LastIndex(digits, ".")]
	}
	digits = strings.NewReplacer(",", "", ".", "").Replace(digits)
	if digits == "" {
		return 0
	}
	parsed, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0
	}
	return parsed
}

// maxPeerCount is the threshold above which Jackett treats a peer count as
// a parsing artefact and zeroes it (its issue #6558): some trackers put a
// placeholder in the column that reads as an enormous number, and a
// release claiming millions of seeders would outrank every genuine one.
const maxPeerCount = 5_000_000

// coercePeerCount parses a seeder or leecher count, applying that clamp.
func coercePeerCount(value string) int {
	if parsed := coerceLong(value); parsed < maxPeerCount {
		return int(parsed)
	}
	return 0
}

// sizeUnit resolves the letters found in a size string to a power of 1024.
// An exact match wins; otherwise the unit is searched for a known spelling,
// so a definition that scrapes "Size: 3.5 GB" without stripping the label
// still yields gigabytes, as it does in Jackett.
// sizeUnitsLongestFirst is every known unit ordered by rune count,
// longest first, so that a spelling contained inside a longer one cannot
// win -- "b" must not match before "gb", nor the Cyrillic "б" before "гб".
//
// The ordering is by runes rather than bytes because the Cyrillic units
// are multi-byte: "гб" is two runes but four bytes, so byte lengths put it
// behind the one-rune "б" and a labelled size like "Размер: 1.5 ГБ" came
// back as one byte.
var sizeUnitsLongestFirst = sortedSizeUnits()

func sortedSizeUnits() []string {
	units := slices.Collect(maps.Keys(sizeUnitExponents))
	slices.SortFunc(units, func(a, b string) int {
		if byLength := utf8.RuneCountInString(b) - utf8.RuneCountInString(a); byLength != 0 {
			return byLength
		}
		// Ties broken by name so the order is fixed between runs.
		return strings.Compare(a, b)
	})
	return units
}

func sizeUnit(unit string) (int, bool) {
	if exponent, ok := sizeUnitExponents[unit]; ok {
		return exponent, true
	}
	for _, known := range sizeUnitsLongestFirst {
		if strings.Contains(unit, known) {
			return sizeUnitExponents[known], true
		}
	}
	return 0, false
}

// parseSize parses a human-readable size string (e.g. "1.2 GB") into a
// number of bytes. An unrecognized unit is an error rather than a silently
// wrong number, so a caller never mistakes a mis-parsed size for a real
// one -- Jackett instead returns the bare number, which for "1.5 parsecs"
// means reporting a 1-byte release.
func parseSize(sizeStr string) (int64, error) {
	digits := keepNumeric(sizeStr)
	if digits == "" {
		return 0, fmt.Errorf("parseSize: no number in %q", sizeStr)
	}
	value, err := strconv.ParseFloat(normalizeDecimal(digits), 64)
	if err != nil {
		return 0, fmt.Errorf("parseSize: %q: %w", sizeStr, err)
	}

	// The unit is every letter in the string, as in Jackett, with "i"
	// dropped so that "KiB" and "KB" are the same unit.
	var letters strings.Builder
	for _, r := range strings.ToLower(sizeStr) {
		if unicode.IsLetter(r) && r != 'i' {
			letters.WriteRune(r)
		}
	}

	unit := letters.String()
	if unit == "" {
		// No unit means the value is already a byte count, which is what a
		// definition selecting a site's raw size field yields.
		return int64(value), nil
	}
	exponent, ok := sizeUnit(unit)
	if !ok {
		return 0, fmt.Errorf("parseSize: unknown unit %q", unit)
	}

	// Multiply step by step, as Jackett's BytesFrom* chain does, so the
	// floating-point rounding matches for large values.
	for range exponent {
		value *= 1024
	}
	return int64(value), nil
}

// FlareSolverrSetting is the per-tracker override, in the tracker's config
// file, that sends its requests through FlareSolverr. It is not declared by
// a definition: it belongs to the operator's deployment, not to the site.
const FlareSolverrSetting = "flaresolverr"

// FlareSolverrEndpoint returns the address requests are posted to for a
// configured FlareSolverr address, and an error for one that is not an http
// or https address. FlareSolverr accepts a POST only at /v1, so a bare
// address such as http://flaresolverr:8191, the form its own documentation
// gives, has /v1 added, and one that already names it is used as it is. An
// empty address means none is configured.
func FlareSolverrEndpoint(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	endpoint, err := url.Parse(raw)
	if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" {
		return "", fmt.Errorf("invalid FlareSolverr URL %q: must be an http or https address, such as http://localhost:8191", raw)
	}
	endpoint.Path = strings.TrimRight(endpoint.Path, "/")
	if endpoint.Path == "" {
		endpoint.Path = "/v1"
	}
	return endpoint.String(), nil
}

// FlareSolverrEnabled reports whether a FlareSolverr endpoint is
// configured, which is what gives FlareSolverrSetting any effect.
func (s *Scraper) FlareSolverrEnabled() bool { return s.flareSolverrURL != "" }

// flareSolverrChoiceKey carries, in a scrape's context, whether the tracker
// being scraped is opted in to FlareSolverr. The scrape reads the tracker's
// config file once, and every fetch it makes — a login page, the credential
// post, each search path — asks the context instead of parsing the file
// again.
type flareSolverrChoiceKey struct{}

func withFlareSolverrChoice(ctx context.Context, isOptedIn bool) context.Context {
	return context.WithValue(ctx, flareSolverrChoiceKey{}, isOptedIn)
}

// usesFlareSolverr reports whether a tracker's requests go through
// FlareSolverr: an endpoint is configured and the tracker's overrides opt it
// in. Every request through FlareSolverr drives a browser, which is slow and
// costs memory on the FlareSolverr host, so a tracker that needs no help
// with an anti-bot challenge is fetched directly. An unreadable override
// file reads as not opted in; the scrape reports it when it resolves the
// tracker's settings.
func (s *Scraper) usesFlareSolverr(ctx context.Context, def *Tracker) bool {
	if s.flareSolverrURL == "" {
		return false
	}
	if isOptedIn, ok := ctx.Value(flareSolverrChoiceKey{}).(bool); ok {
		return isOptedIn
	}
	overrides, err := s.config.Overrides(TrackerID(def))
	if err != nil {
		return false
	}
	return FlareSolverrOptedIn(overrides)
}

// FlareSolverrOptedIn reports whether a tracker's setting overrides opt it
// in to FlareSolverr. A hand-edited config file may write the value as a
// boolean or as a string such as "true", and the panel writes a boolean;
// anything else is not opting in.
func FlareSolverrOptedIn(overrides map[string]any) bool {
	switch value := overrides[FlareSolverrSetting].(type) {
	case bool:
		return value
	case string:
		isEnabled, _ := strconv.ParseBool(value)
		return isEnabled
	}
	return false
}

// errFlareSolverrQueue marks a request that ended while it waited for its
// turn on its tracker's FlareSolverr session. The tracker was never asked
// anything, so this is not a failure of the tracker and finishScrape leaves
// its failure count and backoff alone.
var errFlareSolverrQueue = errors.New("flaresolverr: gave up waiting for a turn")

// errFlareSolverrReply marks an answer from FlareSolverr that could not be
// used: a page over the size cap, or a reply that is not a FlareSolverr
// answer at all. The browser did its work, so this says nothing about the
// session and is not counted against it.
var errFlareSolverrReply = errors.New("flaresolverr: unusable reply")

// errScraperClosed is what a FlareSolverr request gets once Close has run.
var errScraperClosed = errors.New("scraper is closed")

// flareSession is one tracker's FlareSolverr session. FlareSolverr drives a
// session as a single browser tab and takes no lock around it, so two
// requests on it at once would navigate the same page and could return each
// other's results: slot admits one at a time.
type flareSession struct {
	// browserJar holds the cookies the session's browser reported after its
	// requests, for a download that has to present them itself, and is
	// guarded by Scraper.flareSessionsMu. It goes with the session.
	browserJar http.CookieJar
	// browserUserAgent is the user agent the browser presented, which a
	// Cloudflare clearance is bound to, and is guarded like browserJar.
	browserUserAgent string
	// cookies is a fingerprint of the cookies last sent to the session, so
	// they are sent again only when the session has not seen them or they
	// changed. It is reset with id, and is guarded by Scraper.flareSessionsMu.
	cookies string
	// failures counts the requests that failed in a row, and is guarded by
	// Scraper.flareSessionsMu.
	failures int
	// id is the session's name on the FlareSolverr side, empty until one is
	// created, and is guarded by Scraper.flareSessionsMu.
	id string
	// lastUsed is when a request last took the session, which is what picks
	// the one to evict, and is guarded by Scraper.flareSessionsMu.
	lastUsed  time.Time
	slot      chan struct{}
	trackerID string
}

// flareSessionFor returns the tracker's session entry, adding it on first
// use. It fails once Close has run.
func (s *Scraper) flareSessionFor(trackerID string) (*flareSession, error) {
	s.flareSessionsMu.Lock()
	defer s.flareSessionsMu.Unlock()

	if s.isFlareClosed {
		return nil, errScraperClosed
	}
	session, ok := s.flareSessions[trackerID]
	if !ok {
		session = &flareSession{slot: make(chan struct{}, 1), trackerID: trackerID}
		s.flareSessions[trackerID] = session
	}
	return session, nil
}

// flareSolverrSession returns the tracker's FlareSolverr session ID,
// creating one on first use. The session and the browser cookies it carries
// are reused for every request of the tracker, rather than re-solving an
// anti-bot challenge on every single search. The caller holds the tracker's
// slot, so nothing else creates or retires this session meanwhile.
func (s *Scraper) flareSolverrSession(ctx context.Context, session *flareSession) (string, error) {
	s.flareSessionsMu.Lock()
	id, isClosed := session.id, s.isFlareClosed
	session.lastUsed = time.Now()
	s.flareSessionsMu.Unlock()

	if id != "" {
		if id = s.verifyFlareSolverrSession(ctx, session, id); id == "" {
			s.invalidateLogin(session.trackerID)
		}
	}
	if id != "" {
		return id, nil
	}
	if isClosed {
		return "", errScraperClosed
	}

	s.evictIdleFlareSolverrSessions(ctx)
	resp, err := s.flareSolverrRequest(ctx, map[string]any{"cmd": "sessions.create"})
	if err != nil {
		return "", err
	}
	if resp.Session == "" {
		return "", errors.New("flaresolverr: sessions.create returned no session id")
	}

	s.flareSessionsMu.Lock()
	isClosed = s.isFlareClosed
	if !isClosed {
		session.id = resp.Session
	}
	s.flareSessionsMu.Unlock()
	if isClosed {
		// Close ran while this session was being created and could not
		// know about it.
		s.destroyFlareSolverrSession(ctx, resp.Session)
		return "", errScraperClosed
	}
	return resp.Session, nil
}

// verifyFlareSolverrSession asks FlareSolverr whether it still holds the
// session, before every use of it, and returns its id, or "" once it does
// not. Only before a request can it tell: a request that names a session
// FlareSolverr has lost makes a fresh one under that name, so a later check
// would find the id and trust an empty browser. Everything Jacklet
// tied to the old browser, the cookies it had been sent, what it kept for
// downloads, and the tracker's login, went with it, so all of that is
// forgotten and a new session is created in its place. When FlareSolverr
// cannot be asked the session is kept, and the request itself reports the
// failure.
func (s *Scraper) verifyFlareSolverrSession(ctx context.Context, session *flareSession, id string) string {
	resp, err := s.flareSolverrRequest(ctx, map[string]any{"cmd": "sessions.list"})
	if err != nil {
		return id
	}
	s.flareSessionsMu.Lock()
	defer s.flareSessionsMu.Unlock()
	if slices.Contains(resp.Sessions, id) {
		return id
	}
	if session.id == id {
		s.resetFlareSessionLocked(session)
	}
	return ""
}

// resetFlareSessionLocked forgets a session and everything tied to it. The
// caller holds flareSessionsMu, and has the tracker's login dropped once it
// has released it.
func (s *Scraper) resetFlareSessionLocked(session *flareSession) {
	session.id = ""
	session.cookies = ""
	session.failures = 0
	session.browserJar, session.browserUserAgent = nil, ""
}

// evictIdleFlareSolverrSessions destroys the least recently used idle
// sessions until there is room for one more within maxFlareSessions. A
// session is idle when no request holds its slot; one in use is never
// evicted, so when every session is busy the count is allowed to exceed the
// limit until they finish. The evicted tracker's cookies go with its
// session, so its login is forgotten and its next search signs in again.
func (s *Scraper) evictIdleFlareSolverrSessions(ctx context.Context) {
	type evicted struct{ id, trackerID string }
	var victims []evicted

	s.flareSessionsMu.Lock()
	live := 0
	for _, session := range s.flareSessions {
		if session.id != "" {
			live++
		}
	}
	for ; live >= s.maxFlareSessions; live-- {
		var oldest *flareSession
		for _, session := range s.flareSessions {
			if session.id == "" || len(session.slot) != 0 {
				continue
			}
			if oldest == nil || session.lastUsed.Before(oldest.lastUsed) {
				oldest = session
			}
		}
		if oldest == nil {
			break
		}
		victims = append(victims, evicted{id: oldest.id, trackerID: oldest.trackerID})
		s.resetFlareSessionLocked(oldest)
	}
	s.flareSessionsMu.Unlock()

	for _, victim := range victims {
		s.logger.Info("closing an idle FlareSolverr session to stay within the limit; raise it if this repeats, "+
			"since the tracker then solves its challenge and signs in again",
			"tracker", victim.trackerID, "limit", s.maxFlareSessions)
		s.destroyFlareSolverrSession(ctx, victim.id)
		s.invalidateLogin(victim.trackerID)
	}
}

// destroyFlareSolverrSession destroys a session on a context detached from
// the request, so a request that has been cancelled cannot cause its session
// to be forgotten without being destroyed. FlareSolverr keeps a session's
// browser alive until it is told otherwise. It is best effort: a session
// FlareSolverr has already dropped reports an error that changes nothing.
func (s *Scraper) destroyFlareSolverrSession(ctx context.Context, id string) {
	destroyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), flareSolverrDestroyTimeout)
	defer cancel()
	if _, err := s.flareSolverrRequest(destroyCtx, map[string]any{"cmd": "sessions.destroy", "session": id}); err != nil {
		s.logger.Debug("could not destroy a FlareSolverr session", "session", id, "error", err)
	}
}

// retireFlareSolverrSession destroys failed and forgets it, when it is
// still the tracker's session. The session's cookies go with it, so the
// tracker's login can no longer be trusted, and no other tracker's is
// touched: each has a session of its own.
func (s *Scraper) retireFlareSolverrSession(ctx context.Context, session *flareSession, failed string) {
	s.flareSessionsMu.Lock()
	isCurrent := session.id == failed
	if isCurrent {
		s.resetFlareSessionLocked(session)
	}
	s.flareSessionsMu.Unlock()

	if !isCurrent {
		return
	}
	s.destroyFlareSolverrSession(ctx, failed)
	s.invalidateLogin(session.trackerID)
}

type flareSolverrResponse struct {
	Message  string   `json:"message"`
	Session  string   `json:"session"`
	Sessions []string `json:"sessions"`
	Solution struct {
		Cookies   []flareSolverrCookie `json:"cookies"`
		Response  string               `json:"response"`
		Status    int                  `json:"status"`
		URL       string               `json:"url"`
		UserAgent string               `json:"userAgent"`
	} `json:"solution"`
	Status string `json:"status"`
}

func (s *Scraper) flareSolverrRequest(ctx context.Context, payload map[string]any) (*flareSolverrResponse, error) {
	reqBody, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.flareSolverrURL, bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.flareClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	limited := &io.LimitedReader{N: s.maxResponseBytes + 1, R: resp.Body}
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if limited.N == 0 {
		return nil, fmt.Errorf("%w: response is larger than %d bytes", errFlareSolverrReply, s.maxResponseBytes)
	}

	var result flareSolverrResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("%w: HTTP %s: %w", errFlareSolverrReply, resp.Status, err)
	}
	if result.Status != "" && result.Status != "ok" {
		return nil, fmt.Errorf("flaresolverr: %s", result.Message)
	}

	return &result, nil
}

// cookieFingerprint identifies a set of cookies by name and value, in an
// order that does not depend on how the jar listed them. It is a hash so the
// values are not held a second time.
func cookieFingerprint(cookies []*http.Cookie) string {
	sorted := slices.Clone(cookies)
	slices.SortFunc(sorted, func(a, b *http.Cookie) int { return strings.Compare(a.Name, b.Name) })
	hash := sha256.New()
	for _, cookie := range sorted {
		fmt.Fprintf(hash, "%q=%q;", cookie.Name, cookie.Value)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// flareSessionLacksCookies reports whether the tracker's current session has
// not been sent the cookies with this fingerprint.
func (s *Scraper) flareSessionLacksCookies(session *flareSession, fingerprint string) bool {
	s.flareSessionsMu.Lock()
	defer s.flareSessionsMu.Unlock()
	return session.cookies != fingerprint
}

// flareSessionSentCookies records that the session now holds the cookies
// with this fingerprint.
func (s *Scraper) flareSessionSentCookies(session *flareSession, fingerprint string) {
	s.flareSessionsMu.Lock()
	defer s.flareSessionsMu.Unlock()
	session.cookies = fingerprint
}

// flareSolverrMaxTimeout is the maxTimeout, in milliseconds, to send for a
// request with this context: the time left before its deadline, less the
// margin, so FlareSolverr stops before Jacklet would. It reports false when
// the context has no deadline, and FlareSolverr's own default applies.
func flareSolverrMaxTimeout(ctx context.Context) (int64, bool) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return 0, false
	}
	remaining := max(time.Until(deadline)-flareSolverrTimeoutMargin, flareSolverrMinTimeout)
	return remaining.Milliseconds(), true
}

// noteFlareSolverrFailure decides what a failed request means for its
// session.
//
// A request whose context was cancelled may still be running in the
// session's one tab, and the next request would start navigating that same
// tab, so the session is retired. A request that outlived its deadline is
// not: FlareSolverr was told to stop before then, and the session is judged
// like any other failure. Those count, and the session is retired only when
// they come in a row, except for a banned address or an unusable reply, which
// say nothing about the browser.
func (s *Scraper) noteFlareSolverrFailure(ctx context.Context, session *flareSession, id string, err error) {
	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		s.retireFlareSolverrSession(ctx, session, id)
	case strings.Contains(err.Error(), flareSolverrBlockedMessage), errors.Is(err, errFlareSolverrReply):
	default:
		s.flareSessionsMu.Lock()
		session.failures++
		isSpent := session.failures >= flareSessionMaxFailures
		s.flareSessionsMu.Unlock()
		if isSpent {
			s.retireFlareSolverrSession(ctx, session, id)
		}
	}
}

// flareSessionSucceeded ends a run of failures.
func (s *Scraper) flareSessionSucceeded(session *flareSession) {
	s.flareSessionsMu.Lock()
	defer s.flareSessionsMu.Unlock()
	session.failures = 0
}

// flareSolverrCookies renders cookies in the shape FlareSolverr's request
// commands take: a list of name and value pairs.
func flareSolverrCookies(cookies []*http.Cookie) []map[string]string {
	out := make([]map[string]string, 0, len(cookies))
	for _, cookie := range cookies {
		out = append(out, map[string]string{"name": cookie.Name, "value": cookie.Value})
	}
	return out
}

// scrapeWithFlareSolverr fetches a search page through FlareSolverr,
// mirroring the HTTP method the definition declared: a GET definition's
// inputs belong in the query string, not in a POST body, or the tracker
// never sees them.
func (s *Scraper) scrapeWithFlareSolverr(ctx context.Context, trackerID, method string, searchURL *url.URL, encodedForm string) (string, error) {
	payload := map[string]any{"cmd": "request.post", "postData": encodedForm}
	if method == http.MethodGet {
		u := *searchURL
		u.RawQuery = encodedForm
		payload = map[string]any{"cmd": "request.get"}
		searchURL = &u
	}
	payload["url"] = searchURL.String()

	tracker, err := s.flareSessionFor(trackerID)
	if err != nil {
		return "", err
	}

	// FlareSolverr's browser holds its own cookies and cannot see Jacklet's
	// cookie jar, so a cookie the operator configured for a tracker is passed
	// along, and only the ones that apply to this address. A session keeps
	// them once it has them, and FlareSolverr loads the page a second time
	// whenever a request carries any, so they are sent only to a session that
	// has not seen them or when they have changed.
	cookies := s.httpClient.Jar.Cookies(searchURL)
	fingerprint := cookieFingerprint(cookies)

	select {
	case tracker.slot <- struct{}{}:
		defer func() { <-tracker.slot }()
	case <-ctx.Done():
		return "", fmt.Errorf("%w: %w", errFlareSolverrQueue, ctx.Err())
	}

	session, err := s.flareSolverrSession(ctx, tracker)
	if err != nil {
		return "", err
	}

	req := make(map[string]any, len(payload)+3)
	maps.Copy(req, payload)
	req["session"] = session
	if len(cookies) > 0 && s.flareSessionLacksCookies(tracker, fingerprint) {
		req["cookies"] = flareSolverrCookies(cookies)
	}
	if timeout, ok := flareSolverrMaxTimeout(ctx); ok {
		req["maxTimeout"] = timeout
	}

	resp, err := s.flareSolverrRequest(ctx, req)
	if err != nil {
		s.noteFlareSolverrFailure(ctx, tracker, session, err)
		return "", err
	}
	s.flareSessionSucceeded(tracker)

	if len(cookies) > 0 {
		s.flareSessionSentCookies(tracker, fingerprint)
	}
	s.rememberFlareSolverrBrowser(tracker, browserPageURL(searchURL, resp.Solution.URL), resp.Solution.Cookies, resp.Solution.UserAgent)

	// A tracker that answered with an error status and no body leaves the
	// browser on its own error page, and that page is not the tracker's.
	if strings.HasPrefix(resp.Solution.URL, browserErrorPageScheme) {
		return "", fmt.Errorf("%w%s", errBrowserErrorPage, redactedTarget(searchURL))
	}

	// FlareSolverr answers "ok" as long as it drove the browser
	// successfully, whatever the tracker itself replied, and reports the
	// page's status as 200 whatever it was, because Selenium does not expose
	// it. This can therefore only refuse a status a later FlareSolverr
	// reports; an error page that has a body looks like any other page.
	if err := checkStatus(resp.Solution.Status, searchURL); err != nil {
		return "", err
	}

	return resp.Solution.Response, nil
}
