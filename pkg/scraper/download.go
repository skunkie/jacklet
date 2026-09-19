// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// ErrForeignDownloadURL is returned when a stored download link does not
// belong to the tracker it is attributed to. Jacklet fetches the link with
// that tracker's credentials, so it must not be talked into fetching
// somewhere else by a row that says otherwise.
var ErrForeignDownloadURL = errors.New("download link does not belong to this tracker")

// ErrDownloadTooLarge is reported by a Download's Body once the tracker
// has sent more than the download cap. It surfaces as a read error partway
// through the stream rather than from Download itself, since the body is
// handed back unread.
var ErrDownloadTooLarge = errors.New("torrent file is larger than the download cap")

// Download is a torrent file fetched from a tracker. The caller owns Body
// and must close it.
//
// Body is capped: reading it yields at most maxDownloadBytes and then
// fails with ErrDownloadTooLarge, so a tracker cannot push unbounded
// volume through Jacklet to the client that asked for a torrent file.
type Download struct {
	Body        io.ReadCloser
	ContentType string
}

// maxDownloadRedirects is the number of redirects a download may follow,
// matching net/http's own default, which a custom CheckRedirect replaces.
const maxDownloadRedirects = 10

// torrentContentType is what a tracker serving a .torrent file should
// report, and what Jacklet reports to the client.
const torrentContentType = "application/x-bittorrent"

// defaultMaxDownloadBytes bounds one torrent file. It is far below
// defaultMaxResponseBytes because the two are bounding different things: a
// scrape buffers a whole listing page in memory, while a .torrent is a
// small file — a piece-hash list for the release, in practice well under a
// megabyte even for a large multi-file torrent. Sharing the scrape cap
// would leave 32 MiB of slack that no legitimate torrent file needs, for
// each concurrent download.
//
// Unlike a scrape, exceeding this cannot fail the request cleanly: the
// body is streamed straight to the client, so by the time the cap is
// reached the status and headers have been sent and the client already
// holds part of the file. The read fails with ErrDownloadTooLarge, which
// the caller treats as it does any other mid-stream failure — it breaks
// the connection so the client sees a failed transfer rather than a
// truncated file under a 200.
const defaultMaxDownloadBytes = 1 << 20 // 1 MiB

// limitedBody is a response body that stops after a cap. It reports
// ErrDownloadTooLarge instead of io.EOF, so a truncated stream is not
// mistaken for a complete one, and closes the response it wraps so the
// caller still owns exactly one Close.
type limitedBody struct {
	closer io.Closer
	// reader holds one byte more than the cap, so its N reaching zero says
	// the body ran past the cap rather than merely reaching it.
	reader *io.LimitedReader
}

func (b *limitedBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	if b.reader.N == 0 {
		// The probe byte past the cap was read; drop it rather than pass
		// it on, so the caller sees exactly the cap's worth of data.
		if n > 0 {
			n--
		}
		return n, ErrDownloadTooLarge
	}
	return n, err
}

func (b *limitedBody) Close() error { return b.closer.Close() }

// Download fetches a torrent file from a tracker using that tracker's
// authenticated session, so a client that has no session of its own — the
// ordinary case for Sonarr or a BitTorrent client — can still retrieve it.
//
// rawURL must be a link the tracker itself served, and is checked against
// the definition's own links before any request is made.
//
// FlareSolverr is deliberately not used here even when configured: it
// returns a rendered page as text, which would corrupt a torrent file.
func (s *Scraper) Download(ctx context.Context, def *Tracker, rawURL string) (*Download, error) {
	target, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("tracker %s: invalid download link: %w", def.Name, err)
	}
	if target.Scheme != "http" && target.Scheme != "https" {
		return nil, fmt.Errorf("tracker %s: %w: scheme %q", def.Name, ErrForeignDownloadURL, target.Scheme)
	}

	baseURL, ok := matchingBaseURL(def, target)
	if !ok {
		return nil, fmt.Errorf("tracker %s: %w: %s", def.Name, ErrForeignDownloadURL, target.Host)
	}

	cfg, err := s.config.Resolve(def)
	if err != nil {
		s.logger.Warn("failed to load config overrides", "tracker", def.Name, "error", err)
		cfg = defaultConfig(def)
	}
	if err := s.ensureLoggedIn(ctx, def, baseURL, cfg); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), http.NoBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", defaultUserAgent)
	for key, value := range def.Search.Headers {
		req.Header.Set(key, headerValue(value))
	}

	// Download links come from tracker pages, so the first URL is not the
	// whole trust boundary: the tracker can redirect it elsewhere. Keep the
	// proxy on the tracker's own hosts, and do not allow an HTTPS download
	// to be downgraded before the response is returned to the client.
	//
	// Setting CheckRedirect also replaces the client's default cap of ten
	// redirects, so the hop limit has to be enforced here as well; without
	// it a tracker bouncing a link between two of its own hosts would be
	// followed until the client's timeout.
	client := &http.Client{
		CheckRedirect: func(next *http.Request, via []*http.Request) error {
			if len(via) >= maxDownloadRedirects {
				return fmt.Errorf("stopped after %d redirects", len(via))
			}
			// Each hop is judged against the one before it rather than
			// against the original link, so a chain cannot climb to HTTPS
			// and then fall back to plaintext. via is never empty here:
			// it holds the requests already made, oldest first.
			previous := via[len(via)-1].URL
			if !sameDownloadOrigin(def, previous, next.URL) {
				return fmt.Errorf("%w: redirect target %s", ErrForeignDownloadURL, next.URL.Host)
			}
			return nil
		},
		Jar:       s.httpClient.Jar,
		Timeout:   s.httpClient.Timeout,
		Transport: s.httpClient.Transport,
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tracker %s: %w", def.Name, err)
	}
	// resp.Request is the last request made, so a status from the far side
	// of a redirect names the URL that actually answered rather than the
	// one the row stored.
	if err := checkStatus(resp.StatusCode, resp.Request.URL); err != nil {
		resp.Body.Close()
		return nil, fmt.Errorf("tracker %s: %w", def.Name, err)
	}

	contentType := resp.Header.Get("Content-Type")
	// A tracker that has lost the session answers with a login page rather
	// than a 401, and saving that as a ".torrent" is the confusing failure
	// this endpoint exists to avoid.
	if isHTML(contentType) {
		resp.Body.Close()
		s.invalidateLogin(TrackerID(def))
		return nil, fmt.Errorf("tracker %s: download returned a web page rather than a torrent; the session may have lapsed", def.Name)
	}
	if contentType == "" {
		contentType = torrentContentType
	}

	body := &limitedBody{
		closer: resp.Body,
		reader: &io.LimitedReader{N: s.maxDownloadBytes + 1, R: resp.Body},
	}
	return &Download{Body: body, ContentType: contentType}, nil
}

