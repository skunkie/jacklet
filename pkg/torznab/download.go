// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package torznab

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/torrplay/jacklet/pkg/database"
	"github.com/torrplay/jacklet/pkg/scraper"
)

// downloadTimeout bounds fetching one torrent file from a tracker,
// including any login it has to perform first.
const downloadTimeout = 60 * time.Second

// torrentContentType is what a .torrent file is served as.
const torrentContentType = "application/x-bittorrent"

// Download serves "GET /api/v2.0/indexers/{id}/download/{row}": the
// torrent file for one stored result, fetched from the tracker with
// Jacklet's own session.
//
// A Torznab client has no account on a private tracker, so handing it the
// tracker's link gives it a 403 or a login page saved as a ".torrent".
// Jackett proxies the fetch for the same reason.
//
// {row} is the store's own row id, not a URL: the endpoint resolves what
// to fetch from the database, so it cannot be pointed at an arbitrary
// address.
func (t *Torznab) Download(w http.ResponseWriter, r *http.Request) {
	// A torrent file is neither XML nor JSON, so a failure here is a
	// plain status: there is no document the client was going to parse.
	def, ok := t.findAuthorizedTracker(w, r, writePlainError)
	if !ok {
		return
	}

	rowID, err := strconv.ParseInt(r.PathValue("row"), 10, 64)
	if err != nil {
		http.Error(w, "Invalid row id", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), downloadTimeout)
	defer cancel()

	indexerID := scraper.TrackerID(def)
	// Scoped to the tracker in the path, so one indexer's endpoint cannot
	// be used to fetch another's row.
	row, err := t.store.Find(ctx, indexerID, rowID)
	if errors.Is(err, database.ErrNotFound) {
		http.Error(w, "No such result", http.StatusNotFound)
		return
	}
	if err != nil {
		t.logger.Error("failed to look up a download", "row", rowID, "error", err)
		http.Error(w, "Failed to look up the result", http.StatusInternalServerError)
		return
	}

	ServeTorrent(ctx, w, r, t.scrpr, def, row, t.logger)
}

// ServeTorrent writes row's torrent file to w, fetched from the tracker
// with scrpr's session, and named after the release for the client to
// save. A magnet is handed back as a redirect, since the client resolves
// one itself.
//
// The caller decides who may ask: this serves a row it is given, having
// nothing to say about how the request was authenticated. The admin panel
// and the Torznab API both arrive here after their own checks.
//
// A tracker that breaks off once part of the file has been sent panics
// with http.ErrAbortHandler, which is how net/http is told to drop the
// connection rather than close the response cleanly and leave the client
// holding a truncated .torrent it believes is whole. http.Server recovers
// that panic silently, so this must be served by one: recovery middleware
// of the caller's own has to re-panic on http.ErrAbortHandler rather than
// report it as a crash.
func ServeTorrent(ctx context.Context, w http.ResponseWriter, r *http.Request, scrpr *scraper.Scraper, def *scraper.Tracker, row database.Torrent, logger *slog.Logger) {
	// A magnet is handed back for the client to resolve; there is nothing
	// to fetch with the tracker's session.
	if magnet := magnetOf(row); magnet != "" {
		http.Redirect(w, r, magnet, http.StatusFound)
		return
	}
	if row.DownloadURL == "" {
		http.Error(w, "This result has no download link", http.StatusNotFound)
		return
	}

	indexerID := scraper.TrackerID(def)
	download, err := scrpr.Download(ctx, def, row.DownloadURL)
	if err != nil {
		logger.Error("failed to download a torrent", "indexer", indexerID, "row", row.ID, "error", err)
		if errors.Is(err, scraper.ErrForeignDownloadURL) {
			// The rejected host is the tracker's own text, so it stays in
			// the log rather than being reflected back to the client.
			http.Error(w, fmt.Sprintf("Refused to download from %s: the link leaves the tracker", indexerID),
				http.StatusBadRequest)
			return
		}
		http.Error(w, fmt.Sprintf("Failed to download from %s: %v", indexerID, err), http.StatusBadGateway)
		return
	}
	defer download.Body.Close()

	w.Header().Set("Content-Type", download.ContentType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment",
		map[string]string{"filename": torrentFilename(row.Name)}))
	written, err := io.Copy(w, download.Body)
	if err == nil {
		return
	}
	logger.Error("failed to stream a torrent", "indexer", indexerID, "row", row.ID, "error", err)
	if written == 0 {
		// Nothing reached the wire, so WriteHeader has not run yet and the
		// failure can still be reported as one. Returning here instead
		// would send an empty 200 carrying a .torrent filename, which the
		// client saves as a zero-byte torrent.
		w.Header().Del("Content-Disposition")
		http.Error(w, "Failed to download from "+indexerID, http.StatusBadGateway)
		return
	}
	// Part of the file is already on the wire, including when the tracker
	// ran past the download cap. Returning normally would close the
	// chunked response cleanly and the client would keep a truncated
	// .torrent as though it were whole, so the connection is broken
	// instead to make the transfer fail on the client's side too.
	panic(http.ErrAbortHandler)
}

// filenameUnsafe matches what must not reach a Content-Disposition
// filename: path separators, control characters, and the quoting
// characters the header itself uses.
var filenameUnsafe = regexp.MustCompile(`[^\p{L}\p{N} .,_+()\[\]-]`)

// torrentFilename turns a release title into a filename for the client to
// save. The title comes from a third-party page, so everything outside a
// conservative set is replaced rather than escaped.
func torrentFilename(name string) string {
	cleaned := strings.TrimSpace(filenameUnsafe.ReplaceAllString(name, "_"))
	cleaned = strings.Trim(cleaned, ".")
	if cleaned == "" {
		cleaned = "torrent"
	}
	if len(cleaned) > 150 {
		cleaned = strings.TrimSpace(cleaned[:150])
	}
	return cleaned + ".torrent"
}

// magnetOf returns the row's magnet URI, if it has one. The scraper keeps
// it in Magnet, and this package is importable, so a caller may have
// stored it as the download link instead.
func magnetOf(row database.Torrent) string {
	// The scheme is checked here as well as when the row was stored: a
	// redirect target that is not a magnet would send the caller wherever
	// a scraped page named, and this package is importable by a program
	// that fills rows its own way.
	for _, candidate := range []string{row.Magnet, row.DownloadURL} {
		if strings.HasPrefix(strings.ToLower(candidate), "magnet:") {
			return candidate
		}
	}
	return ""
}
