// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// downloadRecorder is a tracker's download endpoint that records what each
// request presented.
type downloadRecorder struct {
	cookies   []string
	mu        sync.Mutex
	userAgent []string
}

func (d *downloadRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	names := make([]string, 0, len(r.Cookies()))
	for _, cookie := range r.Cookies() {
		names = append(names, cookie.Name+"="+cookie.Value)
	}
	d.cookies = append(d.cookies, fmt.Sprint(names))
	d.userAgent = append(d.userAgent, r.UserAgent())
	d.mu.Unlock()
	w.Header().Set("Content-Type", "application/x-bittorrent")
	_, _ = w.Write([]byte("d4:infod4:name7:examplee"))
}

// clientCookie is an outgoing client cookie, as a jar is loaded with.
func clientCookie(name, value string) *http.Cookie {
	// These attributes govern how a server tells a browser to store a
	// cookie; this is not a Set-Cookie being issued to a client.
	//nolint:gosec // G124
	return &http.Cookie{Name: name, Value: value}
}

func downloadDef(id, baseURL string) string {
	return fmt.Sprintf(`
id: %s
name: %s
links:
  - %s/
search:
  paths:
    - path: "/"
  rows:
    selector: ".row"
  fields:
    title:
      selector: a
    download:
      selector: a
      attribute: href
`, id, id, baseURL)
}

// A form login through FlareSolverr leaves the session in the browser's
// cookies, not in Jacklet's jar, and the download does not go through
// FlareSolverr, so without the browser's cookies and user agent a private
// tracker answers it with its login page.
func TestScraper_Download_PresentsTheBrowsersSession(t *testing.T) {
	fake := newFakeFlareSolverr(`<div class="row"><a href="/download/1">Example Release</a></div>`)
	fake.solutionCookies = []map[string]any{{"name": "sid", "value": "abc", "domain": "127.0.0.1", "path": "/", "httpOnly": true}}
	fake.solutionUserAgent = "BrowserUA/1.0"
	flare := httptest.NewServer(fake)
	defer flare.Close()

	tracker := &downloadRecorder{}
	site := httptest.NewServer(tracker)
	defer site.Close()

	store := &fakeStore{}

	def := loadTestTracker(t, t.TempDir(), "private-tracker", downloadDef("private-tracker", site.URL))
	scrpr := NewWithOptions(NewConfigStore(""), "", slog.New(slog.DiscardHandler), Options{Sink: store})
	scrpr.useFlareSolverr(t, flare.URL, "private-tracker")
	require.NoError(t, scrpr.scrapeIndexer(t.Context(), def, SearchParams{Query: "example"}))

	download, err := scrpr.Download(t.Context(), def, site.URL+"/download/1")
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, download.Body)
	download.Body.Close()

	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	require.Equal(t, []string{"[sid=abc]"}, tracker.cookies, "the browser's session cookie was not presented")
	require.Equal(t, []string{"BrowserUA/1.0"}, tracker.userAgent, "the browser's user agent was not presented")
}

// A tracker that is fetched directly has no browser, so a download presents
// Jacklet's own defaults.
func TestScraper_Download_WithoutABrowserPresentsTheDefaults(t *testing.T) {
	tracker := &downloadRecorder{}
	site := httptest.NewServer(tracker)
	defer site.Close()

	def := loadTestTracker(t, t.TempDir(), "plain-tracker", downloadDef("plain-tracker", site.URL))
	scrpr := New(NewConfigStore(""), "http://flaresolverr.example.invalid", slog.New(slog.DiscardHandler))

	download, err := scrpr.Download(t.Context(), def, site.URL+"/download/1")
	require.NoError(t, err)
	download.Body.Close()

	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	require.Equal(t, []string{"[]"}, tracker.cookies)
	require.Equal(t, []string{defaultUserAgent}, tracker.userAgent)
}

