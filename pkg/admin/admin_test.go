// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package admin_test

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/torrplay/jacklet/pkg/admin"
	"github.com/torrplay/jacklet/pkg/database"
	"github.com/torrplay/jacklet/pkg/scraper"
)

const testPassword = "correct-horse-battery-staple"

// panelFixture is a running admin panel with its own definitions and
// config directories.
type panelFixture struct {
	configDir string
	defsDir   string
	mux       *http.ServeMux
	store     *database.Store
}

// storeTorrent puts a torrent in the fixture's store. Fields left unset
// take the zero value, so each test states only what it asserts on.
func (f *panelFixture) storeTorrent(t *testing.T, torrent database.Torrent) {
	t.Helper()
	if torrent.Tracker == "" {
		torrent.Tracker = "demo"
	}
	require.NoError(t, f.store.Upsert(context.Background(), torrent), "failed to store %q", torrent.Name)
}

func (f *panelFixture) storeTorrentForSearch(t *testing.T, torrent database.Torrent, params scraper.SearchParams) {
	t.Helper()
	if torrent.Tracker == "" {
		torrent.Tracker = "demo"
	}
	_, err := f.store.UpsertAllForSearch(context.Background(), []database.Torrent{torrent}, params.ResultKey())
	require.NoError(t, err, "failed to store %q for search", torrent.Name)
}

func newPanel(t *testing.T, password string) *panelFixture {
	t.Helper()
	return newPanelWith(t, password, "")
}

// newPanelWithHash builds a panel authenticating against a stored hash.
func newPanelWithHash(t *testing.T, hash string) *panelFixture {
	t.Helper()
	return newPanelWith(t, "", hash)
}

func newPanelWith(t *testing.T, password, hash string) *panelFixture {
	t.Helper()

	store, err := database.Open(context.Background(), ":memory:")
	require.NoError(t, err, "failed to open database")
	t.Cleanup(func() { store.Close() })

	defsDir := t.TempDir()
	configDir := t.TempDir()

	def := `
id: demo
name: Demo Tracker
language: en-US
type: private
settings:
  - name: username
    type: text
    label: Username
    default: ""
  - name: password
    type: password
    label: Password
    default: ""
  - name: sort
    type: select
    label: Sort
    default: added
    options:
      added: Added
      seeders: Seeders
caps:
  categorymappings:
    - {id: 1, cat: "Movies"}
login:
  path: login.php
  method: post
  inputs:
    username: "{{ .Config.username }}"
    password: "{{ .Config.password }}"
search:
  paths:
    - path: browse.php
  rows:
    selector: .row
  fields:
    title:
      selector: a
`
	require.NoError(t, os.WriteFile(filepath.Join(defsDir, "demo.yml"), []byte(def), 0o600),
		"failed to write definition")

	logger := slog.New(slog.DiscardHandler)
	config := scraper.NewConfigStore(configDir)
	scrpr := scraper.New(store, config, "", logger)
	trackers := scraper.NewDefinitionStore(defsDir, logger)

	credential, err := admin.NewPassword(password, hash)
	require.NoError(t, err, "failed to build credential")

	panel, err := admin.New(store, scrpr, config, trackers, credential, logger)
	require.NoError(t, err, "failed to create panel")

	mux := http.NewServeMux()
	if panel.Enabled() {
		panel.Routes(mux)
	}
	return &panelFixture{configDir: configDir, defsDir: defsDir, mux: mux, store: store}
}

// signIn logs in and returns the session cookie and the CSRF token lifted
// from a rendered page.
func (f *panelFixture) signIn(t *testing.T) (*http.Cookie, string) {
	t.Helper()

	form := url.Values{"password": {testPassword}}
	req := httptest.NewRequest(http.MethodPost, "/admin/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)

	require.Equal(t, http.StatusSeeOther, rec.Code, "sign-in failed: %s", rec.Body.String())

	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "jacklet_session" {
			cookie = c
		}
	}
	require.NotNil(t, cookie, "sign-in set no session cookie")

	body := f.get(t, "/admin", cookie)
	m := regexp.MustCompile(`name="csrf_token" value="([^"]+)"`).FindStringSubmatch(body)
	require.NotNil(t, m, "no CSRF token found on the dashboard")
	return cookie, m[1]
}

