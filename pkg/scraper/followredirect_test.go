// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/torrplay/jacklet/pkg/database"
)

// Cardigann does not follow a redirect from a search unless the path asks
// it to. A tracker answering a search by redirecting to its login page is
// the reason: following one scrapes that page as if it were results,
// turning a lapsed session into a silent "no matches".

// newRedirectingTracker serves "/search" as a redirect to "/results", and
// "/results" as a page with one row. It records which paths were asked
// for, so a test can see whether the redirect was followed.
func newRedirectingTracker(t *testing.T, follow bool) (*Scraper, *Tracker, func() []string) {
	t.Helper()
	var (
		mu   sync.Mutex
		seen []string
	)
	mux := http.NewServeMux()
	record := func(path string) {
		mu.Lock()
		seen = append(seen, path)
		mu.Unlock()
	}
	mux.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		record("/search")
		http.Redirect(w, r, "/results", http.StatusFound)
	})
	mux.HandleFunc("/results", func(w http.ResponseWriter, r *http.Request) {
		record("/results")
		// The cell matters: an <a> placed directly in a <tr> is hoisted
		// out of the table by HTML parsing rules, and the row selector
		// would then match nothing for reasons unrelated to redirects.
		fmt.Fprint(w, `<table><tr class="row"><td><a href="/dl/1">Behind The Redirect</a></td></tr></table>`)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	followLine := ""
	if follow {
		followLine = "\n      followredirect: true"
	}
	def := fmt.Sprintf(`
id: redirect-tracker
name: redirect-tracker
links:
  - %s/
search:
  paths:
    - path: /search%s
  rows:
    selector: ".row"
  fields:
    title:
      selector: "a"
    download:
      selector: "a"
      attribute: "href"
`, server.URL, followLine)

	tracker := loadTestTracker(t, t.TempDir(), "redirect-tracker", def)
	store, err := database.Open(context.Background(), ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { store.Close() })

	return New(store, NewConfigStore(""), "", testLogger()), tracker, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string{}, seen...)
	}
}

// TestScraper_RedirectNotFollowedByDefault pins Cardigann's default: the
// 3xx response is what gets parsed, which for a bare redirect is nothing.
func TestScraper_RedirectNotFollowedByDefault(t *testing.T) {
	scrpr, def, fetched := newRedirectingTracker(t, false)

	// Not an error: an unfollowed redirect is an empty page, not a failure.
	require.NoError(t, scrpr.ScrapeIndexer(context.Background(), def,
		SearchParams{Query: "test", Type: "search"}))

	require.Equal(t, []string{"/search"}, fetched(), "the redirect target must not be requested")
	require.Empty(t, storedTorrents(t, scrpr, def), "nothing is behind an unfollowed redirect")
}

// TestScraper_FollowRedirectOptsIn covers the flag doing its job.
func TestScraper_FollowRedirectOptsIn(t *testing.T) {
	scrpr, def, fetched := newRedirectingTracker(t, true)

	require.NoError(t, scrpr.ScrapeIndexer(context.Background(), def,
		SearchParams{Query: "test", Type: "search"}))

	require.Equal(t, []string{"/search", "/results"}, fetched())

	stored := storedTorrents(t, scrpr, def)
	require.Len(t, stored, 1)
	require.Equal(t, "Behind The Redirect", stored[0].Name)
}

// TestScraper_NoRedirectClientSharesTheCookieJar guards the thing that
// would break quietly: the search client is a second http.Client, and a
// session established by the login flow through the first one has to be
// sent by it.
func TestScraper_NoRedirectClientSharesTheCookieJar(t *testing.T) {
	scrpr := New(nil, NewConfigStore(""), "", testLogger())
	require.NotNil(t, scrpr.noRedirectClient.Jar)
	require.Same(t, scrpr.httpClient.Jar, scrpr.noRedirectClient.Jar)
}

// TestScraper_LoginStillFollowsRedirects: a site that answers the login
// page with a temporary redirect is common, and Jacklet always follows
// redirects during authentication.
func TestScraper_LoginStillFollowsRedirects(t *testing.T) {
	var (
		mu   sync.Mutex
		seen []string
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/login.php", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, "/login.php")
		mu.Unlock()
		http.Redirect(w, r, "/real-login.php", http.StatusFound)
	})
	mux.HandleFunc("/real-login.php", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, "/real-login.php")
		mu.Unlock()
		fmt.Fprint(w, `<html><body>ok</body></html>`)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	scrpr := New(nil, NewConfigStore(""), "", testLogger())
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL+"/login.php", http.NoBody)
	require.NoError(t, err)
	resp, err := scrpr.httpClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"/login.php", "/real-login.php"}, seen)
}

