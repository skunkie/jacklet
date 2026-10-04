// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/PuerkitoBio/goquery"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoginHelpers(t *testing.T) {
	def := &Tracker{ID: "example", Name: "Example Tracker"}
	require.ErrorIs(t, requireCredentials(def, url.Values{"password": {"   "}}), ErrCredentialsMissing)
	require.NoError(t, requireCredentials(def, url.Values{"username": {"user"}}),
		"requireCredentials rejected a populated input")

	scrpr := New(NewConfigStore(""), "", slog.New(slog.DiscardHandler))
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

	db := &fakeStore{}

	return NewWithOptions(NewConfigStore(configDir), "", slog.New(slog.DiscardHandler), Options{Sink: db}), def
}

// A form login's selector inputs read their values off the login page, as
// a field reads its row, and are submitted with the form: a required one
// that matches nothing fails the login before anything is sent, and an
// optional one is left out.
func TestScraperLoginSelectorInputs(t *testing.T) {
	const page = `<html><head><script>var options = {stKey: "token-123",};</script></head>
		<body><form id="loginform" action="takelogin.php"><input name="username"></form>
		<input name="cookie_test" value="Sample Value"></body></html>`

	for _, tc := range []struct {
		name    string
		inputs  string
		want    url.Values
		wantErr string
	}{
		{
			name: "required and optional inputs",
			inputs: `    securitytoken:
      selector: "script:contains(\"stKey: \")"
      filters:
        - name: regexp
          args: "stKey: \"(.+?)\","
    cookie_test:
      selector: input[name="cookie_test"]
      attribute: value
    absent:
      selector: input[name="absent"]
      attribute: value
      optional: true
`,
			want: url.Values{"cookie_test": {"Sample Value"}, "securitytoken": {"token-123"}, "username": {"someone"}},
		},
		{
			name: "a required input that matches nothing",
			inputs: `    absent:
      selector: input[name="absent"]
      attribute: value
`,
			wantErr: `login selector input "absent" matched nothing`,
		},
		{
			// Optional or not, as in Jackett.
			name: "an input naming a variable never set",
			inputs: `    cookie_test:
      selector: input[name="cookie_test"]
      attribute: value
      optional: true
      filters:
        - name: append
          args: "{{ .Config.undeclared }}"
`,
			wantErr: `login selector input "cookie_test" names a variable never set`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotForm url.Values
			mux := http.NewServeMux()
			mux.HandleFunc("GET /login.php", func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, page)
			})
			mux.HandleFunc("POST /takelogin.php", func(_ http.ResponseWriter, r *http.Request) {
				assert.NoError(t, r.ParseForm())
				gotForm = r.PostForm
			})
			server := httptest.NewServer(mux)
			defer server.Close()

			def := loadTestTracker(t, t.TempDir(), "selector-inputs", `
id: selector-inputs
name: Selector Inputs
links:
  - `+server.URL+`/
login:
  path: login.php
  method: form
  form: form#loginform
  inputs:
    username: someone
  selectorinputs:
`+tc.inputs+`search:
  paths:
    - path: browse.php
  rows:
    selector: div.row
  fields:
    title:
      selector: a
`)
			scrpr := New(NewConfigStore(""), "", slog.New(slog.DiscardHandler))
			baseURL, err := url.Parse(server.URL + "/")
			require.NoError(t, err)

			err = scrpr.performLogin(t.Context(), def, baseURL, defaultConfig(def))
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				require.Nil(t, gotForm, "the form was submitted without its required input")
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, gotForm)
		})
	}
}

// With "selectors" set, a form login's input keys are CSS selectors for the
// inputs they fill, submitted under those inputs' names, as Jackett does
// for a page whose names change while its ids or placeholders do not.
func TestScraperLoginInputSelectors(t *testing.T) {
	const page = `<html><body><form action="takelogin.php">
		<input id="username" name="username">
		<input placeholder="Password" name="p_8f2a">
		<input id="unnamed">
	</form></body></html>`

	for _, tc := range []struct {
		name     string
		inputs   string
		username string
		want     url.Values
		wantErr  string
	}{
		{
			name:     "inputs found by id and placeholder",
			inputs:   "    input[id=\"username\"]: \"{{ .Config.username }}\"\n    input[placeholder=\"Password\"]: secret\n",
			username: "someone",
			want:     url.Values{"p_8f2a": {"secret"}, "username": {"someone"}},
		},
		{
			name:    "a selector that matches nothing",
			inputs:  "    input[id=\"missing\"]: someone\n",
			wantErr: `login input "input[id=\"missing\"]" matched nothing`,
		},
		{
			name:    "an input without a name",
			inputs:  "    input[id=\"unnamed\"]: someone\n",
			wantErr: `login input "input[id=\"unnamed\"]" has no name`,
		},
		{
			name:    "a blank username found by its selector",
			inputs:  "    input[id=\"username\"]: \"{{ .Config.username }}\"\n",
			wantErr: ErrCredentialsMissing.Error(),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotForm url.Values
			mux := http.NewServeMux()
			mux.HandleFunc("GET /login.php", func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, page)
			})
			mux.HandleFunc("POST /takelogin.php", func(_ http.ResponseWriter, r *http.Request) {
				assert.NoError(t, r.ParseForm())
				gotForm = r.PostForm
			})
			server := httptest.NewServer(mux)
			defer server.Close()

			def := loadTestTracker(t, t.TempDir(), "input-selectors", `
id: input-selectors
name: Input Selectors
links:
  - `+server.URL+`/
settings:
  - name: username
    type: text
login:
  path: login.php
  method: form
  selectors: true
  inputs:
`+tc.inputs+`search:
  paths:
    - path: browse.php
  rows:
    selector: div.row
  fields:
    title:
      selector: a
`)
			cfg := defaultConfig(def)
			cfg["username"] = tc.username
			scrpr := New(NewConfigStore(""), "", slog.New(slog.DiscardHandler))
			baseURL, err := url.Parse(server.URL + "/")
			require.NoError(t, err)

			err = scrpr.performLogin(t.Context(), def, baseURL, cfg)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				require.Nil(t, gotForm, "the form was submitted after its inputs failed")
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, gotForm)
		})
	}
}