func (f *panelFixture) get(t *testing.T, path string, cookie *http.Cookie) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, http.NoBody)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	return rec.Body.String()
}

func (f *panelFixture) post(t *testing.T, path string, form url.Values, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	return rec
}

// Without a password the panel must not exist at all.
func TestAdmin_DisabledWithoutPassword(t *testing.T) {
	f := newPanel(t, "")
	for _, path := range []string{"/admin", "/admin/login", "/admin/search"} {
		req := httptest.NewRequest(http.MethodGet, path, http.NoBody)
		rec := httptest.NewRecorder()
		f.mux.ServeHTTP(rec, req)
		require.Equal(t, http.StatusNotFound, rec.Code, "%s is served with no password set", path)
	}
}

func TestAdmin_RequiresSignIn(t *testing.T) {
	f := newPanel(t, testPassword)

	t.Run("a page redirects to the login form", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/admin", http.NoBody)
		rec := httptest.NewRecorder()
		f.mux.ServeHTTP(rec, req)
		require.Equal(t, http.StatusSeeOther, rec.Code)
		require.Equal(t, "/admin/login", rec.Header().Get("Location"))
	})

	t.Run("a write also goes to the login form", func(t *testing.T) {
		rec := f.post(t, "/admin/indexers/demo/settings", url.Values{}, nil)
		require.Equal(t, http.StatusSeeOther, rec.Code)
		require.Equal(t, "/admin/login", rec.Header().Get("Location"))
	})

	// A session lives in memory, so every token predates a restart. The
	// cookie is cleared as well, so the browser stops presenting one the
	// panel cannot place.
	t.Run("a token the panel does not know is cleared", func(t *testing.T) {
		//nolint:gosec // G124: a cookie the browser presents, not one the panel sets.
		stale := &http.Cookie{Name: "jacklet_session", Value: "a-token-from-a-previous-process"}

		req := httptest.NewRequest(http.MethodGet, "/admin", http.NoBody)
		req.AddCookie(stale)
		rec := httptest.NewRecorder()
		f.mux.ServeHTTP(rec, req)

		require.Equal(t, http.StatusSeeOther, rec.Code)
		require.Equal(t, "/admin/login", rec.Header().Get("Location"))

		var cleared bool
		for _, c := range rec.Result().Cookies() {
			if c.Name == "jacklet_session" && c.Value == "" && c.MaxAge < 0 {
				cleared = true
			}
		}
		require.True(t, cleared, "the stale session cookie was left in the browser")
	})
}

func TestAdmin_LoginFormAndIndexerTestFailure(t *testing.T) {
	f := newPanel(t, testPassword)

	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/login?next=/admin", http.NoBody))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "Sign in")

	cookie, csrf := f.signIn(t)
	rec = f.post(t, "/admin/indexers/demo/test", url.Values{
		"csrf_token": {csrf},
		"query":      {"example"},
	}, cookie)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "Test failed:", "the indexer test reported no failure")
}

func TestAdmin_RejectsWrongPassword(t *testing.T) {
	f := newPanel(t, testPassword)

	form := url.Values{"password": {"wrong"}}
	rec := f.post(t, "/admin/login", form, nil)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	for _, c := range rec.Result().Cookies() {
		if c.Name == "jacklet_session" {
			require.Empty(t, c.Value, "a failed sign-in issued a session cookie")
		}
	}
}

func TestAdmin_SessionCookieIsHardened(t *testing.T) {
	f := newPanel(t, testPassword)
	cookie, _ := f.signIn(t)

	require.True(t, cookie.HttpOnly, "session cookie is not HttpOnly")
	require.Equal(t, http.SameSiteLaxMode, cookie.SameSite, "session cookie SameSite")
	require.GreaterOrEqual(t, len(cookie.Value), 32, "session token is too short")
}

