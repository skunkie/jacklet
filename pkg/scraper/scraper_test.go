// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// loadTestTracker writes def to a YAML file under dir and loads it back by
// id through the same DefinitionStore production code uses.
func loadTestTracker(t *testing.T, dir, id, def string) *Tracker {
	t.Helper()
	require.NoError(t, os.WriteFile(dir+"/"+id+".yml", []byte(def), 0o600))
	tracker, err := NewDefinitionStore(dir, slog.New(slog.DiscardHandler)).Find(id)
	require.NoError(t, err)
	return tracker
}

// A Scraper guards scrapeState and loginState with a mutex each, so a test
// reaching into either map goes through the helpers below rather than
// indexing it directly. Doing it by hand is safe in a single-goroutine
// test and teaches the next one that the lock is optional, which is how a
// parallel test later arrives at a real data race.

// setScrapeState records a tracker's failure count and backoff deadline,
// standing in for the scrapes that would otherwise have to fail first.
func (s *Scraper) setScrapeState(trackerID string, failures int, nextAllowed time.Time) {
	s.scrapeStateMu.Lock()
	defer s.scrapeStateMu.Unlock()
	s.scrapeState[trackerID] = &scrapeState{failures: failures, nextAllowed: nextAllowed}
}

// clearScrapeState forgets every tracker's rate-limit and backoff
// bookkeeping, so the next scrape of a search just made is not skipped as a
// repeat.
func (s *Scraper) clearScrapeState() {
	s.scrapeStateMu.Lock()
	defer s.scrapeStateMu.Unlock()
	s.scrapeState = make(map[string]*scrapeState)
}

// scrapeStateSnapshot copies a tracker's scrape state so the caller can
// read it after the mutex is released, reporting false when no scrape has
// recorded anything yet.
func (s *Scraper) scrapeStateSnapshot(trackerID string) (scrapeState, bool) {
	s.scrapeStateMu.Lock()
	defer s.scrapeStateMu.Unlock()
	state, ok := s.scrapeState[trackerID]
	if !ok {
		return scrapeState{}, false
	}
	return *state, true
}

// expireLoginState ages a confirmed session out, as the clock would.
func (s *Scraper) expireLoginState(trackerID string) {
	s.loginStateMu.Lock()
	defer s.loginStateMu.Unlock()
	if state, ok := s.loginState[trackerID]; ok {
		state.validUntil = time.Now().Add(-time.Second)
	}
}

// A scrape stores each row it extracts under its tracker, and scraping the
// same search again refreshes those rows rather than adding copies. Rows are
// keyed by tracker and identity, so a scrape must fill in the tracker for a
// re-scrape to refresh that tracker's own rows and never another's.
func TestScraperStoresEachRowOnce(t *testing.T) {
	const testHTML = `
		<table>
			<tr class="torrent_row">
				<td><a href="/download/123">Test Torrent 1</a></td>
				<td>1.5 GB</td>
				<td>100</td>
				<td>10</td>
				<td>2024-01-01</td>
			</tr>
			<tr class="torrent_row">
				<td><a href="/download/456">Test Torrent 2</a></td>
				<td>500 MB</td>
				<td>50</td>
				<td>5</td>
				<td>2024-02-15</td>
			</tr>
		</table>
	`

	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(testHTML))
	}))
	defer testServer.Close()

	testDef := fmt.Sprintf(`
id: test-tracker
name: test-tracker
links:
  - %s/
search:
  paths:
    - path: "/"
  rows:
    selector: ".torrent_row"
  fields:
    title:
      selector: "a"
    download:
      selector: "a"
      attribute: "href"
    size:
      selector: "td:nth-child(2)"
    seeders:
      selector: "td:nth-child(3)"
    leechers:
      selector: "td:nth-child(4)"
    date:
      selector: "td:nth-child(5)"
`, testServer.URL)

	store := &fakeStore{}

	tempDefsDir := t.TempDir()
	def := loadTestTracker(t, tempDefsDir, "test-tracker", testDef)

	scraper := NewWithOptions(NewConfigStore(""), "", slog.New(slog.DiscardHandler), Options{Sink: store})
	require.NoError(t, scraper.scrapeIndexer(t.Context(), def, SearchParams{Query: "test"}))

	require.Len(t, storedTorrents(t, scraper, def), 2, "Expected 2 torrents to be inserted")

	// Clear the rate-limit bookkeeping so this second call re-scrapes
	// instead of being skipped as too soon.
	scraper.clearScrapeState()
	require.NoError(t, scraper.scrapeIndexer(t.Context(), def, SearchParams{Query: "test"}))
	stored := storedTorrents(t, scraper, def)
	require.Len(t, stored, 2, "Expected re-scraping the same query not to duplicate torrents")
	require.Equal(t, "test-tracker", stored[0].Tracker)
}

