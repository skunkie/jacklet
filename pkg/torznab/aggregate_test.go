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

	"github.com/stretchr/testify/require"
	"github.com/torrplay/jacklet/pkg/database"
	"github.com/torrplay/jacklet/pkg/scraper"
	"github.com/torrplay/jacklet/pkg/torznab"
)

// These tests cover "/api/v2.0/indexers/all/...", the aggregate indexer
// that answers
// from every configured definition at once.

// newCategorizedDef writes a definition with its own caps block, so a test
// can assert the aggregate merges categories and modes across trackers
// rather than reporting the first definition's.
func newCategorizedDef(t *testing.T, dir, id, mode, category string) {
	t.Helper()
	def := fmt.Sprintf(`
id: %[1]s
name: %[1]s Name
links:
  - http://%[1]s.example/
caps:
  modes:
    search: [q]
    %[2]s
  categorymappings:
    - {id: 1, cat: "%[3]s"}
search:
  paths:
    - path: "/"
  rows:
    selector: ".row"
  fields:
    title:
      selector: "a"
`, id, mode, category)
	require.NoError(t, os.WriteFile(dir+"/"+id+".yml", []byte(def), 0o600),
		"failed to write definition %q", id)
}

// newDeadDef writes a definition whose links point at a closed server, so
// scraping it always fails.
func newDeadDef(t *testing.T, dir, id string) {
	t.Helper()
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := dead.URL
	dead.Close()

	def := fmt.Sprintf(`
id: %[1]s
name: %[1]s
links:
  - %[2]s/
search:
  paths:
    - path: "/"
  rows:
    selector: ".row"
  fields:
    title:
      selector: "a"
`, id, url)
	require.NoError(t, os.WriteFile(dir+"/"+id+".yml", []byte(def), 0o600),
		"failed to write definition %q", id)
}

func get(t *testing.T, mux *http.ServeMux, path string) *httptest.ResponseRecorder {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, path, http.NoBody)
	require.NoError(t, err)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	return rr
}

// TestTorznabHandler_Aggregate_MergesIndexers is the core of the feature:
// one request returns releases from every definition, in one globally
// sorted, globally counted page — not the first indexer's page.
func TestTorznabHandler_Aggregate_MergesIndexers(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newNamedIndexerDef(t, dir, "alpha")
	newNamedIndexerDef(t, dir, "beta")

	// Interleaved dates, so a correct answer cannot be produced by
	// concatenating one tracker's rows after the other's.
	storeTorrent(t, db, database.Torrent{DownloadURL: "http://alpha.example/1.torrent", Name: "Test Alpha Newest", Published: "2024-03-03T00:00:00Z", Tracker: "alpha"})
	storeTorrent(t, db, database.Torrent{DownloadURL: "http://beta.example/1.torrent", Name: "Test Beta Middle", Published: "2024-03-02T00:00:00Z", Tracker: "beta"})
	storeTorrent(t, db, database.Torrent{DownloadURL: "http://alpha.example/2.torrent", Name: "Test Alpha Oldest", Published: "2024-03-01T00:00:00Z", Tracker: "alpha"})

	mux := newTestHandler(t, db, dir, "")
	rr := get(t, mux, "/api/v2.0/indexers/all/results/torznab/api?t=search&q=test")
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	var feed torznab.Feed
	require.NoError(t, xml.Unmarshal(rr.Body.Bytes(), &feed), "failed to unmarshal feed")

	titles := make([]string, len(feed.Channel.Items))
	for i, item := range feed.Channel.Items {
		titles[i] = item.Title
	}
	require.Equal(t, []string{"Test Alpha Newest", "Test Beta Middle", "Test Alpha Oldest"}, titles,
		"both indexers' releases must be merged newest-first")
	require.Equal(t, 3, feed.Channel.Response.Total, "the total must count both indexers")

	// Each item's download link must go back to the indexer that actually
	// holds the release, not to "/api/v2.0/indexers/all/...", which serves no
	// downloads.
	for _, item := range feed.Channel.Items {
		owner := "alpha"
		if strings.Contains(item.Title, "Beta") {
			owner = "beta"
		}
		require.Contains(t, item.Link, "/api/v2.0/indexers/"+owner+"/download/",
			"%q must link to indexer %q", item.Title, owner)
	}
}

