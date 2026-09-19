// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDownloadStopsRedirectLoop covers the hop limit that a custom
// CheckRedirect has to carry itself: net/http applies its default cap of
// ten only while CheckRedirect is nil, so a tracker redirecting a download
// back to itself would otherwise be followed until the client's timeout.
func TestDownloadStopsRedirectLoop(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, "/loop", http.StatusFound)
	}))
	defer server.Close()

	def := &Tracker{ID: "example", Links: []string{server.URL + "/"}, Name: "Example Tracker"}
	scrpr := New(nil, NewConfigStore(""), "", testLogger())

	_, err := scrpr.Download(context.Background(), def, server.URL+"/download/1")
	require.Error(t, err, "Download followed a redirect loop without failing")
	require.ErrorContains(t, err, "stopped after", "want a redirect limit failure")
	require.Equal(t, int32(maxDownloadRedirects), hits.Load(), "tracker saw the wrong number of requests")
}

func TestSameDownloadOrigin(t *testing.T) {
	parse := func(raw string) *url.URL {
		t.Helper()
		u, err := url.Parse(raw)
		require.NoError(t, err)
		return u
	}

	def := &Tracker{Links: []string{"https://tracker.example/", "https://mirror.example/"}}
	// previous is the hop the redirect is being judged against, which is
	// the one before it rather than the link the download started on.
	tests := []struct {
		name     string
		next     string
		previous string
		want     bool
	}{
		{name: "same host", next: "https://tracker.example/other", previous: "https://tracker.example/file", want: true},
		{name: "own mirror", next: "https://mirror.example/file", previous: "https://tracker.example/file", want: true},
		{name: "foreign host", next: "https://other.example/file", previous: "https://tracker.example/file", want: false},
		{name: "https upgrade", next: "https://tracker.example/file", previous: "http://tracker.example/file", want: true},
		{name: "https downgrade", next: "http://tracker.example/file", previous: "https://tracker.example/file", want: false},
		{name: "mirror downgrade", next: "http://mirror.example/file", previous: "https://tracker.example/file", want: false},
		{name: "unsupported scheme", next: "ftp://tracker.example/file", previous: "https://tracker.example/file", want: false},
		// A tracker writing out the port the scheme already implies is
		// the same site, not a foreign one.
		{name: "default port spelled out", next: "https://tracker.example:443/file", previous: "https://tracker.example/file", want: true},
		{name: "implicit port from explicit", next: "https://tracker.example/file", previous: "https://tracker.example:443/file", want: true},
		{name: "non-default port", next: "https://tracker.example:8443/file", previous: "https://tracker.example/file", want: false},
		// A tracker serving downloads from a host of its own that the
		// definition does not list, which is how this commonly fails.
		{name: "the site's own download host", next: "https://bulk.tracker.example/f", previous: "https://tracker.example/file", want: true},
		{name: "a mirror's download host", next: "https://bulk.mirror.example/f", previous: "https://tracker.example/file", want: true},
		{name: "a look-alike of the site", next: "https://nottracker.example/f", previous: "https://tracker.example/file", want: false},
		{name: "the site as a label of a foreign domain", next: "https://tracker.example.evil.com/f", previous: "https://tracker.example/file", want: false},
		{name: "a download host may not downgrade", next: "http://bulk.tracker.example/f", previous: "https://tracker.example/file", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, sameDownloadOrigin(def, parse(tc.previous), parse(tc.next)))
		})
	}
}

// TestSameDownloadOriginRefusesDowngradeAfterUpgrade covers the reason
// each hop is compared to the one before it: judging every hop against the
// original link would let a chain that started on HTTP climb to HTTPS and
// then fall back to plaintext, since the original link is still HTTP.
func TestSameDownloadOriginRefusesDowngradeAfterUpgrade(t *testing.T) {
	def := &Tracker{Links: []string{"http://tracker.example/"}}
	raws := []string{"http://tracker.example/dl", "https://tracker.example/dl", "http://tracker.example/file"}
	hops := make([]*url.URL, 0, len(raws))
	for _, raw := range raws {
		u, err := url.Parse(raw)
		require.NoError(t, err)
		hops = append(hops, u)
	}

	require.True(t, sameDownloadOrigin(def, hops[0], hops[1]), "the upgrade to HTTPS was refused")
	require.False(t, sameDownloadOrigin(def, hops[1], hops[2]),
		"a downgrade back to HTTP was permitted after the chain reached HTTPS")
}

