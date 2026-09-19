// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package torznab_test

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torrplay/jacklet/pkg/database"
	"github.com/torrplay/jacklet/pkg/scraper"
	"github.com/torrplay/jacklet/pkg/torznab"
	_ "modernc.org/sqlite"
)

// testIndexerID is the id/name/tracker used by every test definition
// newIndexerDef writes, matching the "tracker" column value the tests
// insert directly into the database.
const testIndexerID = "test-tracker"

// newIndexerDef writes a minimal, working tracker definition (id/name
// testIndexerID, backed by a test server that returns no matching rows)
// into dir, so handleSearch's unconditional re-scrape succeeds without
// altering the database, letting tests observe only what they inserted
// directly.
func newIndexerDef(t *testing.T, dir string) {
	t.Helper()
	newNamedIndexerDef(t, dir, testIndexerID)
}

// newNamedIndexerDef is newIndexerDef for a definition with a given id, so
// a test covering more than one indexer (the aggregate) can write several.
func newNamedIndexerDef(t *testing.T, dir, id string) {
	t.Helper()
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html></html>`))
	}))
	t.Cleanup(testServer.Close)

	def := fmt.Sprintf(`
id: %[1]s
name: %[1]s
links:
  - %[2]s/
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
`, id, testServer.URL)
	require.NoError(t, os.WriteFile(dir+"/"+id+".yml", []byte(def), 0o600),
		"failed to write indexer definition")
}

// newTestHandler builds a torznab handler and a mux routing
// the production routes to it, so
// r.PathValue("id") is populated the same way it is in main.go.
func newTestHandler(t *testing.T, store *database.Store, definitionsPath, apiKey string) *http.ServeMux {
	t.Helper()
	return newTestHandlerWithOptions(t, store, definitionsPath, apiKey, torznab.Options{})
}

func newTestHandlerWithOptions(t *testing.T, store *database.Store, definitionsPath, apiKey string, options torznab.Options) *http.ServeMux {
	t.Helper()
	logger := slog.New(slog.DiscardHandler)
	scrpr := scraper.New(store, scraper.NewConfigStore(""), "", logger)
	handler := torznab.NewWithOptions(store, scrpr, scraper.NewDefinitionStore(definitionsPath, logger), apiKey, logger, options)

	mux := http.NewServeMux()
	handler.Routes(mux)
	return mux
}

func newTestDB(t *testing.T) *database.Store {
	t.Helper()
	store, err := database.Open(context.Background(), ":memory:")
	require.NoError(t, err, "failed to initialize in-memory database")
	t.Cleanup(func() { store.Close() })
	return store
}

// storeTorrent puts a torrent in the store and returns its row id. Fields left
// unset take the zero value, so each test states only what it asserts on.
func storeTorrent(t *testing.T, s *database.Store, torrent database.Torrent) int64 {
	t.Helper()
	if torrent.Tracker == "" {
		torrent.Tracker = testIndexerID
	}
	require.NoError(t, s.Upsert(context.Background(), torrent), "failed to store %q", torrent.Name)

	stored, err := s.Recent(context.Background(), torrent.Tracker, 1)
	require.NoError(t, err, "failed to read back %q", torrent.Name)
	require.NotEmpty(t, stored, "failed to read back %q", torrent.Name)
	return stored[0].ID
}

func TestTorznabHandler(t *testing.T) {
	db := newTestDB(t)

	// Insert a test torrent
	storeTorrent(t, db, database.Torrent{
		DetailsURL:  "http://example.com/test.torrent",
		DownloadURL: "magnet:?xt=urn:btih:abcdef1234567890",
		Leechers:    10,
		Name:        "Test.Movie.2024.1080p.BluRay.x264-TEST",
		Published:   "2024-03-15T12:00:00Z",
		Seeders:     100,
		Size:        1234567890,
	})

	tempDefsDir := t.TempDir()
	newIndexerDef(t, tempDefsDir)
	mux := newTestHandler(t, db, tempDefsDir, "")

	req, err := http.NewRequest(http.MethodGet, "/api/v2.0/indexers/test-tracker/results/torznab/api?t=search&q=test", http.NoBody)
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	require.Equal(t, "application/xml; charset=utf-8", rr.Header().Get("Content-Type"))

	var feed torznab.Feed
	require.NoError(t, xml.Unmarshal(rr.Body.Bytes(), &feed), "failed to unmarshal the response body")

	require.Len(t, feed.Channel.Items, 1)
	require.Equal(t, "Test.Movie.2024.1080p.BluRay.x264-TEST", feed.Channel.Items[0].Title)
}

func TestTorznabHandler_UnknownIndexer(t *testing.T) {
	db := newTestDB(t)
	mux := newTestHandler(t, db, t.TempDir(), "")

	req, err := http.NewRequest(http.MethodGet, "/api/v2.0/indexers/does-not-exist/results/torznab/api?t=caps", http.NoBody)
	require.NoError(t, err)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	require.Equal(t, http.StatusNotFound, rr.Code, "want 404 for an unknown indexer id")
}

func TestTorznabHandler_RequiresAPIKey(t *testing.T) {
	db := newTestDB(t)
	tempDefsDir := t.TempDir()
	newIndexerDef(t, tempDefsDir)
	mux := newTestHandler(t, db, tempDefsDir, "secret")

	req, err := http.NewRequest(http.MethodGet, "/api/v2.0/indexers/test-tracker/results/torznab/api?t=caps", http.NoBody)
	require.NoError(t, err)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	require.Equal(t, http.StatusUnauthorized, rr.Code, "the indexer answered without an apikey")

	req, err = http.NewRequest(http.MethodGet, "/api/v2.0/indexers/test-tracker/results/torznab/api?t=caps&apikey=secret", http.NoBody)
	require.NoError(t, err)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, "a correct apikey was refused; body: %s", rr.Body.String())
}

func TestTorznabHandler_FiltersByQueryAndCategory(t *testing.T) {
	db := newTestDB(t)

	insert := func(name string, category int) {
		storeTorrent(t, db, database.Torrent{
			Category:    category,
			DownloadURL: "magnet:?xt=urn:btih:" + name,
			Leechers:    1,
			Name:        name,
			Published:   "2024-03-15T12:00:00Z",
			Seeders:     1,
			Size:        1,
		})
	}
	insert("Some.Movie.2024", 2000)
	insert("Some.Show.S01E01", 5000)

	tempDefsDir := t.TempDir()
	newIndexerDef(t, tempDefsDir)
	mux := newTestHandler(t, db, tempDefsDir, "")

	req, err := http.NewRequest(http.MethodGet, "/api/v2.0/indexers/test-tracker/results/torznab/api?t=search&q=nonexistent-query-xyz&cat=2000", http.NoBody)
	require.NoError(t, err)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	var feed torznab.Feed
	require.NoError(t, xml.Unmarshal(rr.Body.Bytes(), &feed), "body: %s", rr.Body.String())
	require.Empty(t, feed.Channel.Items, "a query that matches nothing returned items")

	req, err = http.NewRequest(http.MethodGet, "/api/v2.0/indexers/test-tracker/results/torznab/api?t=search&cat=2000", http.NoBody)
	require.NoError(t, err)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	feed = torznab.Feed{}
	require.NoError(t, xml.Unmarshal(rr.Body.Bytes(), &feed))
	require.Len(t, feed.Channel.Items, 1, "want only the Movies-category item")
	require.Equal(t, "Some.Movie.2024", feed.Channel.Items[0].Title)
}

func TestTorznabHandler_AcceptsSpecCorrectSearchTypes(t *testing.T) {
	db := newTestDB(t)

	storeTorrent(t, db, database.Torrent{
		DownloadURL: "magnet:?xt=urn:btih:abc",
		Leechers:    1,
		Name:        "Some.Show.S01E01",
		Published:   "2024-03-15T12:00:00Z",
		Seeders:     1,
		Size:        1,
	})

	tempDefsDir := t.TempDir()
	newIndexerDef(t, tempDefsDir)
	mux := newTestHandler(t, db, tempDefsDir, "")

	// The real Newznab/Torznab "t" values are unhyphenated ("tvsearch",
	// "movie"), unlike the hyphenated element names in the caps XML
	// ("tv-search", "movie-search") — this is what Sonarr/Radarr actually
	// send.
	for _, tval := range []string{"tvsearch", "movie"} {
		req, err := http.NewRequest(http.MethodGet, "/api/v2.0/indexers/test-tracker/results/torznab/api?t="+tval, http.NoBody)
		require.NoError(t, err)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		require.Equal(t, http.StatusOK, rr.Code, "t=%s, body: %s", tval, rr.Body.String())
	}
}

func TestTorznabHandler_Pagination(t *testing.T) {
	db := newTestDB(t)

	for i := range 5 {
		storeTorrent(t, db, database.Torrent{
			DownloadURL: fmt.Sprintf("magnet:?xt=urn:btih:%d", i),
			Leechers:    1,
			Name:        fmt.Sprintf("Item.%d", i),
			Published:   "2024-03-15T12:00:00Z",
			Seeders:     1,
			Size:        1,
		})
	}

	tempDefsDir := t.TempDir()
	newIndexerDef(t, tempDefsDir)
	mux := newTestHandler(t, db, tempDefsDir, "")

	req, err := http.NewRequest(http.MethodGet, "/api/v2.0/indexers/test-tracker/results/torznab/api?t=search&limit=2&offset=1", http.NoBody)
	require.NoError(t, err)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	var feed torznab.Feed
	require.NoError(t, xml.Unmarshal(rr.Body.Bytes(), &feed), "body: %s", rr.Body.String())

	require.Len(t, feed.Channel.Items, 2, "want limit=2 honored")
	require.Equal(t, 5, feed.Channel.Response.Total,
		"the total must reflect every matching row regardless of the limit")
	require.Equal(t, 1, feed.Channel.Response.Offset, "the response offset must echo the request's")
}

func TestTorznabHandler_ResultsJSON(t *testing.T) {
	db := newTestDB(t)

	storeTorrent(t, db, database.Torrent{
		Category:    2000,
		DownloadURL: "magnet:?xt=urn:btih:abc123",
		Leechers:    7,
		Name:        "Test.Movie.2024",
		Published:   "2024-03-15T12:00:00Z",
		Seeders:     42,
		Size:        123456,
	})

	tempDefsDir := t.TempDir()
	newIndexerDef(t, tempDefsDir)
	mux := newTestHandler(t, db, tempDefsDir, "")

	req, err := http.NewRequest(http.MethodGet, "/api/v2.0/indexers/test-tracker/results?q=test", http.NoBody)
	require.NoError(t, err)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	require.True(t, strings.HasPrefix(rr.Header().Get("Content-Type"), "application/json"),
		"Content-Type = %q", rr.Header().Get("Content-Type"))

	var got struct {
		Indexers []struct {
			Error   string `json:"Error"`
			ID      string `json:"ID"`
			Results int    `json:"Results"`
			Status  int    `json:"Status"`
		} `json:"Indexers"`
		Results []struct {
			Category  []int  `json:"Category"`
			GUID      string `json:"Guid"`
			Link      string `json:"Link"`
			Peers     int    `json:"Peers"`
			Seeders   int    `json:"Seeders"`
			Size      int64  `json:"Size"`
			Title     string `json:"Title"`
			Tracker   string `json:"Tracker"`
			TrackerID string `json:"TrackerId"`
		} `json:"Results"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got), "body: %s", rr.Body.String())

	require.Len(t, got.Indexers, 1)
	require.Equal(t, "test-tracker", got.Indexers[0].ID)
	require.Equal(t, 2, got.Indexers[0].Status, "Jackett numbers a successful indexer 2")
	require.Empty(t, got.Indexers[0].Error)
	require.Equal(t, 1, got.Indexers[0].Results)
	require.Len(t, got.Results, 1)

	item := got.Results[0]
	require.Equal(t, "Test.Movie.2024", item.Title)
	require.Equal(t, "magnet:?xt=urn:btih:abc123", item.Link)
	require.Equal(t, 42, item.Seeders)
	// Jackett's "Peers" is the leecher count, not the swarm size.
	require.Equal(t, 7, item.Peers)
	require.Equal(t, int64(123456), item.Size)
	require.Equal(t, []int{2000}, item.Category)
	require.Equal(t, "test-tracker", item.TrackerID)
}

