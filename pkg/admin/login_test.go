// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package admin

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torrplay/jacklet/pkg/database"
)

func TestSessionCookieCanBeForcedSecureBehindProxy(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://jacklet.internal/admin", http.NoBody)
	rec := httptest.NewRecorder()
	setSessionCookie(rec, req, "token", true)

	cookies := rec.Result().Cookies()
	require.Len(t, cookies, 1)
	require.True(t, cookies[0].Secure, "the cookie was not marked Secure")
	require.Equal(t, adminPrefix, cookies[0].Path)
}

func TestSafeNext(t *testing.T) {
	accepted := map[string]string{
		"/admin":                   "/admin",
		"/admin/":                  "/admin",
		"/admin/search":            "/admin/search",
		"/admin/indexers/demo":     "/admin/indexers/demo",
		"/admin/search?q=example":  "/admin/search?q=example",
		"/admin/indexers/demo/../": "/admin/indexers",
	}
	for raw, want := range accepted {
		require.Equal(t, want, safeNext(raw), "safeNext(%q)", raw)
	}

	// Anything that could send a visitor off this origin, or out of the
	// panel, resolves to the dashboard instead.
	rejected := []string{
		"",
		"https://evil.test/admin",
		"//evil.test/admin",
		"http://evil.test",
		"/api/v2.0/indexers/demo/results/torznab/api",
		"/healthz",
		"/adminsomething",
		"/admin/../torznab",
		"/admin/%2e%2e/%2e%2e/evil",
		"/admin/..%2f..%2fevil",
		"/admin/../../etc/passwd",
		"\\\\evil.test/admin",
		"javascript:alert(1)",
	}
	for _, raw := range rejected {
		require.Empty(t, safeNext(raw), "safeNext(%q) was not refused", raw)
	}
}

func TestMagnetHref(t *testing.T) {
	magnet := "magnet:?xt=urn:btih:0123456789abcdef&dn=Example+Release"
	for _, raw := range []string{magnet, "MAGNET:?xt=urn:btih:0123456789abcdef"} {
		require.Equal(t, raw, string(magnetHref(raw)), "magnetHref(%q) was not passed through", raw)
	}

	// Anything else reaches the template as an ordinary string, where the
	// sanitiser decides what an href may hold.
	for _, raw := range []string{
		"",
		"https://tracker.test/download/1.torrent",
		"http://tracker.test/download/1.torrent",
		"javascript:alert(1)",
		"data:text/html,<script>alert(1)</script>",
		"/relative/path.torrent",
		"://broken",
	} {
		require.Empty(t, magnetHref(raw), "magnetHref(%q) was not refused", raw)
	}
}

// TestResultLinkTemplate renders the shared result-link snippet, since a
// magnet only reaches the page as template.URL: html/template writes
// "#ZgotmplZ" over any scheme it does not recognise.
func TestResultLinkTemplate(t *testing.T) {
	panel, err := New(nil, nil, nil, nil, &Password{}, slog.New(slog.DiscardHandler))
	require.NoError(t, err)

	render := func(t *testing.T, name string, view resultView) string {
		t.Helper()
		var out strings.Builder
		require.NoError(t, panel.templates.ExecuteTemplate(&out, name, view))
		return out.String()
	}

	t.Run("a magnet link survives into the href", func(t *testing.T) {
		magnet := "magnet:?xt=urn:btih:0123456789abcdef&dn=Example+Release"
		got := render(t, "magnetLink", resultView{Magnet: magnetHref(magnet)})

		require.Contains(t, got, `href="magnet:?xt=urn:btih:0123456789abcdef`, "the magnet did not reach the href")
		require.NotContains(t, got, "ZgotmplZ", "the magnet was written over by the sanitiser")
		require.Contains(t, got, ">Magnet<", "the link is not labelled as a magnet")
	})

	t.Run("an http download goes through the panel's own proxy", func(t *testing.T) {
		got := render(t, "torrentLink", resultView{
			Download: "/admin/indexers/demo/download/7",
			Link:     "https://tracker.test/dl/1.torrent",
		})
		require.Contains(t, got, `href="/admin/indexers/demo/download/7"`, "the proxy link is missing")
		require.NotContains(t, got, `href="https://tracker.test`, "the tracker's own link was offered to the browser")
		require.Contains(t, got, ">Torrent<", "the link is not labelled as a torrent")
	})

	// The scraped link is shown as a title for diagnosis, where it is
	// ordinary escaped text, and never as somewhere to navigate to.
	t.Run("a scraped link is never an href on its own", func(t *testing.T) {
		got := render(t, "torrentLink", resultView{
			Download: downloadPath(database.Torrent{DownloadURL: "javascript:alert(1)", ID: 7, Tracker: "demo"}),
			Link:     "javascript:alert(1)",
		})
		require.NotContains(t, got, `href="javascript:`, "a javascript URL reached the href")
		require.Contains(t, got, `href="/admin/indexers/demo/download/7"`, "the proxy link is missing")
	})

	t.Run("a column with nothing to offer shows a placeholder", func(t *testing.T) {
		for _, name := range []string{"torrentLink", "magnetLink"} {
			require.Contains(t, render(t, name, resultView{}), "—", "%s has no placeholder", name)
		}
	})

	t.Run("each column shows only its own link", func(t *testing.T) {
		view := resultView{Download: "/admin/indexers/demo/download/7", Magnet: magnetHref("magnet:?xt=urn:btih:abc")}
		require.NotContains(t, render(t, "torrentLink", view), "magnet:", "the torrent column carried a magnet")
		require.NotContains(t, render(t, "magnetLink", view), "/download/7", "the magnet column carried a proxy link")
	})
}

func TestDownloadPath(t *testing.T) {
	row := database.Torrent{DownloadURL: "https://tracker.test/dl/1.torrent", ID: 42, Tracker: "demo"}
	require.Equal(t, "/admin/indexers/demo/download/42", downloadPath(row))

	// A tracker id reaches the path, so it is escaped.
	odd := database.Torrent{DownloadURL: "https://tracker.test/dl/1.torrent", ID: 1, Tracker: "a/b c"}
	require.Equal(t, "/admin/indexers/a%2Fb%20c/download/1", downloadPath(odd))

	// A row with no torrent file has nowhere to point. Its magnet, when it
	// has one, is offered in its own column.
	magnetOnly := database.Torrent{ID: 1, Magnet: "magnet:?xt=urn:btih:0123456789abcdef", Tracker: "demo"}
	require.Empty(t, downloadPath(magnetOnly), "a row with no torrent file has nowhere to point")
}