// A page served in a legacy charset is decoded before its fields are
// extracted, so a Cyrillic title is stored as text rather than as mojibake.
func TestScraperDecodesALegacyCharset(t *testing.T) {
	// "Проверка" ("Check") in its windows-1251 bytes.
	var sampleNonUnicodeBytes = []byte{
		0xcf, 0xf0, 0xee, 0xe2, 0xe5, 0xf0, 0xea, 0xe0, // Проверка
	}
	sampleNonUnicodeName := string(sampleNonUnicodeBytes)
	const expectedDecodedName = "Проверка"
	const testHTML = `
		<table>
			<tr class="torrent_row">
				<td><a href="/download.php?id=123">TORRENT_NAME_PLACEHOLDER</a></td>
				<td>1.2 GB</td>
				<td>100</td>
				<td>10</td>
				<td>2024-03-15</td>
			</tr>
		</table>
	`

	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=windows-1251")
		// Safely replace the placeholder with the non-Unicode name.
		html := strings.Replace(testHTML, "TORRENT_NAME_PLACEHOLDER", sampleNonUnicodeName, 1)
		w.Write([]byte(html))
	}))
	defer testServer.Close()

	testDef := fmt.Sprintf(`
id: test-non-unicode-tracker
name: test-non-unicode-tracker
encoding: windows-1251
links:
  - %s/
search:
  paths:
    - path: "/"
  rows:
    selector: ".torrent_row"
  fields:
    title:
      selector: "a"
    download:
      selector: "a"
      attribute: "href"
    size:
      selector: "td:nth-child(2)"
    seeders:
      selector: "td:nth-child(3)"
    leechers:
      selector: "td:nth-child(4)"
    date:
      selector: "td:nth-child(5)"
`, testServer.URL)

	store := &fakeStore{}

	tempDefsDir := t.TempDir()
	def := loadTestTracker(t, tempDefsDir, "test-non-unicode-tracker", testDef)

	scraper := NewWithOptions(NewConfigStore(""), "", slog.New(slog.DiscardHandler), Options{Sink: store})
	require.NoError(t, scraper.scrapeIndexer(t.Context(), def, SearchParams{Query: "test"}))

	stored := storedTorrents(t, scraper, def)
	require.Len(t, stored, 1)
	require.Equal(t, expectedDecodedName, stored[0].Name)
}

func TestScraperGetMethodAndCategoryMapping(t *testing.T) {
	const testHTML = `
		<table>
			<tr class="torrent_row">
				<td><a href="tracker.php?f=925">cat</a></td>
				<td><a href="/download/123">Test Torrent</a></td>
				<td>1.5 GB</td>
				<td>100</td>
				<td>10</td>
				<td>2024-01-01</td>
			</tr>
		</table>
	`

	var gotMethod string
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		w.Write([]byte(testHTML))
	}))
	defer testServer.Close()

	testDef := fmt.Sprintf(`
id: test-get-tracker
name: test-get-tracker
links:
  - %s/
caps:
  categorymappings:
    - {id: 925, cat: Movies, desc: "Movies"}
search:
  paths:
    - path: "/"
      method: get
  rows:
    selector: ".torrent_row"
  fields:
    category_id:
      selector: "td:nth-child(1) a"
      attribute: "href"
      filters:
        - name: querystring
          args: f
    category:
      text: "{{ .Result.category_id }}"
    title:
      selector: "td:nth-child(2) a"
    download:
      selector: "td:nth-child(2) a"
      attribute: "href"
    size:
      selector: "td:nth-child(3)"
    seeders:
      selector: "td:nth-child(4)"
    leechers:
      selector: "td:nth-child(5)"
    date:
      selector: "td:nth-child(6)"
`, testServer.URL)

	store := &fakeStore{}

	tempDefsDir := t.TempDir()
	def := loadTestTracker(t, tempDefsDir, "test-get-tracker", testDef)

	scraper := NewWithOptions(NewConfigStore(""), "", slog.New(slog.DiscardHandler), Options{Sink: store})
	require.NoError(t, scraper.scrapeIndexer(t.Context(), def, SearchParams{Query: "test"}))

	require.Equal(t, http.MethodGet, gotMethod, "expected the search path's declared GET method to be honored")

	stored := storedTorrents(t, scraper, def)
	require.Len(t, stored, 1)
	require.Equal(t, []int{2000, 100925}, stored[0].Categories, "expected the site category 925 to map to standard category Movies (2000) and its own custom category")
}

