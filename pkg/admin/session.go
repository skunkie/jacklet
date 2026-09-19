// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package admin

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"sync"
	"time"
)

// sessionCookie is the name of the cookie carrying a session token.
const sessionCookie = "jacklet_session"

// sessionLifetime is how long a signed-in session remains valid. There is
// no "remember me": the panel can change tracker credentials, so a
// forgotten open tab should stop being useful within a working day.
const sessionLifetime = 12 * time.Hour

// sessionIdleTimeout ends a session that has gone unused, independently of
// its absolute lifetime.
const sessionIdleTimeout = 1 * time.Hour

// tokenBytes is the entropy behind a session token and a CSRF token.
const tokenBytes = 32

// session is one signed-in browser session.
type session struct {
	csrfToken string
	expiresAt time.Time
	lastSeen  time.Time
}

// sessionStore holds the active sessions in memory. Sessions deliberately
// do not survive a restart: they are not worth persisting, and dropping
// them on restart is the safer default.
type sessionStore struct {
	mu       sync.Mutex
	sessions map[string]*session
}

func newSessionStore() *sessionStore {
	return &sessionStore{sessions: make(map[string]*session)}
}

// newToken returns a URL-safe random token, or an error if the system
// random source fails — in which case no session may be issued.
func newToken() (string, error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// create issues a new session and returns its token.
func (s *sessionStore) create() (token string, csrf string, err error) {
	token, err = newToken()
	if err != nil {
		return "", "", err
	}
	csrf, err = newToken()
	if err != nil {
		return "", "", err
	}

	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()

	s.pruneLocked(now)
	s.sessions[token] = &session{
		csrfToken: csrf,
		expiresAt: now.Add(sessionLifetime),
		lastSeen:  now,
	}
	return token, csrf, nil
}

// lookup returns the session for a token, refreshing its idle deadline. It
// reports ok=false for an unknown, expired, or idle-timed-out token.
func (s *sessionStore) lookup(token string) (*session, bool) {
	if token == "" {
		return nil, false
	}

	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()

	sess, ok := s.sessions[token]
	if !ok {
		return nil, false
	}
	if now.After(sess.expiresAt) || now.Sub(sess.lastSeen) > sessionIdleTimeout {
		delete(s.sessions, token)
		return nil, false
	}

	sess.lastSeen = now
	// Return a copy so a caller cannot mutate live session state.
	snapshot := *sess
	return &snapshot, true
}

// destroy ends one session.
func (s *sessionStore) destroy(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, token)
}

// pruneLocked drops expired sessions. The caller holds s.mu.
func (s *sessionStore) pruneLocked(now time.Time) {
	for token, sess := range s.sessions {
		if now.After(sess.expiresAt) || now.Sub(sess.lastSeen) > sessionIdleTimeout {
			delete(s.sessions, token)
		}
	}
}

// setSessionCookie issues the session cookie. forceSecure covers TLS
// termination at a reverse proxy, where the request reaching Jacklet itself
// is plain HTTP.
func setSessionCookie(w http.ResponseWriter, r *http.Request, token string, forceSecure bool) {
	//nolint:gosec // every security attribute is set; Secure reflects direct or proxy TLS
	http.SetCookie(w, &http.Cookie{
		HttpOnly: true,
		MaxAge:   int(sessionLifetime.Seconds()),
		Name:     sessionCookie,
		Path:     adminPrefix,
		SameSite: http.SameSiteLaxMode,
		Secure:   forceSecure || r.TLS != nil,
		Value:    token,
	})
}

// clearSessionCookie expires the session cookie in the browser.
func clearSessionCookie(w http.ResponseWriter, r *http.Request, forceSecure bool) {
	//nolint:gosec // every security attribute is set; Secure reflects direct or proxy TLS
	http.SetCookie(w, &http.Cookie{
		HttpOnly: true,
		MaxAge:   -1,
		Name:     sessionCookie,
		Path:     adminPrefix,
		SameSite: http.SameSiteLaxMode,
		Secure:   forceSecure || r.TLS != nil,
		Value:    "",
	})
}

// sessionToken reads the session token from the request's cookie.
func sessionToken(r *http.Request) string {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return ""
	}
	return c.Value
}

// equalTokens compares two tokens without leaking their contents through
// timing.
func equalTokens(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