// The browser's identity goes with its session: once the session is retired
// the cookies and user agent it held are gone too, and are not presented for
// a session that no longer exists.
func TestScraperRetiringASessionForgetsTheBrowsersIdentity(t *testing.T) {
	fake := newFakeFlareSolverr("<html></html>")
	fake.solutionCookies = []map[string]any{{"name": "sid", "value": "abc", "domain": "127.0.0.1", "path": "/"}}
	fake.solutionUserAgent = "BrowserUA/1.0"
	flare := httptest.NewServer(fake)
	defer flare.Close()

	scrpr := New(NewConfigStore(""), flare.URL, slog.New(slog.DiscardHandler))
	require.NoError(t, scrapeOnce(t.Context(), scrpr))

	jar, userAgent := scrpr.flareSolverrBrowser("example-tracker")
	require.NotNil(t, jar)
	require.Equal(t, "BrowserUA/1.0", userAgent)

	tracker := trackerSession(t, scrpr, "example-tracker")
	current, err := scrpr.flareSolverrSession(t.Context(), tracker)
	require.NoError(t, err)
	scrpr.retireFlareSolverrSession(t.Context(), tracker, current)

	jar, userAgent = scrpr.flareSolverrBrowser("example-tracker")
	require.Nil(t, jar)
	require.Empty(t, userAgent)
}

// A browser reports the domain even for an address with none, which a jar
// refuses, so the cookie is kept for that host alone; a named host keeps its
// domain, and the expiry carries over.
func TestFlareSolverrCookieConversion(t *testing.T) {
	cookie := flareSolverrCookie{Domain: ".example.test", Expiry: 1900000000, HTTPOnly: true, Name: "sid", Path: "/", Secure: true, Value: "abc"}

	named := cookie.httpCookie("tracker.example.test")
	require.Equal(t, ".example.test", named.Domain)
	require.Equal(t, time.Unix(1900000000, 0), named.Expires)
	require.True(t, named.HttpOnly)
	require.True(t, named.Secure)

	require.Empty(t, cookie.httpCookie("127.0.0.1").Domain, "a cookie for a bare IP address kept a domain the jar refuses")
	require.Empty(t, cookie.httpCookie("::1").Domain)
	require.True(t, flareSolverrCookie{Name: "n"}.httpCookie("host.test").Expires.IsZero(), "a session cookie was given an expiry")

	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	pageURL, err := url.Parse("https://tracker.example.test/")
	require.NoError(t, err)
	jar.SetCookies(pageURL, []*http.Cookie{named})
	other, err := url.Parse("https://download.example.test/file")
	require.NoError(t, err)
	require.Len(t, jar.Cookies(other), 1, "a cookie for the site's domain did not reach a host beneath it")
}

// The first jar to hold a cookie of a name wins, so the browser's session
// takes precedence over a stale one in Jacklet's own jar, and writes go to
// the jar that belongs to Jacklet.
func TestLayeredJar(t *testing.T) {
	first, err := cookiejar.New(nil)
	require.NoError(t, err)
	second, err := cookiejar.New(nil)
	require.NoError(t, err)
	target, err := url.Parse("http://tracker.example.test/")
	require.NoError(t, err)

	first.SetCookies(target, []*http.Cookie{clientCookie("sid", "browser")})
	second.SetCookies(target, []*http.Cookie{clientCookie("sid", "stale"), clientCookie("theme", "dark")})

	layered := layeredJar{first, second}
	got := map[string]string{}
	for _, cookie := range layered.Cookies(target) {
		got[cookie.Name] = cookie.Value
	}
	require.Equal(t, map[string]string{"sid": "browser", "theme": "dark"}, got)

	layered.SetCookies(target, []*http.Cookie{clientCookie("fresh", "1")})
	require.Len(t, first.Cookies(target), 1, "a write reached the browser's jar")
	require.Len(t, second.Cookies(target), 3, "a write did not reach Jacklet's own jar")
}

