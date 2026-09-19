// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/torrplay/jacklet/pkg/database"
)

func testLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// loadTestTracker writes def to a YAML file under dir and loads it back by
// id through the same DefinitionStore production code uses.
func loadTestTracker(t *testing.T, dir, id, def string) *Tracker {
	t.Helper()
	require.NoError(t, os.WriteFile(dir+"/"+id+".yml", []byte(def), 0o600))
	tracker, err := NewDefinitionStore(dir, testLogger()).Find(id)
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

// scrapeStateSnapshot copies a tracker's scrape state so the caller can
// read it after the mutex is released, reporting false when no scrape has
// recorded anything yet.
func (s *Scraper) scrapeStateSnapshot(trackerID string) (scrapeState, bool) {
	s.scrapeStateMu.Lock()
	defer s.scrapeStateMu.Unlock()
	st, ok := s.scrapeState[trackerID]
	if !ok {
		return scrapeState{}, false
	}
	return *st, true
}

// expireLoginState ages a confirmed session out, as the clock would.
func (s *Scraper) expireLoginState(trackerID string) {
	s.loginStateMu.Lock()
	defer s.loginStateMu.Unlock()
	if st, ok := s.loginState[trackerID]; ok {
		st.validUntil = time.Now().Add(-time.Second)
	}
}

func TestScraper(t *testing.T) {
	// 1. Create test data (HTML and tracker definition)
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

	// 2. Set up a test server to serve the test HTML.
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(testHTML))
	}))
	defer testServer.Close()

	// 3. Create a test tracker definition that matches the HTML structure.
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

	// 4. Set up the database and temporary definitions directory.
	store, err := database.Open(context.Background(), ":memory:")
	require.NoError(t, err)
	defer store.Close()

	tempDefsDir := t.TempDir()
	def := loadTestTracker(t, tempDefsDir, "test-tracker", testDef)

	// 5. Create and run the scraper.
	scraper := New(store, NewConfigStore(""), "", testLogger())
	// Query text doesn't matter for this test.
	require.NoError(t, scraper.ScrapeIndexer(context.Background(), def, SearchParams{Query: "test"}))

	// 6. Verify that the correct number of torrents were inserted.
	require.Len(t, storedTorrents(t, scraper, def), 2, "Expected 2 torrents to be inserted")

	// 7. Scraping the same query again must not duplicate rows: the
	// (tracker, identity) UNIQUE constraint only works if tracker is
	// populated. Clear the rate-limit bookkeeping first so this second
	// call actually re-scrapes instead of being skipped as too-soon.
	scraper.scrapeState = make(map[string]*scrapeState)
	require.NoError(t, scraper.ScrapeIndexer(context.Background(), def, SearchParams{Query: "test"}))
	stored := storedTorrents(t, scraper, def)
	require.Len(t, stored, 2, "Expected re-scraping the same query not to duplicate torrents")
	require.Equal(t, "test-tracker", stored[0].Tracker)
}

func TestScraper_NonUnicode(t *testing.T) {
	// 1. Create test data for non-Unicode scenario.
	// Define the name "Проверка" (Check) using its windows-1251 byte representation.
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

	// 2. Set up a test server to serve the test HTML with a specific charset.
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=windows-1251")
		// Safely replace the placeholder with the non-Unicode name.
		html := strings.Replace(testHTML, "TORRENT_NAME_PLACEHOLDER", sampleNonUnicodeName, 1)
		w.Write([]byte(html))
	}))
	defer testServer.Close()

	// 3. Create a test tracker definition.
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

	// 4. Set up the database and temporary definitions directory.
	store, err := database.Open(context.Background(), ":memory:")
	require.NoError(t, err)
	defer store.Close()

	tempDefsDir := t.TempDir()
	def := loadTestTracker(t, tempDefsDir, "test-non-unicode-tracker", testDef)

	// 5. Create and run the scraper.
	scraper := New(store, NewConfigStore(""), "", testLogger())
	require.NoError(t, scraper.ScrapeIndexer(context.Background(), def, SearchParams{Query: "test"}))

	// 6. Verify that the name was decoded correctly.
	stored := storedTorrents(t, scraper, def)
	require.Len(t, stored, 1)
	require.Equal(t, expectedDecodedName, stored[0].Name)
}

