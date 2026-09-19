// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/torrplay/jacklet/pkg/database"
	"gopkg.in/yaml.v3"
)

func TestParseSize(t *testing.T) {
	tests := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{in: "1.5 GB", want: 1610612736},
		{in: "500 MB", want: 524288000},
		{in: "1.2GB", want: 1288490188},
		{in: "1,5 GB", want: 1610612736},
		{in: "1.2 TB", want: 1319413953331},
		{in: "3 TiB", want: 3298534883328},
		{in: "700 KiB", want: 716800},
		{in: "1 024 MB", want: 1073741824},
		{in: "1.5 ГБ", want: 1610612736},
		{in: "512 b", want: 512},
		// Many definitions select a site's raw byte count, which carries no
		// unit at all.
		{in: "86261275745", want: 86261275745},
		{in: "1568746393", want: 1568746393},
		{in: "0", want: 0},
		{in: "", wantErr: true},
		{in: "unknown", wantErr: true},
		{in: "N/A", wantErr: true},
		{in: "1.5 parsecs", wantErr: true},
		{in: "GB", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := parseSize(tc.in)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

// A definition with only legacylinks has an empty Links slice, which must
// not be indexed while resolving a row's relative URLs.
func TestScraper_LegacyLinksOnlyDefinition(t *testing.T) {
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<div class="row"><a href="/download/1">Legacy Release</a></div>`)
	}))
	defer testServer.Close()

	dir := t.TempDir()
	def := loadTestTracker(t, dir, "legacy-only", `
id: legacy-only
name: Legacy Only
legacylinks:
  - `+testServer.URL+`/
search:
  paths:
    - path: "/"
      method: get
  rows:
    selector: "div.row"
  fields:
    title:
      selector: "a"
    download:
      selector: "a"
      attribute: "href"
`)

	store, err := database.Open(context.Background(), ":memory:")
	require.NoError(t, err)
	defer store.Close()

	scrpr := New(store, NewConfigStore(""), "", testLogger())
	require.NoError(t, scrpr.ScrapeIndexer(context.Background(), def, SearchParams{Query: "legacy"}))

	require.Equal(t, testServer.URL+"/download/1", storedTorrent(t, scrpr, def, "Legacy Release").DownloadURL)
}

// When the primary link is unreachable, a row's relative URLs must resolve
// against the mirror that actually served it, not against Links[0].
func TestScraper_ResolvesURLsAgainstWorkingMirror(t *testing.T) {
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<div class="row"><a href="/download/1">Mirror Release</a><span class="d">/details/1</span></div>`)
	}))
	defer testServer.Close()

	dir := t.TempDir()
	def := loadTestTracker(t, dir, "mirrored", `
id: mirrored
name: Mirrored
links:
  - http://127.0.0.1:1/
legacylinks:
  - `+testServer.URL+`/
search:
  paths:
    - path: "/"
      method: get
  rows:
    selector: "div.row"
  fields:
    title:
      selector: "a"
    download:
      selector: "a"
      attribute: "href"
    details:
      selector: "span.d"
`)

	store, err := database.Open(context.Background(), ":memory:")
	require.NoError(t, err)
	defer store.Close()

	scrpr := New(store, NewConfigStore(""), "", testLogger())
	require.NoError(t, scrpr.ScrapeIndexer(context.Background(), def, SearchParams{Query: "mirror"}))

	stored := storedTorrent(t, scrpr, def, "Mirror Release")
	require.Equal(t, testServer.URL+"/download/1", stored.DownloadURL)
	require.Equal(t, testServer.URL+"/details/1", stored.DetailsURL)
}

