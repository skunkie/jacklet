// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package torznab_test

import (
	"encoding/xml"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// aggregateIndexersPath is the aggregate's Torznab endpoint asking for the
// indexers behind it.
const aggregateIndexersPath = "/api/v2.0/indexers/all/results/torznab/api?t=indexers"

// indexerListDocument mirrors the response Jackett serves for
// "t=indexers", decoded by the element and attribute names a client reads
// rather than through Jacklet's own types.
type indexerListDocument struct {
	XMLName  xml.Name `xml:"indexers"`
	Indexers []struct {
		ID          string `xml:"id,attr"`
		Configured  bool   `xml:"configured,attr"`
		Title       string `xml:"title"`
		Description string `xml:"description"`
		Link        string `xml:"link"`
		Language    string `xml:"language"`
		Type        string `xml:"type"`
		Caps        struct {
			Categories struct {
				Category []struct {
					ID   int    `xml:"id,attr"`
					Name string `xml:"name,attr"`
				} `xml:"category"`
			} `xml:"categories"`
		} `xml:"caps"`
	} `xml:"indexer"`
}

// TestTorznabHandler_IndexerList covers "t=indexers" on the aggregate: the
// client added one endpoint and asks what is behind it, which is the whole
// reason Jackett offers this on a meta indexer.
func TestTorznabHandler_IndexerList(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newDescribedDef(t, dir, "described")
	newNamedIndexerDef(t, dir, "plain")
	mux := newTestHandler(t, db, dir, "")

	rr := get(t, mux, aggregateIndexersPath)
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	require.Equal(t, "application/xml; charset=utf-8", rr.Header().Get("Content-Type"))

	var doc indexerListDocument
	require.NoError(t, xml.Unmarshal(rr.Body.Bytes(), &doc), "body: %s", rr.Body.String())
	require.Len(t, doc.Indexers, 2, "every definition behind the aggregate must be listed")

	byID := map[string]int{}
	for i, indexer := range doc.Indexers {
		byID[indexer.ID] = i
	}
	require.Contains(t, byID, "described")
	require.Contains(t, byID, "plain")

	described := doc.Indexers[byID["described"]]
	require.True(t, described.Configured)
	require.Equal(t, "Described Tracker", described.Title)
	require.Equal(t, "A tracker with every listed field filled in", described.Description)
	require.Equal(t, "en-US", described.Language)
	require.Equal(t, "private", described.Type)
	require.NotEmpty(t, described.Link, "the entry must carry the tracker's own site link")

	// The nested caps are the definition's own, not the aggregate's
	// merged set: the point of the list is what each indexer can do
	// alone. "plain" declares no category mappings, so a merged set would
	// have handed it the other's.
	require.Len(t, described.Caps.Categories.Category, 1)
	require.Equal(t, 2040, described.Caps.Categories.Category[0].ID)
	require.Empty(t, doc.Indexers[byID["plain"]].Caps.Categories.Category,
		"a definition with no categories must not inherit another's")
}

// TestTorznabHandler_IndexerList_RejectsASingleIndexer pins Jackett's rule
// that this function belongs to a meta indexer. A client asking a plain
// indexer gets error 203 rather than a list of one, which is what it is
// written to handle.
func TestTorznabHandler_IndexerList_RejectsASingleIndexer(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newIndexerDef(t, dir)
	mux := newTestHandler(t, db, dir, "")

	rr := get(t, mux, "/api/v2.0/indexers/"+testIndexerID+"/results/torznab/api?t=indexers")
	require.Equal(t, http.StatusBadRequest, rr.Code)

	var doc struct {
		XMLName     xml.Name `xml:"error"`
		Code        int      `xml:"code,attr"`
		Description string   `xml:"description,attr"`
	}
	require.NoError(t, xml.Unmarshal(rr.Body.Bytes(), &doc),
		"the failure must be a Torznab error document: %s", rr.Body.String())
	require.Equal(t, 203, doc.Code, "want Jackett's code for a function this indexer does not offer")
	require.NotEmpty(t, doc.Description)
}

// TestTorznabHandler_IndexerList_FiltersOnConfigured covers the parameter
// Jackett filters with. Every definition Jacklet loaded is configured, so
// asking for the unconfigured ones is a well-formed request with an empty
// answer.
func TestTorznabHandler_IndexerList_FiltersOnConfigured(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newIndexerDef(t, dir)
	mux := newTestHandler(t, db, dir, "")

	var configured indexerListDocument
	rr := get(t, mux, aggregateIndexersPath+"&configured=true")
	require.Equal(t, http.StatusOK, rr.Code)
	require.NoError(t, xml.Unmarshal(rr.Body.Bytes(), &configured))
	require.Len(t, configured.Indexers, 1, "every loaded definition is configured")

	var unconfigured indexerListDocument
	rr = get(t, mux, aggregateIndexersPath+"&configured=false")
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	require.NoError(t, xml.Unmarshal(rr.Body.Bytes(), &unconfigured))
	require.Empty(t, unconfigured.Indexers, "no loaded definition is unconfigured")
}

func TestTorznabHandler_IndexerList_RequiresTheAPIKey(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newIndexerDef(t, dir)
	mux := newTestHandler(t, db, dir, "secret")

	require.Equal(t, http.StatusUnauthorized, get(t, mux, aggregateIndexersPath).Code,
		"the list names every configured tracker and must not be readable without the key")
	require.Equal(t, http.StatusOK, get(t, mux, aggregateIndexersPath+"&apikey=secret").Code)
}
