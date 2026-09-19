// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package torznab_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// These tests cover the endpoints that exist to be Jackett's: the URL
// space clients are configured with, and the indexer directory in
// Jackett's own JSON shape. What they pin is compatibility with software
// Jacklet cannot change, so they assert the wire bytes -- key spelling and
// capitalization included -- rather than round-tripping through Jacklet's
// own structs, which would agree with any spelling it chose.

// newDescribedDef writes a definition filled in well enough to exercise
// every field of Jackett's indexer DTO: two links, a language, a type, a
// description, and a category mapping to advertise.
func newDescribedDef(t *testing.T, dir, id string) {
	t.Helper()
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html></html>`))
	}))
	t.Cleanup(testServer.Close)

	def := fmt.Sprintf(`
id: %[1]s
name: Described Tracker
description: A tracker with every listed field filled in
language: en-US
type: private
links:
  - %[2]s/
  - https://mirror.invalid/
caps:
  categorymappings:
    - {id: 1, cat: Movies/HD}
  modes:
    search: [q]
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

// TestJackettIndexers_ServesJackettsOwnShape pins the response a consumer
// written against Jackett reads: a bare array, Jackett's lowercased and
// underscored keys, and capitalized "ID"/"Name" inside each capability.
// The casing is inconsistent because Jackett's is; a tidier spelling here
// is one a Jackett client would not find.
func TestJackettIndexers_ServesJackettsOwnShape(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newDescribedDef(t, dir, "described")
	mux := newTestHandler(t, db, dir, "")

	rr := get(t, mux, "/api/v2.0/indexers")
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	require.Equal(t, "application/json; charset=utf-8", rr.Header().Get("Content-Type"))

	// Decoded into []any rather than a struct: a struct would accept any
	// key spelling this package happens to use, which is the thing under
	// test.
	var body []map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body),
		"the response must be a bare JSON array, as Jackett's is")
	require.Len(t, body, 1)

	require.Equal(t, map[string]any{
		"alternativesitelinks": []any{"https://mirror.invalid/"},
		"caps":                 []any{map[string]any{"ID": "2040", "Name": "Movies/HD"}},
		"configured":           true,
		"description":          "A tracker with every listed field filled in",
		"id":                   "described",
		"language":             "en-US",
		"last_error":           "",
		"name":                 "Described Tracker",
		"potatoenabled":        false,
		"site_link":            body[0]["site_link"], // the test server's address
		"tags":                 []any{},
		"type":                 "private",
	}, body[0], "the entry must carry Jackett's fields, spelled Jackett's way")

	require.NotEmpty(t, body[0]["site_link"], "the definition's first link is the site link")
}

// TestJackettIndexers_OmitsNothingForAnEmptyDefinition covers the fields a
// sparse definition leaves blank. Jackett sends them as empty values, so a
// consumer reading one must not find null, which is what a nil slice
// would marshal to.
func TestJackettIndexers_OmitsNothingForAnEmptyDefinition(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newIndexerDef(t, dir)
	mux := newTestHandler(t, db, dir, "")

	rr := get(t, mux, "/api/v2.0/indexers")
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	got := rr.Body.String()
	require.NotContains(t, got, "null",
		"an absent list must be sent as [], not null; Jackett always sends a value")
	require.Contains(t, got, `"caps":[]`)
	require.Contains(t, got, `"alternativesitelinks":[]`)
	require.Contains(t, got, `"tags":[]`)
}

// TestJackettIndexers_OmitsTheAggregate keeps the aggregate out of the
// directory. It is an endpoint over the listed definitions rather than
// another definition alongside them, so a client walking this list to add
// every indexer would otherwise add all of them twice -- once on its own
// endpoint and again through "all".
func TestJackettIndexers_OmitsTheAggregate(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newNamedIndexerDef(t, dir, "alpha")
	newNamedIndexerDef(t, dir, "beta")
	mux := newTestHandler(t, db, dir, "")

	rr := get(t, mux, "/api/v2.0/indexers")
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	var body []struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	require.Len(t, body, 2, "both definitions must be listed")
	for _, indexer := range body {
		require.NotEqual(t, "all", indexer.ID, "the aggregate must not appear as an indexer")
	}
}