// TestTorznabHandler_Aggregate_Caps checks the aggregate advertises the
// union of what its indexers offer: a client must not be told a category
// or mode is unavailable merely because one tracker lacks it.
func TestTorznabHandler_Aggregate_Caps(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newCategorizedDef(t, dir, "movies-only", "movie-search: [q, imdbid]", "Movies")
	newCategorizedDef(t, dir, "books-only", "book-search: [q, author]", "Books/EBook")

	mux := newTestHandler(t, db, dir, "")
	rr := get(t, mux, "/api/v2.0/indexers/all/results/torznab/api?t=caps")
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	var caps torznab.Caps
	require.NoError(t, xml.Unmarshal(rr.Body.Bytes(), &caps), "failed to unmarshal caps")

	require.Equal(t, "yes", caps.Searching.MovieSearch.Available,
		"movie-search must be available from the movies indexer")
	require.Equal(t, "yes", caps.Searching.BookSearch.Available,
		"book-search must be available from the books indexer")
	require.Equal(t, "no", caps.Searching.TVSearch.Available,
		"tv-search must be unavailable, since no indexer declares it")
	require.Contains(t, caps.Searching.MovieSearch.SupportedParams, "imdbid",
		"the movie mode's own params must survive the merge")

	ids := make([]int, len(caps.Categories.Category))
	for i, c := range caps.Categories.Category {
		ids[i] = c.ID
	}
	require.Equal(t, []int{2000, 7020}, ids, "want the union of both indexers' categories sorted by id")
}