// TestScraperCardigannTemplating exercises the Cardigann template
// expressions Jacklet supports end-to-end, matching the patterns real
// definitions use: keywordsfilters rewriting the
// query, "$raw" building a category querystring fragment via
// "{{ range .Categories }}", a field referencing another via
// "{{ .Result.x }}", and a filter argument driven by both ".Config" (from
// a settings default) and ".Result".
func TestScraperCardigannTemplating(t *testing.T) {
	const testHTML = `
		<table>
			<tr class="torrent_row">
				<td><a href="tracker.php?f=925">cat</a></td>
				<td><a href="/download/123">Some Movie</a></td>
				<td>1.5 GB</td>
				<td>100</td>
				<td>10</td>
				<td>2024-01-01</td>
			</tr>
		</table>
	`

	var gotForm url.Values
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotForm, _ = url.ParseQuery(string(body))
		w.Write([]byte(testHTML))
	}))
	defer testServer.Close()

	testDef := fmt.Sprintf(`
id: test-template-tracker
name: test-template-tracker
links:
  - %s/
caps:
  categorymappings:
    - {id: 925, cat: Movies, desc: "Movies"}
settings:
  - name: addrussiantotitle
    type: checkbox
    default: true
search:
  paths:
    - path: "/"
      method: post
  inputs:
    nm: "{{ .Keywords }}"
    $raw: "{{ if .Categories }}{{ range .Categories }}f[]={{.}}&{{end}}{{ else }}f[]=-1{{ end }}"
  keywordsfilters:
    - name: re_replace
      args: ["test", "filtered"]
  rows:
    selector: ".torrent_row"
  fields:
    category_id:
      selector: "td:nth-child(1) a"
      attribute: "href"
      filters:
        - name: querystring
          args: f
    category:
      text: "{{ .Result.category_id }}"
    title:
      selector: "td:nth-child(2) a"
      filters:
        - name: append
          args: "{{ if and (ne .Result.category_id \"913\") (.Config.addrussiantotitle) }} RUS{{ else }}{{ end }}"
    download:
      selector: "td:nth-child(2) a"
      attribute: "href"
    size:
      selector: "td:nth-child(3)"
    seeders:
      selector: "td:nth-child(4)"
    leechers:
      selector: "td:nth-child(5)"
    date:
      selector: "td:nth-child(6)"
`, testServer.URL)

	store := &fakeStore{}

	tempDefsDir := t.TempDir()
	def := loadTestTracker(t, tempDefsDir, "test-template-tracker", testDef)

	scraper := NewWithOptions(NewConfigStore(""), "", slog.New(slog.DiscardHandler), Options{Sink: store})
	// Request standard category 2000 (Movies), which reverse-maps to this
	// tracker's own category 925 via caps.categorymappings.
	params := SearchParams{Categories: []string{"2000"}, Query: "test"}
	require.NoError(t, scraper.scrapeIndexer(t.Context(), def, params))

	require.Equal(t, "filtered", gotForm.Get("nm"), "expected the keywordsfilters-rewritten query to be sent")
	require.Contains(t, gotForm["f[]"], "925", "expected .Categories to reverse-map the requested standard category to the site's own category ID")

	stored := storedTorrents(t, scraper, def)
	require.Len(t, stored, 1)
	require.Equal(t, "Some Movie RUS", stored[0].Name, "expected the append filter to resolve .Config.addrussiantotitle and .Result.category_id")
}

func TestScraperSeasonEpisodeFoldedIntoKeywords(t *testing.T) {
	const testHTML = `<table></table>`

	var gotForm url.Values
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotForm, _ = url.ParseQuery(string(body))
		w.Write([]byte(testHTML))
	}))
	defer testServer.Close()

	testDef := fmt.Sprintf(`
id: test-tv-tracker
name: test-tv-tracker
links:
  - %s/
search:
  paths:
    - path: "/"
      method: post
  inputs:
    nm: "{{ .Keywords }}"
  rows:
    selector: ".torrent_row"
  fields:
    title:
      selector: "a"
    download:
      selector: "a"
      attribute: "href"
`, testServer.URL)

	store := &fakeStore{}

	tempDefsDir := t.TempDir()
	def := loadTestTracker(t, tempDefsDir, "test-tv-tracker", testDef)

	scraper := NewWithOptions(NewConfigStore(""), "", slog.New(slog.DiscardHandler), Options{Sink: store})
	params := SearchParams{Ep: "3", Query: "Some Show", Season: "1"}
	require.NoError(t, scraper.scrapeIndexer(t.Context(), def, params))

	require.Equal(t, "Some Show S01E03", gotForm.Get("nm"), "expected season/episode to be folded into the keywords as SxxEyy")
}

func TestScraperRateLimitsRepeatedScrapes(t *testing.T) {
	const testHTML = `<html></html>`

	var hits int
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write([]byte(testHTML))
	}))
	defer testServer.Close()

	testDef := fmt.Sprintf(`
id: test-rate-limited-tracker
name: test-rate-limited-tracker
links:
  - %s/
search:
  paths:
    - path: "/"
  rows:
    selector: ".torrent_row"
  fields:
    title:
      selector: "a"
`, testServer.URL)

	store := &fakeStore{}

	tempDefsDir := t.TempDir()
	def := loadTestTracker(t, tempDefsDir, "test-rate-limited-tracker", testDef)

	scraper := NewWithOptions(NewConfigStore(""), "", slog.New(slog.DiscardHandler), Options{Sink: store})
	require.NoError(t, scraper.scrapeIndexer(t.Context(), def, SearchParams{Query: "test"}))
	require.NoError(t, scraper.scrapeIndexer(t.Context(), def, SearchParams{Query: "test"}))

	require.Equal(t, 1, hits, "expected the second scrape within minScrapeInterval to be skipped")
}

func TestScraperFallsBackToLegacyLink(t *testing.T) {
	const testHTML = `
		<table>
			<tr class="torrent_row">
				<td><a href="/download/123">Test Torrent</a></td>
			</tr>
		</table>
	`
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(testHTML))
	}))
	defer testServer.Close()

	// The primary link points at a closed port (nothing listening), so the
	// first fetch attempt must fail before falling back to legacylinks.
	testDef := fmt.Sprintf(`
id: test-fallback-tracker
name: test-fallback-tracker
links:
  - http://127.0.0.1:1/
legacylinks:
  - %s/
search:
  paths:
    - path: "/"
  rows:
    selector: ".torrent_row"
  fields:
    title:
      selector: "a"
    download:
      selector: "a"
      attribute: "href"
`, testServer.URL)

	store := &fakeStore{}

	tempDefsDir := t.TempDir()
	def := loadTestTracker(t, tempDefsDir, "test-fallback-tracker", testDef)

	scraper := NewWithOptions(NewConfigStore(""), "", slog.New(slog.DiscardHandler), Options{Sink: store})
	require.NoError(t, scraper.scrapeIndexer(t.Context(), def, SearchParams{Query: "test"}))

	require.Len(t, storedTorrents(t, scraper, def), 1,
		"expected the scrape to succeed via the legacy link after the primary link failed")
}