func TestMatchingBaseURL(t *testing.T) {
	def := &Tracker{Links: []string{"https://primary.example/", "https://mirror.example/base"}}
	for _, tc := range []struct {
		target string
		want   string
	}{
		{target: "https://primary.example/file.torrent", want: "https://primary.example/"},
		{target: "https://mirror.example/file.torrent", want: "https://mirror.example/base"},
		// A link on the tracker's own download host belongs to the site
		// above it, which is the base its credentials are held against.
		{target: "https://bulk.primary.example/file.torrent", want: "https://primary.example/"},
		{target: "https://bulk.mirror.example/file.torrent", want: "https://mirror.example/base"},
	} {
		target, err := url.Parse(tc.target)
		require.NoError(t, err)

		base, ok := matchingBaseURL(def, target)
		require.True(t, ok, "matchingBaseURL(%q) reported no match", tc.target)
		require.Equal(t, tc.want, base.String(), "matchingBaseURL(%q)", tc.target)
	}
	for _, raw := range []string{
		"https://foreign.example/file.torrent",
		"https://notprimary.example/file.torrent",
		"https://primary.example.evil.com/file.torrent",
	} {
		target, _ := url.Parse(raw)
		_, ok := matchingBaseURL(def, target)
		require.False(t, ok, "matchingBaseURL accepted %q", raw)
	}
}

func TestIsHTML(t *testing.T) {
	for _, tc := range []struct {
		contentType string
		want        bool
	}{
		{"text/html", true},
		{"TEXT/HTML; charset=utf-8", true},
		{"application/xhtml+xml", true},
		{"application/x-bittorrent", false},
		{"", false},
	} {
		require.Equal(t, tc.want, isHTML(tc.contentType), "isHTML(%q)", tc.contentType)
	}
}

// newDownloadTracker serves a torrent file of bodyBytes, and returns a
// scraper whose download cap is maxBytes along with the link to fetch.
func newDownloadTracker(t *testing.T, bodyBytes int, maxBytes int64) (*Scraper, *Tracker, string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", torrentContentType)
		_, err := io.WriteString(w, strings.Repeat("x", bodyBytes))
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)

	def := &Tracker{ID: "example", Links: []string{server.URL + "/"}, Name: "Example Tracker"}
	scrpr := NewWithOptions(nil, NewConfigStore(""), "", testLogger(),
		Options{MaxDownloadBytes: maxBytes})
	return scrpr, def, server.URL + "/download/1"
}

// TestDownloadUnderTheCapIsServedWhole checks the download cap does not
// disturb a torrent file that fits, including one exactly on the limit.
func TestDownloadUnderTheCapIsServedWhole(t *testing.T) {
	const limit = 4096
	for _, size := range []int{1, limit - 1, limit} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			scrpr, def, rawURL := newDownloadTracker(t, size, limit)

			download, err := scrpr.Download(context.Background(), def, rawURL)
			require.NoError(t, err, "want a torrent file")
			defer download.Body.Close()

			body, err := io.ReadAll(download.Body)
			require.NoError(t, err, "want the body read whole")
			require.Len(t, body, size)
		})
	}
}

// TestDownloadOverTheCapIsTruncated covers the other side of the cap. A
// .torrent is a small file, so an uncapped stream would let a tracker — or
// a compromised mirror — push arbitrary volume through Jacklet's proxy to
// the client that asked for one. The cap is reached while streaming, after
// the headers have gone out, so it can only truncate and report.
func TestDownloadOverTheCapIsTruncated(t *testing.T) {
	const limit = 4096
	scrpr, def, rawURL := newDownloadTracker(t, 2*limit, limit)

	download, err := scrpr.Download(context.Background(), def, rawURL)
	require.NoError(t, err, "want the response opened and capped while streaming")
	defer download.Body.Close()

	body, err := io.ReadAll(download.Body)
	require.ErrorIs(t, err, ErrDownloadTooLarge)
	require.Len(t, body, limit, "want the cap's worth and no more")
}