// A tracker often redirects, and the browser's cookies belong to the page it
// landed on: a host-only cookie of www.tracker.example does not match a
// request for tracker.example, so loaded under the requested address the jar
// refuses it and the download goes out without the session.
func TestScraperBrowserCookiesFollowTheRedirect(t *testing.T) {
	fake := newFakeFlareSolverr("<html></html>")
	fake.solutionCookies = []map[string]any{{"name": "sid", "value": "abc", "domain": "www.landed.test", "path": "/"}}
	fake.solutionURL = "https://www.landed.test/home"
	flare := httptest.NewServer(fake)
	defer flare.Close()

	scrpr := New(NewConfigStore(""), flare.URL, slog.New(slog.DiscardHandler))
	require.NoError(t, scrapeOnce(t.Context(), scrpr))

	jar, _ := scrpr.flareSolverrBrowser("example-tracker")
	require.NotNil(t, jar)
	landed, err := url.Parse("https://www.landed.test/download/1")
	require.NoError(t, err)
	require.Len(t, jar.Cookies(landed), 1, "the cookie of the page the browser landed on was refused")
}

func TestBrowserPageURL(t *testing.T) {
	requested, err := url.Parse("https://tracker.example.test/search")
	require.NoError(t, err)

	tests := []struct {
		name  string
		final string
		want  string
	}{
		{name: "the page the browser landed on", final: "https://www.tracker.example.test/home", want: "https://www.tracker.example.test/home"},
		{name: "no address reported", final: "", want: requested.String()},
		{name: "the browser's error page", final: "chrome-error://chromewebdata/", want: requested.String()},
		{name: "an address that does not parse", final: "http://[::1", want: requested.String()},
		{name: "not a web address", final: "about:blank", want: requested.String()},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, browserPageURL(requested, tc.final).String())
		})
	}
}

// A clearance is bound to the user agent that earned it, and the definition's
// headers are never sent through FlareSolverr, so a definition that declares
// its own User-Agent must not replace the browser's on the download, while a
// tracker fetched directly keeps sending the definition's.
func TestScraper_Download_UserAgentPrecedence(t *testing.T) {
	withHeaders := func(id, baseURL string) string {
		// The definition's headers are a key of its search block.
		def := downloadDef(id, baseURL)
		return strings.Replace(def, "search:\n", "search:\n  headers:\n    User-Agent: DefinitionUA/2.0\n", 1)
	}

	t.Run("through FlareSolverr the browser's wins", func(t *testing.T) {
		fake := newFakeFlareSolverr(`<div class="row"><a href="/download/1">Example Release</a></div>`)
		fake.solutionUserAgent = "BrowserUA/1.0"
		fake.solutionCookies = []map[string]any{{"name": "sid", "value": "abc", "domain": "127.0.0.1", "path": "/"}}
		flare := httptest.NewServer(fake)
		defer flare.Close()
		tracker := &downloadRecorder{}
		site := httptest.NewServer(tracker)
		defer site.Close()

		store := &fakeStore{}
		def := loadTestTracker(t, t.TempDir(), "declared-ua", withHeaders("declared-ua", site.URL))
		scrpr := NewWithOptions(NewConfigStore(""), "", slog.New(slog.DiscardHandler), Options{Sink: store})
		scrpr.useFlareSolverr(t, flare.URL, "declared-ua")
		require.NoError(t, scrpr.scrapeIndexer(t.Context(), def, SearchParams{Query: "example"}))

		download, err := scrpr.Download(t.Context(), def, site.URL+"/download/1")
		require.NoError(t, err)
		download.Body.Close()

		tracker.mu.Lock()
		defer tracker.mu.Unlock()
		require.Equal(t, []string{"BrowserUA/1.0"}, tracker.userAgent)
	})

	t.Run("fetched directly the definition's is sent", func(t *testing.T) {
		tracker := &downloadRecorder{}
		site := httptest.NewServer(tracker)
		defer site.Close()
		def := loadTestTracker(t, t.TempDir(), "declared-ua", withHeaders("declared-ua", site.URL))
		scrpr := New(NewConfigStore(""), "", slog.New(slog.DiscardHandler))

		download, err := scrpr.Download(t.Context(), def, site.URL+"/download/1")
		require.NoError(t, err)
		download.Body.Close()

		tracker.mu.Lock()
		defer tracker.mu.Unlock()
		require.Equal(t, []string{"DefinitionUA/2.0"}, tracker.userAgent)
	})
}
