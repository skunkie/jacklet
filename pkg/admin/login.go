// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package admin

import (
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

type loginPageData struct {
	Next    string
	Problem string
}

// safeNext returns the page to open after signing in, or "" to use the
// dashboard.
//
// The value arrives in a query string, so it is attacker-controlled: a
// value carrying a scheme or a host would turn the panel into an open
// redirect, sending someone who followed a Jacklet link to another site
// wearing Jacklet's name. Only a path inside the panel is accepted, and
// it is cleaned first so that "/admin/../.." cannot walk out of it.
func safeNext(raw string) string {
	if raw == "" {
		return ""
	}

	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "" || parsed.Host != "" {
		return ""
	}

	// The decoded path is what the browser resolves, so it is what has to
	// be cleaned and checked: "%2e%2e" survives path.Clean untouched in
	// its escaped form while still walking a directory up in the browser.
	cleaned := path.Clean(parsed.Path)
	if cleaned != adminPrefix && !strings.HasPrefix(cleaned, adminPrefix+"/") {
		return ""
	}

	// Rebuilt through url.URL so the path is escaped once, consistently.
	destination := url.URL{Path: cleaned, RawQuery: parsed.RawQuery}
	return destination.String()
}

func (a *Admin) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	if !a.Enabled() {
		http.NotFound(w, r)
		return
	}
	next := safeNext(r.URL.Query().Get("next"))
	if _, ok := a.sessions.lookup(sessionToken(r)); ok {
		//nolint:gosec // G710: next came from safeNext, which admits only a cleaned path inside adminPrefix.
		http.Redirect(w, r, destinationOr(next), http.StatusSeeOther)
		return
	}
	a.render(w, "login.html", loginPageData{Next: next})
}

func (a *Admin) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !a.Enabled() {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Malformed form", http.StatusBadRequest)
		return
	}

	// One attempt at a time, so the delay below cannot be sidestepped by
	// issuing guesses in parallel.
	a.loginMu.Lock()
	defer a.loginMu.Unlock()

	next := safeNext(r.PostFormValue("next"))

	if !a.password.Verify(r.PostFormValue("password")) {
		time.Sleep(loginDelay)
		a.logger.Warn("rejected an admin sign-in attempt", "remote", r.RemoteAddr)
		a.renderStatus(w, http.StatusUnauthorized, "login.html", loginPageData{Next: next, Problem: "Incorrect password."})
		return
	}

	token, _, err := a.sessions.create()
	if err != nil {
		a.logger.Error("failed to create an admin session", "error", err)
		http.Error(w, "Could not start a session", http.StatusInternalServerError)
		return
	}

	setSessionCookie(w, r, token, a.hasSecureCookies)
	a.logger.Info("admin signed in", "remote", r.RemoteAddr)
	//nolint:gosec // G710: next came from safeNext, which admits only a cleaned path inside adminPrefix.
	http.Redirect(w, r, destinationOr(next), http.StatusSeeOther)
}

func (a *Admin) handleLogout(w http.ResponseWriter, r *http.Request, _ *session) {
	a.sessions.destroy(sessionToken(r))
	clearSessionCookie(w, r, a.hasSecureCookies)
	http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
}

// destinationOr returns next, or the dashboard when there is none.
func destinationOr(next string) string {
	if next == "" {
		return adminPrefix
	}
	return next
}
