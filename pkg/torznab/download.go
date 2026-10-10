// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package torznab

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/torrplay/jacklet/pkg/scraper"
)

// downloadTimeout bounds fetching one torrent file from a tracker,
// including any login it has to perform first.
const downloadTimeout = 60 * time.Second

// torrentContentType is what a .torrent file is served as.
const torrentContentType = "application/x-bittorrent"

// maxMagnetBytes bounds a magnet a tracker answers a download with as its
// body, which is read whole before it is sent back as the Location header,
// and common servers and clients refuse a header block much past 8 KiB,
// which the response's other headers share.
const maxMagnetBytes = 4 << 10

// Download serves "GET /api/v2.0/indexers/{id}/download/{row}": the
// torrent file for one stored result, fetched from the tracker with
// Jacklet's own session.
//
// A Torznab client has no account on a private tracker, so handing it the
// tracker's link gives it a 403 or a login page saved as a ".torrent".
// Jackett proxies the fetch for the same reason.
//
// {row} is the store's own row id, not a URL: the endpoint resolves what
// to fetch from the catalog, so it cannot be pointed at an arbitrary
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
	row, err := t.catalog.Find(ctx, indexerID, rowID)
	if errors.Is(err, scraper.ErrNotFound) {
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
// A download that cannot be served, because the tracker fails, answers
// with something other than a torrent file, or leads to a magnet that
// cannot be redirected to, falls back to a redirect to the row's own
// magnet when it carries one that can be, rather than answering 404 or
// 502. That fallback is logged as a warning, and only a download the
// client is refused as an error. A link that leaves the tracker is the
// exception, answered 400 whatever magnet the row carries, so the client
// is told its link was refused rather than served around it.
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
func ServeTorrent(ctx context.Context, w http.ResponseWriter, r *http.Request, scrpr *scraper.Scraper, def *scraper.Tracker, row scraper.Torrent, logger *slog.Logger) {
	indexerID := scraper.TrackerID(def)

	// A download that cannot be served falls back to the magnet the row
	// also carries, which the client can resolve without the tracker. It is
	// a warning rather than an error, since the client is still served.
	redirectToRowMagnet := func(message string, cause error) bool {
		if redirectToMagnet(w, r, row.Magnet) != nil {
			return false
		}
		logger.Warn("redirected a download to the row's magnet", "indexer", indexerID, "row", row.ID, "reason", message, "error", cause)
		return true
	}

	// A magnet is handed back for the client to resolve; there is nothing
	// to fetch with the tracker's session. A download link that is a magnet
	// the redirect cannot carry falls back to the row's own magnet, and
	// when neither can be followed the result is refused as carrying no
	// usable link, with a 404 rather than a 502, since no tracker was asked.
	if magnet := magnetOf(row); magnet != "" {
		const message = "refused to redirect a download to a stored magnet"
		err := redirectToMagnet(w, r, magnet)
		if err == nil || (magnet != row.Magnet && redirectToRowMagnet(message, err)) {
			return
		}
		logger.Error(message, "indexer", indexerID, "row", row.ID, "error", err)
		http.Error(w, "This result's magnet cannot be followed", http.StatusNotFound)
		return
	}
	if row.DownloadURL == "" {
		http.Error(w, "This result has no download link", http.StatusNotFound)
		return
	}

	// The cause names the tracker's address, which may carry its passkey,
	// so it stays in the log.
	refuse := func(message string, cause error) {
		if redirectToRowMagnet(message, cause) {
			return
		}
		logger.Error(message, "indexer", indexerID, "row", row.ID, "error", cause)
		http.Error(w, "Failed to download from "+indexerID, http.StatusBadGateway)
	}

	download, err := scrpr.Download(ctx, def, row.DownloadURL)
	if errors.Is(err, scraper.ErrForeignDownloadURL) {
		logger.Error("failed to download a torrent", "indexer", indexerID, "row", row.ID, "error", err)
		// A link leaving the tracker is answered as a refusal, whatever
		// magnet the row also carries, so the client learns its link was
		// refused rather than being served around it and taking the link
		// for a working one. The rejected host is the tracker's own text,
		// so it stays in the log rather than being reflected back to the
		// client.
		http.Error(w, fmt.Sprintf("Refused to download from %s: the link leaves the tracker", indexerID),
			http.StatusBadRequest)
		return
	}
	if err != nil {
		refuse("failed to download a torrent", err)
		return
	}
	// A tracker's download can lead to a magnet rather than a file, which
	// is handed back as a stored one is.
	if download.Magnet != "" {
		if err := redirectToMagnet(w, r, download.Magnet); err != nil {
			refuse("refused to redirect a download to the magnet a tracker led it to", err)
		}
		return
	}
	defer download.Body.Close()

	// A torrent file is a bencoded dictionary, so it starts with "d", and
	// it, like a magnet link, is longer than "magnet:". A tracker that
	// answers with an error in plain text or JSON, or with a body shorter
	// than that, an empty one included, is reported as a failure rather
	// than handed to the client to save as a torrent. Jackett's download
	// endpoint refuses the same, although a download selector lets an
	// empty body through, so the check is made here, on what is served,
	// rather than in the scraper. The buffer holds a magnet's whole line,
	// its line break included.
	body := bufio.NewReaderSize(download.Body, maxMagnetBytes+len("\r\n"))
	start, err := body.Peek(len("magnet:"))
	if err != nil {
		if errors.Is(err, io.EOF) {
			err = fmt.Errorf("the body is %d bytes, too short for a torrent file or a magnet", len(start))
		}
		refuse(notTorrentMessage, err)
		return
	}
	// A body that is a magnet link is handed back as one, as Jackett's
	// download endpoint does, though the scheme is matched in any case, as
	// a stored magnet's is. Only its first line is the link, and only it is
	// read, so what follows it does not have to arrive.
	if isMagnet(string(start)) {
		magnet, err := readMagnetLine(body)
		if err != nil {
			refuse("failed to read the magnet a tracker answered a download with", err)
			return
		}
		if err := redirectToMagnet(w, r, magnet); err != nil {
			refuse("refused to redirect a download to the magnet a tracker answered it with", err)
		}
		return
	}
	if start[0] != 'd' {
		refuse(notTorrentMessage, fmt.Errorf("the body starts with %q", start[:1]))
		return
	}

	// Whatever type the tracker labeled the file with, what is served is a
	// torrent file, so it goes out as one and a browser may not guess
	// another from the body.
	w.Header().Set("Content-Type", torrentContentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment",
		map[string]string{"filename": torrentFilename(row.Name)}))
	if _, err = io.Copy(w, body); err == nil {
		return
	}
	logger.Error("failed to stream a torrent", "indexer", indexerID, "row", row.ID, "error", err)
	// The file's start is already on its way, including when the tracker
	// ran past the download cap. Returning normally would close the
	// chunked response cleanly and the client would keep a truncated
	// .torrent as though it were whole, so the connection is broken
	// instead to make the transfer fail on the client's side too.
	panic(http.ErrAbortHandler)
}