func TestScraper_GetMethodAndCategoryMapping(t *testing.T) {
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

	store, err := database.Open(context.Background(), ":memory:")
	require.NoError(t, err)
	defer store.Close()

	tempDefsDir := t.TempDir()
	def := loadTestTracker(t, tempDefsDir, "test-get-tracker", testDef)

	scraper := New(store, NewConfigStore(""), "", testLogger())
	require.NoError(t, scraper.ScrapeIndexer(context.Background(), def, SearchParams{Query: "test"}))

	require.Equal(t, http.MethodGet, gotMethod, "expected the search path's declared GET method to be honored")

	stored := storedTorrents(t, scraper, def)
	require.Len(t, stored, 1)
	require.Equal(t, 2000, stored[0].Category, "expected the site category 925 to map to standard category Movies (2000)")
}

// TestScraper_CardigannTemplating exercises the Cardigann template
// expressions Jacklet supports end-to-end, matching the patterns real
// definitions use: keywordsfilters rewriting the
// query, "$raw" building a category querystring fragment via
// "{{ range .Categories }}", a field referencing another via
// "{{ .Result.x }}", and a filter argument driven by both ".Config" (from
// a settings default) and ".Result".
func TestScraper_CardigannTemplating(t *testing.T) {
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

	store, err := database.Open(context.Background(), ":memory:")
	require.NoError(t, err)
	defer store.Close()

	tempDefsDir := t.TempDir()
	def := loadTestTracker(t, tempDefsDir, "test-template-tracker", testDef)

	scraper := New(store, NewConfigStore(""), "", testLogger())
	// Request standard category 2000 (Movies), which reverse-maps to this
	// tracker's own category 925 via caps.categorymappings.
	params := SearchParams{Categories: []string{"2000"}, Query: "test"}
	require.NoError(t, scraper.ScrapeIndexer(context.Background(), def, params))

	require.Equal(t, "filtered", gotForm.Get("nm"), "expected the keywordsfilters-rewritten query to be sent")
	require.Contains(t, gotForm["f[]"], "925", "expected .Categories to reverse-map the requested standard category to the site's own category ID")

	stored := storedTorrents(t, scraper, def)
	require.Len(t, stored, 1)
	require.Equal(t, "Some Movie RUS", stored[0].Name, "expected the append filter to resolve .Config.addrussiantotitle and .Result.category_id")
}

func TestScraper_SeasonEpisodeFoldedIntoKeywords(t *testing.T) {
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

	store, err := database.Open(context.Background(), ":memory:")
	require.NoError(t, err)
	defer store.Close()

	tempDefsDir := t.TempDir()
	def := loadTestTracker(t, tempDefsDir, "test-tv-tracker", testDef)

	scraper := New(store, NewConfigStore(""), "", testLogger())
	params := SearchParams{Ep: "3", Query: "Some Show", Season: "1"}
	require.NoError(t, scraper.ScrapeIndexer(context.Background(), def, params))

	require.Equal(t, "Some Show S01E03", gotForm.Get("nm"), "expected season/episode to be folded into the keywords as SxxEyy")
}

func TestScraper_RateLimitsRepeatedScrapes(t *testing.T) {
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

	store, err := database.Open(context.Background(), ":memory:")
	require.NoError(t, err)
	defer store.Close()

	tempDefsDir := t.TempDir()
	def := loadTestTracker(t, tempDefsDir, "test-rate-limited-tracker", testDef)

	scraper := New(store, NewConfigStore(""), "", testLogger())
	require.NoError(t, scraper.ScrapeIndexer(context.Background(), def, SearchParams{Query: "test"}))
	require.NoError(t, scraper.ScrapeIndexer(context.Background(), def, SearchParams{Query: "test"}))

	require.Equal(t, 1, hits, "expected the second scrape within minScrapeInterval to be skipped")
}

func TestScraper_FallsBackToLegacyLink(t *testing.T) {
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

	store, err := database.Open(context.Background(), ":memory:")
	require.NoError(t, err)
	defer store.Close()

	tempDefsDir := t.TempDir()
	def := loadTestTracker(t, tempDefsDir, "test-fallback-tracker", testDef)

	scraper := New(store, NewConfigStore(""), "", testLogger())
	require.NoError(t, scraper.ScrapeIndexer(context.Background(), def, SearchParams{Query: "test"}))

	require.Len(t, storedTorrents(t, scraper, def), 1,
		"expected the scrape to succeed via the legacy link after the primary link failed")
}

