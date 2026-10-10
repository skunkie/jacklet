// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package torznab_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torrplay/jacklet/pkg/scraper"
	"github.com/torrplay/jacklet/pkg/torznab"
)

// The proxy fetches the torrent with Jacklet's tracker session and streams
// it back, rather than handing the client a link it cannot authenticate.
func TestTorznabHandlerDownloadFetchesWithTheTrackerSession(t *testing.T) {
	const torrent = "d8:announce7:examplee"

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
		// A type a browser could render or sniff, which is not relayed.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
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
	require.Equal(t, "application/x-bittorrent", rec.Header().Get("Content-Type"),
		"the tracker's own content type was relayed")
	require.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"),
		"a torrent file is served without forbidding a browser to sniff another type")

	// The filename comes from a third-party page, so path separators and
	// punctuation must not reach it.
	cd := rec.Header().Get("Content-Disposition")
	require.Contains(t, cd, `filename=`)
	require.NotContains(t, cd, "/", "a path separator reached the filename")
}

// A tracker that has lost the session answers with a login page rather
// than a 401, and one refusing a download may answer with an error in
// plain text or JSON, or with too little to be a torrent file. Saving any
// of them as a ".torrent" is the failure this endpoint exists to avoid, so
// each is reported instead. A magnet body that cannot be read whole is
// reported the same way rather than redirected to in part.
func TestTorznabHandlerDownloadRejectsABodyThatIsNotATorrent(t *testing.T) {
	for _, tc := range []struct {
		name, body, contentType string
		isCutShort              bool
	}{
		{name: "web page", body: `<html><body>Please log in</body></html>`, contentType: "text/html; charset=utf-8"},
		{name: "plain text error", body: "Invalid passkey", contentType: "text/plain"},
		{name: "json error", body: `{"error":"invalid passkey"}`, contentType: "application/json"},
		{name: "empty body", body: "", contentType: "application/x-bittorrent"},
		// An empty dictionary is bencoded, and still not a torrent file.
		{name: "too short", body: "de", contentType: "application/x-bittorrent"},
		{name: "magnet cut short", body: "magnet:?xt=urn:btih:0123", contentType: "text/plain", isCutShort: true},
		{name: "magnet too long", body: "magnet:?xt=urn:btih:0123&dn=" + strings.Repeat("a", 9<<10), contentType: "text/plain"},
		{name: "magnet one byte over the cap", body: sampleMagnetOfLength(torznab.MaxMagnetBytes+1) + "\n", contentType: "text/plain"},
		{name: "magnet with a control character", body: "magnet:?xt=urn:btih:0123\x00junk\n", contentType: "text/plain"},
		{name: "magnet with no exact topic", body: "magnet: link unavailable, please log in\n", contentType: "text/plain"},
		// Starting as a torrent file does is not enough: the whole file has
		// to parse, as it does for Jackett.
		{name: "torrent cut short", body: "d8:announce7:exam", contentType: "application/x-bittorrent"},
		{name: "torrent with a duplicate key", body: "d4:name4:test4:name4:teste", contentType: "application/x-bittorrent"},
		{name: "larger than the cap", body: "d" + strings.Repeat("x", 1<<20), contentType: "application/x-bittorrent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := downloadFrom(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				if tc.isCutShort {
					// The connection closes before the length promised
					// arrives, so reading the body fails partway.
					w.Header().Set("Content-Length", strconv.Itoa(len(tc.body)+100))
				}
				w.Write([]byte(tc.body))
			}, "")

			require.Equal(t, http.StatusBadGateway, rec.Code, "body: %s", rec.Body.String())
			require.Empty(t, rec.Header().Get("Content-Disposition"),
				"the answer was offered to the client to save as a torrent file")
			require.Empty(t, rec.Header().Get("Location"), "the client was redirected to part of a magnet")
		})
	}
}

