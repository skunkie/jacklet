// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

// Package admin serves Jacklet's web administration panel: indexer health,
// stored-result statistics, per-tracker settings and credentials, and a
// manual search for checking that a definition works.
package admin

import (
	"embed"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/torrplay/jacklet/pkg/database"
	"github.com/torrplay/jacklet/pkg/scraper"
)

//go:embed templates/*.html
var templateFS embed.FS

// markOutline is the Jacklet mark, the same outline assets/logo.svg draws
// on its badge. The panel draws it bare and in the page's own text colour,
// so it carries the path rather than the file: an <img> would be a second
// request, and a second copy of the path in each template would be a
// drawing that could fall behind the master without anything saying so.
const markOutline = "M16 33A16 16 0 0 0 48 33V15H40V33A8 8 0 0 1 24 33V21H16Z"

// adminPrefix is the path the panel is mounted under. A sign-in returns
// the visitor to where they were asking for, and only paths inside this
// prefix are accepted as that destination.
const adminPrefix = "/admin"

// loginPath is where an unauthenticated request is sent.
const loginPath = adminPrefix + "/login"

// loginDelay is applied to every failed sign-in. The panel has a single
// account, so a wrong password is either a typo or a guess; slowing both
// down costs a human nothing and makes online guessing impractical.
const loginDelay = 500 * time.Millisecond

// Admin serves the administration panel. It is mounted under a path
// prefix and guards every route behind a session cookie.
type Admin struct {
	config           *scraper.ConfigStore
	hasSecureCookies bool
	logger           *slog.Logger
	loginMu          sync.Mutex
	password         *Password
	scrpr            *scraper.Scraper
	sessions         *sessionStore
	store            *database.Store
	templates        *template.Template
	trackers         scraper.TrackerSource
}

// Options configures deployment-specific panel behavior.
type Options struct {
	// SecureCookies forces session cookies to be marked Secure when TLS is
	// terminated by a reverse proxy before requests reach Jacklet.
	SecureCookies bool
}

// New creates the panel. password is the single admin credential; when it
// is unconfigured the panel is disabled and Enabled reports false, so a
// deployment that has not set one does not expose an unauthenticated way
// to read and write tracker credentials.
func New(store *database.Store, scrpr *scraper.Scraper, config *scraper.ConfigStore, trackers scraper.TrackerSource, password *Password, logger *slog.Logger) (*Admin, error) {
	return NewWithOptions(store, scrpr, config, trackers, password, logger, Options{})
}

// NewWithOptions creates the panel with deployment-specific options.
func NewWithOptions(store *database.Store, scrpr *scraper.Scraper, config *scraper.ConfigStore, trackers scraper.TrackerSource, password *Password, logger *slog.Logger, options Options) (*Admin, error) {
	templates, err := template.New("").Funcs(templateFuncs()).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}

	return &Admin{
		config:           config,
		hasSecureCookies: options.SecureCookies,
		logger:           logger,
		password:         password,
		scrpr:            scrpr,
		sessions:         newSessionStore(),
		store:            store,
		templates:        templates,
		trackers:         trackers,
	}, nil
}

// Enabled reports whether an admin password was configured. A disabled
// panel answers every request with 404, so it is indistinguishable from a
// build that has no panel at all.
func (a *Admin) Enabled() bool {
	return a.password.Configured()
}

// Routes registers the panel's handlers on mux under the "/admin" prefix.
func (a *Admin) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin", a.guard(a.handleDashboard))
	mux.HandleFunc("GET /admin/{$}", a.guard(a.handleDashboard))
	mux.HandleFunc("GET /admin/login", a.handleLoginForm)
	mux.HandleFunc("POST /admin/login", a.handleLogin)
	mux.HandleFunc("POST /admin/logout", a.guard(a.handleLogout))
	mux.HandleFunc("GET /admin/indexers/{id}", a.guard(a.handleIndexer))
	mux.HandleFunc("POST /admin/indexers/{id}/settings", a.guard(a.handleSaveSettings))
	mux.HandleFunc("POST /admin/indexers/{id}/test", a.guard(a.handleTest))
	mux.HandleFunc("GET /admin/indexers/{id}/download/{row}", a.guard(a.handleDownload))
	mux.HandleFunc("GET /admin/search", a.guard(a.handleSearchForm))
	// Searching runs a live scrape, so it is a POST: a state-changing GET
	// would be reachable from another site, since SameSite=Lax still sends
	// the session cookie on a top-level navigation.
	mux.HandleFunc("POST /admin/search", a.guard(a.handleSearch))
}