func TestScraperCookiesPersistWithinAScrape(t *testing.T) {
	var sawCookieOnSecondRequest bool
	requests := 0
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "abc123"}) //nolint:gosec // plain httptest server, not a real cookie-security concern
		} else if c, err := r.Cookie("session"); err == nil && c.Value == "abc123" {
			sawCookieOnSecondRequest = true
		}
		w.Write([]byte(`<html></html>`))
	}))
	defer testServer.Close()

	testDef := fmt.Sprintf(`
id: test-cookie-tracker
name: test-cookie-tracker
links:
  - %s/
search:
  paths:
    - path: "/first"
    - path: "/second"
  rows:
    selector: ".torrent_row"
  fields:
    title:
      selector: "a"
`, testServer.URL)

	store := &fakeStore{}

	tempDefsDir := t.TempDir()
	def := loadTestTracker(t, tempDefsDir, "test-cookie-tracker", testDef)

	scraper := NewWithOptions(NewConfigStore(""), "", slog.New(slog.DiscardHandler), Options{Sink: store})
	require.NoError(t, scraper.scrapeIndexer(t.Context(), def, SearchParams{Query: "test"}))

	require.Equal(t, 2, requests)
	require.True(t, sawCookieOnSecondRequest, "expected a cookie set on the first request to be sent on the second")
}

func TestScraperConfigOverride(t *testing.T) {
	var gotForm url.Values
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotForm, _ = url.ParseQuery(string(body))
		w.Write([]byte(`<html></html>`))
	}))
	defer testServer.Close()

	testDef := fmt.Sprintf(`
id: test-config-tracker
name: test-config-tracker
links:
  - %s/
settings:
  - name: sort
    type: select
    default: created
search:
  paths:
    - path: "/"
      method: post
  inputs:
    o: "{{ .Config.sort }}"
  rows:
    selector: ".torrent_row"
  fields:
    title:
      selector: "a"
`, testServer.URL)

	store := &fakeStore{}

	tempDefsDir := t.TempDir()
	def := loadTestTracker(t, tempDefsDir, "test-config-tracker", testDef)

	configDir := t.TempDir()
	require.NoError(t, os.WriteFile(configDir+"/test-config-tracker.yml", []byte("sort: seeders\n"), 0o600))

	scraper := NewWithOptions(NewConfigStore(configDir), "", slog.New(slog.DiscardHandler), Options{Sink: store})
	require.NoError(t, scraper.scrapeIndexer(t.Context(), def, SearchParams{Query: "test"}))

	require.Equal(t, "seeders", gotForm.Get("o"), "expected the config override to replace the setting's YAML default")
}

func TestScraperBacksOffAfterRepeatedFailures(t *testing.T) {
	testDef := `
id: test-unreachable-tracker
name: test-unreachable-tracker
links:
  - http://127.0.0.1:1/
search:
  paths:
    - path: "/"
  rows:
    selector: ".torrent_row"
  fields:
    title:
      selector: "a"
`

	store := &fakeStore{}

	tempDefsDir := t.TempDir()
	def := loadTestTracker(t, tempDefsDir, "test-unreachable-tracker", testDef)

	scraper := NewWithOptions(NewConfigStore(""), "", slog.New(slog.DiscardHandler), Options{Sink: store})
	id := TrackerID(def)

	err1 := scraper.scrapeIndexer(t.Context(), def, SearchParams{Query: "test"})
	require.Error(t, err1, "expected an error scraping an unreachable tracker with no fallback link")

	state, ok := scraper.scrapeStateSnapshot(id)
	require.True(t, ok, "the failed scrape recorded no state")
	require.Equal(t, 1, state.failures)
	require.True(t, state.nextAllowed.After(time.Now().Add(minScrapeInterval)),
		"expected the backoff window after a failure to extend beyond the plain rate-limit interval")

	// A second call while backed off must not attempt the network again:
	// it's rate-limited, not a fresh failure, so it returns nil and
	// doesn't increment the failure count further.
	require.NoError(t, scraper.scrapeIndexer(t.Context(), def, SearchParams{Query: "test"}))
	state, ok = scraper.scrapeStateSnapshot(id)
	require.True(t, ok)
	require.Equal(t, 1, state.failures)
}