// A tracker's download link can answer with a magnet link as its body
// rather than a torrent file, which is handed to the client as a stored
// magnet is, as Jackett's download endpoint does. Its scheme is matched in
// any case, as a stored magnet's is, where Jackett matches only lowercase.
// Only the first line is read, so what follows it need not arrive whole.
func TestTorznabHandlerDownloadRedirectsToAMagnetTheTrackerAnswersWith(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		isCutShort               bool
		leading, lineEnd, magnet string
	}{
		{name: "lowercase scheme", lineEnd: "\n", magnet: "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"},
		{name: "uppercase scheme", lineEnd: "\n", magnet: "MAGNET:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"},
		{name: "at the length cap", lineEnd: "\r\n", magnet: sampleMagnetOfLength(torznab.MaxMagnetBytes)},
		{name: "connection drops after the line", isCutShort: true, lineEnd: "\n", magnet: "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"},
		{name: "no line break", magnet: "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"},
		{name: "after leading space", leading: "\uFEFF\n ", lineEnd: "\n", magnet: "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A line break is followed by more text, which is not the link.
			body := tc.leading + tc.magnet
			if tc.lineEnd != "" {
				body += tc.lineEnd + "<!-- served by node 3 -->\n"
			}
			rec := downloadFrom(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/plain")
				if tc.isCutShort {
					// The connection closes before the length promised
					// arrives, after the magnet's line has been sent.
					w.Header().Set("Content-Length", strconv.Itoa(len(body)+100))
				}
				w.Write([]byte(body))
			}, "")

			require.Equal(t, http.StatusFound, rec.Code, "body: %s", rec.Body.String())
			require.Equal(t, tc.magnet, rec.Header().Get("Location"), "the text after the magnet's line reached the redirect")
		})
	}
}

// downloadFrom requests the download of a stored row whose link is on a
// test tracker answering with answer, and which carries storedMagnet as
// its magnet, and returns the response.
func downloadFrom(t *testing.T, answer http.HandlerFunc, storedMagnet string) *httptest.ResponseRecorder {
	t.Helper()
	tracker := httptest.NewServer(answer)
	t.Cleanup(tracker.Close)

	dir := t.TempDir()
	writeTrackerDef(t, dir, tracker.URL)

	db := newTestDB(t)
	rowID := storeTorrent(t, db, scraper.Torrent{
		DownloadURL: tracker.URL + "/download/1",
		Magnet:      storedMagnet,
		Name:        "Release",
	})

	rec := httptest.NewRecorder()
	newTestHandler(t, db, dir, "").ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/api/v2.0/indexers/%s/download/%d", testIndexerID, rowID), http.NoBody))
	return rec
}

// writeTrackerDef writes a definition for testIndexerID whose site is
// trackerURL into dir.
func writeTrackerDef(t *testing.T, dir, trackerURL string) {
	t.Helper()
	def := fmt.Sprintf("id: %[1]s\nname: %[1]s\nlinks:\n  - %[2]s/\nsearch:\n  paths:\n    - path: \"/\"\n  rows:\n    selector: .r\n  fields:\n    title:\n      selector: a\n",
		testIndexerID, trackerURL)
	require.NoError(t, os.WriteFile(dir+"/"+testIndexerID+".yml", []byte(def), 0o600),
		"failed to write indexer definition")
}

// A download the client is still served through the row's magnet is
// logged as a warning, and only one the client is refused as an error, so
// error-level logs mean a client went without.
func TestServeTorrent_LogsAFallbackAsAWarning(t *testing.T) {
	const magnet = "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"
	for _, tc := range []struct {
		name, storedMagnet string
		wantLevel          string
	}{
		{name: "served through the magnet", storedMagnet: magnet, wantLevel: slog.LevelWarn.String()},
		{name: "refused", wantLevel: slog.LevelError.String()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tracker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/plain")
				w.Write([]byte("Invalid passkey"))
			}))
			defer tracker.Close()
			dir := t.TempDir()
			writeTrackerDef(t, dir, tracker.URL)

			var logged bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logged, nil))
			def, err := scraper.NewDefinitionStore(dir, logger).Find(testIndexerID)
			require.NoError(t, err)
			row := scraper.Torrent{DownloadURL: tracker.URL + "/download/1", ID: 1, Magnet: tc.storedMagnet, Name: "Sample Release"}
			torznab.ServeTorrent(t.Context(), httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", http.NoBody),
				scraper.New(scraper.NewConfigStore(""), "", logger), def, row, logger)

			var levels []string
			for line := range strings.Lines(logged.String()) {
				var record struct {
					Indexer string `json:"indexer"`
					Level   string `json:"level"`
				}
				require.NoError(t, json.Unmarshal([]byte(line), &record))
				if record.Indexer == testIndexerID {
					levels = append(levels, record.Level)
				}
			}
			require.Equal(t, []string{tc.wantLevel}, levels, "the download's outcome was logged at the wrong level")
		})
	}
}

// A tracker's script can emit whitespace or a byte order mark ahead of the
// torrent file, which is served without them, as the file it precedes.
func TestTorznabHandlerDownloadServesATorrentAfterLeadingSpace(t *testing.T) {
	const torrent = "d8:announce7:examplee"
	rec := downloadFrom(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-bittorrent")
		w.Write([]byte("\uFEFF\r\n \t" + torrent))
	}, "")

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	require.Equal(t, torrent, rec.Body.String(), "the leading whitespace reached the torrent file")
}