// TestTorznabHandler_Aggregate_ResultsJSON covers the JSON endpoint's
// per-row tracker attribution, which is the only way a client can tell
// which indexer an aggregated release came from.
func TestTorznabHandler_Aggregate_ResultsJSON(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newNamedIndexerDef(t, dir, "alpha")
	newCategorizedDef(t, dir, "beta", "movie-search: [q]", "Movies")

	storeTorrent(t, db, database.Torrent{Name: "Test Alpha", Published: "2024-03-02T00:00:00Z", Tracker: "alpha"})
	storeTorrent(t, db, database.Torrent{Name: "Test Beta", Published: "2024-03-01T00:00:00Z", Tracker: "beta"})

	mux := newTestHandler(t, db, dir, "")
	rr := get(t, mux, "/api/v2.0/indexers/all/results?q=test")
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	var body struct {
		Indexers []struct {
			ID      string `json:"ID"`
			Results int    `json:"Results"`
		} `json:"Indexers"`
		Results []struct {
			Title     string `json:"Title"`
			Tracker   string `json:"Tracker"`
			TrackerID string `json:"TrackerId"`
		} `json:"Results"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body), "failed to unmarshal results")

	require.Len(t, body.Results, 2, "want a result from each indexer")
	require.Len(t, body.Indexers, 2, "the response must name every indexer it searched")
	require.Equal(t, 1, body.Indexers[0].Results, "each indexer reports what it contributed")
	require.Equal(t, 1, body.Indexers[1].Results, "each indexer reports what it contributed")
	require.Equal(t, "alpha", body.Results[0].TrackerID, "each result must be attributed to its own indexer")
	require.Equal(t, "beta", body.Results[1].TrackerID, "each result must be attributed to its own indexer")
	// The display name comes from the definition, not from the id.
	require.Equal(t, "beta Name", body.Results[1].Tracker, "want the owning definition's name")
}

// TestTorznabHandler_Aggregate_SurvivesOneFailingIndexer is the point of
// the aggregate: one unreachable tracker must not take the whole search
// down with it, the way it legitimately does on its own endpoint.
func TestTorznabHandler_Aggregate_SurvivesOneFailingIndexer(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newNamedIndexerDef(t, dir, "alive")
	newDeadDef(t, dir, "dead")

	storeTorrent(t, db, database.Torrent{Name: "Test Alive", Published: "2024-03-01T00:00:00Z", Tracker: "alive"})

	mux := newTestHandler(t, db, dir, "")
	rr := get(t, mux, "/api/v2.0/indexers/all/results/torznab/api?t=search&q=test")
	require.Equal(t, http.StatusOK, rr.Code, "one dead indexer took the search down; body: %s", rr.Body.String())

	var feed torznab.Feed
	require.NoError(t, xml.Unmarshal(rr.Body.Bytes(), &feed), "failed to unmarshal feed")
	require.Len(t, feed.Channel.Items, 1, "want the reachable indexer's release")
	require.Equal(t, "Test Alive", feed.Channel.Items[0].Title)
}

// TestTorznabHandler_Aggregate_FailsWhenEveryIndexerFails checks the
// aggregate still reports an outage when there is nothing left to serve:
// a silently empty feed would read as "no releases match".
func TestTorznabHandler_Aggregate_FailsWhenEveryIndexerFails(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newDeadDef(t, dir, "dead-one")
	newDeadDef(t, dir, "dead-two")

	mux := newTestHandler(t, db, dir, "")
	rr := get(t, mux, "/api/v2.0/indexers/all/results/torznab/api?t=search&q=test")
	require.Equal(t, http.StatusBadGateway, rr.Code,
		"want an outage reported when every indexer failed and nothing is stored; body: %s", rr.Body.String())
}

// TestTorznabHandler_Aggregate_NoDefinitions distinguishes "nothing is
// configured" from "nothing matched".
func TestTorznabHandler_Aggregate_NoDefinitions(t *testing.T) {
	db := newTestDB(t)
	mux := newTestHandler(t, db, t.TempDir(), "")

	rr := get(t, mux, "/api/v2.0/indexers/all/results/torznab/api?t=search&q=test")
	require.Equal(t, http.StatusServiceUnavailable, rr.Code,
		"want 503 with no definitions configured; body: %s", rr.Body.String())
}

// TestTorznabHandler_Aggregate_Pagination checks paging is applied to the
// merged result, not per indexer — the second page must continue the
// global order rather than restart in another tracker.
func TestTorznabHandler_Aggregate_Pagination(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newNamedIndexerDef(t, dir, "alpha")
	newNamedIndexerDef(t, dir, "beta")

	storeTorrent(t, db, database.Torrent{Name: "Test One", Published: "2024-03-04T00:00:00Z", Tracker: "alpha"})
	storeTorrent(t, db, database.Torrent{Name: "Test Two", Published: "2024-03-03T00:00:00Z", Tracker: "beta"})
	storeTorrent(t, db, database.Torrent{Name: "Test Three", Published: "2024-03-02T00:00:00Z", Tracker: "alpha"})
	storeTorrent(t, db, database.Torrent{Name: "Test Four", Published: "2024-03-01T00:00:00Z", Tracker: "beta"})

	mux := newTestHandler(t, db, dir, "")
	rr := get(t, mux, "/api/v2.0/indexers/all/results/torznab/api?t=search&q=test&limit=2&offset=2")
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	var feed torznab.Feed
	require.NoError(t, xml.Unmarshal(rr.Body.Bytes(), &feed), "failed to unmarshal feed")
	require.Equal(t, 4, feed.Channel.Response.Total, "the total must count every match across indexers")
	require.Len(t, feed.Channel.Items, 2)
	require.Equal(t, "Test Three", feed.Channel.Items[0].Title, "want the second global page")
	require.Equal(t, "Test Four", feed.Channel.Items[1].Title, "want the second global page")
}

// TestTorznabHandler_Aggregate_RequiresAPIKey checks the aggregate is not
// an unauthenticated way around the per-indexer endpoints' key check.
func TestTorznabHandler_Aggregate_RequiresAPIKey(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newNamedIndexerDef(t, dir, "alpha")

	mux := newTestHandler(t, db, dir, "secret")
	require.Equal(t, http.StatusUnauthorized, get(t, mux, "/api/v2.0/indexers/all/results/torznab/api?t=caps").Code,
		"the aggregate answered without an apikey")
	require.Equal(t, http.StatusOK, get(t, mux, "/api/v2.0/indexers/all/results/torznab/api?t=caps&apikey=secret").Code,
		"the aggregate refused a correct apikey")
}

// TestTorznabHandler_Aggregate_PanicIsOneFailedRequest is the client's side
// of panic containment: a tracker that panics leaves the aggregate
// answering like any unreachable one, rather than ending the process that
// was serving the request.
func TestTorznabHandler_Aggregate_PanicIsOneFailedRequest(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newNamedIndexerDef(t, dir, "alpha")
	newNamedIndexerDef(t, dir, "beta")

	logger := slog.New(slog.DiscardHandler)
	// A nil scraper panics on entry to every tracker's scrape, standing in
	// for a panic raised anywhere in extraction over what a tracker serves.
	handler := torznab.New(db, nil, scraper.NewDefinitionStore(dir, logger), "", logger)
	mux := http.NewServeMux()
	handler.Routes(mux)

	rr := get(t, mux, "/api/v2.0/indexers/all/results/torznab/api?t=search&q=example")

	// Nothing was scraped and the store holds nothing to fall back on, so
	// the request fails the way an unreachable indexer's does.
	require.Equal(t, http.StatusBadGateway, rr.Code, "body: %s", rr.Body.String())
	require.Contains(t, rr.Body.String(), "panic", "the failure did not name its cause")
}