// readMagnetLine reads the magnet link body starts with: its first line,
// without the line break, which the body's end also closes.
func readMagnetLine(body *bufio.Reader) (string, error) {
	line, err := body.ReadSlice('\n')
	if errors.Is(err, io.EOF) {
		err = nil
	}
	magnet := strings.TrimSpace(string(line))
	if errors.Is(err, bufio.ErrBufferFull) || (err == nil && len(magnet) > maxMagnetBytes) {
		err = fmt.Errorf("the magnet is longer than %d bytes", maxMagnetBytes)
	}
	return magnet, err
}

// redirectToMagnet hands magnet to the client as a redirect, or, writing
// nothing, reports why it could not be followed as one.
func redirectToMagnet(w http.ResponseWriter, r *http.Request, magnet string) error {
	if err := checkMagnet(magnet); err != nil {
		return err
	}
	//nolint:gosec // G710: checkMagnet admits only a magnet link, which names no web address to be sent to.
	http.Redirect(w, r, magnet, http.StatusFound)
	return nil
}

// checkMagnet reports why magnet cannot be sent as a redirect's Location:
// it carries a control character, which net/http does not replace there
// unless it is a line break, or names no exact topic ("xt") for a client
// to resolve.
func checkMagnet(magnet string) error {
	if strings.IndexFunc(magnet, unicode.IsControl) >= 0 {
		return errors.New("the magnet contains a control character")
	}
	link, err := url.Parse(magnet)
	if err != nil || !strings.EqualFold(link.Scheme, "magnet") || link.Opaque != "" {
		return errors.New("the magnet is not a link")
	}
	// A parameter that does not parse leaves the rest of the link usable,
	// so only the exact topic's presence decides.
	parameters, _ := url.ParseQuery(link.RawQuery)
	if !parameters.Has("xt") {
		return errors.New("the magnet names no exact topic")
	}
	return nil
}

// notTorrentMessage is what the log says of a download answered with
// something other than a torrent file or a magnet.
const notTorrentMessage = "tracker answered a download with something other than a torrent file"

// filenameUnsafe matches what must not reach a Content-Disposition
// filename: path separators, control characters, and the quoting
// characters the header itself uses.
var filenameUnsafe = regexp.MustCompile(`[^\p{L}\p{N} .,_+()\[\]-]`)

// maxFilenameBytes bounds the name torrentFilename returns, before its
// extension.
const maxFilenameBytes = 150

// torrentFilename turns a release title into a filename for the client to
// save. The title comes from a third-party page, so everything outside a
// conservative set is replaced rather than escaped.
func torrentFilename(name string) string {
	cleaned := strings.TrimSpace(filenameUnsafe.ReplaceAllString(name, "_"))
	cleaned = strings.Trim(cleaned, ".")
	if cleaned == "" {
		cleaned = "torrent"
	}
	if len(cleaned) > maxFilenameBytes {
		// Cut at a character boundary, since a byte offset can land inside
		// a multibyte letter and leave the name invalid UTF-8.
		cut := maxFilenameBytes
		for !utf8.RuneStart(cleaned[cut]) {
			cut--
		}
		cleaned = strings.TrimRight(cleaned[:cut], ". ")
	}
	return cleaned + ".torrent"
}

// magnetOf returns the magnet to hand back rather than fetch: the download
// link when it is one, or else the row's magnet when there is no download
// link at all, since a torrent file the row links to is what its proxied
// link promises. The scraper keeps a magnet in Magnet, and this package is
// importable, so a caller may have stored it as the download link instead.
func magnetOf(row scraper.Torrent) string {
	// The scheme is checked here as well as when the row was stored: a
	// redirect target that is not a magnet would send the caller wherever
	// a scraped page named, and this package is importable by a program
	// that fills rows its own way.
	link := row.DownloadURL
	if link == "" {
		link = row.Magnet
	}
	if !isMagnet(link) {
		return ""
	}
	return link
}

// isMagnet reports whether link is a magnet link, its scheme matched in
// any case.
func isMagnet(link string) bool {
	return strings.HasPrefix(strings.ToLower(link), "magnet:")
}