// A torrent file is served as Jackett serves it, encoded again with its
// dictionary keys sorted, since Sonarr refuses one whose keys are not, and
// without whatever the tracker sent after it.
func TestTorznabHandlerDownloadServesATorrentWithSortedKeys(t *testing.T) {
	rec := downloadFrom(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-bittorrent")
		w.Write([]byte("d4:infod4:name4:test6:lengthi5ee8:announce3:urle\n"))
	}, "")

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	require.Equal(t, "d8:announce3:url4:infod6:lengthi5e4:name4:testee", rec.Body.String())
	require.Equal(t, strconv.Itoa(rec.Body.Len()), rec.Header().Get("Content-Length"),
		"the file went out without its length")
}

// A row that links to a torrent file is served the file, as its proxied
// link in the feed promises, whatever magnet it also carries: a client
// wanting the magnet reads it from the feed.
func TestTorznabHandlerDownloadServesTheTorrentFileOfARowWithAMagnet(t *testing.T) {
	const torrent = "d8:announce7:examplee"
	rec := downloadFrom(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-bittorrent")
		w.Write([]byte(torrent))
	}, "magnet:?dn=Sample+Release")

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	require.Equal(t, torrent, rec.Body.String(), "the row's magnet was preferred to its torrent file")
}

// A row whose torrent file cannot be fetched or is not one is handed the
// magnet it also carries, which the client resolves without the tracker.
func TestTorznabHandlerDownloadFallsBackToTheMagnetOfARowWhoseTorrentFails(t *testing.T) {
	const magnet = "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"
	for _, tc := range []struct {
		name   string
		answer http.HandlerFunc
	}{
		{name: "tracker error", answer: func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		}},
		{name: "not a torrent file", answer: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte("Invalid passkey"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := downloadFrom(t, tc.answer, magnet)

			require.Equal(t, http.StatusFound, rec.Code, "body: %s", rec.Body.String())
			require.Equal(t, magnet, rec.Header().Get("Location"), "the row's magnet was not offered in place of its torrent file")
		})
	}
}

// A download link that is a magnet the redirect cannot carry falls back to
// the row's own magnet, as a torrent file that cannot be served does.
func TestTorznabHandlerDownloadFallsBackToTheMagnetOfARowWhoseMagnetLinkFails(t *testing.T) {
	const magnet = "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"
	db := newTestDB(t)
	dir := t.TempDir()
	newIndexerDef(t, dir)
	rowID := storeTorrent(t, db, scraper.Torrent{
		DownloadURL: "magnet:?dn=Sample+Release",
		Magnet:      magnet,
		Name:        "Sample Release",
	})

	rec := httptest.NewRecorder()
	newTestHandler(t, db, dir, "").ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/api/v2.0/indexers/%s/download/%d", testIndexerID, rowID), http.NoBody))

	require.Equal(t, http.StatusFound, rec.Code, "body: %s", rec.Body.String())
	require.Equal(t, magnet, rec.Header().Get("Location"), "the row's magnet was not offered in place of its download link")
}

// A stored magnet the feed already hands out is redirected to at any
// length: the cap bounds only how much of a tracker's answer is read as a
// magnet.
func TestTorznabHandlerDownloadRedirectsToALongStoredMagnet(t *testing.T) {
	magnet := sampleMagnetOfLength(torznab.MaxMagnetBytes + 1)
	db := newTestDB(t)
	dir := t.TempDir()
	newIndexerDef(t, dir)
	rowID := storeTorrent(t, db, scraper.Torrent{Magnet: magnet, Name: "Sample Release"})

	rec := httptest.NewRecorder()
	newTestHandler(t, db, dir, "").ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/api/v2.0/indexers/%s/download/%d", testIndexerID, rowID), http.NoBody))

	require.Equal(t, http.StatusFound, rec.Code, "body: %s", rec.Body.String())
	require.Equal(t, magnet, rec.Header().Get("Location"))
}

// A stored magnet is redirected to only when the redirect can carry it: a
// control character would reach the client in Location, and a link naming
// no exact topic resolves to nothing. No tracker was asked, so the result
// is reported as carrying no usable link rather than as a tracker failure.
func TestTorznabHandlerDownloadRefusesAStoredMagnetItCannotRedirectTo(t *testing.T) {
	for _, tc := range []struct{ name, magnet string }{
		{name: "control character", magnet: "magnet:?xt=urn:btih:0123\tjunk"},
		{name: "no exact topic", magnet: "magnet:?dn=Sample+Release"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := newTestDB(t)
			dir := t.TempDir()
			newIndexerDef(t, dir)
			rowID := storeTorrent(t, db, scraper.Torrent{Magnet: tc.magnet, Name: "Sample Release"})

			rec := httptest.NewRecorder()
			newTestHandler(t, db, dir, "").ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
				fmt.Sprintf("/api/v2.0/indexers/%s/download/%d", testIndexerID, rowID), http.NoBody))

			require.Equal(t, http.StatusNotFound, rec.Code, "body: %s", rec.Body.String())
			require.Empty(t, rec.Header().Get("Location"), "the client was redirected to a magnet it cannot use")
		})
	}
}