func TestScraper_CookiesPersistWithinAScrape(t *testing.T) {
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
    - path: "/"
    - path: "/"
  rows:
    selector: ".torrent_row"
  fields:
    title:
      selector: "a"
`, testServer.URL)

	store, err := database.Open(context.Background(), ":memory:")
	require.NoError(t, err)
	defer store.Close()

	tempDefsDir := t.TempDir()
	def := loadTestTracker(t, tempDefsDir, "test-cookie-tracker", testDef)

	scraper := New(store, NewConfigStore(""), "", testLogger())
	require.NoError(t, scraper.ScrapeIndexer(context.Background(), def, SearchParams{Query: "test"}))

	require.Equal(t, 2, requests)
	require.True(t, sawCookieOnSecondRequest, "expected a cookie set on the first request to be sent on the second")
}

func TestScraper_ConfigOverride(t *testing.T) {
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
  inputs:
    o: "{{ .Config.sort }}"
  rows:
    selector: ".torrent_row"
  fields:
    title:
      selector: "a"
`, testServer.URL)

	store, err := database.Open(context.Background(), ":memory:")
	require.NoError(t, err)
	defer store.Close()

	tempDefsDir := t.TempDir()
	def := loadTestTracker(t, tempDefsDir, "test-config-tracker", testDef)

	configDir := t.TempDir()
	require.NoError(t, os.WriteFile(configDir+"/test-config-tracker.yml", []byte("sort: seeders\n"), 0o600))

	scraper := New(store, NewConfigStore(configDir), "", testLogger())
	require.NoError(t, scraper.ScrapeIndexer(context.Background(), def, SearchParams{Query: "test"}))

	require.Equal(t, "seeders", gotForm.Get("o"), "expected the config override to replace the setting's YAML default")
}

func TestScraper_BacksOffAfterRepeatedFailures(t *testing.T) {
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

	store, err := database.Open(context.Background(), ":memory:")
	require.NoError(t, err)
	defer store.Close()

	tempDefsDir := t.TempDir()
	def := loadTestTracker(t, tempDefsDir, "test-unreachable-tracker", testDef)

	scraper := New(store, NewConfigStore(""), "", testLogger())
	id := TrackerID(def)

	err1 := scraper.ScrapeIndexer(context.Background(), def, SearchParams{Query: "test"})
	require.Error(t, err1, "expected an error scraping an unreachable tracker with no fallback link")

	st, ok := scraper.scrapeStateSnapshot(id)
	require.True(t, ok, "the failed scrape recorded no state")
	require.Equal(t, 1, st.failures)
	require.True(t, st.nextAllowed.After(time.Now().Add(minScrapeInterval)),
		"expected the backoff window after a failure to extend beyond the plain rate-limit interval")

	// A second call while backed off must not attempt the network again:
	// it's rate-limited, not a fresh failure, so it returns nil and
	// doesn't increment the failure count further.
	require.NoError(t, scraper.ScrapeIndexer(context.Background(), def, SearchParams{Query: "test"}))
	st, ok = scraper.scrapeStateSnapshot(id)
	require.True(t, ok)
	require.Equal(t, 1, st.failures)
}

func TestScraper_FlareSolverrSessionReuseAndCleanup(t *testing.T) {
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
  rows:
    selector: ".torrent_row"
  fields:
    title:
      selector: "a"
`

	store, err := database.Open(context.Background(), ":memory:")
	require.NoError(t, err)
	defer store.Close()

	tempDefsDir := t.TempDir()
	def := loadTestTracker(t, tempDefsDir, "test-flaresolverr-tracker", testDef)

	scraper := New(store, NewConfigStore(""), testFlareSolverr.URL, testLogger())

	require.NoError(t, scraper.ScrapeIndexer(context.Background(), def, SearchParams{Query: "test"}))
	require.Equal(t, 1, sessionsCreated, "expected exactly one session to be created")

	scraper.scrapeState = make(map[string]*scrapeState) // bypass rate limiting for a second call
	require.NoError(t, scraper.ScrapeIndexer(context.Background(), def, SearchParams{Query: "test2"}))
	require.Equal(t, 1, sessionsCreated, "expected the cached session to be reused, not recreated")

	require.NoError(t, scraper.Close(context.Background()))
	require.Equal(t, 1, sessionsDestroyed, "expected Close to destroy the FlareSolverr session")
}