// A state-changing request must carry the session's CSRF token.
func TestAdmin_RejectsMissingCSRFToken(t *testing.T) {
	f := newPanel(t, testPassword)
	cookie, csrf := f.signIn(t)

	t.Run("without a token", func(t *testing.T) {
		rec := f.post(t, "/admin/indexers/demo/settings", url.Values{"username": {"someone"}}, cookie)
		require.Equal(t, http.StatusForbidden, rec.Code)
	})

	t.Run("with a wrong token", func(t *testing.T) {
		form := url.Values{"csrf_token": {"not-the-token"}, "username": {"someone"}}
		rec := f.post(t, "/admin/indexers/demo/settings", form, cookie)
		require.Equal(t, http.StatusForbidden, rec.Code)
	})

	t.Run("with the right token", func(t *testing.T) {
		form := url.Values{"csrf_token": {csrf}, "username": {"someone"}}
		rec := f.post(t, "/admin/indexers/demo/settings", form, cookie)
		require.Equal(t, http.StatusSeeOther, rec.Code, "body: %s", rec.Body.String())
	})
}

func TestAdmin_SignOutEndsTheSession(t *testing.T) {
	f := newPanel(t, testPassword)
	cookie, csrf := f.signIn(t)

	rec := f.post(t, "/admin/logout", url.Values{"csrf_token": {csrf}}, cookie)
	require.Equal(t, http.StatusSeeOther, rec.Code, "sign-out")

	req := httptest.NewRequest(http.MethodGet, "/admin", http.NoBody)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusSeeOther, rec.Code, "the old session still works")
}

// Saving credentials must write them to the tracker's config file.
func TestAdmin_SavesSettings(t *testing.T) {
	f := newPanel(t, testPassword)
	cookie, csrf := f.signIn(t)

	form := url.Values{
		"csrf_token": {csrf},
		"password":   {"s3cret"},
		"sort":       {"seeders"},
		"username":   {"someone"},
	}
	rec := f.post(t, "/admin/indexers/demo/settings", form, cookie)
	require.Equal(t, http.StatusSeeOther, rec.Code, "save failed: %s", rec.Body.String())

	saved, err := os.ReadFile(filepath.Join(f.configDir, "demo.yml"))
	require.NoError(t, err, "no config file was written")
	for _, want := range []string{"username: someone", "password: s3cret", "sort: seeders"} {
		require.Contains(t, string(saved), want, "config file is missing a saved setting")
	}

	// Windows files carry no Unix permission bits, so there is nothing to
	// assert there; on every other platform the saved credentials must not
	// be readable beyond their owner.
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(f.configDir, "demo.yml"))
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "the config file holds credentials")
	}
}

// A stored password must never be rendered back into the page.
func TestAdmin_NeverRendersStoredSecrets(t *testing.T) {
	f := newPanel(t, testPassword)
	cookie, csrf := f.signIn(t)

	form := url.Values{"csrf_token": {csrf}, "password": {"s3cret"}, "username": {"someone"}}
	f.post(t, "/admin/indexers/demo/settings", form, cookie)

	page := f.get(t, "/admin/indexers/demo", cookie)
	require.NotContains(t, page, "s3cret", "the stored password was rendered into the page")
	require.Contains(t, page, "someone", "the non-secret username should still be shown")
}

// Re-saving without retyping a password must keep the stored one, not
// blank it out.
func TestAdmin_KeepsSecretWhenResubmittedMasked(t *testing.T) {
	f := newPanel(t, testPassword)
	cookie, csrf := f.signIn(t)

	first := url.Values{"csrf_token": {csrf}, "password": {"s3cret"}, "username": {"someone"}}
	f.post(t, "/admin/indexers/demo/settings", first, cookie)

	// The form re-submits the mask the page rendered.
	second := url.Values{"csrf_token": {csrf}, "password": {"••••••••"}, "username": {"someone-else"}}
	rec := f.post(t, "/admin/indexers/demo/settings", second, cookie)
	require.Equal(t, http.StatusSeeOther, rec.Code, "second save")

	saved, err := os.ReadFile(filepath.Join(f.configDir, "demo.yml"))
	require.NoError(t, err)
	require.Contains(t, string(saved), "password: s3cret", "the stored password was lost")
	require.Contains(t, string(saved), "username: someone-else", "the username edit was not saved")
}

