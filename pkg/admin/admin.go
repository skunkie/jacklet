// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

// Package admin serves Jacklet's web administration panel: indexer health,
// stored-result statistics, per-tracker settings and credentials, and a
// manual search for checking that a definition works.
package admin

import (
	"context"
	"embed"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/torrplay/jacklet/pkg/scraper"
	"github.com/torrplay/jacklet/pkg/torznab"
)

//go:embed templates/*.html
var templateFS embed.FS

// markOutline is the Jacklet mark, the same outline assets/logo.svg draws
// on its badge. The panel draws it bare and in the page's own text colour,
// so it carries the path rather than the file: an <img> would be a second
// request, and a second copy of the path in each template would be a
// drawing that could fall behind the master without anything saying so.
const markOutline = "M16 33A16 16 0 0 0 48 33V15H40V33A8 8 0 0 1 24 33V21H16Z"

// DefaultPrefix is the path the panel is mounted under unless
// Options.Prefix says otherwise. A sign-in returns
// the visitor to where they were asking for, and only paths inside this
// prefix are accepted as that destination.
const DefaultPrefix = "/admin"

// loginDelay is the least time between two sign-in attempts from one
// address. The panel has a single account, so a wrong password is either a
// typo or a guess; spacing them costs a human nothing and makes online
// guessing impractical.
const loginDelay = 500 * time.Millisecond

// Admin serves the administration panel. It is mounted under a path
// prefix and guards every route behind a session cookie.
type Admin struct {
	attempts         loginLimiter
	config           Config
	hasSecureCookies bool
	logger           *slog.Logger
	password         *Password
	prefix           string
	scrpr            *scraper.Scraper
	sessions         *sessionStore
	store            Store
	templates        *template.Template
	trackers         scraper.TrackerSource
	trustedProxies   []netip.Prefix
}

// Options configures deployment-specific panel behavior.
type Options struct {
	// DocsURL is the address of the "API docs" link in the navigation. Empty
	// leaves the link out, since only the host program knows whether it
	// serves API documentation anywhere.
	DocsURL string

	// FaviconURL is the address of the tab icon the pages declare. Empty
	// declares none, for the same reason. The pages content security
	// policy admits images from the panel's own origin only, so it should be a
	// path on the host, not an address elsewhere.
	FaviconURL string

	// Prefix is the path the panel is served under, such as "/ops/jacklet".
	// It must begin with a slash, must not end with one, and must be
	// a cleaned path with no query or fragment. Empty means DefaultPrefix.
	Prefix string

	// SecureCookies forces session cookies to be marked Secure when TLS is
	// terminated by a reverse proxy before requests reach Jacklet.
	SecureCookies bool

	// Title is the name shown in the header, the sign-in page and the
	// browser tab. Empty means "Jacklet".
	Title string

	// TrustedProxies are the reverse proxies whose X-Forwarded-For header
	// names the client, for spacing sign-in attempts per client rather than
	// per proxy. A request from any other address is counted against that
	// address and its header is ignored, since a client can send one itself.
	// A client inside a listed range is skipped like a proxy and can choose
	// the address it is counted under, so list the proxies' own addresses
	// rather than a network clients also sit in. Empty trusts no header.
	TrustedProxies []netip.Prefix
}

// Config is where the panel reads and writes per-indexer settings and
// credentials. *scraper.ConfigStore, which keeps them as files in a
// directory, satisfies it.
//
// The scraper reads the same settings through scraper.ConfigSource, so the
// scraper must be given the source the panel writes to, or a saved change
// never reaches a scrape. An implementation must be safe for concurrent use.
// The configtest package's Run checks an implementation against all of it.
type Config interface {
	scraper.ConfigSource
	// Enabled reports whether settings can be saved. A panel over a
	// disabled config shows the settings but offers no way to change them.
	Enabled() bool
	// Location describes where a tracker's settings are written, shown next
	// to the save button; "" when there is nowhere.
	Location(trackerID string) string
	// Save replaces a tracker's overrides. An empty map returns the tracker
	// to its definition's defaults.
	Save(trackerID string, overrides map[string]any) error
}

// Store is what the panel reads stored results and summary figures from: a
// torznab.Catalog for looking up and searching torrents, and the
// per-tracker figures the dashboard shows. *database.Store, the SQLite store
// Jacklet ships, satisfies it.
//
// The panel shows what a scrape stored, and a scrape's results reach the
// store only through the Sink of the scraper the panel is given, so that
// scraper must have been built with a Sink that writes where Store reads
// (scraper.Options.Sink). An implementation must be safe for concurrent use.
// The storetest package's Run checks an implementation against all of it.
type Store interface {
	torznab.Catalog
	// Stats returns the stored-torrent summary for every tracker present,
	// keyed by tracker id. A tracker never scraped has no entry. The
	// dashboard's total is the sum of these, and an error from here is what
	// it reports as an unreachable store.
	Stats(ctx context.Context) (map[string]scraper.TrackerStats, error)
}

// New creates the panel. password is the single admin credential; when it
// is unconfigured the panel is disabled and Enabled reports false, so a
// deployment that has not set one does not expose an unauthenticated way
// to read and write tracker credentials.
func New(store Store, scrpr *scraper.Scraper, config Config, trackers scraper.TrackerSource, password *Password, logger *slog.Logger) (*Admin, error) {
	return NewWithOptions(store, scrpr, config, trackers, password, logger, Options{})
}