func TestNewDefaultsTheDownloadCap(t *testing.T) {
	scrpr := New(nil, NewConfigStore(""), "", testLogger())
	require.Equal(t, int64(defaultMaxDownloadBytes), scrpr.maxDownloadBytes)
}

// TestNewWithOptions checks what an embedding program can and
// cannot ask for: a limit of its own is taken, and a zero or negative one
// falls back to the default rather than turning the cap off.
func TestNewWithOptions(t *testing.T) {
	for _, tc := range []struct {
		name         string
		options      Options
		wantDownload int64
		wantResponse int64
	}{
		{
			name:         "both limits set",
			options:      Options{MaxDownloadBytes: 4096, MaxResponseBytes: 8192},
			wantDownload: 4096,
			wantResponse: 8192,
		},
		{
			name:         "one limit set leaves the other at its default",
			options:      Options{MaxDownloadBytes: 4096},
			wantDownload: 4096,
			wantResponse: defaultMaxResponseBytes,
		},
		{
			name:         "the zero Options is what New builds",
			options:      Options{},
			wantDownload: defaultMaxDownloadBytes,
			wantResponse: defaultMaxResponseBytes,
		},
		{
			name:         "a negative limit does not disable the cap",
			options:      Options{MaxDownloadBytes: -1, MaxResponseBytes: -1},
			wantDownload: defaultMaxDownloadBytes,
			wantResponse: defaultMaxResponseBytes,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scrpr := NewWithOptions(nil, NewConfigStore(""), "", testLogger(), tc.options)
			require.Equal(t, tc.wantDownload, scrpr.maxDownloadBytes)
			require.Equal(t, tc.wantResponse, scrpr.maxResponseBytes)
		})
	}
}

// TestWithinSite covers which hosts count as the tracker's own. The
// subdomain rule exists because trackers serve downloads from a host they
// do not list in the definition, and it is the one place where widening
// this check could let Jacklet carry a tracker's session somewhere the
// tracker does not control.
func TestWithinSite(t *testing.T) {
	parse := func(raw string) *url.URL {
		t.Helper()
		u, err := url.Parse(raw)
		require.NoError(t, err)
		return u
	}

	for _, tc := range []struct {
		base string
		host string
		name string
		want bool
	}{
		{base: "https://tracker.example/", host: "https://tracker.example/file", name: "the site itself", want: true},
		{base: "https://tracker.example/", host: "https://bulk.tracker.example/file", name: "a subdomain", want: true},
		{base: "https://tracker.example/", host: "https://a.b.tracker.example/file", name: "a deeper subdomain", want: true},
		{base: "https://Tracker.Example/", host: "https://BULK.tracker.example/file", name: "case is ignored", want: true},
		{base: "https://tracker.example/base/x", host: "https://bulk.tracker.example/f", name: "the site's own path does not matter", want: true},

		// The dot boundary: a host that merely ends in the site's name is
		// a different registration and must not be taken for part of it.
		{base: "https://tracker.example/", host: "https://nottracker.example/file", name: "a look-alike suffix", want: false},
		{base: "https://tracker.example/", host: "https://tracker.example.evil/f", name: "the name as a prefix of another host", want: false},
		{base: "https://tracker.example/", host: "https://tracker.example.evil.com/f", name: "the site as a label of a foreign domain", want: false},

		// Only ever downwards: a parent may be a shared host the tracker
		// does not control.
		{base: "https://www.tracker.example/", host: "https://tracker.example/f", name: "the parent of a declared subdomain", want: false},
		{base: "https://www.tracker.example/", host: "https://bulk.tracker.example/f", name: "a sibling of a declared subdomain", want: false},

		{base: "https://tracker.example/", host: "https://other.example/file", name: "a foreign host", want: false},
		{base: "https://tracker.example/", host: "https://bulk.tracker.example:8443/f", name: "a subdomain on a non-default port", want: false},
		{base: "https://tracker.example/", host: "https://bulk.tracker.example:443/f", name: "the default port spelled out", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, withinSite(parse(tc.base), parse(tc.host)))
		})
	}
}