// A torrent seen again on a later scrape must have its swarm counts
// refreshed rather than keeping the values it was first stored with.
func TestScraper_RefreshesExistingTorrent(t *testing.T) {
	seeders := "10"
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<div class="row"><a href="/download/1">Same Release</a><span class="s">`+seeders+`</span></div>`)
	}))
	defer testServer.Close()

	dir := t.TempDir()
	def := loadTestTracker(t, dir, "refresh", `
id: refresh
name: Refresh
links:
  - `+testServer.URL+`/
search:
  paths:
    - path: "/"
      method: get
  rows:
    selector: "div.row"
  fields:
    title:
      selector: "a"
    download:
      selector: "a"
      attribute: "href"
    seeders:
      selector: "span.s"
`)

	store, err := database.Open(context.Background(), ":memory:")
	require.NoError(t, err)
	defer store.Close()

	scrpr := New(store, NewConfigStore(""), "", testLogger())
	require.NoError(t, scrpr.ScrapeIndexer(context.Background(), def, SearchParams{}))

	require.Equal(t, 10, storedTorrent(t, scrpr, def, "Same Release").Seeders)

	// Re-scrape with a different swarm count, bypassing the rate limiter.
	seeders = "42"
	scrpr.scrapeState = make(map[string]*scrapeState)
	require.NoError(t, scrpr.ScrapeIndexer(context.Background(), def, SearchParams{}))

	require.Equal(t, 42, storedTorrent(t, scrpr, def, "Same Release").Seeders)
	require.Len(t, storedTorrents(t, scrpr, def), 1, "refreshing must not duplicate the row")
}

// The published column is compared lexicographically when pruning and
// parsed back when serving a feed, so it must be stored as UTC RFC3339 —
// and left empty, not zero-valued, when no date could be parsed.
func TestScraper_StoresNormalizedPublishedDate(t *testing.T) {
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `
			<div class="row"><a href="/d/1">Dated</a><span class="p">2024-03-05 14:30:00</span></div>
			<div class="row"><a href="/d/2">Undated</a><span class="p">not a date</span></div>`)
	}))
	defer testServer.Close()

	dir := t.TempDir()
	def := loadTestTracker(t, dir, "dates", `
id: dates
name: Dates
links:
  - `+testServer.URL+`/
search:
  paths:
    - path: "/"
      method: get
  rows:
    selector: "div.row"
  fields:
    title:
      selector: "a"
    download:
      selector: "a"
      attribute: "href"
    date:
      selector: "span.p"
`)

	store, err := database.Open(context.Background(), ":memory:")
	require.NoError(t, err)
	defer store.Close()

	scrpr := New(store, NewConfigStore(""), "", testLogger())
	require.NoError(t, scrpr.ScrapeIndexer(context.Background(), def, SearchParams{}))

	dated := storedTorrent(t, scrpr, def, "Dated").Published
	undated := storedTorrent(t, scrpr, def, "Undated").Published

	parsed, err := time.Parse(time.RFC3339, dated)
	require.NoError(t, err)
	// A tracker printing no offset means its own wall clock, which is read
	// as local time; the stored value is that instant normalized to UTC.
	// Asserting against a fixed "...Z" would only hold on a UTC host.
	require.Equal(t,
		time.Date(2024, time.March, 5, 14, 30, 0, 0, time.Local).UTC(), //nolint:gosmopolitan // asserting the local-time behaviour under test, on whatever host runs it
		parsed.UTC(),
	)
	require.Empty(t, undated, "an unparseable date must not be stored as the zero time")
}

// A GET definition's inputs belong in the query string even when the fetch
// is routed through FlareSolverr, or the tracker never sees them.
func TestScraper_FlareSolverrHonorsGetMethod(t *testing.T) {
	var gotCmd, gotURL, gotPostData string

	flare := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&payload))

		if payload["cmd"] == "sessions.create" {
			assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"status": "ok", "session": "s1"}))
			return
		}
		gotCmd, _ = payload["cmd"].(string)
		gotURL, _ = payload["url"].(string)
		gotPostData, _ = payload["postData"].(string)
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"status":   "ok",
			"solution": map[string]any{"response": `<div class="row"><a href="/d/1">Flare Release</a></div>`},
		}))
	}))
	defer flare.Close()

	dir := t.TempDir()
	def := loadTestTracker(t, dir, "flare-get", `
id: flare-get
name: Flare Get
links:
  - http://tracker.example/
search:
  paths:
    - path: search
      method: get
  inputs:
    q: "{{ .Keywords }}"
  rows:
    selector: "div.row"
  fields:
    title:
      selector: "a"
    download:
      selector: "a"
      attribute: "href"
`)

	db, err := database.Open(context.Background(), ":memory:")
	require.NoError(t, err)
	defer db.Close()

	scrpr := New(db, NewConfigStore(""), flare.URL, testLogger())
	require.NoError(t, scrpr.ScrapeIndexer(context.Background(), def, SearchParams{Query: "example"}))

	require.Equal(t, "request.get", gotCmd)
	require.Equal(t, "http://tracker.example/search?q=example", gotURL)
	require.Empty(t, gotPostData, "a GET must not carry a POST body")
}

func TestExpandCategoryIDs(t *testing.T) {
	t.Run("expands a parent into its subcategories", func(t *testing.T) {
		got := ExpandCategoryIDs([]string{"2000"})
		require.Contains(t, got, "2000")
		require.Contains(t, got, "2040") // Movies/HD
		require.Contains(t, got, "2080") // Movies/WEB-DL
		require.NotContains(t, got, "5040")
	})

	t.Run("leaves a subcategory alone", func(t *testing.T) {
		require.Equal(t, []string{"5040"}, ExpandCategoryIDs([]string{"5040"}))
	})

	t.Run("ignores a non-numeric category", func(t *testing.T) {
		require.Equal(t, []string{"junk"}, ExpandCategoryIDs([]string{"junk"}))
	})

	t.Run("returns nil for no categories", func(t *testing.T) {
		require.Nil(t, ExpandCategoryIDs(nil))
	})
}

// A Jackett definition using a subcategory name must map to that
// subcategory's ID rather than collapsing to Other.
func TestMapCategory_SubcategoryNames(t *testing.T) {
	def := &Tracker{Caps: Caps{CategoryMappings: []CategoryMapping{
		{Cat: "Movies/HD", ID: "11"},
		{Cat: "TV/Anime", ID: "22"},
		{Cat: "PC/Games", ID: "33"},
		{Cat: "Nonsense/Unknown", ID: "44"},
	}}}

	require.Equal(t, 2040, mapCategory(def, "11"))
	require.Equal(t, 5070, mapCategory(def, "22"))
	require.Equal(t, 4050, mapCategory(def, "33"))
	require.Equal(t, defaultCategoryID, mapCategory(def, "44"))
}

// Jackett's own definitions spell a site category id as a bare number, a
// quoted number, or a non-numeric name. Every one must decode, because a
// rejected id fails the whole definition file, not just the mapping.
func TestCategoryMappings_AcceptEverySpellingOfAnID(t *testing.T) {
	const definition = `
id: example
name: Example
caps:
  categorymappings:
    - {id: 48, cat: Movies/HD}
    - {id: "22", cat: TV/Anime}
    - {id: tv, cat: TV}
    - {id: 1.5, cat: PC/Games}
`

	var def Tracker
	require.NoError(t, yaml.Unmarshal([]byte(definition), &def))
	require.Equal(t, []CategoryMapping{
		{Cat: "Movies/HD", ID: "48"},
		{Cat: "TV/Anime", ID: "22"},
		{Cat: "TV", ID: "tv"},
		{Cat: "PC/Games", ID: "1.5"},
	}, def.Caps.CategoryMappings)

	// A non-numeric site category maps like any other.
	require.Equal(t, 5000, mapCategory(&def, "tv"))
	require.Equal(t, 2040, mapCategory(&def, "48"))
	// A row's scraped category is matched as text, whitespace trimmed.
	require.Equal(t, 5070, mapCategory(&def, " 22 "))
	require.Equal(t, defaultCategoryID, mapCategory(&def, "nosuch"))

	// The same opaque ids are what a search's ".Categories" carries. A
	// request for the TV parent selects the TV/Anime mapping as well.
	require.Equal(t, []string{"22", "tv"}, siteCategoryIDs(&def, []string{"5000"}))
}

func TestSearchModes(t *testing.T) {
	t.Run("uses the definition's declared modes", func(t *testing.T) {
		def := &Tracker{Caps: Caps{Modes: map[string][]string{
			"search":      {"q"},
			"book-search": {"q", "author"},
		}}}
		modes := SearchModes(def)
		require.Equal(t, []string{"q", "author"}, modes["book-search"])
		require.NotContains(t, modes, "tv-search")
		modes["book-search"][0] = "mutated"
		require.Equal(t, []string{"q", "author"}, def.Caps.Modes["book-search"])
	})

	t.Run("falls back when no modes are declared", func(t *testing.T) {
		modes := SearchModes(&Tracker{})
		require.Equal(t, defaultSearchModes, modes)
		modes["search"][0] = "mutated"
		delete(modes, "tv-search")
		require.Equal(t, []string{"q"}, SearchModes(&Tracker{})["search"])
		require.Contains(t, SearchModes(&Tracker{}), "tv-search")
	})
}

// An anti-bot or error page served with a 4xx/5xx status must fail the
// scrape rather than being parsed as a results page that happens to match
// nothing: reported as zero results, it would hide the real cause and
// leave the failure backoff disengaged.
func TestScraper_ErrorStatusFailsTheScrape(t *testing.T) {
	var userAgent string
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userAgent = r.UserAgent()
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `<html><body>Access denied</body></html>`)
	}))
	defer testServer.Close()

	dir := t.TempDir()
	def := loadTestTracker(t, dir, "forbidden", `
id: forbidden
name: Forbidden
links:
  - `+testServer.URL+`/
search:
  paths:
    - path: "/search"
      method: get
  rows:
    selector: "div.row"
  fields:
    title:
      selector: "a"
`)

	store, err := database.Open(context.Background(), ":memory:")
	require.NoError(t, err)
	defer store.Close()

	scrpr := New(store, NewConfigStore(""), "", testLogger())
	err = scrpr.ScrapeIndexer(context.Background(), def, SearchParams{Query: "anything"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "Forbidden")
	// The message names the path but never the query string, which carries
	// the search terms and, for some definitions, a passkey.
	require.Contains(t, err.Error(), "/search")
	require.NotContains(t, err.Error(), "anything")

	// The failure is recorded, so the tracker is backed off rather than
	// retried at the ordinary rate-limit interval.
	require.Equal(t, 1, scrpr.Status("forbidden").Failures)
	require.True(t, scrpr.Status("forbidden").BackedOff())

	// Go's default User-Agent is refused by many trackers, so one is sent.
	require.Contains(t, userAgent, "Mozilla/5.0")
	require.NotContains(t, userAgent, "Go-http-client")
}

// A definition that declares its own User-Agent must win over the default.
func TestScraper_DefinitionUserAgentWins(t *testing.T) {
	var userAgent string
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userAgent = r.UserAgent()
		fmt.Fprint(w, `<div class="row"><a>Release</a></div>`)
	}))
	defer testServer.Close()

	dir := t.TempDir()
	def := loadTestTracker(t, dir, "custom-ua", `
id: custom-ua
name: Custom UA
links:
  - `+testServer.URL+`/
search:
  paths:
    - path: "/"
      method: get
  headers:
    User-Agent: "jacklet-test/1.0"
  rows:
    selector: "div.row"
  fields:
    title:
      selector: "a"
`)

	store, err := database.Open(context.Background(), ":memory:")
	require.NoError(t, err)
	defer store.Close()

	scrpr := New(store, NewConfigStore(""), "", testLogger())
	require.NoError(t, scrpr.ScrapeIndexer(context.Background(), def, SearchParams{Query: "x"}))
	require.Equal(t, "jacklet-test/1.0", userAgent)
}

// The rate-limit window must key on the search, not just the tracker.
// Suppressing a *different* search because the tracker was scraped a
// moment ago answers it from the previous search's results, which is
// wrong rather than merely stale.
func TestScraper_RateLimitIsPerSearch(t *testing.T) {
	var mu sync.Mutex
	var queries []string
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		queries = append(queries, r.URL.Query().Get("q"))
		mu.Unlock()
		fmt.Fprint(w, `<div class="row"><a>`+html.EscapeString(r.URL.Query().Get("q"))+` Release</a></div>`)
	}))
	defer testServer.Close()

	dir := t.TempDir()
	def := loadTestTracker(t, dir, "per-search", `
id: per-search
name: Per Search
links:
  - `+testServer.URL+`/
search:
  paths:
    - path: "/"
      method: get
  inputs:
    q: "{{ .Keywords }}"
  rows:
    selector: "div.row"
  fields:
    title:
      selector: "a"
`)

	store, err := database.Open(context.Background(), ":memory:")
	require.NoError(t, err)
	defer store.Close()

	scrpr := New(store, NewConfigStore(""), "", testLogger())
	ctx := context.Background()

	require.NoError(t, scrpr.ScrapeIndexer(ctx, def, SearchParams{Query: "alpha"}))
	// A different search right behind it still reaches the tracker.
	require.NoError(t, scrpr.ScrapeIndexer(ctx, def, SearchParams{Query: "beta"}))
	// Differing only by category is a different search too.
	require.NoError(t, scrpr.ScrapeIndexer(ctx, def, SearchParams{Categories: []string{"2000"}, Query: "beta"}))
	// Repeating one already run inside the window is answered from the
	// store instead of hitting the site again.
	require.NoError(t, scrpr.ScrapeIndexer(ctx, def, SearchParams{Query: "alpha"}))

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"alpha", "beta", "beta"}, queries)
}

// Concurrent duplicates of one search must collapse onto a single scrape
// rather than each reserving its own.
func TestScraper_ConcurrentDuplicateSearchesScrapeOnce(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var hits atomic.Int64
	var enteredOnce sync.Once
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		enteredOnce.Do(func() { close(entered) })
		<-release
		fmt.Fprint(w, `<div class="row"><a>Release</a></div>`)
	}))
	defer testServer.Close()

	dir := t.TempDir()
	def := loadTestTracker(t, dir, "concurrent", `
id: concurrent
name: Concurrent
links:
  - `+testServer.URL+`/
search:
  paths:
    - path: "/"
      method: get
  rows:
    selector: "div.row"
  fields:
    title:
      selector: "a"
`)

	store, err := database.Open(context.Background(), ":memory:")
	require.NoError(t, err)
	defer store.Close()

	scrpr := New(store, NewConfigStore(""), "", testLogger())

	results := make(chan error, 2)
	go func() {
		results <- scrpr.ScrapeIndexer(context.Background(), def, SearchParams{Query: "same"})
	}()
	<-entered
	go func() {
		results <- scrpr.ScrapeIndexer(context.Background(), def, SearchParams{Query: "same"})
	}()

	select {
	case err := <-results:
		require.FailNowf(t, "a duplicate search did not wait",
			"it returned before the shared scrape completed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	require.NoError(t, <-results)
	require.NoError(t, <-results)

	require.Equal(t, int64(1), hits.Load())
	stored, err := store.Recent(context.Background(), "concurrent", 10)
	require.NoError(t, err)
	require.Len(t, stored, 1)
}

// A tracker inside its failure backoff is skipped rather than waited on:
// a backoff runs to ten minutes, which would strand the request.
func TestScraper_BackoffSkipsRatherThanWaits(t *testing.T) {
	var hits atomic.Int64
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer testServer.Close()

	dir := t.TempDir()
	def := loadTestTracker(t, dir, "backing-off", `
id: backing-off
name: Backing Off
links:
  - `+testServer.URL+`/
search:
  paths:
    - path: "/"
      method: get
  rows:
    selector: "div.row"
  fields:
    title:
      selector: "a"
`)

	store, err := database.Open(context.Background(), ":memory:")
	require.NoError(t, err)
	defer store.Close()

	scrpr := New(store, NewConfigStore(""), "", testLogger())
	require.Error(t, scrpr.ScrapeIndexer(context.Background(), def, SearchParams{Query: "one"}))
	require.Equal(t, int64(1), hits.Load())

	// A different search would ordinarily be allowed through, but the
	// tracker is now backed off, so it returns at once from the store.
	start := time.Now()
	require.NoError(t, scrpr.ScrapeIndexer(context.Background(), def, SearchParams{Query: "two"}))
	require.Less(t, time.Since(start), minScrapeSpacing)
	require.Equal(t, int64(1), hits.Load())
}
