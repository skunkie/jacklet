// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"time"
)

// flareSolverrCookie is a cookie as FlareSolverr reports the browser's:
// Selenium's own shape, with the expiry in seconds since the epoch.
type flareSolverrCookie struct {
	Domain   string  `json:"domain"`
	Expiry   float64 `json:"expiry"`
	HTTPOnly bool    `json:"httpOnly"`
	Name     string  `json:"name"`
	Path     string  `json:"path"`
	Secure   bool    `json:"secure"`
	Value    string  `json:"value"`
}

// httpCookie converts a browser cookie for a jar, as it was set on host. A
// browser reports the domain even for a bare IP address, which a cookie jar
// refuses, so it is dropped there and the cookie stays with that host.
func (c flareSolverrCookie) httpCookie(host string) *http.Cookie {
	//nolint:gosec // G124: an outgoing client cookie being loaded into a jar, not a Set-Cookie being issued
	cookie := &http.Cookie{
		Domain:   c.Domain,
		HttpOnly: c.HTTPOnly,
		Name:     c.Name,
		Path:     c.Path,
		Secure:   c.Secure,
		Value:    c.Value,
	}
	if c.Expiry > 0 {
		cookie.Expires = time.Unix(int64(c.Expiry), 0)
	}
	if net.ParseIP(host) != nil {
		cookie.Domain = ""
	}
	return cookie
}

// browserPageURL is the address the browser's cookies belong to: the page it
// finished on, which FlareSolverr reports, since a tracker often redirects
// and a host-only cookie of the page it landed on does not match the address
// that was requested. The requested address stands when that one is missing,
// does not parse, or is not a web address, such as the browser's error page.
func browserPageURL(requested *url.URL, final string) *url.URL {
	landed, err := url.Parse(final)
	if err != nil || landed.Hostname() == "" || (landed.Scheme != "http" && landed.Scheme != "https") {
		return requested
	}
	return landed
}

// rememberFlareSolverrBrowser keeps what the tracker's browser held after a
// request: its cookies and the user agent it presented. A download does not
// go through FlareSolverr, since that returns a page as text and would
// corrupt a torrent, so it has to present the browser's identity itself: a
// form login leaves the session in the browser's cookies and not in Jacklet's
// jar, and a Cloudflare clearance is bound to the user agent that earned it.
func (s *Scraper) rememberFlareSolverrBrowser(session *flareSession, pageURL *url.URL, cookies []flareSolverrCookie, userAgent string) {
	s.flareSessionsMu.Lock()
	if session.browserJar == nil {
		jar, _ := cookiejar.New(nil) // New never fails with nil options.
		session.browserJar = jar
	}
	jar := session.browserJar
	if userAgent != "" {
		session.browserUserAgent = userAgent
	}
	s.flareSessionsMu.Unlock()

	converted := make([]*http.Cookie, 0, len(cookies))
	for _, cookie := range cookies {
		converted = append(converted, cookie.httpCookie(pageURL.Hostname()))
	}
	jar.SetCookies(pageURL, converted)
}

// flareSolverrBrowser returns the cookie jar and user agent the tracker's
// browser last held, or nil and "" when it has none: the tracker has not been
// fetched through FlareSolverr, or its session has since been retired.
func (s *Scraper) flareSolverrBrowser(trackerID string) (http.CookieJar, string) {
	s.flareSessionsMu.Lock()
	defer s.flareSessionsMu.Unlock()

	session, ok := s.flareSessions[trackerID]
	if !ok || session.browserJar == nil {
		return nil, ""
	}
	return session.browserJar, session.browserUserAgent
}

// layeredJar is a cookie jar that reads from several: the first one to
// have a cookie of a name wins. Writes go to the last, where a jar's own
// cookies already live.
type layeredJar []http.CookieJar

func (j layeredJar) Cookies(u *url.URL) []*http.Cookie {
	var out []*http.Cookie
	seen := map[string]bool{}
	for _, jar := range j {
		for _, cookie := range jar.Cookies(u) {
			if !seen[cookie.Name] {
				seen[cookie.Name] = true
				out = append(out, cookie)
			}
		}
	}
	return out
}

func (j layeredJar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	j[len(j)-1].SetCookies(u, cookies)
}
