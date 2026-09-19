// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package torznab_test

import (
	"encoding/json"
	"encoding/xml"
	"net/http"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// A Torznab client branches on the error code, not on the status: 100
// means "the key is wrong, stop retrying" where 900 means "something
// broke, try later". These tests pin the code each failure carries, which
// the status alone cannot express -- a 404 for an unknown indexer and a
// 404 for an unknown release are the same status and different answers.

// torznabErrorDocument is the error response, decoded by the names a
// client reads rather than through Jacklet's own type.
type torznabErrorDocument struct {
	XMLName     xml.Name `xml:"error"`
	Code        int      `xml:"code,attr"`
	Description string   `xml:"description,attr"`
}

// TestTorznabHandler_ErrorsAreTorznabDocuments covers the XML endpoint's
// failures, which Jackett reports as error documents carrying the code a
// client acts on.
func TestTorznabHandler_ErrorsAreTorznabDocuments(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newIndexerDef(t, dir)
	mux := newTestHandler(t, db, dir, "secret")

	base := "/api/v2.0/indexers/" + testIndexerID + "/results/torznab/api"

	tests := []struct {
		name       string
		path       string
		wantStatus int
		wantCode   int
	}{
		{
			name:       "a rejected apikey",
			path:       base + "?t=caps",
			wantStatus: http.StatusUnauthorized,
			// Jackett's code for a bad key. A client that retries on a
			// transport error must not retry on this one.
			wantCode: 100,
		},
		{
			name:       "an indexer with no definition",
			path:       "/api/v2.0/indexers/absent/results/torznab/api?t=caps&apikey=secret",
			wantStatus: http.StatusNotFound,
			wantCode:   201,
		},
		{
			name:       "an unrecognized operation",
			path:       base + "?t=nonsense&apikey=secret",
			wantStatus: http.StatusBadRequest,
			wantCode:   202,
		},
		{
			name:       "an operation this indexer does not offer",
			path:       base + "?t=indexers&apikey=secret",
			wantStatus: http.StatusBadRequest,
			wantCode:   203,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rr := get(t, mux, tc.path)
			require.Equal(t, tc.wantStatus, rr.Code, "body: %s", rr.Body.String())
			require.Equal(t, "application/xml; charset=utf-8", rr.Header().Get("Content-Type"))

			var doc torznabErrorDocument
			require.NoError(t, xml.Unmarshal(rr.Body.Bytes(), &doc),
				"want a Torznab error document, got: %s", rr.Body.String())
			require.Equal(t, tc.wantCode, doc.Code)
			require.NotEmpty(t, doc.Description, "an error document must say what went wrong")
		})
	}
}

// TestTorznabHandler_UnreachableIndexerIsAnErrorDocument covers the
// failure a client meets most often: the tracker is down and nothing is
// stored to answer from. Jackett reports any such failure under its
// catch-all code.
func TestTorznabHandler_UnreachableIndexerIsAnErrorDocument(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newUnreachableDef(t, dir)
	mux := newTestHandler(t, db, dir, "")

	rr := get(t, mux, "/api/v2.0/indexers/"+unreachableIndexerID+"/results/torznab/api?t=search&q=test")
	require.Equal(t, http.StatusBadGateway, rr.Code, "body: %s", rr.Body.String())

	var doc torznabErrorDocument
	require.NoError(t, xml.Unmarshal(rr.Body.Bytes(), &doc),
		"want a Torznab error document, got: %s", rr.Body.String())
	require.Equal(t, 900, doc.Code, "Jackett reports a failed search under its catch-all code")
}

// TestTorznabHandler_JSONErrorsStayJSON keeps the error document on the
// XML endpoint where it belongs. The JSON endpoint is Jackett's own and
// answers with its own shape, which has no code field, so a client
// decoding JSON must not suddenly be handed XML.
func TestTorznabHandler_JSONErrorsStayJSON(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newIndexerDef(t, dir)
	mux := newTestHandler(t, db, dir, "secret")

	rr := get(t, mux, "/api/v2.0/indexers/"+testIndexerID+"/results?q=test")
	require.Equal(t, http.StatusUnauthorized, rr.Code)
	require.Equal(t, "application/json; charset=utf-8", rr.Header().Get("Content-Type"))

	var body map[string]string
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body), "got: %s", rr.Body.String())
	require.NotEmpty(t, body["error"])
	require.NotContains(t, rr.Body.String(), "<error", "the JSON endpoint must not answer with XML")
}

// TestJackettIndexers_ErrorsStayJSON covers the directory endpoint, which
// is JSON and must fail in JSON like its sibling. The format is asserted
// rather than the status alone, which cannot tell the two apart.
func TestJackettIndexers_ErrorsStayJSON(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newIndexerDef(t, dir)
	mux := newTestHandler(t, db, dir, "secret")

	rr := get(t, mux, "/api/v2.0/indexers")
	require.Equal(t, http.StatusUnauthorized, rr.Code)
	require.Equal(t, "application/json; charset=utf-8", rr.Header().Get("Content-Type"))

	var body map[string]string
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body), "got: %s", rr.Body.String())
	require.NotEmpty(t, body["error"])
}

// TestTorznabHandler_DownloadErrorsStayPlain covers the third format: a
// torrent file is neither XML nor JSON, so there is no document the
// client was going to parse and the status carries the failure alone.
func TestTorznabHandler_DownloadErrorsStayPlain(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newIndexerDef(t, dir)
	mux := newTestHandler(t, db, dir, "secret")

	rr := get(t, mux, "/api/v2.0/indexers/"+testIndexerID+"/download/1")
	require.Equal(t, http.StatusUnauthorized, rr.Code)
	require.NotContains(t, rr.Body.String(), "<error", "a download failure is a status, not a document")
}

// unreachableIndexerID is the id of the definition newUnreachableDef
// writes.
const unreachableIndexerID = "test-unreachable"

// newUnreachableDef writes a definition pointed at a port nothing listens
// on, so every scrape of it fails and the handler has to answer from the
// store alone.
func newUnreachableDef(t *testing.T, dir string) {
	t.Helper()
	def := `
id: ` + unreachableIndexerID + `
name: ` + unreachableIndexerID + `
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
	require.NoError(t, os.WriteFile(dir+"/"+unreachableIndexerID+".yml", []byte(def), 0o600),
		"failed to write indexer definition")
}