func TestAdmin_DashboardShowsIndexers(t *testing.T) {
	f := newPanel(t, testPassword)
	cookie, _ := f.signIn(t)

	f.storeTorrent(t, database.Torrent{DownloadURL: "magnet:?xt=1", Name: "Stored Release", Published: "2024-01-01T00:00:00Z"})

	page := f.get(t, "/admin", cookie)
	for _, want := range []string{"Demo Tracker", "demo", "Stored torrents"} {
		require.Contains(t, page, want, "dashboard is missing an expected section")
	}
}

// scrollWrapper matches a scroll container's opening tag together with the
// table it must wrap, allowing only whitespace between the two. The class may
// sit anywhere in the tag, so that reordering attributes does not fail the
// test; the capture is the whole attribute list.
var scrollWrapper = regexp.MustCompile(`<div([^>]*\bclass="scroll"[^>]*)>\s*<table`)

// The result tables carry six columns. Left to themselves they set the width
// of the page on a phone, so the whole document scrolls sideways and the
// header goes with it; instead each one is wrapped in a container that
// scrolls on its own. The wrapper is focusable because a scroll container
// that cannot take focus cannot be scrolled by keyboard, which would leave a
// keyboard-only reader unable to reach the columns it has cut off.
func TestAdmin_WideTablesScrollInsideTheirCard(t *testing.T) {
	f := newPanel(t, testPassword)
	cookie, csrf := f.signIn(t)

	f.storeTorrentForSearch(t,
		database.Torrent{DownloadURL: "magnet:?xt=1", Name: "Big Release"},
		scraper.SearchParams{Query: "Big", Type: "search"})
	search := url.Values{"csrf_token": {csrf}, "indexer": {"demo"}, "q": {"Big"}}

	dashboard := f.get(t, "/admin", cookie)
	results := f.post(t, "/admin/search", search, cookie).Body.String()

	// The wrapper is inert markup without the rule that makes it scroll, and
	// the panel carries its stylesheet inline, so the served page has it.
	require.Regexp(t, `\.scroll\s*\{[^}]*overflow-x:\s*auto`, dashboard,
		"the stylesheet no longer makes the scroll container scroll")

	for _, page := range []struct {
		name string
		body string
	}{
		{"dashboard", dashboard},
		{"search results", results},
	} {
		t.Run(page.name, func(t *testing.T) {
			require.Contains(t, page.body, "<table", "the page rendered no table to wrap")

			match := scrollWrapper.FindStringSubmatch(page.body)
			require.NotNil(t, match,
				"the table is not wrapped in a scroll container, so it will widen the whole page on a narrow screen")

			attributes := match[1]
			require.Contains(t, attributes, `tabindex="0"`,
				"the scroll container cannot take focus, so a keyboard reader cannot scroll to a cut-off column")
			require.Contains(t, attributes, `role="region"`,
				"the scroll container is not announced as a region")
			require.Contains(t, attributes, `aria-label="`,
				"the scroll container has no accessible name")
		})
	}
}

func TestAdmin_ReportsDefinitionErrors(t *testing.T) {
	f := newPanel(t, testPassword)
	cookie, _ := f.signIn(t)

	require.NoError(t, os.WriteFile(filepath.Join(f.defsDir, "broken.yml"), []byte("\tnope: [\n"), 0o600))

	page := f.get(t, "/admin", cookie)
	require.Contains(t, page, "Definition errors")
	require.Contains(t, page, "broken.yml")
}