// sampleMagnetOfLength is a magnet link exactly length bytes long.
func sampleMagnetOfLength(length int) string {
	const prefix = "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&dn="
	return prefix + strings.Repeat("a", length-len(prefix))
}

// A definition's download block can lead from the stored details page to a
// magnet, which is handed to the client as a stored magnet is, and refused
// as a tracker failure when it names no exact topic to resolve.
func TestTorznabHandlerDownloadRedirectsToAMagnetTheTrackerLeadsTo(t *testing.T) {
	const magnet = "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"
	for _, tc := range []struct {
		name, magnet, wantLocation string
		wantStatus                 int
	}{
		{name: "well formed", magnet: magnet, wantLocation: magnet, wantStatus: http.StatusFound},
		{name: "no exact topic", magnet: "magnet:?dn=Sample+Release", wantStatus: http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tracker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Write([]byte(`<html><body><a class="magnet" href="` + tc.magnet + `">Magnet</a></body></html>`))
			}))
			defer tracker.Close()

			dir := t.TempDir()
			def := fmt.Sprintf("id: %[1]s\nname: %[1]s\nlinks:\n  - %[2]s/\ndownload:\n  selectors:\n    - selector: a.magnet\n      attribute: href\nsearch:\n  paths:\n    - path: \"/\"\n  rows:\n    selector: .r\n  fields:\n    title:\n      selector: a\n",
				testIndexerID, tracker.URL)
			require.NoError(t, os.WriteFile(dir+"/"+testIndexerID+".yml", []byte(def), 0o600),
				"failed to write indexer definition")

			db := newTestDB(t)
			rowID := storeTorrent(t, db, scraper.Torrent{
				DownloadURL: tracker.URL + "/details/1",
				Name:        "Release",
			})

			rec := httptest.NewRecorder()
			newTestHandler(t, db, dir, "").ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
				fmt.Sprintf("/api/v2.0/indexers/%s/download/%d", testIndexerID, rowID), http.NoBody))

			require.Equal(t, tc.wantStatus, rec.Code, "body: %s", rec.Body.String())
			require.Equal(t, tc.wantLocation, rec.Header().Get("Location"))
		})
	}
}

// The endpoint takes a row id, never a URL, so it cannot be turned into an
// open proxy. A row whose link points somewhere other than the tracker is
// refused rather than fetched with that tracker's credentials, and the
// client is told so even when the row also carries a magnet.
func TestTorznabHandlerDownloadRefusesAForeignLink(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newIndexerDef(t, dir)

	rowID := storeTorrent(t, db, scraper.Torrent{
		DownloadURL: "http://attacker.example/steal",
		Magnet:      "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567",
		Name:        "Elsewhere",
	})

	mux := newTestHandler(t, db, dir, "")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/api/v2.0/indexers/%s/download/%d", testIndexerID, rowID), http.NoBody))
	require.Equal(t, http.StatusBadRequest, rec.Code, "foreign link: %s", rec.Body.String())
	require.Empty(t, rec.Header().Get("Location"), "the refused link was served around through the row's magnet")

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
// the torrent client rejects it. The file is read whole before anything
// is sent, so the failure is answered as one.
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
					_, _ = w.Write([]byte(strings.Repeat("d8:announce", tc.write))[:tc.write])
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
			require.NoError(t, err)
			defer resp.Body.Close()
			require.Equal(t, http.StatusBadGateway, resp.StatusCode, "a failed download was not answered as one")
		})
	}
}

// The cause of a failed download names the tracker's address, which may
// carry the passkey in a shape no redaction recognizes, so the client is
// told only which indexer failed.
func TestTorznabHandlerDownloadFailureNamesNoAddress(t *testing.T) {
	tracker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			_, _ = w.Write([]byte(`<html></html>`))
			return
		}
		// Hanging up before answering is how a reset connection fails.
		if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
			_ = conn.Close()
		}
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
		DownloadURL: tracker.URL + "/download/SAMPLE+KEY/file.torrent?passkey=SAMPLEKEY",
		Name:        "Unreachable Release",
	})

	rec := httptest.NewRecorder()
	newTestHandler(t, db, dir, "").ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/api/v2.0/indexers/%s/download/%d", testIndexerID, rowID), http.NoBody))

	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Equal(t, "Failed to download from "+testIndexerID+"\n", rec.Body.String())
}
