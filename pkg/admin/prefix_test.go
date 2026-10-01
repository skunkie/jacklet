// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package admin

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/torrplay/jacklet/pkg/scraper"
)

// A panel mounted under a custom prefix must serve, link, redirect and scope
// its cookie under that prefix alone, leaving the default path unrouted.
func TestAdminServesUnderCustomPrefix(t *testing.T) {
	credential, err := NewPassword("sample-password", "")
	require.NoError(t, err)
	panel, err := NewWithOptions(nil, nil, nil, nil, credential, slog.New(slog.DiscardHandler), Options{Prefix: "/ops/jacklet"})
	require.NoError(t, err)
	mux := http.NewServeMux()
	panel.Routes(mux)

	serve := func(req *http.Request) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	require.Equal(t, http.StatusNotFound, serve(httptest.NewRequest(http.MethodGet, "/admin/login", http.NoBody)).Code,
		"the default prefix must not be routed")

	rec := serve(httptest.NewRequest(http.MethodGet, "/ops/jacklet/search?q=x", http.NoBody))
	require.Equal(t, http.StatusSeeOther, rec.Code)
	require.Equal(t, "/ops/jacklet/login?next=%2Fops%2Fjacklet%2Fsearch%3Fq%3Dx", rec.Header().Get("Location"))

	page := serve(httptest.NewRequest(http.MethodGet, "/ops/jacklet/login", http.NoBody)).Body.String()
	require.Contains(t, page, `action="/ops/jacklet/login"`)
	require.NotContains(t, page, `"/admin`, "a link still names the default prefix")

	form := url.Values{"next": {"/admin/search"}, "password": {"sample-password"}}
	req := httptest.NewRequest(http.MethodPost, "/ops/jacklet/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = serve(req)
	require.Equal(t, http.StatusSeeOther, rec.Code)
	require.Equal(t, "/ops/jacklet", rec.Header().Get("Location"), "a next path outside the prefix must be refused")
	cookies := rec.Result().Cookies()
	require.Len(t, cookies, 1)
	require.Equal(t, "/ops/jacklet", cookies[0].Path)
}

func TestNew_ValidatesPrefix(t *testing.T) {
	credential, err := NewPassword("sample-password", "")
	require.NoError(t, err)
	for _, prefix := range []string{"admin", "/", "/admin/", "/a/../b", "/a//b", "/a?b", "/a#b", "/a b", "/{x}", "/a%2fb", "/ops;x", "/админ", "/a\x7f"} {
		_, err := NewWithOptions(nil, nil, nil, nil, credential, slog.New(slog.DiscardHandler), Options{Prefix: prefix})
		require.Error(t, err, "prefix %q was accepted", prefix)
	}
	for _, prefix := range []string{"", "/admin", "/ops/jacklet"} {
		_, err := NewWithOptions(nil, nil, nil, nil, credential, slog.New(slog.DiscardHandler), Options{Prefix: prefix})
		require.NoError(t, err, "prefix %q was refused", prefix)
	}
}

func TestDownloadPath_UsesPrefix(t *testing.T) {
	require.Equal(t, "/ops/indexers/demo/download/7", downloadPath("/ops", scraper.Torrent{DownloadURL: "https://example.test/a.torrent", ID: 7, Tracker: "demo"}))
}

// Handler must serve the same routes as Routes, under the same prefix, so a
// program with its own router can mount the panel without a *http.ServeMux.
func TestAdmin_Handler_ServesUnderPrefix(t *testing.T) {
	credential, err := NewPassword("sample-password", "")
	require.NoError(t, err)
	panel, err := NewWithOptions(nil, nil, nil, nil, credential, slog.New(slog.DiscardHandler), Options{Prefix: "/ops/jacklet"})
	require.NoError(t, err)
	handler := panel.Handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ops/jacklet/login", http.NoBody))
	require.Equal(t, http.StatusOK, rec.Code)

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/login", http.NoBody))
	require.Equal(t, http.StatusNotFound, rec.Code)
}

// Branding is the host's to choose: the title replaces the name everywhere it
// is shown, and a link or icon the host does not supply is left out rather
// than pointing at a path only Jacklet's own server has.
func TestAdminBranding(t *testing.T) {
	credential, err := NewPassword("sample-password", "")
	require.NoError(t, err)
	page := func(options Options) string {
		panel, err := NewWithOptions(nil, nil, nil, nil, credential, slog.New(slog.DiscardHandler), options)
		require.NoError(t, err)
		rec := httptest.NewRecorder()
		panel.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/login", http.NoBody))
		return rec.Body.String()
	}

	plain := page(Options{})
	require.Contains(t, plain, "<h1>Jacklet</h1>", "an empty title must fall back to the default name")
	require.NotContains(t, plain, `rel="icon"`)

	branded := page(Options{FaviconURL: "/assets/icon.svg", Title: "Example Console"})
	require.Contains(t, branded, "<h1>Example Console</h1>")
	require.Contains(t, branded, "Sign in &middot; Example Console")
	require.Contains(t, branded, `rel="icon" href="/assets/icon.svg"`)
	require.NotContains(t, branded, "Jacklet</h1>")

	unsafe := page(Options{FaviconURL: "javascript:alert(1)"})
	require.NotContains(t, unsafe, "javascript:", "a script URL must not reach the page")
}

func TestAdminDocsLinkFollowsOption(t *testing.T) {
	credential, err := NewPassword("sample-password", "")
	require.NoError(t, err)
	signedIn := func(options Options) string {
		panel, err := NewWithOptions(nil, nil, nil, nil, credential, slog.New(slog.DiscardHandler), options)
		require.NoError(t, err)
		var buf strings.Builder
		require.NoError(t, panel.templates.ExecuteTemplate(&buf, "nav", "token"))
		return buf.String()
	}
	require.NotContains(t, signedIn(Options{}), "API docs")
	require.Contains(t, signedIn(Options{DocsURL: "/reference"}), `<a href="/reference">API docs</a>`)
}

func signInRequest(password string) *http.Request {
	form := url.Values{"password": {password}}
	req := httptest.NewRequest(http.MethodPost, "/admin/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

// Attempts are spaced per client address: a guess inside its address's delay is
// refused without being checked, another address is unaffected, and a correct
// password frees the slot.
func TestAdminSignInAttemptsAreSpacedPerAddress(t *testing.T) {
	credential, err := NewPassword("sample-password", "")
	require.NoError(t, err)
	panel, err := New(nil, nil, nil, nil, credential, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	handler := panel.Handler()

	attempt := func(remote, password string) int {
		req := signInRequest(password)
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}

	require.Equal(t, http.StatusUnauthorized, attempt("192.0.2.1:1000", "wrong"))
	require.Equal(t, http.StatusTooManyRequests, attempt("192.0.2.1:2000", "sample-password"),
		"a second attempt inside the delay was checked, so parallel guesses would not be limited")
	require.Equal(t, http.StatusSeeOther, attempt("192.0.2.2:1000", "sample-password"),
		"another address waited behind a failed one")

	time.Sleep(loginDelay + 50*time.Millisecond)
	require.Equal(t, http.StatusSeeOther, attempt("192.0.2.1:3000", "sample-password"))
	require.Equal(t, http.StatusSeeOther, attempt("192.0.2.1:3000", "sample-password"),
		"a correct sign-in left its address throttled")
}

func TestLoginLimiterDropsExpiredAddresses(t *testing.T) {
	var limiter loginLimiter
	now := time.Now()
	require.True(t, limiter.reserve("192.0.2.1", now))
	require.False(t, limiter.reserve("192.0.2.1", now.Add(loginDelay/2)))
	require.True(t, limiter.reserve("192.0.2.2", now.Add(loginDelay)))
	require.Len(t, limiter.notBefore, 1, "an expired address stayed tracked")
}

// Signing in again must revoke the session the browser already held.
func TestAdminSignInEndsThePreviousSession(t *testing.T) {
	credential, err := NewPassword("sample-password", "")
	require.NoError(t, err)
	panel, err := New(nil, nil, nil, nil, credential, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	handler := panel.Handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, signInRequest("sample-password"))
	first := rec.Result().Cookies()[0]
	_, ok := panel.sessions.lookup(first.Value)
	require.True(t, ok)

	req := signInRequest("sample-password")
	req.AddCookie(first)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusSeeOther, rec.Code)

	_, ok = panel.sessions.lookup(first.Value)
	require.False(t, ok, "the earlier session survived a new sign-in")
	_, ok = panel.sessions.lookup(rec.Result().Cookies()[0].Value)
	require.True(t, ok)
}

// An IPv6 client may own a whole /48, so every address in one must share a
// slot; an IPv4-mapped address must match its IPv4 form.
func TestClientAddress_GroupsIPv6ByAllocation(t *testing.T) {
	address := func(remote string) string {
		req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
		req.RemoteAddr = remote
		return clientAddress(req, nil)
	}
	require.Equal(t, address("[2001:db8:1:2::1]:80"), address("[2001:db8:1:2:ffff:ffff:ffff:ffff]:9"),
		"two addresses in one /64 got separate slots")
	require.Equal(t, address("[2001:db8:1:2::1]:80"), address("[2001:db8:1:ffff::1]:80"),
		"two /64s in one /48 got separate slots")
	require.NotEqual(t, address("[2001:db8:1:2::1]:80"), address("[2001:db8:2:2::1]:80"))
	require.Equal(t, address("192.0.2.1:80"), address("[::ffff:192.0.2.1]:80"))
	require.NotEqual(t, address("192.0.2.1:80"), address("192.0.2.2:80"))
	require.Equal(t, "not-an-address", address("not-an-address"))
}

// Behind a trusted proxy the client is the rightmost forwarded address that is
// not one of the operator's proxies; anything a client could have written
// itself is never read.
func TestClientAddress_ReadsForwardedForFromTrustedProxies(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("::1/128")}
	address := func(remote string, forwardedFor ...string) string {
		req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
		req.RemoteAddr = remote
		for _, value := range forwardedFor {
			req.Header.Add("X-Forwarded-For", value)
		}
		return clientAddress(req, trusted)
	}

	tests := []struct {
		name         string
		forwardedFor []string
		remote       string
		want         string
	}{
		{name: "a direct client's header is ignored", forwardedFor: []string{"198.51.100.7"}, remote: "192.0.2.1:1000", want: "192.0.2.1"},
		{name: "one trusted proxy", forwardedFor: []string{"198.51.100.7"}, remote: "10.0.0.2:1000", want: "198.51.100.7"},
		{name: "a chain of trusted proxies", forwardedFor: []string{"198.51.100.7, 10.0.0.3", "10.0.0.4"}, remote: "10.0.0.2:1000", want: "198.51.100.7"},
		{name: "a spoofed leftmost entry is not read", forwardedFor: []string{"203.0.113.9, 198.51.100.7"}, remote: "10.0.0.2:1000", want: "198.51.100.7"},
		{name: "every entry trusted means the leftmost", forwardedFor: []string{"10.0.0.9, 10.0.0.3"}, remote: "10.0.0.2:1000", want: "10.0.0.9"},
		{name: "no header means the proxy", remote: "10.0.0.2:1000", want: "10.0.0.2"},
		{name: "a malformed entry means the proxy", forwardedFor: []string{"198.51.100.7, unknown"}, remote: "10.0.0.2:1000", want: "10.0.0.2"},
		{name: "an entry with a port", forwardedFor: []string{"198.51.100.7:4711"}, remote: "10.0.0.2:1000", want: "198.51.100.7"},
		{name: "an IPv6 entry with a port", forwardedFor: []string{"[2001:db8:1:2::1]:4711"}, remote: "[::1]:1000", want: "2001:db8:1::/48"},
		{name: "an IPv6 client is reduced to its /48", forwardedFor: []string{"2001:db8:1:ffff::1"}, remote: "10.0.0.2:1000", want: "2001:db8:1::/48"},
		{name: "an IPv4-mapped proxy is trusted", forwardedFor: []string{"::ffff:198.51.100.7"}, remote: "[::ffff:10.0.0.2]:1000", want: "198.51.100.7"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, address(tc.remote, tc.forwardedFor...))
		})
	}
}