// matchingBaseURL returns the tracker link target belongs to. A tracker's
// mirrors are distinct hosts, so the one that served a link is the one its
// credentials belong to.
func matchingBaseURL(def *Tracker, target *url.URL) (*url.URL, bool) {
	for _, baseURL := range candidateBaseURLs(def) {
		if withinSite(baseURL, target) {
			return baseURL, true
		}
	}
	return nil, false
}

// withinSite reports whether target belongs to baseURL's site: the same
// host, or a host beneath it. Trackers commonly hand a download off to a
// host of their own that the definition does not list -- a bulk or CDN
// subdomain -- and refusing those leaves the release with a link that
// cannot be fetched at all, which is the whole point of proxying it.
//
// The subdomain match is anchored on a dot, so a host merely ending in the
// site's name ("nottracker.example" against "tracker.example") is not
// taken for part of it. It only ever descends: a definition naming
// "www.tracker.example" does not thereby vouch for "tracker.example", nor
// for the rest of what lives under a parent it may not control.
//
// A host beneath the site is matched only on the site's own port, since
// hostWithoutDefaultPort leaves a non-default one in the string being
// compared. That is the same strictness a mirror on an unusual port
// already meets.
func withinSite(baseURL, target *url.URL) bool {
	site := hostWithoutDefaultPort(baseURL)
	host := hostWithoutDefaultPort(target)
	if strings.EqualFold(host, site) {
		return true
	}
	// The dot has to be part of the host rather than of the site, or
	// "evil.com" would vouch for anything ending in it.
	return len(host) > len(site)+1 &&
		host[len(host)-len(site)-1] == '.' &&
		strings.EqualFold(host[len(host)-len(site):], site)
}

// sameHost reports whether two URLs address the same host, reading a port
// as a browser does: "https://tracker.example" and
// "https://tracker.example:443" are one site, while a non-default port is
// a different one. A tracker writing the port out in a redirect is
// otherwise mistaken for a foreign host.
func sameHost(a, b *url.URL) bool {
	return strings.EqualFold(hostWithoutDefaultPort(a), hostWithoutDefaultPort(b))
}

// hostWithoutDefaultPort is u's host with a port the scheme already
// implies removed.
func hostWithoutDefaultPort(u *url.URL) string {
	port := u.Port()
	if port == "" ||
		(port == "80" && strings.EqualFold(u.Scheme, "http")) ||
		(port == "443" && strings.EqualFold(u.Scheme, "https")) {
		return u.Hostname()
	}
	return net.JoinHostPort(u.Hostname(), port)
}

// sameDownloadOrigin permits a redirect that stays on the tracker: either
// the host the hop before it was served from, or another host the
// definition claims as its own, since a tracker routinely answers a
// download from one of its mirrors. A hop may upgrade from HTTP to HTTPS,
// but never the reverse — comparing against the previous hop rather than
// the original link is what stops a chain from upgrading and then
// downgrading back to plaintext.
func sameDownloadOrigin(def *Tracker, previous, next *url.URL) bool {
	if !sameHost(previous, next) {
		if _, ok := matchingBaseURL(def, next); !ok {
			return false
		}
	}
	if next.Scheme != "http" && next.Scheme != "https" {
		return false
	}
	return previous.Scheme != "https" || next.Scheme == "https"
}

// isHTML reports whether a Content-Type names a web page.
func isHTML(contentType string) bool {
	mediaType, _, _ := strings.Cut(contentType, ";")
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	return mediaType == "text/html" || mediaType == "application/xhtml+xml"
}