// NewWithOptions creates the panel with deployment-specific options.
func NewWithOptions(store Store, scrpr *scraper.Scraper, config Config, trackers scraper.TrackerSource, password *Password, logger *slog.Logger, options Options) (*Admin, error) {
	if !scrpr.HasSink() {
		logger.Warn("the scraper has no Sink, so scrapes are not stored and the panel shows only what the store already holds; " +
			"build the scraper with scraper.Options.Sink set to the store the panel reads")
	}
	prefix, err := cleanPrefix(options.Prefix)
	if err != nil {
		return nil, err
	}

	trustedProxies := make([]netip.Prefix, 0, len(options.TrustedProxies))
	for _, proxy := range options.TrustedProxies {
		if !proxy.IsValid() {
			return nil, fmt.Errorf("trusted proxy %q is not a valid address range", proxy)
		}
		trustedProxies = append(trustedProxies, proxy.Masked())
	}

	templates, err := template.New("").Funcs(templateFuncs(prefix, options)).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}

	return &Admin{
		config:           config,
		hasSecureCookies: options.SecureCookies,
		logger:           logger,
		password:         password,
		prefix:           prefix,
		scrpr:            scrpr,
		sessions:         newSessionStore(),
		store:            store,
		templates:        templates,
		trackers:         trackers,
		trustedProxies:   trustedProxies,
	}, nil
}

// prefixBytes are the characters a prefix may contain. The set is what a
// cookie Path and a route pattern both carry unchanged: http.SetCookie drops
// other bytes from a Path, which would scope the session cookie to a path
// the routes are not served under.
const prefixBytes = "/abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._~-"

// cleanPrefix validates a configured mount path, returning DefaultPrefix for
// an empty one. A prefix that is not already in cleaned form would be
// registered as a route pattern that no request path can match, or would
// make the cookie scope and the redirect check disagree with the routes.
func cleanPrefix(prefix string) (string, error) {
	if prefix == "" {
		return DefaultPrefix, nil
	}
	if !strings.HasPrefix(prefix, "/") || prefix == "/" || path.Clean(prefix) != prefix ||
		strings.TrimLeft(prefix, prefixBytes) != "" {
		return "", fmt.Errorf("admin prefix %q must be a cleaned path such as %q, starting with a slash and not ending with one, and made of letters, digits and \"._~-/\" only", prefix, DefaultPrefix)
	}
	return prefix, nil
}

// Prefix is the path the panel is served under: Options.Prefix, or
// DefaultPrefix when that was empty.
func (a *Admin) Prefix() string {
	return a.prefix
}

// Enabled reports whether an admin password was configured. A disabled
// panel answers every request with 404, so it is indistinguishable from a
// build that has no panel at all.
func (a *Admin) Enabled() bool {
	return a.password.Configured()
}

// Handler returns the panel as an http.Handler, for a program that routes
// with something other than a *http.ServeMux. It answers only paths under
// the configured prefix, so mount it at that same path without stripping it
// (http.StripPrefix would leave the panel's links and cookie pointing at
// paths the outer router does not serve).
func (a *Admin) Handler() http.Handler {
	mux := http.NewServeMux()
	a.Routes(mux)
	return mux
}

// Routes registers the panel's handlers on mux under the configured prefix,
// "/admin" unless Options.Prefix says otherwise.
func (a *Admin) Routes(mux *http.ServeMux) {
	p := a.prefix
	mux.HandleFunc("GET "+p, a.guard(a.handleDashboard))
	mux.HandleFunc("GET "+p+"/{$}", a.guard(a.handleDashboard))
	mux.HandleFunc("GET "+p+"/login", a.handleLoginForm)
	mux.HandleFunc("POST "+p+"/login", a.handleLogin)
	mux.HandleFunc("POST "+p+"/logout", a.guard(a.handleLogout))
	mux.HandleFunc("GET "+p+"/indexers/{id}", a.guard(a.handleIndexer))
	mux.HandleFunc("POST "+p+"/indexers/{id}/settings", a.guard(a.handleSaveSettings))
	mux.HandleFunc("POST "+p+"/indexers/{id}/test", a.guard(a.handleTest))
	mux.HandleFunc("GET "+p+"/indexers/{id}/download/{row}", a.guard(a.handleDownload))
	mux.HandleFunc("GET "+p+"/search", a.guard(a.handleSearchForm))
	// Searching runs a live scrape, so it is a POST: a state-changing GET
	// would be reachable from another site, since SameSite=Lax still sends
	// the session cookie on a top-level navigation.
	mux.HandleFunc("POST "+p+"/search", a.guard(a.handleSearch))
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
			clearSessionCookie(w, r, a.hasSecureCookies, a.prefix)
			//nolint:gosec // G710: returnTo passes the path through safeNext, which admits only a cleaned path inside the prefix.
			http.Redirect(w, r, a.prefix+"/login"+a.returnTo(r), http.StatusSeeOther)
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

func templateFuncs(prefix string, options Options) template.FuncMap {
	title := options.Title
	if title == "" {
		title = "Jacklet"
	}
	return template.FuncMap{
		"docsURL":    func() string { return options.DocsURL },
		"faviconURL": func() string { return options.FaviconURL },
		"title":      func() string { return title },
		"prefix":     func() string { return prefix },
		"mark":       func() string { return markOutline },
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
func (a *Admin) returnTo(r *http.Request) string {
	if r.Method != http.MethodGet {
		return ""
	}
	next := safeNext(a.prefix, r.URL.RequestURI())
	if next == "" || next == a.prefix {
		return ""
	}
	return "?next=" + url.QueryEscape(next)
}