// Two clients behind one trusted proxy are spaced separately, which is the
// point of trusting it: otherwise one guessing client queues everyone else.
func TestAdminSignInAttemptsBehindATrustedProxyAreSpacedPerClient(t *testing.T) {
	credential, err := NewPassword("sample-password", "")
	require.NoError(t, err)
	panel, err := NewWithOptions(nil, nil, nil, nil, credential, slog.New(slog.DiscardHandler),
		Options{TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.2/32")}})
	require.NoError(t, err)
	handler := panel.Handler()

	attempt := func(client, password string) int {
		req := signInRequest(password)
		req.RemoteAddr = "10.0.0.2:1000"
		req.Header.Set("X-Forwarded-For", client)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}

	require.Equal(t, http.StatusUnauthorized, attempt("192.0.2.1", "wrong"))
	require.Equal(t, http.StatusTooManyRequests, attempt("192.0.2.1", "wrong"))
	require.Equal(t, http.StatusSeeOther, attempt("192.0.2.2", "sample-password"),
		"a second client behind the proxy waited behind the first")
}

func TestNewWithOptions_RefusesAnInvalidTrustedProxy(t *testing.T) {
	credential, err := NewPassword("sample-password", "")
	require.NoError(t, err)
	_, err = NewWithOptions(nil, nil, nil, nil, credential, slog.New(slog.DiscardHandler),
		Options{TrustedProxies: []netip.Prefix{{}}})
	require.ErrorContains(t, err, "trusted proxy")
}

func TestLoginLimiterSweepsOncePerDelay(t *testing.T) {
	var limiter loginLimiter
	now := time.Now()
	require.True(t, limiter.reserve("a", now))
	require.True(t, limiter.reserve("b", now.Add(loginDelay+1)), "a fresh address was refused")
	require.True(t, limiter.reserve("c", now.Add(loginDelay+2)))
	require.False(t, limiter.reserve("c", now.Add(loginDelay+3)), "an address inside its delay was admitted")
	require.Len(t, limiter.notBefore, 2, "the expired address was not swept, or the sweep dropped a live one")
}

// Prefix reports the path the panel is mounted under, with the default
// standing in for an empty option, so a host can build links to it.
func TestAdminReportsItsPrefix(t *testing.T) {
	credential, err := NewPassword("sample-password", "")
	require.NoError(t, err)
	build := func(options Options) string {
		panel, err := NewWithOptions(nil, nil, nil, nil, credential, slog.New(slog.DiscardHandler), options)
		require.NoError(t, err)
		return panel.Prefix()
	}
	require.Equal(t, DefaultPrefix, build(Options{}))
	require.Equal(t, "/ops/jacklet", build(Options{Prefix: "/ops/jacklet"}))
}
