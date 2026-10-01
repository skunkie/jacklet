// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package torznab_test

import (
	"context"
	"fmt"
	"go/parser"
	"go/token"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torrplay/jacklet/pkg/scraper"
	"github.com/torrplay/jacklet/pkg/torznab"
	"github.com/torrplay/jacklet/pkg/torznab/catalogtest"
)

// fakeCatalog is an in-memory Catalog that is also a scraper.Sink, so a
// scrape can feed it and a handler read from it, with no database anywhere.
// It answers scraper.Query the way the documented contract says.
type fakeCatalog struct {
	mu   sync.Mutex
	rows []catalogRow
}

type catalogRow struct {
	isManaged bool
	keys      map[string]bool
	torrent   scraper.Torrent
}

// Add keeps torrents with no search provenance, like a program writing
// straight into its own storage.
func (f *fakeCatalog) Add(_ context.Context, torrents []scraper.Torrent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range torrents {
		torrents[i].ID = int64(len(f.rows) + 1)
		f.rows = append(f.rows, catalogRow{keys: map[string]bool{}, torrent: torrents[i]})
	}
	return nil
}

// UpsertAllForSearch keeps each torrent under the search key that produced it.
func (f *fakeCatalog) UpsertAllForSearch(_ context.Context, torrents []scraper.Torrent, searchKey string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range torrents {
		torrent := torrents[i]
		at := slices.IndexFunc(f.rows, func(row catalogRow) bool {
			return row.torrent.Tracker == torrent.Tracker && row.torrent.Name == torrent.Name
		})
		if at < 0 {
			torrent.ID = int64(len(f.rows) + 1)
			f.rows = append(f.rows, catalogRow{keys: map[string]bool{}, torrent: torrent})
			at = len(f.rows) - 1
		}
		f.rows[at].isManaged = true
		f.rows[at].keys[searchKey] = true
	}
	return len(torrents), nil
}

func (f *fakeCatalog) Find(_ context.Context, tracker string, id int64) (scraper.Torrent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.rows {
		if f.rows[i].torrent.ID == id && f.rows[i].torrent.Tracker == tracker {
			return f.rows[i].torrent, nil
		}
	}
	return scraper.Torrent{}, fmt.Errorf("%w: %s/%d", scraper.ErrNotFound, tracker, id)
}

func (f *fakeCatalog) Search(_ context.Context, q scraper.Query) ([]scraper.Torrent, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var matched []scraper.Torrent
	for i := range f.rows {
		row, torrent := &f.rows[i], f.rows[i].torrent
		name := strings.ToLower(torrent.Name)
		switch {
		case len(q.Trackers) > 0 && !slices.Contains(q.Trackers, torrent.Tracker):
		case len(q.Categories) > 0 && !slices.Contains(q.Categories, strconv.Itoa(torrent.Category)):
		case q.QueryKey != "" && row.isManaged && !row.keys[q.QueryKey]:
		case !slices.ContainsFunc(q.Terms, func(term string) bool { return !strings.Contains(name, strings.ToLower(term)) }):
			matched = append(matched, torrent)
		}
	}
	slices.SortStableFunc(matched, func(a, b scraper.Torrent) int {
		return strings.Compare(b.Published, a.Published)
	})

	total := len(matched)
	if q.Offset >= total {
		return nil, total, nil
	}
	matched = matched[q.Offset:]
	if q.Limit < len(matched) {
		matched = matched[:max(q.Limit, 0)]
	}
	return matched, total, nil
}

// A handler works over any Catalog: here a scrape lands in an in-memory
// catalog, the search reads it back from there, and a download of a row the
// catalog does not hold is a 404 rather than an error.
func TestTorznabHandlerServesFromACustomCatalog(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<div class="row"><a href="/dl/1">Sample Release One</a></div>
<div class="row"><a href="/dl/2">Sample Release Two</a></div>`)
	}))
	t.Cleanup(site.Close)

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, testIndexerID+".yml"), []byte(fmt.Sprintf(`
id: %s
name: Custom Catalog Tracker
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
`, testIndexerID, site.URL)), 0o600))

	catalog := &fakeCatalog{}
	logger := slog.New(slog.DiscardHandler)
	scrpr := scraper.NewWithOptions(scraper.NewConfigStore(""), "", logger, scraper.Options{Sink: catalog})
	handler := torznab.New(catalog, scrpr, scraper.NewDefinitionStore(dir, logger), "", logger)
	mux := http.NewServeMux()
	handler.Routes(mux)

	items := searchFeed(t, mux, "q=sample").Channel.Items
	require.Len(t, items, 2, "the scrape did not reach the catalog the handler reads")
	require.ElementsMatch(t, []string{"Sample Release One", "Sample Release Two"}, []string{items[0].Title, items[1].Title})

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v2.0/indexers/"+testIndexerID+"/download/999", http.NoBody))
	require.Equal(t, http.StatusNotFound, rec.Code, "an unknown row was not reported as not found")
	require.Contains(t, rec.Body.String(), "No such result", "the 404 did not come from the handler's not-found branch")
}

// pkg/torznab is storage-free, so an embedding program serving its own
// Catalog is not made to carry a SQLite driver: nothing in the package may
// import the database package.
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

// The in-memory catalog the handler tests use is held to the same contract
// as Store and as any catalog an embedding program supplies.
func TestFakeCatalogMeetsTheCatalogContract(t *testing.T) {
	catalogtest.Run(t, func(t *testing.T) catalogtest.Fixture {
		t.Helper()
		catalog := &fakeCatalog{}
		return catalogtest.Fixture{
			Add: catalog.Add,
			AddForSearch: func(ctx context.Context, torrents []scraper.Torrent, searchKey string) error {
				_, err := catalog.UpsertAllForSearch(ctx, torrents, searchKey)
				return err
			},
			Catalog: catalog,
		}
	})
}

// A handler over a scraper with no Sink can never see a scrape's results, so
// building one says so; a scraper that has a Sink is not warned about.
func TestNew_WarnsWhenTheScraperHasNoSink(t *testing.T) {
	build := func(options scraper.Options) string {
		var logged strings.Builder
		logger := slog.New(slog.NewTextHandler(&logged, nil))
		scrpr := scraper.NewWithOptions(scraper.NewConfigStore(""), "", logger, options)
		torznab.New(&fakeCatalog{}, scrpr, scraper.NewDefinitionStore(t.TempDir(), logger), "", logger)
		return logged.String()
	}

	require.Contains(t, build(scraper.Options{}), "the scraper has no Sink")
	require.NotContains(t, build(scraper.Options{Sink: &fakeCatalog{}}), "no Sink")
}