// guard wraps a handler so it runs only for a signed-in session, and so a
// state-changing request also carries a matching CSRF token.
func (a *Admin) guard(next func(http.ResponseWriter, *http.Request, *session)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.Enabled() {
			http.NotFound(w, r)
			return
		}

		sess, ok := a.sessions.lookup(sessionToken(r))
		if !ok {
			// The browser holds a token the panel has no session for: it
			// aged out, or the process restarted and took the in-memory
			// store with it. Clear the cookie so the next request arrives
			// clean, and send every method to the sign-in form — 303 makes
			// the browser follow with a GET, so a submitted form lands on
			// the login page.
			clearSessionCookie(w, r, a.hasSecureCookies)
			//nolint:gosec // G710: returnTo passes the path through safeNext, which admits only a cleaned path inside adminPrefix.
			http.Redirect(w, r, loginPath+returnTo(r), http.StatusSeeOther)
			return
		}

		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if err := r.ParseForm(); err != nil {
				http.Error(w, "Malformed form", http.StatusBadRequest)
				return
			}
			if !equalTokens(r.PostFormValue("csrf_token"), sess.csrfToken) {
				a.logger.Warn("rejected admin request with a bad CSRF token", "path", r.URL.Path)
				http.Error(w, "Invalid CSRF token", http.StatusForbidden)
				return
			}
		}

		next(w, r, sess)
	}
}

// render writes a template with a 200, reporting a failure rather than
// leaving a half-written page to look like a success.
func (a *Admin) render(w http.ResponseWriter, name string, data any) {
	a.renderStatus(w, http.StatusOK, name, data)
}

// renderStatus writes a template under the given status code. The status
// is written here rather than by the caller because net/http discards
// headers set after WriteHeader, which would cost the page every
// protection set below.
func (a *Admin) renderStatus(w http.ResponseWriter, status int, name string, data any) {
	var buf strings.Builder
	if err := a.templates.ExecuteTemplate(&buf, name, data); err != nil {
		a.logger.Error("failed to render admin template", "template", name, "error", err)
		http.Error(w, "Failed to render page", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The pages load nothing from anywhere else, and the one image they
	// do load, the mark in the header and the tab, comes from this origin.
	w.Header().Set("Content-Security-Policy", "default-src 'none'; img-src 'self'; style-src 'unsafe-inline'; form-action 'self'")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// These pages are behind a session and show tracker configuration, so
	// they must not be kept in the browser's cache, where the back button
	// would still reach them after signing out.
	w.Header().Set("Cache-Control", "no-store, max-age=0")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(status)
	if _, err := w.Write([]byte(buf.String())); err != nil {
		a.logger.Error("failed to write admin page", "error", err)
	}
}

func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"mark": func() string { return markOutline },
		"since": func(t time.Time) string {
			if t.IsZero() {
				return "never"
			}
			return humanizeDuration(time.Since(t)) + " ago"
		},
		"bytes": humanizeBytes,
		"until": func(t time.Time) string {
			d := time.Until(t)
			if d <= 0 {
				return "now"
			}
			return humanizeDuration(d)
		},
	}
}

// humanizeBytes renders a byte count the way a person reads a torrent
// size, rather than as a raw number of bytes.
func humanizeBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}

	value := float64(n)
	for _, suffix := range []string{"KB", "MB", "GB", "TB", "PB"} {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.2f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%.2f EB", value/unit)
}

// humanizeDuration renders a duration at a granularity a person reads at
// a glance. Go's own formatting keeps every component, which turns three
// weeks into "504h0m0s".
func humanizeDuration(d time.Duration) string {
	if d < 0 {
		d = -d
	}

	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 365*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	default:
		years := d.Hours() / 24 / 365
		return fmt.Sprintf("%.1fy", years)
	}
}

// returnTo renders the query string that carries r's page to the sign-in
// form, so the visitor lands back where they were asking for.
//
// Only a GET names a page: the target of a form submission is a handler
// that answers a POST, which a browser following a redirect cannot ask
// for, so those sign in and arrive at the dashboard.
func returnTo(r *http.Request) string {
	if r.Method != http.MethodGet {
		return ""
	}
	next := safeNext(r.URL.RequestURI())
	if next == "" || next == adminPrefix {
		return ""
	}
	return "?next=" + url.QueryEscape(next)
}