func TestAdmin_UnknownIndexerIsNotFound(t *testing.T) {
	f := newPanel(t, testPassword)
	cookie, _ := f.signIn(t)

	req := httptest.NewRequest(http.MethodGet, "/admin/indexers/absent", http.NoBody)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

// The panel renders values from definitions and the database; they must be
// escaped rather than injected into the page.
func TestAdmin_EscapesDefinitionContent(t *testing.T) {
	f := newPanel(t, testPassword)
	cookie, _ := f.signIn(t)

	def := fmt.Sprintf(`
id: xss
name: %s
search:
  paths:
    - path: "/"
  rows:
    selector: .row
  fields:
    title:
      selector: a
`, `"><script>alert(1)</script>`)
	require.NoError(t, os.WriteFile(filepath.Join(f.defsDir, "xss.yml"), []byte(def), 0o600))

	page := f.get(t, "/admin", cookie)
	require.NotContains(t, page, "<script>alert(1)</script>", "a definition's name was rendered unescaped")
}

// Searching scrapes the tracker, so it must be a POST carrying the CSRF
// token rather than a GET another site could trigger.
func TestAdmin_SearchRequiresPostWithCSRF(t *testing.T) {
	f := newPanel(t, testPassword)
	cookie, csrf := f.signIn(t)

	t.Run("GET only renders the form", func(t *testing.T) {
		page := f.get(t, "/admin/search?indexer=demo&q=anything", cookie)
		require.NotContains(t, page, "Results", "a GET ran a search")
		require.Contains(t, page, "Manual search", "the search form was not rendered")
	})

	t.Run("POST without a token is refused", func(t *testing.T) {
		rec := f.post(t, "/admin/search", url.Values{"indexer": {"demo"}}, cookie)
		require.Equal(t, http.StatusForbidden, rec.Code)
	})

	t.Run("POST with a token runs the search", func(t *testing.T) {
		form := url.Values{"csrf_token": {csrf}, "indexer": {"demo"}, "q": {"anything"}}
		rec := f.post(t, "/admin/search", form, cookie)
		require.Equal(t, http.StatusOK, rec.Code)
		// The tracker is unreachable in tests, so the page should say so
		// rather than silently showing nothing.
		require.Contains(t, rec.Body.String(), "Scrape failed", "the page did not report the failed scrape")
	})
}

// The panel's manual search must find stored results regardless of case,
// including for non-ASCII titles.
func TestAdmin_SearchMatchesNonASCIICaseInsensitively(t *testing.T) {
	f := newPanel(t, testPassword)
	cookie, csrf := f.signIn(t)

	for _, term := range []string{"тестовый", "Тестовый", "ТЕСТОВЫЙ"} {
		f.storeTorrentForSearch(t,
			database.Torrent{DownloadURL: "magnet:?xt=1", Name: "Тестовый Релиз (2009) BDRip"},
			scraper.SearchParams{Query: term, Type: "search"})
		t.Run(term, func(t *testing.T) {
			form := url.Values{"csrf_token": {csrf}, "indexer": {"demo"}, "q": {term}}
			rec := f.post(t, "/admin/search", form, cookie)
			require.Equal(t, http.StatusOK, rec.Code)
			require.Contains(t, rec.Body.String(), "Тестовый Релиз (2009) BDRip",
				"searching %q did not find the stored title", term)
		})
	}
}

func TestHumanizeBytesRendersInSearchResults(t *testing.T) {
	f := newPanel(t, testPassword)
	cookie, csrf := f.signIn(t)

	f.storeTorrentForSearch(t,
		database.Torrent{DownloadURL: "magnet:?xt=1", Name: "Big Release", Size: 33987351504},
		scraper.SearchParams{Query: "Big", Type: "search"})

	form := url.Values{"csrf_token": {csrf}, "indexer": {"demo"}, "q": {"Big"}}
	body := f.post(t, "/admin/search", form, cookie).Body.String()
	require.Contains(t, body, "31.65 GB", "size was not rendered in human-readable form")
	require.NotContains(t, body, "33987351504", "the raw byte count is still shown")
}

func TestDashboardRendersReadableAges(t *testing.T) {
	f := newPanel(t, testPassword)
	cookie, _ := f.signIn(t)

	// A release a few weeks old must not render as Go's raw duration.
	published := time.Now().Add(-23 * 24 * time.Hour).UTC().Format(time.RFC3339)
	f.storeTorrent(t, database.Torrent{DownloadURL: "magnet:?xt=1", Name: "Older Release", Published: published})

	page := f.get(t, "/admin", cookie)
	require.NotContains(t, page, "h0m0s", "an age was rendered in Go's raw duration format")
	require.Contains(t, page, "23d ago")
}

// Panel pages sit behind a session and show tracker configuration, so the
// browser must not retain them after signing out.
func TestAdmin_PagesAreNotCacheable(t *testing.T) {
	f := newPanel(t, testPassword)
	cookie, _ := f.signIn(t)

	for _, path := range []string{"/admin", "/admin/search", "/admin/indexers/demo"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, http.NoBody)
			req.AddCookie(cookie)
			rec := httptest.NewRecorder()
			f.mux.ServeHTTP(rec, req)

			require.Contains(t, rec.Header().Get("Cache-Control"), "no-store", "Cache-Control")
		})
	}
}

