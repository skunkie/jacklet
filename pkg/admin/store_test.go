// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package admin_test

import (
	"context"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torrplay/jacklet/pkg/admin"
	"github.com/torrplay/jacklet/pkg/database"
	"github.com/torrplay/jacklet/pkg/scraper"
)

// stubStore is an admin.Store that holds nothing and answers Stats with
// whatever a test sets, so the dashboard can be shown a store that has
// figures or one that is down.
type stubStore struct {
	err   error
	stats map[string]scraper.TrackerStats
}

func (s *stubStore) Find(context.Context, string, int64) (scraper.Torrent, error) {
	return scraper.Torrent{}, scraper.ErrNotFound
}

func (s *stubStore) Search(context.Context, scraper.Query) ([]scraper.Torrent, int, error) {
	return nil, 0, nil
}

func (s *stubStore) Stats(context.Context) (map[string]scraper.TrackerStats, error) {
	return s.stats, s.err
}

// panelOver builds a signed-in panel over any admin.Store, with no
// definitions.
func panelOver(t *testing.T, store admin.Store) (*panelFixture, *http.Cookie) {
	t.Helper()
	logger := slog.New(slog.DiscardHandler)
	config := scraper.NewConfigStore(t.TempDir())
	credential, err := admin.NewPassword(testPassword, "")
	require.NoError(t, err)
	panel, err := admin.New(store, scraper.New(config, "", logger), config, scraper.NewDefinitionStore(t.TempDir(), logger), credential, logger)
	require.NoError(t, err)
	mux := http.NewServeMux()
	panel.Routes(mux)
	fixture := &panelFixture{mux: mux}
	cookie, _ := fixture.signIn(t)
	return fixture, cookie
}

// The dashboard's total is the sum of the per-tracker figures the store
// reports.
func TestAdminDashboardTotalsTheStoreFigures(t *testing.T) {
	fixture, cookie := panelOver(t, &stubStore{stats: map[string]scraper.TrackerStats{
		"alpha": {Torrents: 3, Tracker: "alpha"},
		"beta":  {Torrents: 4, Tracker: "beta"},
	}})

	page := fixture.get(t, "/admin", cookie)
	require.Contains(t, page, "<strong>7</strong>", "the total was not the sum of the stored figures")
	require.Contains(t, page, "healthy")
}

// A store that cannot answer is reported on the dashboard, which still
// renders, instead of failing the whole page.
func TestAdminDashboardReportsAnUnreachableStore(t *testing.T) {
	fixture, cookie := panelOver(t, &stubStore{err: errors.New("store unavailable")})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin", http.NoBody)
	req.AddCookie(cookie)
	fixture.mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "an unreachable store failed the page")
	require.Contains(t, rec.Body.String(), "unreachable")
}

// The test button shows what the search it ran produced, not whatever else
// the store holds for the tracker: a row stored for a different search
// stays out of the result.
func TestAdminTestShowsWhatItsSearchProduced(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<div class="row"><a href="/dl/1">Sample Release One</a></div>`)
	}))
	t.Cleanup(site.Close)

	store, err := database.Open(t.Context(), ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { store.Close() })

	defsDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(defsDir, "demo.yml"), []byte(`
id: demo
name: Demo Tracker
links:
  - `+site.URL+`/
caps:
  categorymappings:
    - {id: 1, cat: Movies}
search:
  paths:
    - path: "/"
  rows:
    selector: ".row"
  fields:
    category:
      text: "1"
    title:
      selector: "a"
    download:
      selector: "a"
      attribute: "href"
`), 0o600))
	_, err = store.UpsertAllForSearch(t.Context(),
		[]scraper.Torrent{{Name: "Sample Other Search", Published: "2024-01-01T00:00:00Z", Tracker: "demo"}}, "another-search")
	require.NoError(t, err)

	logger := slog.New(slog.DiscardHandler)
	config := scraper.NewConfigStore(t.TempDir())
	scrpr := scraper.NewWithOptions(config, "", logger, scraper.Options{Sink: store})
	credential, err := admin.NewPassword(testPassword, "")
	require.NoError(t, err)
	panel, err := admin.New(store, scrpr, config, scraper.NewDefinitionStore(defsDir, logger), credential, logger)
	require.NoError(t, err)
	mux := http.NewServeMux()
	panel.Routes(mux)
	fixture := &panelFixture{mux: mux, store: store}
	cookie, csrf := fixture.signIn(t)

	page := fixture.post(t, "/admin/indexers/demo/test", url.Values{"csrf_token": {csrf}, "query": {"sample"}}, cookie).Body.String()
	require.Contains(t, page, "Test succeeded.")
	require.Contains(t, page, "Sample Release One")
	require.Contains(t, page, `<td class="num">2000</td>`, "the test result's category cell did not show its category")
	require.NotContains(t, page, "Sample Other Search", "a row stored for a different search was shown as this test's result")
}

// pkg/admin is storage-free, so an embedding program serving its own Store
// is not made to carry a SQLite driver: nothing in the package may import
// the database package.
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

// A panel over a scraper with no Sink shows only what the store already
// holds, so building one says so; a scraper that has a Sink is not warned
// about.
func TestNew_WarnsWhenTheScraperHasNoSink(t *testing.T) {
	build := func(options scraper.Options) string {
		var logged strings.Builder
		logger := slog.New(slog.NewTextHandler(&logged, nil))
		config := scraper.NewConfigStore(t.TempDir())
		credential, err := admin.NewPassword(testPassword, "")
		require.NoError(t, err)
		_, err = admin.New(&stubStore{}, scraper.NewWithOptions(config, "", logger, options), config, scraper.NewDefinitionStore(t.TempDir(), logger), credential, logger)
		require.NoError(t, err)
		return logged.String()
	}

	require.Contains(t, build(scraper.Options{}), "the scraper has no Sink")
	require.NotContains(t, build(scraper.Options{Sink: &stubSink{}}), "no Sink")
}

// stubSink is a scraper.Sink that keeps nothing.
type stubSink struct{}

func (stubSink) UpsertAllForSearch(context.Context, []scraper.Torrent, string) (int, error) {
	return 0, nil
}