// TestTorznabHandler_ResultsJSON_UnreachableIndexer covers how the JSON
// endpoint reports a tracker it could not reach: Jackett's shape has a
// per-indexer slot for the failure, so the request succeeds and the
// failure is recorded there. A client searching several indexers at once
// must not lose the ones that answered because one did not.
func TestTorznabHandler_ResultsJSON_UnreachableIndexer(t *testing.T) {
	db := newTestDB(t)
	tempDefsDir := t.TempDir()
	newUnreachableDef(t, tempDefsDir)
	mux := newTestHandler(t, db, tempDefsDir, "")

	req, err := http.NewRequest(http.MethodGet, "/api/v2.0/indexers/test-unreachable/results?q=test", http.NoBody)
	require.NoError(t, err)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	require.True(t, strings.HasPrefix(rr.Header().Get("Content-Type"), "application/json"),
		"Content-Type = %q", rr.Header().Get("Content-Type"))

	var body struct {
		Indexers []struct {
			Error  string `json:"Error"`
			ID     string `json:"ID"`
			Status int    `json:"Status"`
		} `json:"Indexers"`
		Results []struct{} `json:"Results"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body), "body: %s", rr.Body.String())

	require.Empty(t, body.Results, "an unreachable indexer with nothing stored has no results")
	require.Len(t, body.Indexers, 1)
	require.Equal(t, "test-unreachable", body.Indexers[0].ID)
	require.Equal(t, 1, body.Indexers[0].Status, "Jackett numbers a failed indexer 1")
	require.NotEmpty(t, body.Indexers[0].Error, "want the failure reported against the indexer")
}

// TestTorznabHandler_Directory_FallsBackToNameForID covers a definition
// that omits "id" (valid: TrackerID falls back to "name"). The directory
// must use the same fallback (scraper.TrackerID), not raw def.ID, or it
// advertises an empty id for an indexer that is perfectly reachable by
// name, and a client built from the listing asks for
// "/api/v2.0/indexers//results/torznab".
func TestTorznabHandler_Directory_FallsBackToNameForID(t *testing.T) {
	db := newTestDB(t)
	tempDefsDir := t.TempDir()

	def := `
name: no-explicit-id
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
	require.NoError(t, os.WriteFile(tempDefsDir+"/no-id.yml", []byte(def), 0o600),
		"failed to write indexer definition")

	logger := slog.New(slog.DiscardHandler)
	scrpr := scraper.New(db, scraper.NewConfigStore(""), "", logger)
	handler := torznab.New(db, scrpr, scraper.NewDefinitionStore(tempDefsDir, logger), "", logger)

	mux := http.NewServeMux()
	handler.Routes(mux)

	req, err := http.NewRequest(http.MethodGet, "/api/v2.0/indexers", http.NoBody)
	require.NoError(t, err)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	got := rr.Body.String()
	require.Contains(t, got, `"id":"no-explicit-id"`, "the id must fall back to the definition's name")
	require.NotContains(t, got, `"id":""`)
}

// TestTorznabHandler_ResultsJSON_AcceptsJackettParameterNames covers the
// parameter names a client written against Jackett sends to this address:
// Jackett binds an ApiSearch model there, so the query is "Query" and the
// categories are "Category[]", neither of which is Torznab's spelling. A
// search that ignored them would answer with unrelated stored releases
// rather than with nothing, which is worse than an error.
func TestTorznabHandler_ResultsJSON_AcceptsJackettParameterNames(t *testing.T) {
	db := newTestDB(t)
	storeTorrent(t, db, database.Torrent{
		Category:  2000,
		Name:      "Sample Release 2024",
		Published: "2024-03-15T12:00:00Z",
	})
	storeTorrent(t, db, database.Torrent{
		Category:  5000,
		Name:      "Unrelated Compilation",
		Published: "2024-03-14T12:00:00Z",
	})

	dir := t.TempDir()
	newIndexerDef(t, dir)
	mux := newTestHandler(t, db, dir, "")

	titles := func(path string) []string {
		rr := get(t, mux, path)
		require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

		var body struct {
			Results []struct {
				Title string `json:"Title"`
			} `json:"Results"`
		}
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body), "body: %s", rr.Body.String())

		got := make([]string, len(body.Results))
		for i, result := range body.Results {
			got[i] = result.Title
		}
		return got
	}

	base := "/api/v2.0/indexers/" + testIndexerID + "/results"
	require.Equal(t, []string{"Sample Release 2024"}, titles(base+"?Query=Sample"),
		`"Query" must search, not be ignored`)
	require.Empty(t, titles(base+"?Query=nothingmatchesthis"),
		"a query matching nothing must return nothing, not the latest releases")
	require.Equal(t, []string{"Sample Release 2024"}, titles(base+"?Category%5B%5D=2000"),
		`"Category[]" must filter by category`)
	require.Empty(t, titles(base+"?Category%5B%5D=3000"),
		"a category nothing matches must return nothing")
}