func TestScraper_FlareSolverrSession_ReuseAndCleanup(t *testing.T) {
	var sessionsCreated, sessionsDestroyed int
	var sessionID = "session-123"

	testFlareSolverr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)

		w.Header().Set("Content-Type", "application/json")
		switch req["cmd"] {
		case "sessions.create":
			sessionsCreated++
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "session": sessionID})
		case "sessions.destroy":
			sessionsDestroyed++
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
		case "request.post":
			if req["session"] != sessionID {
				w.Write([]byte(`{"status":"error","message":"bad session"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status":   "ok",
				"solution": map[string]any{"response": "<html></html>"},
			})
		}
	}))
	defer testFlareSolverr.Close()

	testDef := `
id: test-flaresolverr-tracker
name: test-flaresolverr-tracker
links:
  - http://example.invalid/
search:
  paths:
    - path: "/"
      method: post
  rows:
    selector: ".torrent_row"
  fields:
    title:
      selector: "a"
`

	store := &fakeStore{}

	tempDefsDir := t.TempDir()
	def := loadTestTracker(t, tempDefsDir, "test-flaresolverr-tracker", testDef)

	scraper := NewWithOptions(NewConfigStore(""), "", slog.New(slog.DiscardHandler), Options{Sink: store})
	scraper.useFlareSolverr(t, testFlareSolverr.URL, "test-flaresolverr-tracker")

	require.NoError(t, scraper.scrapeIndexer(t.Context(), def, SearchParams{Query: "test"}))
	require.Equal(t, 1, sessionsCreated, "expected exactly one session to be created")

	scraper.clearScrapeState() // bypass rate limiting for a second call
	require.NoError(t, scraper.scrapeIndexer(t.Context(), def, SearchParams{Query: "test2"}))
	require.Equal(t, 1, sessionsCreated, "expected the cached session to be reused, not recreated")

	require.NoError(t, scraper.Close(t.Context()))
	require.Equal(t, 1, sessionsDestroyed, "expected Close to destroy the FlareSolverr session")
}

// A definition with only legacylinks has an empty Links slice, which must
// not be indexed while resolving a row's relative URLs.
func TestScraperLegacyLinksOnlyDefinition(t *testing.T) {
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

	store := &fakeStore{}

	scrpr := NewWithOptions(NewConfigStore(""), "", slog.New(slog.DiscardHandler), Options{Sink: store})
	require.NoError(t, scrpr.scrapeIndexer(t.Context(), def, SearchParams{Query: "legacy"}))

	require.Equal(t, testServer.URL+"/download/1", storedTorrent(t, scrpr, def, "Legacy Release").DownloadURL)
}

// When the primary link is unreachable, a row's relative URLs must resolve
// against the mirror that actually served it, not against Links[0].
func TestScraperResolvesURLsAgainstWorkingMirror(t *testing.T) {
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

	store := &fakeStore{}

	scrpr := NewWithOptions(NewConfigStore(""), "", slog.New(slog.DiscardHandler), Options{Sink: store})
	require.NoError(t, scrpr.scrapeIndexer(t.Context(), def, SearchParams{Query: "mirror"}))

	stored := storedTorrent(t, scrpr, def, "Mirror Release")
	require.Equal(t, testServer.URL+"/download/1", stored.DownloadURL)
	require.Equal(t, testServer.URL+"/details/1", stored.DetailsURL)
}

// An anti-bot or error page served with a 4xx/5xx status must fail the
// scrape rather than being parsed as a results page that happens to match
// nothing: reported as zero results, it would hide the real cause and
// leave the failure backoff disengaged.
func TestScraperErrorStatusFailsTheScrape(t *testing.T) {
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

	store := &fakeStore{}

	scrpr := NewWithOptions(NewConfigStore(""), "", slog.New(slog.DiscardHandler), Options{Sink: store})
	err := scrpr.scrapeIndexer(t.Context(), def, SearchParams{Query: "anything"})
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
func TestScraperDefinitionUserAgentWins(t *testing.T) {
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

	store := &fakeStore{}

	scrpr := NewWithOptions(NewConfigStore(""), "", slog.New(slog.DiscardHandler), Options{Sink: store})
	require.NoError(t, scrpr.scrapeIndexer(t.Context(), def, SearchParams{Query: "x"}))
	require.Equal(t, "jacklet-test/1.0", userAgent)
}

// Options.UserAgent replaces the built-in default for searches and
// downloads alike, so an operator can keep it current without editing every
// definition.
func TestScraperPresentsTheConfiguredUserAgent(t *testing.T) {
	tracker := &downloadRecorder{}
	site := httptest.NewServer(tracker)
	defer site.Close()

	def := loadTestTracker(t, t.TempDir(), "example-tracker", downloadDef("example-tracker", site.URL))
	scrpr := NewWithOptions(NewConfigStore(""), "", slog.New(slog.DiscardHandler), Options{UserAgent: "ExampleBrowser/9.0"})

	_, err := scrpr.Scrape(t.Context(), def, SearchParams{Query: "x"})
	require.NoError(t, err)
	download, err := scrpr.Download(t.Context(), def, site.URL+"/download/1")
	require.NoError(t, err)
	download.Body.Close()

	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	require.Equal(t, []string{"ExampleBrowser/9.0", "ExampleBrowser/9.0"}, tracker.userAgent, "the search and the download each present the configured user agent")
}

// The rate-limit window must key on the search, not just the tracker.
// Suppressing a *different* search because the tracker was scraped a
// moment ago answers it from the previous search's results, which is
// wrong rather than merely stale.
func TestScraperRateLimitIsPerSearch(t *testing.T) {
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

	store := &fakeStore{}

	scrpr := NewWithOptions(NewConfigStore(""), "", slog.New(slog.DiscardHandler), Options{Sink: store})
	ctx := t.Context()

	require.NoError(t, scrpr.scrapeIndexer(ctx, def, SearchParams{Query: "alpha"}))
	// A different search right behind it still reaches the tracker.
	require.NoError(t, scrpr.scrapeIndexer(ctx, def, SearchParams{Query: "beta"}))
	// Differing only by category is a different search too.
	require.NoError(t, scrpr.scrapeIndexer(ctx, def, SearchParams{Categories: []string{"2000"}, Query: "beta"}))
	// Repeating one already run inside the window is answered from the
	// store instead of hitting the site again.
	require.NoError(t, scrpr.scrapeIndexer(ctx, def, SearchParams{Query: "alpha"}))

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"alpha", "beta", "beta"}, queries)
}

// A tracker inside its failure backoff is skipped rather than waited on:
// a backoff runs to ten minutes, which would strand the request.
func TestScraperBackoffSkipsRatherThanWaits(t *testing.T) {
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

	store := &fakeStore{}

	scrpr := NewWithOptions(NewConfigStore(""), "", slog.New(slog.DiscardHandler), Options{Sink: store})
	require.Error(t, scrpr.scrapeIndexer(t.Context(), def, SearchParams{Query: "one"}))
	require.Equal(t, int64(1), hits.Load())

	// A different search would ordinarily be allowed through, but the
	// tracker is now backed off, so it returns at once from the store.
	start := time.Now()
	require.NoError(t, scrpr.scrapeIndexer(t.Context(), def, SearchParams{Query: "two"}))
	require.Less(t, time.Since(start), minScrapeSpacing)
	require.Equal(t, int64(1), hits.Load())
}

func TestScraperSearchErrorIsReported(t *testing.T) {
	const body = `<div class="err">Flood protection: wait 60 seconds</div>`

	scrpr, def := newScrapeFixture(t, "errsite", body, `
id: errsite
name: Error Site
links:
  - %[1]s/
search:
  paths:
    - path: "/"
      method: get
  error:
    - selector: div.err
      message:
        selector: div.err
  rows:
    selector: div.row
  fields:
    title:
      selector: a
`)

	err := scrpr.scrapeIndexer(t.Context(), def, SearchParams{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "Flood protection")
}

// An input that renders empty or blank is not sent, as Jackett does not
// send it, since a tracker may read an empty parameter as a search for
// nothing; a definition that wants them sent says so with allowEmptyInputs.
// "$raw" is never dropped this way.
func TestScraperOmitsEmptySearchInputs(t *testing.T) {
	for _, tc := range []struct {
		name             string
		allowEmptyInputs string
		want             url.Values
	}{
		{
			name:             "left out by default",
			allowEmptyInputs: "false",
			want:             url.Values{"raw": {"1"}, "sort": {"seeders"}},
		},
		{
			name:             "sent when the definition allows them",
			allowEmptyInputs: "true",
			want:             url.Values{"blank": {"  "}, "empty": {""}, "q": {""}, "raw": {"1"}, "sort": {"seeders"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotQuery url.Values
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotQuery = r.URL.Query()
				fmt.Fprint(w, `<div class="row"><a href="/d/1">Release</a></div>`)
			}))
			defer server.Close()

			def := loadTestTracker(t, t.TempDir(), "empty-inputs", `
id: empty-inputs
name: Empty Inputs
links:
  - `+server.URL+`/
search:
  allowEmptyInputs: `+tc.allowEmptyInputs+`
  inputs:
    $raw: "raw=1&"
    blank: "  "
    empty: ""
    q: "{{ .Keywords }}"
    sort: seeders
  paths:
    - path: "/"
      method: get
  rows:
    selector: div.row
  fields:
    title:
      selector: a
    download:
      selector: a
      attribute: href
`)

			scrpr := NewWithOptions(NewConfigStore(""), "", slog.New(slog.DiscardHandler), Options{Sink: &fakeStore{}})
			require.NoError(t, scrpr.scrapeIndexer(t.Context(), def, SearchParams{}))
			require.Equal(t, tc.want, gotQuery)
		})
	}
}

// Two paths that build the same GET are asked once, as Jackett asks them,
// since the second request would fetch the same page and store every row
// twice; a path that differs is still asked.
func TestScraperSkipsARepeatedSearchRequest(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.RequestURI())
		fmt.Fprint(w, `<div class="row"><a href="/d/1">Release</a></div>`)
	}))
	defer server.Close()

	def := loadTestTracker(t, t.TempDir(), "repeated", `
id: repeated
name: Repeated
links:
  - `+server.URL+`/
search:
  paths:
    - path: "/"
      method: get
      inputs:
        q: "{{ .Keywords }}"
    - path: "/"
      method: get
      inputs:
        q: "{{ .Keywords }}"
        p: "{{ if .Keywords }}{{ else }}2{{ end }}"
    - path: "/"
      method: get
      inputs:
        q: "{{ .Keywords }}"
        p: "3"
  rows:
    selector: div.row
  fields:
    title:
      selector: a
    download:
      selector: a
      attribute: href
`)

	db := &fakeStore{}
	scrpr := NewWithOptions(NewConfigStore(""), "", slog.New(slog.DiscardHandler), Options{Sink: db})
	torrents, err := scrpr.Scrape(t.Context(), def, SearchParams{Query: "example"})
	require.NoError(t, err)
	require.Equal(t, []string{"/?q=example", "/?p=3&q=example"}, requests)
	require.Len(t, torrents, 2, "a repeated request's rows were returned again")
}

// A path that names no method is searched with a GET carrying its inputs
// in the query string, as Jackett searches it; only "post" sends a form.
func TestScraperSearchesWithGetUnlessThePathSaysPost(t *testing.T) {
	for _, tc := range []struct {
		name       string
		method     string
		wantMethod string
	}{
		{name: "no method", wantMethod: http.MethodGet},
		{name: "get", method: "get", wantMethod: http.MethodGet},
		{name: "post", method: "POST", wantMethod: http.MethodPost},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotMethod, gotQuery string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotMethod = r.Method
				assert.NoError(t, r.ParseForm())
				gotQuery = r.Form.Get("q")
				fmt.Fprint(w, `<div class="row"><a href="/d/1">Release</a></div>`)
			}))
			defer server.Close()

			methodLine := ""
			if tc.method != "" {
				methodLine = "      method: " + tc.method + "\n"
			}
			def := loadTestTracker(t, t.TempDir(), "method", `
id: method
name: Method
links:
  - `+server.URL+`/
search:
  paths:
    - path: "/"
`+methodLine+`  inputs:
    q: "{{ .Keywords }}"
  rows:
    selector: div.row
  fields:
    title:
      selector: a
    download:
      selector: a
      attribute: href
`)

			scrpr := NewWithOptions(NewConfigStore(""), "", slog.New(slog.DiscardHandler), Options{Sink: &fakeStore{}})
			require.NoError(t, scrpr.scrapeIndexer(t.Context(), def, SearchParams{Query: "example"}))
			require.Equal(t, tc.wantMethod, gotMethod)
			require.Equal(t, "example", gotQuery)
		})
	}
}

// A search path is a template, as in Jackett: the keyword it writes is
// encoded into the path, a "/" in it included, and a "?" the path itself
// carries starts the query string the inputs are added to.
func TestScraperRendersTheSearchPath(t *testing.T) {
	var gotRequest string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRequest = r.RequestURI
		fmt.Fprint(w, `<div class="row"><a href="/d/1">Release</a></div>`)
	}))
	defer server.Close()

	def := loadTestTracker(t, t.TempDir(), "templated-path", `
id: templated-path
name: Templated Path
links:
  - `+server.URL+`/
search:
  paths:
    - path: "{{ if .Keywords }}search/{{ .Keywords }}{{ else }}latest{{ end }}/1/?lang=en"
  inputs:
    sort: seeders
  rows:
    selector: div.row
  fields:
    title:
      selector: a
    download:
      selector: a
      attribute: href
`)

	scrpr := NewWithOptions(NewConfigStore(""), "", slog.New(slog.DiscardHandler), Options{Sink: &fakeStore{}})
	require.NoError(t, scrpr.scrapeIndexer(t.Context(), def, SearchParams{Query: "Example A/B"}))
	require.Equal(t, "/search/Example%20A%2FB/1/?lang=en&sort=seeders", gotRequest)
}

func TestScraperPerPathInputsAndHeaders(t *testing.T) {
	var gotQuery, gotHeader string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		gotHeader = r.Header.Get("X-Requested-With")
		fmt.Fprint(w, `<div class="row"><a href="/d/1">Release</a></div>`)
	}))
	defer server.Close()

	dir := t.TempDir()
	def := loadTestTracker(t, dir, "paths", `
id: paths
name: Paths
links:
  - `+server.URL+`/
search:
  headers:
    X-Requested-With: XMLHttpRequest
  inputs:
    shared: "yes"
    q: "{{ .Keywords }}"
  paths:
    - path: "/"
      method: get
      inputs:
        extra: "1"
  rows:
    selector: div.row
  fields:
    title:
      selector: a
    download:
      selector: a
      attribute: href
`)

	db := &fakeStore{}

	scrpr := NewWithOptions(NewConfigStore(""), "", slog.New(slog.DiscardHandler), Options{Sink: db})
	require.NoError(t, scrpr.scrapeIndexer(t.Context(), def, SearchParams{Query: "example"}))

	require.Contains(t, gotQuery, "shared=yes", "shared inputs should be inherited")
	require.Contains(t, gotQuery, "extra=1", "a path's own inputs should be submitted")
	require.Contains(t, gotQuery, "q=example")
	require.Equal(t, "XMLHttpRequest", gotHeader)
}

// An older definition names its one search path with "path" rather than a
// "paths" list, and Jackett searches it all the same, with the search's
// inputs, after any "paths" the definition also lists.
func TestScraperSearchesASinglePath(t *testing.T) {
	var gotPath, gotQuery string
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.Query().Get("search")
		fmt.Fprint(w, `<div class="row"><a href="/d/1">Example Release</a></div>`)
	}))
	defer site.Close()
	def := loadTestTracker(t, t.TempDir(), "single-path", `
id: single-path
name: Single Path
links:
  - `+site.URL+`/
search:
  path: browse.php
  inputs:
    search: "{{ .Keywords }}"
  rows:
    selector: div.row
  fields:
    title:
      selector: a
    download:
      selector: a
      attribute: href
`)

	scrpr := NewWithOptions(NewConfigStore(""), "", slog.New(slog.DiscardHandler), Options{Sink: &fakeStore{}})
	torrents, err := scrpr.Scrape(t.Context(), def, SearchParams{Query: "example"})
	require.NoError(t, err)
	require.Len(t, torrents, 1)
	require.Equal(t, "/browse.php", gotPath)
	require.Equal(t, "example", gotQuery, "the single path did not inherit the search's inputs")
}

func TestSearch_UnmarshalYAML(t *testing.T) {
	var search Search
	require.NoError(t, yaml.Unmarshal([]byte("paths:\n  - path: first.php\npath: second.php\n"), &search))
	require.Len(t, search.Paths, 2)
	require.Equal(t, "first.php", search.Paths[0].Path)
	require.Equal(t, "second.php", search.Paths[1].Path, "the single path did not come after the listed ones")
	require.True(t, search.Paths[1].InheritsInputs())

	search = Search{}
	require.NoError(t, yaml.Unmarshal([]byte("paths:\n  - path: only.php\n"), &search))
	require.Len(t, search.Paths, 1, "a definition without a single path gained one")
}

// A renamed tracker searches with the settings saved under the id it
// replaces, and is opted in to FlareSolverr by them, until it has settings
// of its own.
func TestScraperUsesTheSettingsOfAReplacedID(t *testing.T) {
	var gotUser string
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser = r.URL.Query().Get("user")
		fmt.Fprint(w, `<div class="row"><a href="/d/1">Example Release</a></div>`)
	}))
	defer site.Close()
	def := loadTestTracker(t, t.TempDir(), "renamed", `
id: renamed
name: Renamed
replaces:
  - former
links:
  - `+site.URL+`/
settings:
  - name: username
    type: text
search:
  paths:
    - path: /
  inputs:
    user: "{{ .Config.username }}"
  rows:
    selector: div.row
  fields:
    title:
      selector: a
    download:
      selector: a
      attribute: href
`)
	config := NewConfigStore(t.TempDir())
	require.NoError(t, config.Save("former", map[string]any{"username": "someone", FlareSolverrSetting: true}))

	scrpr := NewWithOptions(config, "", slog.New(slog.DiscardHandler), Options{Sink: &fakeStore{}})
	_, err := scrpr.Scrape(t.Context(), def, SearchParams{})
	require.NoError(t, err)
	require.Equal(t, "someone", gotUser, "the search did not use the replaced id's settings")

	scrpr = NewWithOptions(config, "http://flaresolverr.invalid", slog.New(slog.DiscardHandler), Options{})
	require.True(t, scrpr.usesFlareSolverr(t.Context(), def), "the replaced id's FlareSolverr opt-in was not honored")
}

// A search URL carries the passkey for some definitions, and a connection
// that fails before any answer is reported by net/http with the whole URL.
func TestScraperRedactsTheQueryFromASearchNetworkError(t *testing.T) {
	server := httptest.NewServer(dropConnection(t))
	defer server.Close()

	def := loadTestTracker(t, t.TempDir(), "passkey-tracker", `
id: passkey-tracker
name: Passkey Tracker
links:
  - `+server.URL+`/
search:
  paths:
    - path: "/search.php?passkey=SAMPLEKEY"
  rows:
    selector: ".row"
  fields:
    title:
      selector: "a"
`)
	scrpr := New(NewConfigStore(""), "", slog.New(slog.DiscardHandler))

	err := scrpr.scrapeIndexer(t.Context(), def, SearchParams{Query: "example"})
	require.Error(t, err)
	require.NotContains(t, err.Error(), "SAMPLEKEY", "the tracker's passkey reached the error")
	require.Contains(t, err.Error(), "query redacted")
}

// A rendered search path that does not parse is reported by url.Parse with
// the whole path, passkey included, and that error is what Scrape returns,
// records as the tracker's failure and logs. A definition may also write
// its passkey into the path itself, and the path is logged too.
func TestScraperRedactsTheQueryFromAnUnparsableSearchPath(t *testing.T) {
	for _, tc := range []struct {
		name, path string
	}{
		{name: "a passkey from a setting", path: "/search%zz.php?passkey={{ .Config.passkey }}"},
		{name: "a passkey written into the path", path: "/search%zz.php?passkey=SAMPLEKEY"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			def := loadTestTracker(t, t.TempDir(), "passkey-tracker", `
id: passkey-tracker
name: Passkey Tracker
links:
  - https://tracker.test/
settings:
  - name: passkey
    type: text
    default: SAMPLEKEY
search:
  paths:
    - path: "`+tc.path+`"
  rows:
    selector: ".row"
  fields:
    title:
      selector: "a"
`)
			var logged strings.Builder
			scrpr := New(NewConfigStore(""), "", slog.New(slog.NewJSONHandler(&logged, nil)))

			_, err := scrpr.Scrape(t.Context(), def, SearchParams{Query: "example"})
			require.Error(t, err)
			require.NotContains(t, err.Error(), "SAMPLEKEY", "the tracker's passkey reached the error")
			require.Contains(t, err.Error(), "all links unreachable", "the outer message was lost")
			require.Contains(t, err.Error(), "invalid URL escape")
			require.Contains(t, logged.String(), "failed to build search path")
			require.NotContains(t, logged.String(), "SAMPLEKEY", "the tracker's passkey reached the log")
		})
	}
}
