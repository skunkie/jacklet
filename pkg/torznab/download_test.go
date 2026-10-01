// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package torznab_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torrplay/jacklet/pkg/scraper"
)

// The proxy fetches the torrent with Jacklet's tracker session and streams
// it back, rather than handing the client a link it cannot authenticate.
func TestTorznabHandlerDownloadFetchesWithTheTrackerSession(t *testing.T) {
	const torrent = "d8:announce7:exampleeee"

	var gotCookie, gotUserAgent string
	tracker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			// A stand-in tracker handing out a session cookie, not a
			// cookie Jacklet issues to a browser.
			//nolint:gosec // G124: the test tracker, not Jacklet, sets this
			http.SetCookie(w, &http.Cookie{Name: "session", Path: "/", Value: "granted"})
			w.Write([]byte(`<html><body>ok</body></html>`))
			return
		}
		if c, err := r.Cookie("session"); err == nil {
			gotCookie = c.Value
		}
		gotUserAgent = r.UserAgent()
		w.Header().Set("Content-Type", "application/x-bittorrent")
		w.Write([]byte(torrent))
	}))
	defer tracker.Close()

	dir := t.TempDir()
	def := fmt.Sprintf(`
id: %[1]s
name: %[1]s
links:
  - %[2]s/
login:
  path: /login
  method: post
  inputs:
    username: user
    password: pass
search:
  paths:
    - path: "/"
  rows:
    selector: ".torrent_row"
  fields:
    title:
      selector: "a"
`, testIndexerID, tracker.URL)
	require.NoError(t, os.WriteFile(dir+"/"+testIndexerID+".yml", []byte(def), 0o600),
		"failed to write indexer definition")

	db := newTestDB(t)
	rowID := storeTorrent(t, db, scraper.Torrent{
		DownloadURL: tracker.URL + "/download/1",
		Name:        "Some/Release: 2024",
	})

	rec := httptest.NewRecorder()
	newTestHandler(t, db, dir, "").ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/api/v2.0/indexers/%s/download/%d", testIndexerID, rowID), http.NoBody))

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	require.Equal(t, torrent, rec.Body.String(), "want the torrent streamed back verbatim")
	require.Equal(t, "granted", gotCookie, "want the session cookie login established")
	require.Contains(t, gotUserAgent, "Mozilla/5.0", "want a browser user agent")
	require.Equal(t, "application/x-bittorrent", rec.Header().Get("Content-Type"))

	// The filename comes from a third-party page, so path separators and
	// punctuation must not reach it.
	cd := rec.Header().Get("Content-Disposition")
	require.Contains(t, cd, `filename=`)
	require.NotContains(t, cd, "/", "a path separator reached the filename")
}