// TestTorznabHandler_ResultsJSON_IsUnpaged covers the one place the JSON
// endpoint must not follow the Torznab feed: Jackett's returns the whole
// result set and takes no limit, so a client sends none and would lose
// everything past the feed's default page without noticing.
func TestTorznabHandler_ResultsJSON_IsUnpaged(t *testing.T) {
	db := newTestDB(t)
	const stored = 75
	for i := range stored {
		storeTorrent(t, db, database.Torrent{
			Name:      fmt.Sprintf("Sample Release %02d", i),
			Published: fmt.Sprintf("2024-03-15T12:%02d:00Z", i),
		})
	}

	dir := t.TempDir()
	newIndexerDef(t, dir)
	mux := newTestHandler(t, db, dir, "")

	rr := get(t, mux, "/api/v2.0/indexers/"+testIndexerID+"/results?Query=Sample")
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	var body struct {
		Results []struct{} `json:"Results"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	require.Len(t, body.Results, stored, "the JSON endpoint truncated an unpaged search")
}

// TestTorznabHandler_FilterIndexer covers a filter expression standing in
// for an indexer id, which is how a client offering "all working
// indexers" addresses this API. A tracker nothing has scraped yet has
// recorded no failure, so it is healthy and a fresh process answers the
// filter with every configured indexer rather than with none.
func TestTorznabHandler_FilterIndexer(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newNamedIndexerDef(t, dir, "alpha")
	newNamedIndexerDef(t, dir, "beta")

	storeTorrent(t, db, database.Torrent{Name: "Sample Alpha", Published: "2024-03-02T00:00:00Z", Tracker: "alpha"})
	storeTorrent(t, db, database.Torrent{Name: "Sample Beta", Published: "2024-03-01T00:00:00Z", Tracker: "beta"})

	mux := newTestHandler(t, db, dir, "")
	rr := get(t, mux, "/api/v2.0/indexers/status:healthy/results?Query=Sample")
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	var body struct {
		Indexers []struct {
			ID string `json:"ID"`
		} `json:"Indexers"`
		Results []struct {
			TrackerID string `json:"TrackerId"`
		} `json:"Results"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body), "body: %s", rr.Body.String())

	require.Len(t, body.Indexers, 2, "both indexers are healthy until one fails")
	require.Len(t, body.Results, 2)
	require.Equal(t, "alpha", body.Results[0].TrackerID)
	require.Equal(t, "beta", body.Results[1].TrackerID)
}

