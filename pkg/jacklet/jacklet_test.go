// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package jacklet_test

import (
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torrplay/jacklet/pkg/admin"
	"github.com/torrplay/jacklet/pkg/database"
	"github.com/torrplay/jacklet/pkg/jacklet"
	"github.com/torrplay/jacklet/pkg/scraper"
)

// newSQLiteStore opens an in-memory store, which satisfies jacklet.Store.
func newSQLiteStore(t *testing.T) *database.Store {
	t.Helper()
	store, err := database.Open(t.Context(), ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { store.Close() })
	return store
}

// newSite serves two rows and returns a definitions directory whose one
// tracker, "demo", scrapes them.
func newSite(t *testing.T) string {
	t.Helper()
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<div class="row"><a href="/dl/1">Sample Release One</a></div>
<div class="row"><a href="/dl/2">Sample Release Two</a></div>`)
	}))
	t.Cleanup(site.Close)

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "demo.yml"), []byte(fmt.Sprintf(`
id: demo
name: Demo Tracker
links:
  - %s/
search:
  paths:
    - path: "/"
  rows:
    selector: "div.row"
  fields:
    title:
      selector: "a"
    download:
      selector: "a"
      attribute: "href"
`, site.URL)), 0o600))
	return dir
}

func get(t *testing.T, handler http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, http.NoBody))
	return rec
}

func newStack(t *testing.T, options jacklet.Options) (*jacklet.Stack, *database.Store) {
	t.Helper()
	logger := slog.New(slog.DiscardHandler)
	store := newSQLiteStore(t)
	stack, err := jacklet.New(store, scraper.NewConfigStore(t.TempDir()), scraper.NewDefinitionStore(newSite(t), logger), logger, options)
	require.NoError(t, err)
	return stack, store
}

// One store serves every role: a search through the API scrapes the tracker
// into it and reads the results back from it, with nothing wired by hand.
func TestNew_SharesOneStoreAcrossTheParts(t *testing.T) {
	password, err := admin.NewPassword("sample-admin-password", "")
	require.NoError(t, err)
	stack, store := newStack(t, jacklet.Options{AdminPassword: password})
	handler, err := stack.Handler()
	require.NoError(t, err)

	rec := get(t, handler, "/api/v2.0/indexers/demo/results?q=sample")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var results struct {
		Results []struct {
			Title string `json:"Title"`
		} `json:"Results"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &results))
	require.Len(t, results.Results, 2, "the scrape did not reach the store the API reads")

	stored, total, err := store.Search(t.Context(), scraper.Query{Limit: 10, Trackers: []string{"demo"}})
	require.NoError(t, err)
	require.Equal(t, 2, total)
	require.Len(t, stored, 2, "the scraper did not write into the store")

	require.Equal(t, http.StatusOK, get(t, handler, "/admin/login").Code, "the panel was not mounted")
}

// Without a password the panel is not served, and the API still is.
func TestStackServesNoPanelWithoutAPassword(t *testing.T) {
	stack, _ := newStack(t, jacklet.Options{})
	handler, err := stack.Handler()
	require.NoError(t, err)

	require.Equal(t, http.StatusNotFound, get(t, handler, "/admin/login").Code)
	require.Equal(t, http.StatusOK, get(t, handler, "/api/v2.0/indexers").Code)
}

// The panel follows the prefix it was given.
func TestStackServesThePanelUnderItsPrefix(t *testing.T) {
	password, err := admin.NewPassword("sample-admin-password", "")
	require.NoError(t, err)
	stack, _ := newStack(t, jacklet.Options{Admin: admin.Options{Prefix: "/ops"}, AdminPassword: password})
	handler, err := stack.Handler()
	require.NoError(t, err)

	require.Equal(t, http.StatusOK, get(t, handler, "/ops/login").Code)
	require.Equal(t, http.StatusNotFound, get(t, handler, "/admin/login").Code)
}

func TestStack_Routes(t *testing.T) {
	// A prefix naming a path the mux already serves is an error to return,
	// not a panic to crash on.
	t.Run("reports a colliding prefix", func(t *testing.T) {
		password, err := admin.NewPassword("sample-admin-password", "")
		require.NoError(t, err)
		stack, _ := newStack(t, jacklet.Options{Admin: admin.Options{Prefix: "/docs"}, AdminPassword: password})

		mux := http.NewServeMux()
		mux.HandleFunc("GET /docs", func(http.ResponseWriter, *http.Request) {})
		require.ErrorContains(t, stack.Routes(mux), `admin prefix "/docs"`)
	})

	// An API path the mux already serves is reported the same way.
	t.Run("reports a colliding API path", func(t *testing.T) {
		stack, _ := newStack(t, jacklet.Options{})

		mux := http.NewServeMux()
		mux.HandleFunc("GET /api/v2.0/indexers", func(http.ResponseWriter, *http.Request) {})
		require.ErrorContains(t, stack.Routes(mux), "the API under /api/v2.0/")
	})

	// Only a pattern conflict is an error to return; a bug inside the routes
	// still panics, rather than being reported as a collision it is not.
	t.Run("panics on a runtime error", func(t *testing.T) {
		stack, _ := newStack(t, jacklet.Options{})
		require.Panics(t, func() { _ = stack.Routes(nil) })
	})
}