// A tracker that has lost the session answers with a login page rather
// than a 401. Saving that as a ".torrent" is the failure this endpoint
// exists to avoid, so it is reported instead.
func TestTorznabHandlerDownloadRejectsAWebPage(t *testing.T) {
	tracker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<html><body>Please log in</body></html>`))
	}))
	defer tracker.Close()

	dir := t.TempDir()
	def := fmt.Sprintf("id: %[1]s\nname: %[1]s\nlinks:\n  - %[2]s/\nsearch:\n  paths:\n    - path: \"/\"\n  rows:\n    selector: .r\n  fields:\n    title:\n      selector: a\n",
		testIndexerID, tracker.URL)
	require.NoError(t, os.WriteFile(dir+"/"+testIndexerID+".yml", []byte(def), 0o600),
		"failed to write indexer definition")

	db := newTestDB(t)
	rowID := storeTorrent(t, db, scraper.Torrent{
		DownloadURL: tracker.URL + "/download/1",
		Name:        "Release",
	})

	rec := httptest.NewRecorder()
	newTestHandler(t, db, dir, "").ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/api/v2.0/indexers/%s/download/%d", testIndexerID, rowID), http.NoBody))

	require.Equal(t, http.StatusBadGateway, rec.Code, "body: %s", rec.Body.String())
}

// The endpoint takes a row id, never a URL, so it cannot be turned into an
// open proxy. A row whose link points somewhere other than the tracker is
// refused rather than fetched with that tracker's credentials.
func TestTorznabHandlerDownloadRefusesAForeignLink(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newIndexerDef(t, dir)

	rowID := storeTorrent(t, db, scraper.Torrent{
		DownloadURL: "http://attacker.example/steal",
		Name:        "Elsewhere",
	})

	mux := newTestHandler(t, db, dir, "")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/api/v2.0/indexers/%s/download/%d", testIndexerID, rowID), http.NoBody))
	require.Equal(t, http.StatusBadRequest, rec.Code, "foreign link: %s", rec.Body.String())

	// An unknown row is a 404, not an attempt to fetch anything.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/api/v2.0/indexers/"+testIndexerID+"/download/999999", http.NoBody))
	require.Equal(t, http.StatusNotFound, rec.Code, "unknown row")
}

// A tracker cannot turn its download proxy into a redirect-following open
// proxy: redirects to another host are rejected before the response is
// returned to the client.
func TestTorznabHandlerDownloadRefusesForeignRedirect(t *testing.T) {
	var targetRequests int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetRequests++
		w.Header().Set("Content-Type", "application/x-bittorrent")
		_, _ = w.Write([]byte("not a tracker torrent"))
	}))
	defer target.Close()

	tracker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/download/1" {
			http.Redirect(w, r, target.URL+"/secret", http.StatusFound)
			return
		}
		w.Write([]byte(`<html></html>`))
	}))
	defer tracker.Close()

	dir := t.TempDir()
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
`, testIndexerID, tracker.URL)
	require.NoError(t, os.WriteFile(dir+"/"+testIndexerID+".yml", []byte(def), 0o600),
		"failed to write indexer definition")

	db := newTestDB(t)
	rowID := storeTorrent(t, db, scraper.Torrent{
		DownloadURL: tracker.URL + "/download/1",
		Name:        "Redirected Release",
	})

	rec := httptest.NewRecorder()
	newTestHandler(t, db, dir, "").ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/api/v2.0/indexers/%s/download/%d", testIndexerID, rowID), http.NoBody))

	require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	require.Zero(t, targetRequests, "the foreign redirect target was requested")
}

// The download endpoint is behind the same apikey as every other one.
func TestTorznabHandlerDownloadRequiresTheAPIKey(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newIndexerDef(t, dir)

	rec := httptest.NewRecorder()
	newTestHandler(t, db, dir, "secret").ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/api/v2.0/indexers/"+testIndexerID+"/download/1", http.NoBody))
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

// A tracker that breaks off mid-download must not look like a success to
// the client: a .torrent is saved to disk, so an empty or truncated file
// arriving under a 200 is the failure that gets noticed only later, when
// the torrent client rejects it.
func TestTorznabHandlerDownloadFailureIsNotAQuietSuccess(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write int
	}{
		{name: "before any bytes", write: 0},
		{name: "part way through", write: 32},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tracker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/download/1" {
					_, _ = w.Write([]byte(`<html></html>`))
					return
				}
				// Promising more than is delivered and then dropping the
				// connection is how a tracker fails part way through.
				w.Header().Set("Content-Length", "4096")
				w.Header().Set("Content-Type", "application/x-bittorrent")
				// The headers are flushed first so the response itself
				// succeeds and only the body fails, which is what puts the
				// failure inside the proxy's copy rather than its request.
				w.WriteHeader(http.StatusOK)
				if tc.write > 0 {
					_, _ = w.Write(make([]byte, tc.write))
				}
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
				panic(http.ErrAbortHandler)
			}))
			defer tracker.Close()

			dir := t.TempDir()
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
`, testIndexerID, tracker.URL)
			require.NoError(t, os.WriteFile(dir+"/"+testIndexerID+".yml", []byte(def), 0o600),
				"failed to write indexer definition")

			db := newTestDB(t)
			rowID := storeTorrent(t, db, scraper.Torrent{
				DownloadURL: tracker.URL + "/download/1",
				Name:        "Interrupted Release",
			})

			jacklet := httptest.NewServer(newTestHandler(t, db, dir, ""))
			defer jacklet.Close()

			resp, err := jacklet.Client().Get(fmt.Sprintf("%s/api/v2.0/indexers/%s/download/%d", jacklet.URL, testIndexerID, rowID))
			if err != nil {
				// The connection was broken after the headers went out,
				// which is the truncated case reported as a failure.
				return
			}
			defer resp.Body.Close()
			body, readErr := io.ReadAll(resp.Body)
			require.False(t, resp.StatusCode == http.StatusOK && readErr == nil,
				"a failed download returned 200 with %d bytes and no error", len(body))
		})
	}
}