// A session that the server no longer knows must be refused, which is what
// happens to every session after a restart.
func TestAdmin_RejectsUnknownSessionToken(t *testing.T) {
	f := newPanel(t, testPassword)

	req := httptest.NewRequest(http.MethodGet, "/admin", http.NoBody)
	// An inbound request cookie carries only a name and value; the
	// attributes gosec looks for belong to a Set-Cookie response.
	//nolint:gosec // G124: request cookie, not a Set-Cookie
	req.AddCookie(&http.Cookie{Name: "jacklet_session", Value: "a-token-from-a-previous-process"})
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)

	require.Equal(t, http.StatusSeeOther, rec.Code)
	require.Equal(t, "/admin/login", rec.Header().Get("Location"))
}

func TestAdmin_ReturnsToTheRequestedPage(t *testing.T) {
	f := newPanel(t, testPassword)

	t.Run("a guarded page is carried to the sign-in form", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/admin/search", http.NoBody)
		rec := httptest.NewRecorder()
		f.mux.ServeHTTP(rec, req)

		require.Equal(t, "/admin/login?next=%2Fadmin%2Fsearch", rec.Header().Get("Location"))
	})

	t.Run("a write carries nothing, since a browser cannot re-ask for it", func(t *testing.T) {
		rec := f.post(t, "/admin/indexers/demo/settings", url.Values{}, nil)
		require.Equal(t, "/admin/login", rec.Header().Get("Location"))
	})

	t.Run("signing in lands on the page that was asked for", func(t *testing.T) {
		form := url.Values{"password": {testPassword}, "next": {"/admin/search"}}
		rec := f.post(t, "/admin/login", form, nil)
		require.Equal(t, "/admin/search", rec.Header().Get("Location"))
	})

	t.Run("a hostile destination is ignored", func(t *testing.T) {
		form := url.Values{"password": {testPassword}, "next": {"https://evil.test/"}}
		rec := f.post(t, "/admin/login", form, nil)
		require.Equal(t, "/admin", rec.Header().Get("Location"))
	})

	t.Run("the destination survives a wrong password", func(t *testing.T) {
		form := url.Values{"password": {"wrong"}, "next": {"/admin/search"}}
		rec := f.post(t, "/admin/login", form, nil)
		require.Contains(t, rec.Body.String(), `name="next" value="/admin/search"`,
			"the sign-in form dropped the destination after a failed attempt")
	})
}

func TestAdmin_DownloadProxy(t *testing.T) {
	f := newPanel(t, testPassword)
	cookie, _ := f.signIn(t)

	fetch := func(t *testing.T, path string, c *http.Cookie) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, http.NoBody)
		if c != nil {
			req.AddCookie(c)
		}
		rec := httptest.NewRecorder()
		f.mux.ServeHTTP(rec, req)
		return rec
	}

	t.Run("it is behind the session like every other page", func(t *testing.T) {
		rec := fetch(t, "/admin/indexers/demo/download/1", nil)
		require.Equal(t, http.StatusSeeOther, rec.Code, "want a redirect to the sign-in form")
	})

	t.Run("an unknown indexer is refused", func(t *testing.T) {
		require.Equal(t, http.StatusNotFound, fetch(t, "/admin/indexers/nosuch/download/1", cookie).Code)
	})

	t.Run("a row id that is not a number is refused", func(t *testing.T) {
		require.Equal(t, http.StatusBadRequest, fetch(t, "/admin/indexers/demo/download/notanumber", cookie).Code)
	})

	// The row is looked up under the indexer in the path, so one
	// indexer's page cannot reach another's rows.
	t.Run("a row the indexer does not have is refused", func(t *testing.T) {
		require.Equal(t, http.StatusNotFound, fetch(t, "/admin/indexers/demo/download/999999", cookie).Code)
	})
}

