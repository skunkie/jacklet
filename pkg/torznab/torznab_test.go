// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package torznab_test

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

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
	scrpr := scraper.NewWithOptions(scraper.NewConfigStore(""), "", logger, scraper.Options{Sink: store})
	handler := torznab.NewWithOptions(store, scrpr, scraper.NewDefinitionStore(definitionsPath, logger), apiKey, logger, options)

	mux := http.NewServeMux()
	handler.Routes(mux)
	return mux
}

func newTestDB(t *testing.T) *database.Store {
	t.Helper()
	store, err := database.Open(t.Context(), ":memory:")
	require.NoError(t, err, "failed to initialize in-memory database")
	t.Cleanup(func() { store.Close() })
	return store
}

// storeTorrent puts a torrent in the store and returns its row id. Fields left
// unset take the zero value, so each test states only what it asserts on.
func storeTorrent(t *testing.T, s *database.Store, torrent scraper.Torrent) int64 {
	t.Helper()
	if torrent.Tracker == "" {
		torrent.Tracker = testIndexerID
	}
	require.NoError(t, s.Upsert(t.Context(), torrent), "failed to store %q", torrent.Name)

	stored, _, err := s.Search(t.Context(), scraper.Query{
		Limit:    100,
		Terms:    strings.Fields(torrent.Name),
		Trackers: []string{torrent.Tracker},
	})
	require.NoError(t, err, "failed to read back %q", torrent.Name)
	for i := range stored {
		if stored[i].Name == torrent.Name && stored[i].DetailsURL == torrent.DetailsURL && stored[i].InfoHash == torrent.InfoHash {
			return stored[i].ID
		}
	}
	require.FailNow(t, "failed to read back the torrent", "no stored row named %q", torrent.Name)
	return 0
}

