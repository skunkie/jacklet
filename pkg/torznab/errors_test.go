// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package torznab_test

import (
	"encoding/json"
	"encoding/xml"
	"net/http"
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
	Code        int      `xml:"code,attr"`
	Description string   `xml:"description,attr"`
	XMLName     xml.Name `xml:"error"`
}

// TestTorznabHandlerErrorsAreTorznabDocuments covers the XML endpoint's
// failures, which Jackett reports as error documents carrying the code a
// client acts on.
func TestTorznabHandlerErrorsAreTorznabDocuments(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newIndexerDef(t, dir)
	mux := newTestHandler(t, db, dir, "secret")

	base := "/api/v2.0/indexers/" + testIndexerID + "/results/torznab/api"

	tests := []struct {
		name       string
		path       string
		wantCode   int
		wantStatus int
	}{
		{
			// 100 is Jackett's code for a bad key. A client that retries on
			// a transport error must not retry on this one.
			name:       "a rejected apikey",
			path:       base + "?t=caps",
			wantCode:   100,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "an indexer with no definition",
			path:       "/api/v2.0/indexers/absent/results/torznab/api?t=caps&apikey=secret",
			wantCode:   201,
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "an unrecognized operation",
			path:       base + "?t=nonsense&apikey=secret",
			wantCode:   202,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "an operation this indexer does not offer",
			path:       base + "?t=indexers&apikey=secret",
			wantCode:   203,
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := get(t, mux, tc.path)
			require.Equal(t, tc.wantStatus, rec.Code, "body: %s", rec.Body.String())
			require.Equal(t, "application/xml; charset=utf-8", rec.Header().Get("Content-Type"))

			var doc torznabErrorDocument
			require.NoError(t, xml.Unmarshal(rec.Body.Bytes(), &doc),
				"want a Torznab error document, got: %s", rec.Body.String())
			require.Equal(t, tc.wantCode, doc.Code)
			require.NotEmpty(t, doc.Description, "an error document must say what went wrong")
		})
	}
}

// TestTorznabHandlerUnreachableIndexerIsAnErrorDocument covers the
// failure a client meets most often: the tracker is down and nothing is
// stored to answer from. Jackett reports any such failure under its
// catch-all code.
func TestTorznabHandlerUnreachableIndexerIsAnErrorDocument(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newUnreachableDef(t, dir, unreachableIndexerID)
	mux := newTestHandler(t, db, dir, "")

	rec := get(t, mux, "/api/v2.0/indexers/"+unreachableIndexerID+"/results/torznab/api?t=search&q=test")
	require.Equal(t, http.StatusBadGateway, rec.Code, "body: %s", rec.Body.String())

	var doc torznabErrorDocument
	require.NoError(t, xml.Unmarshal(rec.Body.Bytes(), &doc),
		"want a Torznab error document, got: %s", rec.Body.String())
	require.Equal(t, 900, doc.Code, "Jackett reports a failed search under its catch-all code")
	require.Equal(t, "Indexer "+unreachableIndexerID+" is unreachable", doc.Description,
		"the cause names the tracker's address and belongs in the log")
}

// TestTorznabHandlerJSONErrorsStayJSON keeps the error document on the
// XML endpoint where it belongs. The JSON endpoint is Jackett's own and
// answers with its own shape, which has no code field, so a client
// decoding JSON must not suddenly be handed XML.
func TestTorznabHandlerJSONErrorsStayJSON(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newIndexerDef(t, dir)
	mux := newTestHandler(t, db, dir, "secret")

	rec := get(t, mux, "/api/v2.0/indexers/"+testIndexerID+"/results?q=test")
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Equal(t, "application/json; charset=utf-8", rec.Header().Get("Content-Type"))

	var body map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), "got: %s", rec.Body.String())
	require.NotEmpty(t, body["error"])
	require.NotContains(t, rec.Body.String(), "<error", "the JSON endpoint must not answer with XML")
}

// TestJackettIndexersErrorsStayJSON covers the directory endpoint, which
// is JSON and must fail in JSON like its sibling. The format is asserted
// rather than the status alone, which cannot tell the two apart.
func TestJackettIndexersErrorsStayJSON(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newIndexerDef(t, dir)
	mux := newTestHandler(t, db, dir, "secret")

	rec := get(t, mux, "/api/v2.0/indexers")
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Equal(t, "application/json; charset=utf-8", rec.Header().Get("Content-Type"))

	var body map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), "got: %s", rec.Body.String())
	require.NotEmpty(t, body["error"])
}

// TestTorznabHandlerDownloadErrorsStayPlain covers the third format: a
// torrent file is neither XML nor JSON, so there is no document the
// client was going to parse and the status carries the failure alone.
func TestTorznabHandlerDownloadErrorsStayPlain(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newIndexerDef(t, dir)
	mux := newTestHandler(t, db, dir, "secret")

	rec := get(t, mux, "/api/v2.0/indexers/"+testIndexerID+"/download/1")
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.NotContains(t, rec.Body.String(), "<error", "a download failure is a status, not a document")
}
