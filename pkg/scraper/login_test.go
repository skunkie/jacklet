// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torrplay/jacklet/pkg/database"
)

func TestLoginHelpers(t *testing.T) {
	def := &Tracker{ID: "example", Name: "Example Tracker"}
	require.ErrorIs(t, requireCredentials(def, url.Values{"password": {"   "}}), ErrCredentialsMissing)
	require.NoError(t, requireCredentials(def, url.Values{"username": {"user"}}),
		"requireCredentials rejected a populated input")

	scrpr := New(nil, NewConfigStore(""), "", testLogger())
	base, err := url.Parse("https://tracker.example/")
	require.NoError(t, err)

	require.ErrorIs(t, scrpr.loginWithCookie(def, base, map[string]any{}), ErrCredentialsMissing)
	require.NoError(t, scrpr.loginWithCookie(def, base, map[string]any{"cookie": "session=abc; theme=dark"}))

	cookies := scrpr.httpClient.Jar.Cookies(base)
	require.Len(t, cookies, 2, "want the session and theme cookies")
	require.Equal(t, "session", cookies[0].Name)
	require.Equal(t, "abc", cookies[0].Value)
}

// loginSite is a tracker that requires a session cookie, serves a login
// form carrying a CSRF token, and counts how often each route is hit.
type loginSite struct {
	logins    atomic.Int32
	server    *httptest.Server
	tests     atomic.Int32
	wantPass  string
	wantToken string
}