func TestTorznabHandlerServesStoredResultsAsAFeed(t *testing.T) {
	db := newTestDB(t)

	// Insert a test torrent
	storeTorrent(t, db, scraper.Torrent{
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

func TestTorznabHandlerUnknownIndexer(t *testing.T) {
	db := newTestDB(t)
	mux := newTestHandler(t, db, t.TempDir(), "")

	req, err := http.NewRequest(http.MethodGet, "/api/v2.0/indexers/does-not-exist/results/torznab/api?t=caps", http.NoBody)
	require.NoError(t, err)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	require.Equal(t, http.StatusNotFound, rr.Code, "want 404 for an unknown indexer id")
}

func TestTorznabHandlerRequiresAPIKey(t *testing.T) {
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

func TestTorznabHandlerFiltersByQueryAndCategory(t *testing.T) {
	db := newTestDB(t)

	insert := func(name string, category int) {
		storeTorrent(t, db, scraper.Torrent{
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

func TestTorznabHandlerAcceptsSpecCorrectSearchTypes(t *testing.T) {
	db := newTestDB(t)

	storeTorrent(t, db, scraper.Torrent{
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

func TestTorznabHandlerPagination(t *testing.T) {
	db := newTestDB(t)

	for i := range 5 {
		storeTorrent(t, db, scraper.Torrent{
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

func TestTorznabHandlerResultsJSON(t *testing.T) {
	db := newTestDB(t)

	storeTorrent(t, db, scraper.Torrent{
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

// TestTorznabHandlerResultsJSONUnreachableIndexer covers how the JSON
// endpoint reports a tracker it could not reach: Jackett's shape has a
// per-indexer slot for the failure, so the request succeeds and the
// failure is recorded there. A client searching several indexers at once
// must not lose the ones that answered because one did not.
func TestTorznabHandlerResultsJSONUnreachableIndexer(t *testing.T) {
	db := newTestDB(t)
	tempDefsDir := t.TempDir()
	newUnreachableDef(t, tempDefsDir, unreachableIndexerID)
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

// TestTorznabHandlerDirectoryFallsBackToNameForID covers a definition
// that omits "id" (valid: TrackerID falls back to "name"). The directory
// must use the same fallback (scraper.TrackerID), not raw def.ID, or it
// advertises an empty id for an indexer that is perfectly reachable by
// name, and a client built from the listing asks for
// "/api/v2.0/indexers//results/torznab".
func TestTorznabHandlerDirectoryFallsBackToNameForID(t *testing.T) {
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
	scrpr := scraper.NewWithOptions(scraper.NewConfigStore(""), "", logger, scraper.Options{Sink: db})
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

// TestTorznabHandlerResultsJSONAcceptsJackettParameterNames covers the
// parameter names a client written against Jackett sends to this address:
// Jackett binds an ApiSearch model there, so the query is "Query" and the
// categories are "Category[]", neither of which is Torznab's spelling. A
// search that ignored them would answer with unrelated stored releases
// rather than with nothing, which is worse than an error.
func TestTorznabHandlerResultsJSONAcceptsJackettParameterNames(t *testing.T) {
	db := newTestDB(t)
	storeTorrent(t, db, scraper.Torrent{
		Category:  2000,
		Name:      "Sample Release 2024",
		Published: "2024-03-15T12:00:00Z",
	})
	storeTorrent(t, db, scraper.Torrent{
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

// TestTorznabHandlerResultsJSONIsUnpaged covers the one place the JSON
// endpoint must not follow the Torznab feed: Jackett's returns the whole
// result set and takes no limit, so a client sends none and would lose
// everything past the feed's default page without noticing.
func TestTorznabHandlerResultsJSONIsUnpaged(t *testing.T) {
	db := newTestDB(t)
	const stored = 75
	for i := range stored {
		storeTorrent(t, db, scraper.Torrent{
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

// TestTorznabHandlerFilterIndexer covers a filter expression standing in
// for an indexer id, which is how a client offering "all working
// indexers" addresses this API. A tracker nothing has scraped yet has
// recorded no failure, so it is healthy and a fresh process answers the
// filter with every configured indexer rather than with none.
func TestTorznabHandlerFilterIndexer(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newNamedIndexerDef(t, dir, "alpha")
	newNamedIndexerDef(t, dir, "beta")

	storeTorrent(t, db, scraper.Torrent{Name: "Sample Alpha", Published: "2024-03-02T00:00:00Z", Tracker: "alpha"})
	storeTorrent(t, db, scraper.Torrent{Name: "Sample Beta", Published: "2024-03-01T00:00:00Z", Tracker: "beta"})

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

// TestTorznabHandlerFilterIndexerUnsupported covers a filter Jacklet
// does not serve, which is refused rather than answered with an empty
// search a client would read as "no indexer matches".
func TestTorznabHandlerFilterIndexerUnsupported(t *testing.T) {
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

// TestTorznabHandlerResultsJSONNarrowsToRequestedTrackers covers
// Jackett's "Tracker[]": a client re-runs one search over a subset of
// what the addressed indexer covers, rather than addressing each indexer
// separately. Naming none of them is the client's own answer, so it is an
// empty result rather than an error.
func TestTorznabHandlerResultsJSONNarrowsToRequestedTrackers(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newNamedIndexerDef(t, dir, "alpha")
	newNamedIndexerDef(t, dir, "beta")

	storeTorrent(t, db, scraper.Torrent{Name: "Sample Alpha", Published: "2024-03-02T00:00:00Z", Tracker: "alpha"})
	storeTorrent(t, db, scraper.Torrent{Name: "Sample Beta", Published: "2024-03-01T00:00:00Z", Tracker: "beta"})

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

// TestTorznabHandlerResultsJSONNullsUnscrapedNumbers covers the fields
// Jackett leaves nullable. A definition that scraped no value for one of
// them leaves zero in the store, and zero there means absent, so the
// response says null rather than claiming a release of no bytes or a
// seeding requirement of no seconds.
//
// Seeders, Peers and the volume factors are the deliberate exception: for
// those zero is an answer, so they stay numbers even when they are zero.
func TestTorznabHandlerResultsJSONNullsUnscrapedNumbers(t *testing.T) {
	db := newTestDB(t)
	storeTorrent(t, db, scraper.Torrent{
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
	storeTorrent(t, db, scraper.Torrent{
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

// TestTorznabHandlerFilterIndexerMatchesNothing covers a filter that
// selects none of the configured indexers. It is a well-formed meta
// indexer that happens to cover nothing, so it answers empty rather than
// reporting the install broken — which is what a client sees when a user
// asks for the healthy indexers while every tracker is failing.
//
// The empty answer must not come from the store. A search scoped to no
// trackers is scoped to every tracker there, so falling through to it
// would return the whole store instead of nothing.
func TestTorznabHandlerFilterIndexerMatchesNothing(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newNamedIndexerDef(t, dir, "alpha")
	newNamedIndexerDef(t, dir, "beta")

	storeTorrent(t, db, scraper.Torrent{Name: "Sample Alpha", Published: "2024-03-02T00:00:00Z", Tracker: "alpha"})
	storeTorrent(t, db, scraper.Torrent{Name: "Sample Beta", Published: "2024-03-01T00:00:00Z", Tracker: "beta"})

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

// TestTorznabHandlerMetaIndexerReportsNothingConfigured keeps the one
// case that is a broken install distinct from the empty answers above: no
// definition loaded at all. Every meta indexer reports it, not only the
// aggregate — a client that addresses this API by filter rather than by
// id would otherwise be the one kind of client never told, and a filter
// is exactly what a client offering "all healthy indexers" sends.
func TestTorznabHandlerMetaIndexerReportsNothingConfigured(t *testing.T) {
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

// unreachableIndexerID is the id newUnreachableDef is given when no stored
// rows have to share it; a test answering from rows stored under
// testIndexerID passes that id instead.
const unreachableIndexerID = "test-unreachable"

// newUnreachableDef writes a definition for id whose only link refuses
// connections, so every scrape of it fails and a search can be answered
// from the store alone.
func newUnreachableDef(t *testing.T, dir, id string) {
	t.Helper()
	def := fmt.Sprintf(`
id: %[1]s
name: %[1]s
links:
  - http://127.0.0.1:1/
search:
  paths:
    - path: "/"
      method: get
  rows:
    selector: ".torrent_row"
  fields:
    title:
      selector: "a"
`, id)
	require.NoError(t, os.WriteFile(dir+"/"+id+".yml", []byte(def), 0o600),
		"failed to write indexer definition")
}

// The store exists so a tracker outage degrades to stale results rather
// than to no results at all.
func TestTorznabHandlerServesStoredResultsWhenScrapeFails(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newUnreachableDef(t, dir, testIndexerID)

	storeTorrent(t, db, scraper.Torrent{
		Category:    2000,
		DownloadURL: "magnet:?xt=1",
		Leechers:    1,
		Name:        "Cached Release",
		Published:   "2024-01-01T00:00:00Z",
		Seeders:     5,
		Size:        100,
	})

	mux := newTestHandler(t, db, dir, "")

	items := searchFeed(t, mux, "q=Cached").Channel.Items
	require.Len(t, items, 1, "want the stored release served")
	require.Equal(t, "Cached Release", items[0].Title)

	// The JSON endpoint shares the pipeline and must behave the same way.
	req := httptest.NewRequest(http.MethodGet, "/api/v2.0/indexers/"+testIndexerID+"/results?q=Cached", http.NoBody)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "results: %s", rec.Body.String())
}

// With nothing stored to fall back on, an unreachable tracker is still an
// error rather than a silently empty feed.
func TestTorznabHandlerFailsWhenScrapeFailsWithNothingStored(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newUnreachableDef(t, dir, testIndexerID)

	req := httptest.NewRequest(http.MethodGet, "/api/v2.0/indexers/"+testIndexerID+"/results/torznab/api?t=search&q=anything", http.NoBody)
	rec := httptest.NewRecorder()
	newTestHandler(t, db, dir, "").ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadGateway, rec.Code, "body: %s", rec.Body.String())
}

// A "%" in the query is a literal character to match, not a wildcard.
func TestTorznabHandlerEscapesLikeWildcards(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newIndexerDef(t, dir)

	for _, name := range []string{"100% Example", "Other Example"} {
		storeTorrent(t, db, scraper.Torrent{
			Category:    2000,
			DownloadURL: "magnet:?xt=1",
			Leechers:    1,
			Name:        name,
			Seeders:     5,
			Size:        100,
		})
	}

	mux := newTestHandler(t, db, dir, "")

	items := searchFeed(t, mux, "q=100%25+Example").Channel.Items
	require.Len(t, items, 1, "want only the literal match")
	require.Equal(t, "100% Example", items[0].Title)

	// A bare "%" must not match every row.
	require.Len(t, searchFeed(t, mux, "q=%25").Channel.Items, 1, "a literal %% matched the wrong number of rows")
}

// Asking for a top-level category returns results filed under any of its
// subcategories, as Jackett does.
func TestTorznabHandlerParentCategoryMatchesSubcategories(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newIndexerDef(t, dir)

	for name, cat := range map[string]int{"HD Movie": 2040, "SD Movie": 2030, "A TV Show": 5040} {
		storeTorrent(t, db, scraper.Torrent{
			Category:    cat,
			DownloadURL: "magnet:?xt=1",
			Leechers:    1,
			Name:        name,
			Seeders:     5,
			Size:        100,
		})
	}

	items := searchFeed(t, newTestHandler(t, db, dir, ""), "cat=2000").Channel.Items
	require.Len(t, items, 2, "want both movie subcategories")
	for _, item := range items {
		require.NotContains(t, item.Title, "TV", "a TV result leaked into a Movies query")
	}
}

// Caps reflect what the definition declares rather than a fixed set.
func TestTorznabHandlerCapsFromDefinitionModes(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()

	def := fmt.Sprintf(`
id: %[1]s
name: %[1]s
links:
  - http://tracker.example/
caps:
  modes:
    search: [q]
    book-search: [q, author]
  categorymappings:
    - {id: 1, cat: "Books/EBook"}
search:
  paths:
    - path: "/"
  rows:
    selector: ".row"
  fields:
    title:
      selector: "a"
`, testIndexerID)
	require.NoError(t, os.WriteFile(dir+"/"+testIndexerID+".yml", []byte(def), 0o600),
		"failed to write definition")

	req := httptest.NewRequest(http.MethodGet, "/api/v2.0/indexers/"+testIndexerID+"/results/torznab/api?t=caps", http.NoBody)
	rec := httptest.NewRecorder()
	newTestHandler(t, db, dir, "").ServeHTTP(rec, req)

	var caps struct {
		Categories struct {
			Category []struct {
				ID int `xml:"id,attr"`
			} `xml:"category"`
		} `xml:"categories"`
		Searching struct {
			BookSearch struct {
				Available       string `xml:"available,attr"`
				SupportedParams string `xml:"supportedParams,attr"`
			} `xml:"book-search"`
			TVSearch struct {
				Available string `xml:"available,attr"`
			} `xml:"tv-search"`
		} `xml:"searching"`
	}
	require.NoError(t, xml.Unmarshal(rec.Body.Bytes(), &caps), "failed to decode caps")

	require.Equal(t, "yes", caps.Searching.BookSearch.Available, "book-search available")
	require.Equal(t, "q,author", caps.Searching.BookSearch.SupportedParams, "book-search params")
	require.Equal(t, "no", caps.Searching.TVSearch.Available, "tv-search available")
	require.Len(t, caps.Categories.Category, 1, "want only the Books/EBook subcategory")
	require.Equal(t, 7020, caps.Categories.Category[0].ID, "want the Books/EBook subcategory")
}

// The JSON endpoint carries the details URL and publish date too.
func TestTorznabHandlerResultsJSONIncludesDetailsAndDate(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newIndexerDef(t, dir)

	const details = "http://tracker.example/details/7"
	const published = "2024-05-06T07:08:09Z"
	storeTorrent(t, db, scraper.Torrent{
		Category:    2000,
		DetailsURL:  details,
		DownloadURL: "magnet:?xt=1",
		Leechers:    1,
		Name:        "JSON Release",
		Published:   published,
		Seeders:     5,
		Size:        100,
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v2.0/indexers/"+testIndexerID+"/results?q=JSON", http.NoBody)
	rec := httptest.NewRecorder()
	newTestHandler(t, db, dir, "").ServeHTTP(rec, req)

	var body struct {
		Results []struct {
			Details     string `json:"details"`
			GUID        string `json:"guid"`
			PublishDate string `json:"publishDate"`
		} `json:"results"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), "failed to decode results")
	require.Len(t, body.Results, 1)
	require.Equal(t, details, body.Results[0].Details, "details")
	require.Equal(t, details, body.Results[0].GUID, "guid")
	require.Equal(t, published, body.Results[0].PublishDate, "publishDate")
}

// Every mode handleCaps advertises as available must be accepted by the
// search endpoint, or a client takes Jacklet up on an offer it rejects.
func TestTorznabHandlerAcceptsEveryAdvertisedMode(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()

	def := fmt.Sprintf(`
id: %[1]s
name: %[1]s
links:
  - http://tracker.example/
caps:
  modes:
    search: [q]
    tv-search: [q, season, ep]
    movie-search: [q]
    music-search: [q]
    book-search: [q]
  categorymappings:
    - {id: 1, cat: "Movies"}
search:
  paths:
    - path: "/"
  rows:
    selector: ".row"
  fields:
    title:
      selector: "a"
`, testIndexerID)
	require.NoError(t, os.WriteFile(dir+"/"+testIndexerID+".yml", []byte(def), 0o600),
		"failed to write definition")

	// A stored row keeps an unreachable tracker from turning into a 502,
	// leaving the response status to reflect the "t" handling alone.
	storeTorrent(t, db, scraper.Torrent{
		Category:    2000,
		DownloadURL: "magnet:?xt=1",
		Leechers:    1,
		Name:        "Stored",
		Seeders:     1,
		Size:        1,
	})

	mux := newTestHandler(t, db, dir, "")
	for _, mode := range []string{"search", "tvsearch", "movie", "music", "book", "audio"} {
		t.Run(mode, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v2.0/indexers/"+testIndexerID+"/results/torznab/api?t="+mode, http.NoBody)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			require.Equal(t, http.StatusOK, rec.Code, "t=%s: %s", mode, rec.Body.String())
		})
	}
}

// A multi-word query must require every word, so unrelated stored releases
// do not leak into a result set.
func TestTorznabHandlerRequiresEveryQueryTerm(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newIndexerDef(t, dir)

	for _, name := range []string{"Example Series S01E01", "Other Series S01E01", "Example Series Soundtrack"} {
		storeTorrent(t, db, scraper.Torrent{
			Category:    5000,
			DownloadURL: "magnet:?xt=1",
			Leechers:    1,
			Name:        name,
			Seeders:     1,
			Size:        1,
		})
	}

	items := searchFeed(t, newTestHandler(t, db, dir, ""), "q=Example+Series").Channel.Items
	require.Len(t, items, 2, "want the two Example Series releases")
	for _, item := range items {
		require.Contains(t, item.Title, "Example Series", "an unrelated release matched")
	}
}

// staticTrackerSource serves definitions from memory, standing in for a
// consumer of the scraper package that keeps its definitions somewhere
// other than a directory of YAML files.
type staticTrackerSource struct {
	defs []scraper.Tracker
}

func (s staticTrackerSource) Find(id string) (*scraper.Tracker, error) {
	for i := range s.defs {
		if scraper.TrackerID(&s.defs[i]) == id {
			found := s.defs[i]
			return &found, nil
		}
	}
	return nil, fmt.Errorf("%w: %q", scraper.ErrTrackerNotFound, id)
}

func (s staticTrackerSource) Trackers() ([]scraper.Tracker, []scraper.DefinitionError, error) {
	return s.defs, nil, nil
}

// The handler must work against any TrackerSource, not just the
// directory-backed store.
func TestTorznabHandlerAcceptsACustomTrackerSource(t *testing.T) {
	db := newTestDB(t)

	source := staticTrackerSource{defs: []scraper.Tracker{{
		Caps: scraper.Caps{CategoryMappings: []scraper.CategoryMapping{{Cat: "Movies", ID: "1"}}},
		ID:   testIndexerID,
		Name: "In-Memory Tracker",
	}}}

	logger := slog.New(slog.DiscardHandler)
	scrpr := scraper.NewWithOptions(scraper.NewConfigStore(""), "", logger, scraper.Options{Sink: db})
	handler := torznab.New(db, scrpr, source, "", logger)

	mux := http.NewServeMux()
	handler.Routes(mux)

	t.Run("serves caps", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v2.0/indexers/"+testIndexerID+"/results/torznab/api?t=caps", http.NoBody)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
		require.Contains(t, rec.Body.String(), `id="2000"`, "want the Movies category in caps")
	})

	t.Run("lists the indexer", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v2.0/indexers", http.NoBody)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		var body []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), "failed to decode")
		require.Len(t, body, 1)
		require.Equal(t, "In-Memory Tracker", body[0].Name)
	})

	t.Run("reports an unknown id", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v2.0/indexers/absent/results/torznab/api?t=caps", http.NoBody)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		require.Equal(t, http.StatusNotFound, rec.Code)
	})
}

// A search must match a non-ASCII title regardless of case. SQLite's LIKE
// folds case for ASCII only, which would make a Cyrillic query silently
// return nothing on a Russian-language tracker.
func TestTorznabHandlerMatchesNonASCIICaseInsensitively(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newIndexerDef(t, dir)

	for _, name := range []string{"Тестовый Релиз (2009) BDRip", "Тёмный Образец (1977)"} {
		storeTorrent(t, db, scraper.Torrent{
			Category:    2000,
			DownloadURL: "magnet:?xt=1",
			Leechers:    1,
			Name:        name,
			Seeders:     1,
			Size:        1,
		})
	}

	mux := newTestHandler(t, db, dir, "")

	tests := []struct {
		name  string
		query string
		want  string
	}{
		{name: "lowercase query, capitalized title", query: "q=%D1%82%D0%B5%D1%81%D1%82%D0%BE%D0%B2%D1%8B%D0%B9", want: "Тестовый Релиз (2009) BDRip"},
		{name: "matching case", query: "q=%D0%A2%D0%B5%D1%81%D1%82%D0%BE%D0%B2%D1%8B%D0%B9", want: "Тестовый Релиз (2009) BDRip"},
		{name: "term containing ё", query: "q=%D1%82%D1%91%D0%BC%D0%BD%D1%8B%D0%B9", want: "Тёмный Образец (1977)"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			items := searchFeed(t, mux, tc.query).Channel.Items
			require.Len(t, items, 1)
			require.Equal(t, tc.want, items[0].Title)
		})
	}
}

func TestTorznabHandlerScrapeDeadlineStillAllowsStoredFallback(t *testing.T) {
	tracker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer tracker.Close()

	dir := t.TempDir()
	def := fmt.Sprintf("id: %[1]s\nname: %[1]s\nlinks:\n  - %[2]s/\nsearch:\n  paths:\n    - path: /\n  rows:\n    selector: .row\n  fields:\n    title:\n      selector: a\n", testIndexerID, tracker.URL)
	require.NoError(t, os.WriteFile(dir+"/"+testIndexerID+".yml", []byte(def), 0o600))

	db := newTestDB(t)
	storeTorrent(t, db, scraper.Torrent{DownloadURL: "magnet:?xt=1", Name: "Fallback Example"})
	mux := newTestHandlerWithOptions(t, db, dir, "", torznab.Options{ScrapeTimeout: 20 * time.Millisecond})
	items := searchFeed(t, mux, "q=Fallback").Channel.Items
	require.Len(t, items, 1)
	require.Equal(t, "Fallback Example", items[0].Title)
}

func TestTorznabHandlerFailedScrapeAllowsPagePastLastResult(t *testing.T) {
	tracker := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	trackerURL := tracker.URL
	tracker.Close()

	dir := t.TempDir()
	def := fmt.Sprintf("id: %[1]s\nname: %[1]s\nlinks:\n  - %[2]s/\nsearch:\n  paths:\n    - path: /\n  rows:\n    selector: .row\n  fields:\n    title:\n      selector: a\n", testIndexerID, trackerURL)
	require.NoError(t, os.WriteFile(dir+"/"+testIndexerID+".yml", []byte(def), 0o600))

	db := newTestDB(t)
	storeTorrent(t, db, scraper.Torrent{DownloadURL: "magnet:?xt=1", Name: "Paged Example"})
	feed := searchFeed(t, newTestHandler(t, db, dir, ""), "q=Paged&offset=10")
	require.Empty(t, feed.Channel.Items, "items were served past the last page")
}

// The contact address advertised in caps and in the feed is the operator's
// to set: an unconfigured deployment must advertise none at all, rather
// than a placeholder address that clients show to their users.
func TestTorznabHandlerContactEmailIsConfigurable(t *testing.T) {
	body := func(t *testing.T, options torznab.Options, path string) string {
		t.Helper()
		db := newTestDB(t)
		dir := t.TempDir()
		newIndexerDef(t, dir)

		req := httptest.NewRequest(http.MethodGet, path, http.NoBody)
		rec := httptest.NewRecorder()
		newTestHandlerWithOptions(t, db, dir, "", options).ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
		return rec.Body.String()
	}

	capsPath := "/api/v2.0/indexers/" + testIndexerID + "/results/torznab/api?t=caps"
	feedPath := "/api/v2.0/indexers/" + testIndexerID + "/results/torznab/api?t=search"

	t.Run("omitted by default", func(t *testing.T) {
		// Each absence is paired with what must still be there, so a
		// response that lost the whole element — or never rendered —
		// cannot pass for an omitted address.
		caps := body(t, torznab.Options{}, capsPath)
		require.Contains(t, caps, "<server ", "caps did not render a server element at all")
		require.NotContains(t, caps, "email=", "caps advertised an email nobody configured")

		feed := body(t, torznab.Options{}, feedPath)
		require.Contains(t, feed, "<channel>", "the feed did not render a channel at all")
		require.NotContains(t, feed, "webMaster", "the feed advertised a webMaster nobody configured")
	})

	t.Run("advertised when configured", func(t *testing.T) {
		options := torznab.Options{ContactEmail: "ops@tracker.invalid"}
		require.Contains(t, body(t, options, capsPath), `email="ops@tracker.invalid"`, "caps dropped the configured address")
		require.Contains(t, body(t, options, feedPath), "<webMaster>ops@tracker.invalid</webMaster>", "the feed dropped the configured address")
	})
}

// A search repeated inside the scraper's de-duplication window fetches
// nothing, and the handler answers it from the store: the skip is not a
// failure of the tracker, so it must not turn into a 502 when nothing is
// stored yet either.
func TestTorznabHandlerRepeatedSearchIsNotAFailure(t *testing.T) {
	for _, target := range []string{
		"/api/v2.0/indexers/" + testIndexerID + "/results/torznab/api?t=search&q=sample",
		"/api/v2.0/indexers/" + torznab.AggregateID + "/results/torznab/api?t=search&q=sample",
	} {
		t.Run(target, func(t *testing.T) {
			dir := t.TempDir()
			newIndexerDef(t, dir)
			mux := newTestHandler(t, newTestDB(t), dir, "")

			for attempt := 1; attempt <= 2; attempt++ {
				rec := get(t, mux, target)
				require.Equal(t, http.StatusOK, rec.Code, "attempt %d: %s", attempt, rec.Body.String())
			}
		})
	}
}