// TestDownloadFollowsProtocolRelativeRedirect covers a tracker answering a
// download with a scheme-less "Location", which is how one commonly writes
// the hand-off to its download host. The scheme is inherited from the hop
// before it, so the redirect is judged as HTTP rather than as a scheme the
// guard does not recognize.
//
// The hand-off to a host *beneath* the site is covered by TestWithinSite
// and TestSameDownloadOrigin rather than here: httptest serves every
// server on 127.0.0.1, so no two of them can stand in a subdomain
// relationship, and a definition naming both hosts outright would pass
// this test whether or not that rule exists.
func TestDownloadFollowsProtocolRelativeRedirect(t *testing.T) {
	bulk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", torrentContentType)
		_, _ = w.Write([]byte("d8:announcee"))
	}))
	defer bulk.Close()

	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", strings.TrimPrefix(bulk.URL, "http:")+"/download.php?id=1")
		w.WriteHeader(http.StatusFound)
	}))
	defer site.Close()

	def := &Tracker{Name: "T", Links: []string{site.URL + "/", bulk.URL + "/"}}
	scrpr := New(nil, NewConfigStore(""), "", slog.New(slog.DiscardHandler))

	got, err := scrpr.Download(context.Background(), def, site.URL+"/dl.php?id=1")
	require.NoError(t, err, "the scheme-less redirect was refused")
	defer got.Body.Close()

	payload, err := io.ReadAll(got.Body)
	require.NoError(t, err)
	require.Equal(t, "d8:announcee", string(payload))
}

// TestDownloadRefusesRedirectOffTheSite keeps the guard honest: widening
// it to subdomains must not let a tracker hand Jacklet's session to a host
// that merely resembles its own.
func TestDownloadRefusesRedirectOffTheSite(t *testing.T) {
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", torrentContentType)
		_, _ = w.Write([]byte("d8:announcee"))
	}))
	defer foreign.Close()

	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, foreign.URL+"/steal", http.StatusFound)
	}))
	defer site.Close()

	def := &Tracker{Name: "T", Links: []string{site.URL + "/"}}
	scrpr := New(nil, NewConfigStore(""), "", slog.New(slog.DiscardHandler))

	_, err := scrpr.Download(context.Background(), def, site.URL+"/dl.php?id=1")
	require.ErrorIs(t, err, ErrForeignDownloadURL)
}

// TestMatchingBaseURLDoesNotAscend pins the direction of the site match
// where a definition's own links are involved: a declared host vouches for
// itself and for what lies beneath it, never for its parent.
//
// Ascent is the tempting simplification, because symmetry looks natural in
// a host-matching helper and the asymmetry only makes sense once you know
// whose credentials are attached to the request. It does not merely reach
// the parent either: reaching it reaches every other child under it, so a
// definition naming one subdomain would hand the tracker's session to
// anything else the parent happens to host.
//
// TestWithinSite covers the same rule for the helper. This covers it for
// the declared links, where every other test names a site that has no
// parent short of a bare label, and an ascent would therefore be caught
// only incidentally, by over-accepting hosts unrelated to the tracker.
func TestMatchingBaseURLDoesNotAscend(t *testing.T) {
	const declared = "https://www.tracker.example/"
	def := &Tracker{Links: []string{declared}}

	for _, raw := range []string{
		"https://www.tracker.example/file.torrent",
		"https://bulk.www.tracker.example/file.torrent",
	} {
		target, err := url.Parse(raw)
		require.NoError(t, err)

		base, ok := matchingBaseURL(def, target)
		require.True(t, ok, "matchingBaseURL refused %q", raw)
		require.Equal(t, declared, base.String(), "matchingBaseURL(%q)", raw)
	}

	for _, raw := range []string{
		"https://tracker.example/file.torrent",
		"https://sibling.tracker.example/file.torrent",
		"https://example/file.torrent",
	} {
		target, err := url.Parse(raw)
		require.NoError(t, err)

		_, ok := matchingBaseURL(def, target)
		require.False(t, ok, "a definition naming %q vouched for %q", declared, raw)
	}
}