// TestJackettIndexers_AcceptsConfiguredFilter covers the parameter Jackett
// filters its list with. Every definition Jacklet loads is configured, so
// the answer is the same either way -- but a caller passing it must not be
// refused.
func TestJackettIndexers_AcceptsConfiguredFilter(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newIndexerDef(t, dir)
	mux := newTestHandler(t, db, dir, "")

	unfiltered := get(t, mux, "/api/v2.0/indexers")
	require.Equal(t, http.StatusOK, unfiltered.Code)

	filtered := get(t, mux, "/api/v2.0/indexers?configured=true")
	require.Equal(t, http.StatusOK, filtered.Code, "body: %s", filtered.Body.String())
	require.Equal(t, unfiltered.Body.String(), filtered.Body.String(),
		"every loaded definition is configured, so the filter changes nothing")
}

func TestJackettIndexers_RequiresTheAPIKey(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newIndexerDef(t, dir)
	mux := newTestHandler(t, db, dir, "secret")

	require.Equal(t, http.StatusUnauthorized, get(t, mux, "/api/v2.0/indexers").Code,
		"the directory names every configured tracker and must not be readable without the key")
	require.Equal(t, http.StatusOK, get(t, mux, "/api/v2.0/indexers?apikey=secret").Code)
}

// TestRoutes_ServesEveryJackettTorznabPath covers Jackett's own routing of
// the torznab action, which is "[action]/{ignored?}": the trailing segment
// is optional and its value means nothing. Clients send "api", but a
// client that sends the bare path or something else is one Jackett would
// have answered.
func TestRoutes_ServesEveryJackettTorznabPath(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newIndexerDef(t, dir)
	mux := newTestHandler(t, db, dir, "")

	for _, path := range []string{
		"/api/v2.0/indexers/" + testIndexerID + "/results/torznab?t=caps",
		"/api/v2.0/indexers/" + testIndexerID + "/results/torznab/api?t=caps",
		"/api/v2.0/indexers/" + testIndexerID + "/results/torznab/anything?t=caps",
	} {
		rr := get(t, mux, path)
		require.Equal(t, http.StatusOK, rr.Code, "%s: body: %s", path, rr.Body.String())
		require.Contains(t, rr.Body.String(), "<caps>", "%s must serve the caps document", path)
	}
}

// TestRoutes_AcceptsTrailingSlashes covers the form an operator is most
// likely to paste: Jackett's UI hands out its Torznab feed URL ending in
// a slash, and ASP.NET matches that as the same route. Go's "{ignored}"
// wildcard requires a non-empty segment, so without an explicit pattern
// for it the copied URL answers 404.
func TestRoutes_AcceptsTrailingSlashes(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newIndexerDef(t, dir)
	mux := newTestHandler(t, db, dir, "")

	for _, path := range []string{
		"/api/v2.0/indexers/",
		"/api/v2.0/indexers/" + testIndexerID + "/results/torznab/?t=caps",
	} {
		rr := get(t, mux, path)
		require.Equal(t, http.StatusOK, rr.Code, "%s: body: %s", path, rr.Body.String())
	}

	// The JSON endpoint reaches the tracker, which the test definition
	// answers with no rows, so it is enough that it routed at all.
	require.NotEqual(t, http.StatusNotFound,
		get(t, mux, "/api/v2.0/indexers/"+testIndexerID+"/results/?q=test").Code,
		"the JSON results endpoint must route with a trailing slash")
}

// TestRoutes_RejectsDeeperTorznabPaths keeps the trailing-segment
// wildcard from turning into a catch-all. Jackett's route takes one
// optional segment, so a deeper path is a 404 there too.
func TestRoutes_RejectsDeeperTorznabPaths(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newIndexerDef(t, dir)
	mux := newTestHandler(t, db, dir, "")

	require.Equal(t, http.StatusNotFound,
		get(t, mux, "/api/v2.0/indexers/"+testIndexerID+"/results/torznab/api/extra?t=caps").Code)
}