// charset.NewReader reports an empty read as io.EOF, so a body with
// nothing in it arrives looking like a failed read. A tracker that
// answers with nothing has answered: the scrape holds zero rows and the
// mirror stands. A 204, a 200 with no body, and an unfollowed redirect
// are all that answer, which is why this sits beside the redirect tests.
func TestScraper_EmptyResponseBodyIsNotAFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
	}{
		{name: "200 with no body", status: http.StatusOK},
		{name: "204", status: http.StatusNoContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			}))
			t.Cleanup(server.Close)

			def := fmt.Sprintf(`
id: empty-tracker
name: empty-tracker
links:
  - %s/
search:
  paths:
    - path: /
  rows:
    selector: ".row"
  fields:
    title:
      selector: "a"
`, server.URL)
			tracker := loadTestTracker(t, t.TempDir(), "empty-tracker", def)
			store, err := database.Open(context.Background(), ":memory:")
			require.NoError(t, err)
			t.Cleanup(func() { store.Close() })

			scrpr := New(store, NewConfigStore(""), "", testLogger())
			require.NoError(t, scrpr.ScrapeIndexer(context.Background(), tracker,
				SearchParams{Query: "test", Type: "search"}),
				"an empty body is an answer, not an outage")
			require.Empty(t, storedTorrents(t, scrpr, tracker))
		})
	}
}

// TestScraper_WarnsWhenFlareSolverrCannotCarryHeaders covers the one thing
// that can be done about a definition's search.headers under FlareSolverr:
// say so. The request is made by a real browser, which supplies its own
// headers, and FlareSolverr v3 removed the option to override them — so a
// definition relying on an Accept or an API key is answered differently
// and nothing would otherwise explain why.
func TestScraper_WarnsWhenFlareSolverrCannotCarryHeaders(t *testing.T) {
	flare := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		if req["cmd"] == "sessions.create" {
			fmt.Fprint(w, `{"status":"ok","session":"s1"}`)
			return
		}
		// There is nowhere in the payload for the headers to go.
		assert.NotContains(t, req, "headers")
		fmt.Fprint(w, `{"status":"ok","solution":{"response":"<table></table>"}}`)
	}))
	t.Cleanup(flare.Close)

	var logged bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelWarn}))

	server, _ := newFormRecorder(t)
	scrpr, def := newQueryTracker(t, "flare-headers", server.URL, `    nm: "{{ .Keywords }}"`)
	def.Search.Headers = map[string]any{"Accept": "application/json", "X-Api-Key": "k"}

	scrpr.flareSolverrURL = flare.URL
	scrpr.logger = logger

	ctx := context.Background()
	require.NoError(t, scrpr.ScrapeIndexer(ctx, def, SearchParams{Query: "test", Type: "search"}))

	out := logged.String()
	require.Contains(t, out, "search headers are not sent when FlareSolverr is in use")
	require.Contains(t, out, "Accept, X-Api-Key", "the warning names which headers")

	// Once per tracker, not once per search: a warning repeated on every
	// poll is one an operator stops reading.
	before := strings.Count(out, "FlareSolverr is in use")
	params := SearchParams{Query: "other", Type: "search"}
	require.NoError(t, scrpr.ScrapeIndexer(ctx, def, params))
	require.Equal(t, before, strings.Count(logged.String(), "FlareSolverr is in use"))
}

// TestScraper_NoHeaderWarningWithoutHeaders keeps the warning from firing
// for the definitions it has nothing to say about.
func TestScraper_NoHeaderWarningWithoutHeaders(t *testing.T) {
	flare := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		if req["cmd"] == "sessions.create" {
			fmt.Fprint(w, `{"status":"ok","session":"s1"}`)
			return
		}
		fmt.Fprint(w, `{"status":"ok","solution":{"response":"<table></table>"}}`)
	}))
	t.Cleanup(flare.Close)

	var logged bytes.Buffer
	server, _ := newFormRecorder(t)
	scrpr, def := newQueryTracker(t, "flare-plain", server.URL, `    nm: "{{ .Keywords }}"`)
	scrpr.flareSolverrURL = flare.URL
	scrpr.logger = slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelWarn}))

	require.NoError(t, scrpr.ScrapeIndexer(context.Background(), def,
		SearchParams{Query: "test", Type: "search"}))
	require.NotContains(t, logged.String(), "FlareSolverr is in use")
}