// TestTorznabHandler_FilterIndexer_Unsupported covers a filter Jacklet
// does not serve, which is refused rather than answered with an empty
// search a client would read as "no indexer matches".
func TestTorznabHandler_FilterIndexer_Unsupported(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newIndexerDef(t, dir)
	mux := newTestHandler(t, db, dir, "")

	rr := get(t, mux, "/api/v2.0/indexers/kind:public/results?Query=Sample")
	require.Equal(t, http.StatusNotFound, rr.Code, "body: %s", rr.Body.String())

	var body map[string]string
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body), "body: %s", rr.Body.String())
	require.Contains(t, body["error"], "unsupported indexer filter")
}

// TestTorznabHandler_ResultsJSON_NarrowsToRequestedTrackers covers
// Jackett's "Tracker[]": a client re-runs one search over a subset of
// what the addressed indexer covers, rather than addressing each indexer
// separately. Naming none of them is the client's own answer, so it is an
// empty result rather than an error.
func TestTorznabHandler_ResultsJSON_NarrowsToRequestedTrackers(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newNamedIndexerDef(t, dir, "alpha")
	newNamedIndexerDef(t, dir, "beta")

	storeTorrent(t, db, database.Torrent{Name: "Sample Alpha", Published: "2024-03-02T00:00:00Z", Tracker: "alpha"})
	storeTorrent(t, db, database.Torrent{Name: "Sample Beta", Published: "2024-03-01T00:00:00Z", Tracker: "beta"})

	mux := newTestHandler(t, db, dir, "")

	read := func(path string) (indexers, owners []string) {
		rr := get(t, mux, path)
		require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

		var body struct {
			Indexers []struct {
				ID string `json:"ID"`
			} `json:"Indexers"`
			Results []struct {
				TrackerID string `json:"TrackerId"`
			} `json:"Results"`
		}
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body), "body: %s", rr.Body.String())

		for _, indexer := range body.Indexers {
			indexers = append(indexers, indexer.ID)
		}
		for _, result := range body.Results {
			owners = append(owners, result.TrackerID)
		}
		return indexers, owners
	}

	base := "/api/v2.0/indexers/all/results?Query=Sample"

	indexers, owners := read(base)
	require.Equal(t, []string{"alpha", "beta"}, indexers)
	require.Equal(t, []string{"alpha", "beta"}, owners)

	indexers, owners = read(base + "&Tracker%5B%5D=beta")
	require.Equal(t, []string{"beta"}, indexers, "the search must not reach an indexer it was narrowed away from")
	require.Equal(t, []string{"beta"}, owners)

	// An id the addressed indexer does not cover is ignored, as Jackett
	// ignores it, leaving only the ones it does cover.
	indexers, owners = read(base + "&Tracker%5B%5D=beta,nonexistent")
	require.Equal(t, []string{"beta"}, indexers)
	require.Equal(t, []string{"beta"}, owners)

	indexers, owners = read(base + "&Tracker%5B%5D=nonexistent")
	require.Empty(t, indexers, "naming no covered indexer searches nothing")
	require.Empty(t, owners)
}

