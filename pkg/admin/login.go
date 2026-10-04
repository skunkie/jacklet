// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package admin

import (
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"slices"
	"strings"
	"sync"
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
// it is cleaned first so that "<prefix>/../.." cannot walk out of it.
func safeNext(prefix, raw string) string {
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
	if cleaned != prefix && !strings.HasPrefix(cleaned, prefix+"/") {
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
	next := safeNext(a.prefix, r.URL.Query().Get("next"))
	if _, ok := a.sessions.lookup(sessionToken(r)); ok {
		//nolint:gosec // G710: next came from safeNext, which admits only a cleaned path inside the prefix.
		http.Redirect(w, r, a.destinationOr(next), http.StatusSeeOther)
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

	next := safeNext(a.prefix, r.PostFormValue("next"))

	// Attempts are spaced per client address, not across the panel: a global
	// limit would let anyone who can reach the panel keep the administrator's
	// own sign-in queued behind theirs. The slot is taken before the password
	// is checked, so parallel guesses from one address cannot slip through
	// while the first is still being verified.
	client := clientAddress(r, a.trustedProxies)
	if !a.attempts.reserve(client, time.Now()) {
		a.logger.Warn("throttled an admin sign-in attempt", "remote", r.RemoteAddr, "client", client)
		w.Header().Set("Retry-After", "1")
		a.renderStatus(w, http.StatusTooManyRequests, "login.html", loginPageData{Next: next, Problem: "Too many attempts. Wait a moment and try again."})
		return
	}

	if !a.password.Verify(r.PostFormValue("password")) {
		a.logger.Warn("rejected an admin sign-in attempt", "remote", r.RemoteAddr, "client", client)
		a.renderStatus(w, http.StatusUnauthorized, "login.html", loginPageData{Next: next, Problem: "Incorrect password."})
		return
	}
	a.attempts.release(client)

	// Signing in again ends the session this browser already held, so an
	// earlier token does not outlive the sign-in that replaced it.
	a.sessions.destroy(sessionToken(r))

	token, _, err := a.sessions.create()
	if err != nil {
		a.logger.Error("failed to create an admin session", "error", err)
		http.Error(w, "Could not start a session", http.StatusInternalServerError)
		return
	}

	setSessionCookie(w, r, token, a.hasSecureCookies, a.prefix)
	a.logger.Info("admin signed in", "remote", r.RemoteAddr, "client", client)
	//nolint:gosec // G710: next came from safeNext, which admits only a cleaned path inside the prefix.
	http.Redirect(w, r, a.destinationOr(next), http.StatusSeeOther)
}

func (a *Admin) handleLogout(w http.ResponseWriter, r *http.Request, _ *session) {
	a.sessions.destroy(sessionToken(r))
	clearSessionCookie(w, r, a.hasSecureCookies, a.prefix)
	http.Redirect(w, r, a.prefix+"/login", http.StatusSeeOther)
}

// destinationOr returns next, or the dashboard when there is none.
func (a *Admin) destinationOr(next string) string {
	if next == "" {
		return a.prefix
	}
	return next
}

// clientAddress names the client the limiter counts attempts against: the
// request's host, or the client a trusted proxy forwarded it for (see
// forwardedClient). An IPv6 client is reduced to its /48, the allocation a
// site is customarily given, so a client cannot claim a fresh slot for every
// guess by rotating through addresses, or through the /64s of a larger
// allocation, it owns.
func clientAddress(r *http.Request, trustedProxies []netip.Prefix) string {
	addrPort, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	addr := addrPort.Addr().Unmap()
	if isTrustedProxy(addr, trustedProxies) {
		addr = forwardedClient(r.Header.Values("X-Forwarded-For"), trustedProxies, addr)
	}
	if addr.Is6() {
		if prefix, err := addr.Prefix(48); err == nil {
			return prefix.String()
		}
	}
	return addr.String()
}

// forwardedClient reads the client out of the X-Forwarded-For values a
// trusted proxy at peer passed on. Each proxy appends the address it was
// reached from, so the list is read from the right: entries written by the
// operator's own proxies are skipped, and the first address that is not one
// of them is the client. The entries further left were sent by that client
// and could be anything, so they are never read while an untrusted one sits
// to their right; reading the leftmost would let a client pick a fresh slot
// for every guess. When every entry is a trusted proxy the leftmost is the
// client, and when there is no entry, or one that is not an address, the
// peer is.
func forwardedClient(headers []string, trustedProxies []netip.Prefix, peer netip.Addr) netip.Addr {
	entries := strings.Split(strings.Join(headers, ","), ",")
	client := peer
	for _, entry := range slices.Backward(entries) {
		addr, ok := parseForwardedAddr(entry)
		if !ok {
			return peer
		}
		client = addr
		if !isTrustedProxy(addr, trustedProxies) {
			break
		}
	}
	return client
}

// parseForwardedAddr reads one X-Forwarded-For entry, which some proxies
// write with the port they were reached from.
func parseForwardedAddr(entry string) (netip.Addr, bool) {
	entry = strings.TrimSpace(entry)
	if addr, err := netip.ParseAddr(entry); err == nil {
		return addr.Unmap(), true
	}
	if addrPort, err := netip.ParseAddrPort(entry); err == nil {
		return addrPort.Addr().Unmap(), true
	}
	return netip.Addr{}, false
}

func isTrustedProxy(addr netip.Addr, trustedProxies []netip.Prefix) bool {
	for _, proxy := range trustedProxies {
		if proxy.Contains(addr) {
			return true
		}
	}
	return false
}

// loginLimiter spaces sign-in attempts from one client address by
// loginDelay, tracking only addresses whose slot has not yet expired.
type loginLimiter struct {
	mu        sync.Mutex
	nextPrune time.Time
	notBefore map[string]time.Time
}

// reserve takes key's slot and reports true, or reports false when an
// attempt from key is still inside its delay.
func (l *loginLimiter) reserve(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.notBefore == nil {
		l.notBefore = make(map[string]time.Time)
	}
	if until, ok := l.notBefore[key]; ok && now.Before(until) {
		return false
	}
	// Expired entries are swept at most once per delay, so an attempt costs a
	// constant amount however many addresses are tracked; a stale entry
	// lingering until then is harmless, since the check above compares times.
	if !now.Before(l.nextPrune) {
		for other, until := range l.notBefore {
			if !now.Before(until) {
				delete(l.notBefore, other)
			}
		}
		l.nextPrune = now.Add(loginDelay)
	}
	l.notBefore[key] = now.Add(loginDelay)
	return true
}

// release frees key's slot after a correct password, so a successful
// sign-in does not delay the next one.
func (l *loginLimiter) release(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.notBefore, key)
}