// A form login whose definition names no form uses the page's first one,
// as Jackett does, and adopts only the inputs a browser would submit: not
// a disabled one, nor a checkbox or radio button left unchecked.
func TestScraperFormLoginUsesTheFirstForm(t *testing.T) {
	const page = `<html><body>
		<form action="takelogin.php">
			<input name="username">
			<input name="token" value="Sample Token">
			<input name="legacy" value="old" disabled>
			<input type="checkbox" name="remember" value="yes" checked>
			<input type="checkbox" name="newsletter" value="yes">
			<input type="radio" name="scope" value="mine" checked>
			<input type="RADIO" name="scope" value="all">
		</form>
		<form action="search.php"><input name="q"></form>
	</body></html>`

	var gotForm url.Values
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login.php", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, page)
	})
	mux.HandleFunc("POST /takelogin.php", func(_ http.ResponseWriter, r *http.Request) {
		assert.NoError(t, r.ParseForm())
		gotForm = r.PostForm
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	def := loadTestTracker(t, t.TempDir(), "first-form", `
id: first-form
name: First Form
links:
  - `+server.URL+`/
login:
  path: login.php
  method: form
  inputs:
    username: someone
search:
  paths:
    - path: browse.php
  rows:
    selector: div.row
  fields:
    title:
      selector: a
`)
	scrpr := New(NewConfigStore(""), "", slog.New(slog.DiscardHandler))
	baseURL, err := url.Parse(server.URL + "/")
	require.NoError(t, err)

	require.NoError(t, scrpr.performLogin(t.Context(), def, baseURL, defaultConfig(def)))
	require.Equal(t, url.Values{
		"remember": {"yes"},
		"scope":    {"mine"},
		"token":    {"Sample Token"},
		"username": {"someone"},
	}, gotForm)
}

// A post or form login sends the cookies its definition declares, as
// Jackett does: "JAVA=OK" gets past a tracker's check that the browser runs
// scripts, which otherwise redirects the login page away.
func TestScraperLoginSendsDeclaredCookies(t *testing.T) {
	for _, method := range []string{"post", "form"} {
		t.Run(method, func(t *testing.T) {
			var gotLoginCookie string
			mux := http.NewServeMux()
			mux.HandleFunc("GET /login.php", func(w http.ResponseWriter, r *http.Request) {
				if c, err := r.Cookie("JAVA"); err != nil || c.Value != "OK" {
					http.Redirect(w, r, "/jscheck.php", http.StatusFound)
					return
				}
				fmt.Fprint(w, `<form action="takelogin.php"><input name="username"></form>`)
			})
			mux.HandleFunc("POST /takelogin.php", func(_ http.ResponseWriter, r *http.Request) {
				if c, err := r.Cookie("JAVA"); err == nil {
					gotLoginCookie = c.Value
				}
			})
			server := httptest.NewServer(mux)
			defer server.Close()

			path := "takelogin.php"
			if method == "form" {
				path = "login.php"
			}
			def := loadTestTracker(t, t.TempDir(), "login-cookies", `
id: login-cookies
name: Login Cookies
links:
  - `+server.URL+`/
login:
  path: `+path+`
  method: `+method+`
  cookies: ["JAVA=OK"]
  inputs:
    username: someone
search:
  paths:
    - path: browse.php
  rows:
    selector: div.row
  fields:
    title:
      selector: a
`)
			scrpr := New(NewConfigStore(""), "", slog.New(slog.DiscardHandler))
			baseURL, err := url.Parse(server.URL + "/")
			require.NoError(t, err)

			require.NoError(t, scrpr.performLogin(t.Context(), def, baseURL, defaultConfig(def)))
			require.Equal(t, "OK", gotLoginCookie, "the login was sent without the declared cookie")
		})
	}
}

func TestScraperLogsInBeforeSearching(t *testing.T) {
	site := newLoginSite(t)
	scrpr, def := newLoginTracker(t, site, site.wantPass)

	require.NoError(t, scrpr.scrapeIndexer(t.Context(), def, SearchParams{}))

	stored := storedTorrents(t, scrpr, def)
	require.Len(t, stored, 1)
	require.Equal(t, "Private Release", stored[0].Name, "the authenticated page should have been scraped")
	require.Equal(t, int32(1), site.logins.Load())
}

func TestScraperReusesAnEstablishedSession(t *testing.T) {
	site := newLoginSite(t)
	scrpr, def := newLoginTracker(t, site, site.wantPass)

	require.NoError(t, scrpr.scrapeIndexer(t.Context(), def, SearchParams{}))

	// Clear the rate limiter so the second call really re-scrapes.
	scrpr.clearScrapeState()
	require.NoError(t, scrpr.scrapeIndexer(t.Context(), def, SearchParams{}))

	require.Equal(t, int32(1), site.logins.Load(), "a cached session must not re-submit credentials")
}

func TestScraperReportsRejectedCredentials(t *testing.T) {
	site := newLoginSite(t)
	scrpr, def := newLoginTracker(t, site, "wrong-password")

	err := scrpr.scrapeIndexer(t.Context(), def, SearchParams{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "Invalid username or password")
}

func TestScraperReportsMissingCredentials(t *testing.T) {
	site := newLoginSite(t)
	scrpr, def := newLoginTracker(t, site, "")

	err := scrpr.scrapeIndexer(t.Context(), def, SearchParams{})
	require.ErrorIs(t, err, ErrCredentialsMissing)
}

func TestScraperReportsCaptchaAsUnsupported(t *testing.T) {
	site := newLoginSite(t)
	scrpr, def := newLoginTracker(t, site, site.wantPass)
	def.Login.Captcha = &Captcha{}

	err := scrpr.scrapeIndexer(t.Context(), def, SearchParams{})
	require.ErrorIs(t, err, ErrCaptchaRequired)
	require.Equal(t, int32(0), site.logins.Load(), "no credentials should be sent for a CAPTCHA login")
}

func TestScraperNoLoginBlockSkipsAuthentication(t *testing.T) {
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

	store := &fakeStore{}

	scrpr := NewWithOptions(NewConfigStore(""), "", slog.New(slog.DiscardHandler), Options{Sink: store})
	require.NoError(t, scrpr.scrapeIndexer(t.Context(), def, SearchParams{}))

	stored := storedTorrents(t, scrpr, def)
	require.Len(t, stored, 1)
	require.Equal(t, "Public Release", stored[0].Name)
}

// Only the captcha block's presence is modeled, so a real definition's
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

// A login reads its form, its selector-keyed inputs, its test and its
// error blocks through the same selector adaptation as a search, so a
// definition writing an attribute value unquoted logs in as in Jackett.
// The values are ones cascadia refuses unquoted, such as a number; an
// unquoted identifier it already accepts.
func TestScraperLoginAdaptsSelectors(t *testing.T) {
	var gotForm url.Values
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login.php", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<html><body><form data-id="2" action="takelogin.php">
			<input data-role="1" name="u_1"><input name="password"></form></body></html>`)
	})
	mux.HandleFunc("POST /takelogin.php", func(_ http.ResponseWriter, r *http.Request) {
		assert.NoError(t, r.ParseForm())
		gotForm = r.PostForm
	})
	mux.HandleFunc("GET /index.php", func(w http.ResponseWriter, _ *http.Request) {
		if gotForm == nil {
			fmt.Fprint(w, `<a href="login.php">Log in</a>`)
			return
		}
		fmt.Fprint(w, `<a href="logout.php">Log out</a>`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	def := loadTestTracker(t, t.TempDir(), "unquoted-login", `
id: unquoted-login
name: Unquoted Login
links:
  - `+server.URL+`/
login:
  path: login.php
  method: form
  form: form[data-id=2]
  selectors: true
  inputs:
    input[data-role=1]: someone
    input[name=password]: secret
  test:
    path: index.php
    selector: a[href=logout.php]
search:
  paths:
    - path: browse.php
  rows:
    selector: div.row
  fields:
    title:
      selector: a
`)
	scrpr := New(NewConfigStore(""), "", slog.New(slog.DiscardHandler))
	baseURL, err := url.Parse(server.URL + "/")
	require.NoError(t, err)

	require.NoError(t, scrpr.ensureLoggedIn(t.Context(), def, baseURL, defaultConfig(def)))
	require.Equal(t, url.Values{"password": {"secret"}, "u_1": {"someone"}}, gotForm)

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(`<div data-kind="3"><b data-part="4">Sample failure</b></div>`))
	require.NoError(t, err)
	err = checkErrorBlocks(def, doc, []ErrorBlock{{Message: Field{Selector: "b[data-part=4]"}, Selector: "div[data-kind=3]"}}, slog.New(slog.DiscardHandler))
	require.ErrorContains(t, err, "Sample failure")
}