// TestTorznabHandler_ResultsJSON_NullsUnscrapedNumbers covers the fields
// Jackett leaves nullable. A definition that scraped no value for one of
// them leaves zero in the store, and zero there means absent, so the
// response says null rather than claiming a release of no bytes or a
// seeding requirement of no seconds.
//
// Seeders, Peers and the volume factors are the deliberate exception: for
// those zero is an answer, so they stay numbers even when they are zero.
func TestTorznabHandler_ResultsJSON_NullsUnscrapedNumbers(t *testing.T) {
	db := newTestDB(t)
	storeTorrent(t, db, database.Torrent{
		Files:           3,
		Grabs:           9,
		MinimumRatio:    1.5,
		MinimumSeedTime: 172800,
		Name:            "Sample Complete",
		Published:       "2024-03-15T12:00:00Z",
		Seeders:         4,
		Size:            2 * 1024 * 1024 * 1024,
		Year:            2024,
	})
	storeTorrent(t, db, database.Torrent{
		Name:      "Sample Sparse",
		Published: "2024-03-14T12:00:00Z",
	})

	dir := t.TempDir()
	newIndexerDef(t, dir)
	mux := newTestHandler(t, db, dir, "")

	rr := get(t, mux, "/api/v2.0/indexers/"+testIndexerID+"/results?Query=Sample")
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	var body struct {
		Results []struct {
			DownloadVolumeFactor *float64 `json:"DownloadVolumeFactor"`
			Files                *int     `json:"Files"`
			Gain                 *float64 `json:"Gain"`
			Grabs                *int     `json:"Grabs"`
			MinimumRatio         *float64 `json:"MinimumRatio"`
			MinimumSeedTime      *int     `json:"MinimumSeedTime"`
			Peers                *int     `json:"Peers"`
			Seeders              *int     `json:"Seeders"`
			Size                 *int64   `json:"Size"`
			Title                string   `json:"Title"`
			UploadVolumeFactor   *float64 `json:"UploadVolumeFactor"`
			Year                 *int     `json:"Year"`
		} `json:"Results"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body), "body: %s", rr.Body.String())
	require.Len(t, body.Results, 2)

	complete, sparse := body.Results[0], body.Results[1]
	require.Equal(t, "Sample Complete", complete.Title)
	require.Equal(t, "Sample Sparse", sparse.Title)

	require.Equal(t, 3, *complete.Files)
	require.Equal(t, 9, *complete.Grabs)
	require.Equal(t, 1.5, *complete.MinimumRatio)
	require.Equal(t, 172800, *complete.MinimumSeedTime)
	require.Equal(t, int64(2*1024*1024*1024), *complete.Size)
	require.Equal(t, 2024, *complete.Year)
	// Two gibibytes seeded by four peers.
	require.Equal(t, 8.0, *complete.Gain)

	require.Nil(t, sparse.Files, "an unscraped file count must be null")
	require.Nil(t, sparse.Grabs, "an unscraped grab count must be null")
	require.Nil(t, sparse.MinimumRatio, "an unscraped minimum ratio must be null")
	require.Nil(t, sparse.MinimumSeedTime, "an unscraped minimum seed time must be null")
	require.Nil(t, sparse.Size, "an unscraped size must be null")
	require.Nil(t, sparse.Year, "an unscraped year must be null")
	require.Nil(t, sparse.Gain, "gain follows the size it is derived from")

	// Zero here is the swarm being empty and the release being counted
	// normally, not a field nobody scraped.
	require.NotNil(t, sparse.Seeders, "a seeder count of zero is an answer, not a gap")
	require.Equal(t, 0, *sparse.Seeders)
	require.NotNil(t, sparse.Peers, "a leecher count of zero is an answer, not a gap")
	require.Equal(t, 0, *sparse.Peers)
	require.NotNil(t, sparse.DownloadVolumeFactor, "a volume factor is always reported")
	require.NotNil(t, sparse.UploadVolumeFactor, "a volume factor is always reported")
}

// TestTorznabHandler_FilterIndexer_MatchesNothing covers a filter that
// selects none of the configured indexers. It is a well-formed meta
// indexer that happens to cover nothing, so it answers empty rather than
// reporting the install broken — which is what a client sees when a user
// asks for the healthy indexers while every tracker is failing.
//
// The empty answer must not come from the store. A search scoped to no
// trackers is scoped to every tracker there, so falling through to it
// would return the whole store instead of nothing.
func TestTorznabHandler_FilterIndexer_MatchesNothing(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newNamedIndexerDef(t, dir, "alpha")
	newNamedIndexerDef(t, dir, "beta")

	storeTorrent(t, db, database.Torrent{Name: "Sample Alpha", Published: "2024-03-02T00:00:00Z", Tracker: "alpha"})
	storeTorrent(t, db, database.Torrent{Name: "Sample Beta", Published: "2024-03-01T00:00:00Z", Tracker: "beta"})

	mux := newTestHandler(t, db, dir, "")

	// No definition here declares a type, so neither filter matches one.
	for _, filter := range []string{"type:private", "tag:nope", "status:failing"} {
		t.Run(filter, func(t *testing.T) {
			rr := get(t, mux, "/api/v2.0/indexers/"+filter+"/results?Query=Sample")
			require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

			var body struct {
				Indexers []struct{} `json:"Indexers"`
				Results  []struct{} `json:"Results"`
			}
			require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body), "body: %s", rr.Body.String())
			require.Empty(t, body.Results, "a filter matching no indexer must not answer from the whole store")
			require.Empty(t, body.Indexers, "no indexer was searched")
		})
	}

	// The XML feed answers the same request with an empty feed.
	rr := get(t, mux, "/api/v2.0/indexers/type:private/results/torznab/api?t=search&q=Sample")
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	require.NotContains(t, rr.Body.String(), "Sample Alpha",
		"a filter matching no indexer must not answer from the whole store")

	// The page that was asked for is the page described back, so an empty
	// answer reports the offset the client sent rather than the zero it
	// would take for a different request.
	rr = get(t, mux, "/api/v2.0/indexers/type:private/results/torznab/api?t=search&q=Sample&offset=50")
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	require.Contains(t, rr.Body.String(), `<response offset="50" total="0">`,
		"an empty answer described a page the client did not ask for")
}

// TestTorznabHandler_MetaIndexer_ReportsNothingConfigured keeps the one
// case that is a broken install distinct from the empty answers above: no
// definition loaded at all. Every meta indexer reports it, not only the
// aggregate — a client that addresses this API by filter rather than by
// id would otherwise be the one kind of client never told, and a filter
// is exactly what a client offering "all healthy indexers" sends.
func TestTorznabHandler_MetaIndexer_ReportsNothingConfigured(t *testing.T) {
	mux := newTestHandler(t, newTestDB(t), t.TempDir(), "")

	for _, id := range []string{"all", "status:healthy", "!type:private"} {
		t.Run(id, func(t *testing.T) {
			rr := get(t, mux, "/api/v2.0/indexers/"+id+"/results?Query=Sample")
			require.Equal(t, http.StatusServiceUnavailable, rr.Code, "body: %s", rr.Body.String())

			var body map[string]string
			require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body), "body: %s", rr.Body.String())
			require.Contains(t, body["error"], "No indexer definitions are configured")
		})
	}
}