func newLoginSite(t *testing.T) *loginSite {
	t.Helper()
	site := &loginSite{wantPass: "s3cret", wantToken: "csrf-42"}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /login.php", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<form id="loginform" action="/takelogin.php">
			<input name="token" value="%s">
			<input name="username" value="">
			<input name="password" value="">
		</form>`, site.wantToken)
	})
	mux.HandleFunc("POST /takelogin.php", func(w http.ResponseWriter, r *http.Request) {
		site.logins.Add(1)
		_ = r.ParseForm()
		if r.PostForm.Get("password") != site.wantPass || r.PostForm.Get("token") != site.wantToken {
			fmt.Fprint(w, `<div class="error">Invalid username or password</div>`)
			return
		}
		// Secure is deliberately unset: this test tracker is served over
		// plain HTTP by httptest, and a secure cookie would never be sent
		// back.
		//nolint:gosec // G124: test server, not a production Set-Cookie
		http.SetCookie(w, &http.Cookie{
			HttpOnly: true,
			Name:     "session",
			Path:     "/",
			SameSite: http.SameSiteLaxMode,
			Value:    "ok",
		})
		fmt.Fprint(w, `<p>Welcome</p>`)
	})
	mux.HandleFunc("GET /index.php", func(w http.ResponseWriter, r *http.Request) {
		site.tests.Add(1)
		if c, err := r.Cookie("session"); err == nil && c.Value == "ok" {
			fmt.Fprint(w, `<a href="/logout.php">Logout</a>`)
			return
		}
		fmt.Fprint(w, `<a href="/login.php">Login</a>`)
	})
	mux.HandleFunc("GET /browse.php", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("session"); err != nil || c.Value != "ok" {
			fmt.Fprint(w, `<p>Please log in</p>`)
			return
		}
		fmt.Fprint(w, `<div class="row"><a href="/d/1">Private Release</a></div>`)
	})

	site.server = httptest.NewServer(mux)
	t.Cleanup(site.server.Close)
	return site
}

// newLoginTracker writes a definition for site, with the given credential
// overrides placed in a config directory.
func newLoginTracker(t *testing.T, site *loginSite, password string) (*Scraper, *Tracker) {
	t.Helper()

	dir := t.TempDir()
	def := loadTestTracker(t, dir, "private", `
id: private
name: Private Tracker
links:
  - `+site.server.URL+`/
settings:
  - name: username
    type: text
    default: ""
  - name: password
    type: password
    default: ""
login:
  path: login.php
  method: form
  form: form#loginform
  inputs:
    username: "{{ .Config.username }}"
    password: "{{ .Config.password }}"
  error:
    - selector: div.error
      message:
        selector: div.error
  test:
    path: index.php
    selector: a[href*="logout"]
search:
  paths:
    - path: browse.php
      method: get
  rows:
    selector: div.row
  fields:
    title:
      selector: a
    download:
      selector: a
      attribute: href
`)

	configDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		configDir+"/private.yml",
		fmt.Appendf(nil, "username: someone\npassword: %s\n", password),
		0o600,
	))

	db, err := database.Open(context.Background(), ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	return New(db, NewConfigStore(configDir), "", testLogger()), def
}

func TestScraper_LogsInBeforeSearching(t *testing.T) {
	site := newLoginSite(t)
	scrpr, def := newLoginTracker(t, site, site.wantPass)

	require.NoError(t, scrpr.ScrapeIndexer(context.Background(), def, SearchParams{}))

	stored := storedTorrents(t, scrpr, def)
	require.Len(t, stored, 1)
	require.Equal(t, "Private Release", stored[0].Name, "the authenticated page should have been scraped")
	require.Equal(t, int32(1), site.logins.Load())
}

func TestScraper_ReusesAnEstablishedSession(t *testing.T) {
	site := newLoginSite(t)
	scrpr, def := newLoginTracker(t, site, site.wantPass)

	require.NoError(t, scrpr.ScrapeIndexer(context.Background(), def, SearchParams{}))

	// Clear the rate limiter so the second call really re-scrapes.
	scrpr.scrapeState = make(map[string]*scrapeState)
	require.NoError(t, scrpr.ScrapeIndexer(context.Background(), def, SearchParams{}))

	require.Equal(t, int32(1), site.logins.Load(), "a cached session must not re-submit credentials")
}

func TestScraper_ReportsRejectedCredentials(t *testing.T) {
	site := newLoginSite(t)
	scrpr, def := newLoginTracker(t, site, "wrong-password")

	err := scrpr.ScrapeIndexer(context.Background(), def, SearchParams{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "Invalid username or password")
}

func TestScraper_ReportsMissingCredentials(t *testing.T) {
	site := newLoginSite(t)
	scrpr, def := newLoginTracker(t, site, "")

	err := scrpr.ScrapeIndexer(context.Background(), def, SearchParams{})
	require.ErrorIs(t, err, ErrCredentialsMissing)
}

func TestScraper_ReportsCaptchaAsUnsupported(t *testing.T) {
	site := newLoginSite(t)
	scrpr, def := newLoginTracker(t, site, site.wantPass)
	def.Login.Captcha = &Captcha{}

	err := scrpr.ScrapeIndexer(context.Background(), def, SearchParams{})
	require.ErrorIs(t, err, ErrCaptchaRequired)
	require.Equal(t, int32(0), site.logins.Load(), "no credentials should be sent for a CAPTCHA login")
}

func TestScraper_NoLoginBlockSkipsAuthentication(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<div class="row"><a href="/d/1">Public Release</a></div>`)
	}))
	defer server.Close()

	dir := t.TempDir()
	def := loadTestTracker(t, dir, "public", `
id: public
name: Public
links:
  - `+server.URL+`/
search:
  paths:
    - path: "/"
      method: get
  rows:
    selector: div.row
  fields:
    title:
      selector: a
    download:
      selector: a
      attribute: href
`)

	store, err := database.Open(context.Background(), ":memory:")
	require.NoError(t, err)
	defer store.Close()

	scrpr := New(store, NewConfigStore(""), "", testLogger())
	require.NoError(t, scrpr.ScrapeIndexer(context.Background(), def, SearchParams{}))

	stored := storedTorrents(t, scrpr, def)
	require.Len(t, stored, 1)
	require.Equal(t, "Public Release", stored[0].Name)
}

// Only the captcha block's presence is modelled, so a real definition's
// captcha keys must still decode into the empty struct and leave the
// pointer non-nil.
func TestCaptchaBlockIsDetectedFromYAML(t *testing.T) {
	dir := t.TempDir()

	t.Run("a declared captcha is detected", func(t *testing.T) {
		def := loadTestTracker(t, dir, "captcha-site", `
id: captcha-site
name: Captcha Site
links:
  - http://tracker.example/
login:
  path: login.php
  method: form
  captcha:
    type: image
    selector: img#captcha
    input: captcha_code
search:
  paths:
    - path: browse.php
  rows:
    selector: .row
  fields:
    title:
      selector: a
`)
		require.NotNil(t, def.Login)
		require.NotNil(t, def.Login.Captcha, "a captcha block should decode to a non-nil pointer")
	})

	t.Run("no captcha block leaves it nil", func(t *testing.T) {
		def := loadTestTracker(t, dir, "plain-site", `
id: plain-site
name: Plain Site
links:
  - http://tracker.example/
login:
  path: login.php
  method: form
search:
  paths:
    - path: browse.php
  rows:
    selector: .row
  fields:
    title:
      selector: a
`)
		require.NotNil(t, def.Login)
		require.Nil(t, def.Login.Captcha)
	})
}