// The scraper's Sink is the store's to set, so one set beside it is refused
// rather than silently replaced or ignored.
func TestNew_RefusesASinkOfItsOwn(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	store := newSQLiteStore(t)
	_, err := jacklet.New(store, scraper.NewConfigStore(""), scraper.NewDefinitionStore(t.TempDir(), logger), logger,
		jacklet.Options{Scraper: scraper.Options{Sink: newSQLiteStore(t)}})
	require.ErrorContains(t, err, "Sink")
}

func TestNew_RequiresItsParts(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	store := newSQLiteStore(t)
	config := scraper.NewConfigStore("")
	trackers := scraper.NewDefinitionStore(t.TempDir(), logger)

	_, err := jacklet.New(nil, config, trackers, logger, jacklet.Options{})
	require.ErrorContains(t, err, "store")
	_, err = jacklet.New(store, nil, trackers, logger, jacklet.Options{})
	require.ErrorContains(t, err, "settings")
	_, err = jacklet.New(store, config, nil, logger, jacklet.Options{})
	require.ErrorContains(t, err, "definition")
}

func TestNew_ReportsAMalformedPanelPrefix(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	_, err := jacklet.New(newSQLiteStore(t), scraper.NewConfigStore(""), scraper.NewDefinitionStore(t.TempDir(), logger), logger,
		jacklet.Options{Admin: admin.Options{Prefix: "admin/"}})
	require.ErrorContains(t, err, "admin prefix")
}

// pkg/jacklet is storage-free, so a program supplying its own Store is not
// made to carry a SQLite driver: nothing in the package may import the
// database package.
func TestPackageImportsNoStorage(t *testing.T) {
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		require.NoError(t, err)
		for _, imported := range parsed.Imports {
			require.NotContains(t, imported.Path.Value, "pkg/database", "%s imports the storage package", name)
		}
	}
}

// One settings source serves the panel and the scraper: a setting saved in
// the panel is the one the next scrape sends.
func TestNew_SharesOneSettingsSourceAcrossTheParts(t *testing.T) {
	var sort atomic.Value
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sort.Store(r.URL.Query().Get("sort"))
		fmt.Fprint(w, `<div class="row"><a href="/dl/1">Sample Release One</a></div>`)
	}))
	t.Cleanup(site.Close)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "demo.yml"), []byte(fmt.Sprintf(`
id: demo
name: Demo Tracker
links:
  - %s/
settings:
  - name: sort
    type: text
    label: Sort
    default: added
search:
  paths:
    - path: "/"
      method: get
  inputs:
    sort: "{{ .Config.sort }}"
  rows:
    selector: "div.row"
  fields:
    title:
      selector: "a"
`, site.URL)), 0o600))

	logger := slog.New(slog.DiscardHandler)
	password, err := admin.NewPassword("sample-admin-password", "")
	require.NoError(t, err)
	stack, err := jacklet.New(newSQLiteStore(t), scraper.NewConfigStore(t.TempDir()), scraper.NewDefinitionStore(dir, logger), logger,
		jacklet.Options{AdminPassword: password})
	require.NoError(t, err)
	handler, err := stack.Handler()
	require.NoError(t, err)

	search := func(query string) {
		require.Equal(t, http.StatusOK, get(t, handler, "/api/v2.0/indexers/demo/results?q="+query).Code)
	}
	search("first")
	require.Equal(t, "added", sort.Load(), "the definition's default was not in force before any save")

	post := func(target string, form url.Values, cookie *http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if cookie != nil {
			req.AddCookie(cookie)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	login := post("/admin/login", url.Values{"password": {"sample-admin-password"}}, nil)
	require.Equal(t, http.StatusSeeOther, login.Code)
	cookie := login.Result().Cookies()[0]

	req := httptest.NewRequest(http.MethodGet, "/admin/indexers/demo", http.NoBody)
	req.AddCookie(cookie)
	page := httptest.NewRecorder()
	handler.ServeHTTP(page, req)
	csrf := regexp.MustCompile(`name="csrf_token"[^>]*\bvalue="([^"]+)"`).FindStringSubmatch(page.Body.String())
	require.NotNil(t, csrf, "no CSRF token on the indexer page")

	saved := post("/admin/indexers/demo/settings", url.Values{"csrf_token": {csrf[1]}, "sort": {"seeders"}}, cookie)
	require.Equal(t, http.StatusSeeOther, saved.Code, saved.Body.String())

	search("second")
	require.Equal(t, "seeders", sort.Load(), "a setting saved in the panel did not reach the scraper")
}