// The panel shows the Jacklet mark in its header, drawn inline so it
// takes the page's own text colour, and again as the tab icon, which is
// fetched. The fetched one is why the CSP has to admit an image at all:
// under default-src 'none' alone the browser blocks it with the markup
// still correct.
func TestAdmin_ShowsLogo(t *testing.T) {
	f := newPanel(t, testPassword)
	cookie, _ := f.signIn(t)

	for _, page := range []struct {
		auth *http.Cookie
		name string
		path string
	}{
		{name: "login", path: "/admin/login"},
		{auth: cookie, name: "dashboard", path: "/admin"},
	} {
		t.Run(page.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, page.path, http.NoBody)
			if page.auth != nil {
				req.AddCookie(page.auth)
			}
			rec := httptest.NewRecorder()
			f.mux.ServeHTTP(rec, req)

			require.Equal(t, http.StatusOK, rec.Code)
			require.Contains(t, rec.Body.String(), `<svg class="mark"`, "the page shows no mark")
			require.Contains(t, rec.Body.String(), `fill="currentColor"`, "the mark does not follow the page's text colour")
			require.Contains(t, rec.Body.String(), `rel="icon" href="/favicon.svg"`, "the page declares no tab icon")

			csp := rec.Result().Header.Get("Content-Security-Policy")
			require.Contains(t, csp, "img-src 'self'", "the CSP blocks the mark the page references")
		})
	}
}

// Every rendered page carries the panel's security headers, whatever its
// status code. net/http discards headers set after WriteHeader, so a
// handler that writes its own status before rendering serves the page
// with none of them.
func TestAdmin_SecurityHeadersOnEveryStatus(t *testing.T) {
	f := newPanel(t, testPassword)
	cookie, _ := f.signIn(t)

	assertHeaders := func(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int) {
		t.Helper()
		require.Equal(t, wantStatus, rec.Code)
		require.Contains(t, rec.Body.String(), "<html", "not an admin page, so the headers below prove nothing")
		// Result() reports the headers as they went on the wire, taken
		// at WriteHeader time. The recorder's live map keeps writes made
		// after it too, which would satisfy an assertion here without
		// ever reaching a client.
		got := rec.Result().Header
		require.Equal(t,
			"default-src 'none'; img-src 'self'; style-src 'unsafe-inline'; form-action 'self'",
			got.Get("Content-Security-Policy"))
		require.Equal(t, "nosniff", got.Get("X-Content-Type-Options"))
		require.Equal(t, "same-origin", got.Get("Referrer-Policy"))
		require.Equal(t, "no-store, max-age=0", got.Get("Cache-Control"))
	}

	t.Run("200 login page", func(t *testing.T) {
		rec := httptest.NewRecorder()
		f.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/login", http.NoBody))
		assertHeaders(t, rec, http.StatusOK)
	})

	t.Run("401 wrong password", func(t *testing.T) {
		rec := f.post(t, "/admin/login", url.Values{"password": {"wrong"}}, nil)
		assertHeaders(t, rec, http.StatusUnauthorized)
	})

	t.Run("200 dashboard", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/admin", http.NoBody)
		req.AddCookie(cookie)
		f.mux.ServeHTTP(rec, req)
		assertHeaders(t, rec, http.StatusOK)
	})
}

// The panel draws the mark from its own constant, since a package cannot
// embed a file outside its directory. That makes the constant a second
// copy of a drawing whose master is assets/logo.svg, and this is what
// holds it to that master.
func TestAdmin_MarkMatchesTheMasterLogo(t *testing.T) {
	master := filepath.Join("..", "..", "assets", "logo.svg")
	data, err := os.ReadFile(master)
	require.NoError(t, err)

	// The master draws the badge first and knocks the mark out of it, so
	// the mark is the second path.
	outlines := regexp.MustCompile(`<path[^>]*\sd="([^"]+)"`).FindAllStringSubmatch(string(data), -1)
	require.Len(t, outlines, 2, "%s no longer draws a badge and a mark", master)

	rec := httptest.NewRecorder()
	f := newPanel(t, testPassword)
	f.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/login", http.NoBody))

	require.Contains(t, rec.Body.String(), outlines[1][1],
		"the mark the panel draws has drifted from %s; copy its second path into markOutline", master)
}
