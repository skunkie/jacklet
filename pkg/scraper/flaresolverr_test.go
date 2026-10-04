// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeFlareSolverr is an in-memory FlareSolverr: it creates and destroys
// sessions, tracks which are still alive, and records how many requests
// were in flight, on one session and overall. failNext makes the next
// page requests answer with an error, as a challenge that did not solve does.
type fakeFlareSolverr struct {
	beforePage        func()
	concurrent        int
	created           int
	delay             time.Duration
	destroyed         []string
	failMessage       string
	failNext          int
	inFlight          map[string]int
	listCalls         int
	live              map[string]bool
	maxConcurrent     int
	maxInFlight       int
	mu                sync.Mutex
	page              string
	pageRequests      []map[string]any
	rawReply          string
	requests          int
	shouldFailList    bool
	solutionCookies   []map[string]any
	solutionURL       string
	solutionUserAgent string
}

func newFakeFlareSolverr(page string) *fakeFlareSolverr {
	return &fakeFlareSolverr{
		inFlight: map[string]int{},
		live:     map[string]bool{},
		page:     page,
	}
}

func (f *fakeFlareSolverr) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req map[string]any
	_ = json.Unmarshal(body, &req)
	w.Header().Set("Content-Type", "application/json")

	f.mu.Lock()
	switch req["cmd"] {
	case "sessions.create":
		f.created++
		id := fmt.Sprintf("session-%d", f.created)
		f.live[id] = true
		f.mu.Unlock()
		writeJSON(w, map[string]any{"status": "ok", "session": id})
		return
	case "sessions.destroy":
		id, _ := req["session"].(string)
		delete(f.live, id)
		f.destroyed = append(f.destroyed, id)
		f.mu.Unlock()
		writeJSON(w, map[string]any{"status": "ok"})
		return
	case "sessions.list":
		f.listCalls++
		if f.shouldFailList {
			f.mu.Unlock()
			writeJSON(w, map[string]any{"status": "error", "message": "Error: no list for you"})
			return
		}
		ids := make([]string, 0, len(f.live))
		for id := range f.live {
			ids = append(ids, id)
		}
		f.mu.Unlock()
		writeJSON(w, map[string]any{"status": "ok", "sessions": ids})
		return
	}

	session, _ := req["session"].(string)
	if session != "" {
		f.live[session] = true
	}
	f.requests++
	f.pageRequests = append(f.pageRequests, req)
	beforePage := f.beforePage
	if f.failNext > 0 {
		f.failNext--
		message := f.failMessage
		f.mu.Unlock()
		if message == "" {
			message = "Error: Error solving the challenge. Timeout after 60 seconds."
		}
		writeJSON(w, map[string]any{"status": "error", "message": message})
		return
	}
	f.inFlight[session]++
	f.maxInFlight = max(f.maxInFlight, f.inFlight[session])
	f.concurrent++
	f.maxConcurrent = max(f.maxConcurrent, f.concurrent)
	f.mu.Unlock()

	if beforePage != nil {
		beforePage()
	}
	if f.rawReply != "" {
		_, _ = w.Write([]byte(f.rawReply))
		return
	}

	time.Sleep(f.delay)

	f.mu.Lock()
	f.inFlight[session]--
	f.concurrent--
	f.mu.Unlock()
	solution := map[string]any{"status": 200, "response": f.page}
	if f.solutionURL != "" {
		solution["url"] = f.solutionURL
	}
	if f.solutionCookies != nil {
		solution["cookies"] = f.solutionCookies
	}
	if f.solutionUserAgent != "" {
		solution["userAgent"] = f.solutionUserAgent
	}
	writeJSON(w, map[string]any{"status": "ok", "solution": solution})
}

// writeJSON answers with v, which is always a map of strings and numbers,
// so marshaling it cannot fail.
func writeJSON(w http.ResponseWriter, v map[string]any) {
	body, _ := json.Marshal(v) //nolint:errchkjson // the values are strings and numbers
	_, _ = w.Write(body)
}

// forgetSessions drops every session, as a restart of FlareSolverr does. A
// request that still names one gets a fresh, empty browser under that name,
// without a word.
func (f *fakeFlareSolverr) forgetSessions() {
	f.mu.Lock()
	defer f.mu.Unlock()
	clear(f.live)
}

func (f *fakeFlareSolverr) liveSessions() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.live)
}

func (f *fakeFlareSolverr) isInFlight(session string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inFlight[session] == 1
}

// useFlareSolverr points the Scraper at a FlareSolverr endpoint and opts the
// trackers in, as an operator does with the flaresolverr setting in each
// tracker's config file.
func (s *Scraper) useFlareSolverr(t *testing.T, endpoint string, trackerIDs ...string) {
	t.Helper()
	dir := t.TempDir()
	for _, id := range trackerIDs {
		require.NoError(t, os.WriteFile(filepath.Join(dir, id+".yml"), []byte(FlareSolverrSetting+": true\n"), 0o600))
	}
	s.config = NewConfigStore(dir)
	s.flareSolverrURL = endpoint
}

func testFlareSolverrDef(id string) string {
	return fmt.Sprintf(`
id: %s
name: %s
links:
  - http://example.invalid/
search:
  paths:
    - path: "/"
  rows:
    selector: ".row"
  fields:
    title:
      selector: "a"
`, id, id)
}

// trackerSession returns a tracker's session entry, failing the test if
// the Scraper refuses one.
func trackerSession(t *testing.T, scrpr *Scraper, trackerID string) *flareSession {
	t.Helper()
	session, err := scrpr.flareSessionFor(trackerID)
	require.NoError(t, err)
	return session
}

var testFlareSolverrPage = &url.URL{Scheme: "http", Host: "example.invalid", Path: "/"}

// FlareSolverr takes no lock around a session, so two requests on one
// tracker's session at once would drive one browser tab and could return
// each other's pages. Requests of a tracker take turns.
func TestScraperFlareSolverrAdmitsOneRequestPerTrackerAtATime(t *testing.T) {
	fake := newFakeFlareSolverr("<html></html>")
	fake.delay = 30 * time.Millisecond
	flare := httptest.NewServer(fake)
	defer flare.Close()

	scrpr := New(NewConfigStore(""), flare.URL, slog.New(slog.DiscardHandler))

	const requests = 4
	errs := make(chan error, requests)
	for range requests {
		go func() {
			_, err := scrpr.scrapeWithFlareSolverr(t.Context(), "example-tracker", http.MethodGet, testFlareSolverrPage, "")
			errs <- err
		}()
	}
	for range requests {
		require.NoError(t, <-errs)
	}

	require.Equal(t, 1, fake.maxInFlight, "requests overlapped on one tracker's session")
	require.Equal(t, 1, fake.created, "one tracker should use one session")
	require.Equal(t, requests, fake.requests)
}

// Different trackers have sessions of their own, so an aggregate search
// drives them at the same time instead of queueing them behind one another.
func TestScraperFlareSolverrDrivesDifferentTrackersAtOnce(t *testing.T) {
	fake := newFakeFlareSolverr("<html></html>")
	fake.delay = 100 * time.Millisecond
	flare := httptest.NewServer(fake)
	defer flare.Close()

	scrpr := New(NewConfigStore(""), flare.URL, slog.New(slog.DiscardHandler))

	const trackers = 4
	errs := make(chan error, trackers)
	for i := range trackers {
		id := fmt.Sprintf("tracker-%d", i)
		go func() {
			_, err := scrpr.scrapeWithFlareSolverr(t.Context(), id, http.MethodGet, testFlareSolverrPage, "")
			errs <- err
		}()
	}
	for range trackers {
		require.NoError(t, <-errs)
	}

	require.Equal(t, trackers, fake.created, "each tracker should have a session of its own")
	require.Equal(t, 1, fake.maxInFlight, "a tracker's session carried more than its own request")
	require.Greater(t, fake.maxConcurrent, 1, "the trackers were queued behind one another")
}

// Waiting for a turn is part of the request, so it ends with the request's
// context instead of outliving a client that has gone.
func TestScraperFlareSolverrWaitEndsWithTheContext(t *testing.T) {
	fake := newFakeFlareSolverr("<html></html>")
	fake.delay = 400 * time.Millisecond
	flare := httptest.NewServer(fake)
	defer flare.Close()

	scrpr := New(NewConfigStore(""), flare.URL, slog.New(slog.DiscardHandler))

	holding := make(chan error, 1)
	go func() {
		_, err := scrpr.scrapeWithFlareSolverr(t.Context(), "example-tracker", http.MethodGet, testFlareSolverrPage, "")
		holding <- err
	}()
	require.Eventually(t, func() bool { return fake.isInFlight("session-1") },
		2*time.Second, 5*time.Millisecond, "the first request never reached FlareSolverr")

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := scrpr.scrapeWithFlareSolverr(ctx, "example-tracker", http.MethodGet, testFlareSolverrPage, "")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, time.Since(start), 300*time.Millisecond, "the wait outlived its context")

	require.NoError(t, <-holding)
}

// A request that gave up waiting for its turn never reached the tracker,
// so it must not count against it: a second search of a tracker can wait
// out its context behind a slow challenge, and recording that as a failure
// would back a healthy tracker off for minutes.
func TestScraperFlareSolverrQueueTimeoutIsNotATrackerFailure(t *testing.T) {
	fake := newFakeFlareSolverr(`<div class="row"><a>Example Release</a></div>`)
	fake.delay = 1600 * time.Millisecond
	flare := httptest.NewServer(fake)
	defer flare.Close()

	store := &fakeStore{}

	def := loadTestTracker(t, t.TempDir(), "example-tracker", testFlareSolverrDef("example-tracker"))
	scrpr := NewWithOptions(NewConfigStore(""), "", slog.New(slog.DiscardHandler), Options{Sink: store})
	scrpr.useFlareSolverr(t, flare.URL, "example-tracker")

	holding := make(chan error, 1)
	go func() {
		holding <- scrpr.scrapeIndexer(t.Context(), def, SearchParams{Query: "first"})
	}()
	require.Eventually(t, func() bool { return fake.isInFlight("session-1") },
		2*time.Second, 5*time.Millisecond, "the first search never reached FlareSolverr")

	// A different search of the same tracker waits out the scrape spacing,
	// then queues behind the first for its turn, and its context ends there.
	ctx, cancel := context.WithTimeout(t.Context(), 1300*time.Millisecond)
	defer cancel()
	err := scrpr.scrapeIndexer(ctx, def, SearchParams{Query: "second"})
	require.ErrorIs(t, err, errFlareSolverrQueue)
	require.ErrorIs(t, err, context.DeadlineExceeded)

	state, ok := scrpr.scrapeStateSnapshot("example-tracker")
	require.True(t, ok)
	require.Zero(t, state.failures, "a wait for a turn was recorded as a tracker failure")
	require.True(t, state.nextAllowed.Before(time.Now().Add(2*time.Second)),
		"the tracker was backed off for a wait it did not cause")

	require.NoError(t, <-holding)
}

// Every fetch goes through FlareSolverr when it is configured, and each
// tracker has a session of its own, so an aggregate search over a large
// directory would start a browser per definition. The least recently used
// idle session is destroyed to stay within the limit.
func TestScraperFlareSolverrKeepsToItsSessionLimit(t *testing.T) {
	fake := newFakeFlareSolverr("<html></html>")
	flare := httptest.NewServer(fake)
	defer flare.Close()

	scrpr := NewWithOptions(NewConfigStore(""), flare.URL, slog.New(slog.DiscardHandler), Options{MaxFlareSolverrSessions: 2})
	scrape := func(trackerID string) {
		t.Helper()
		_, err := scrpr.scrapeWithFlareSolverr(t.Context(), trackerID, http.MethodGet, testFlareSolverrPage, "")
		require.NoError(t, err)
	}

	scrape("first-tracker")
	scrape("second-tracker")
	require.Equal(t, 2, fake.liveSessions())
	scrape("third-tracker")
	require.Equal(t, 2, fake.liveSessions(), "a third tracker took the count past the limit")
	require.Equal(t, []string{"session-1"}, fake.destroyed, "the least recently used session was not the one evicted")

	// The first tracker comes back: it starts a new session, and the least
	// recently used of the others, the second, makes room for it.
	scrape("first-tracker")
	require.Equal(t, 2, fake.liveSessions())
	require.Equal(t, []string{"session-1", "session-2"}, fake.destroyed)
	require.Equal(t, 4, fake.created)
}

// A session that is being used is not idle: destroying it would abort the
// request running on it. When every session is busy the limit gives way
// until they finish, and the next new session trims back to it.
func TestScraperFlareSolverrNeverEvictsASessionInUse(t *testing.T) {
	fake := newFakeFlareSolverr("<html></html>")
	fake.delay = 300 * time.Millisecond
	flare := httptest.NewServer(fake)
	defer flare.Close()

	scrpr := NewWithOptions(NewConfigStore(""), flare.URL, slog.New(slog.DiscardHandler), Options{MaxFlareSolverrSessions: 1})

	busy := make(chan error, 1)
	go func() {
		_, err := scrpr.scrapeWithFlareSolverr(t.Context(), "busy-tracker", http.MethodGet, testFlareSolverrPage, "")
		busy <- err
	}()
	require.Eventually(t, func() bool { return fake.isInFlight("session-1") },
		2*time.Second, 5*time.Millisecond, "the first request never reached FlareSolverr")

	_, err := scrpr.scrapeWithFlareSolverr(t.Context(), "other-tracker", http.MethodGet, testFlareSolverrPage, "")
	require.NoError(t, err)
	require.NoError(t, <-busy, "the busy session's request was aborted")
	require.Empty(t, fake.destroyed, "a session in use was evicted")
	require.Equal(t, 2, fake.liveSessions())

	_, err = scrpr.scrapeWithFlareSolverr(t.Context(), "third-tracker", http.MethodGet, testFlareSolverrPage, "")
	require.NoError(t, err)
	require.Equal(t, 1, fake.liveSessions(), "the idle sessions were not trimmed back to the limit")
}

// The evicted tracker's cookies went with its session, so its login can no
// longer be trusted.
func TestScraperFlareSolverrEvictionForgetsThatTrackersLogin(t *testing.T) {
	fake := newFakeFlareSolverr("<html></html>")
	flare := httptest.NewServer(fake)
	defer flare.Close()

	scrpr := NewWithOptions(NewConfigStore(""), flare.URL, slog.New(slog.DiscardHandler), Options{MaxFlareSolverrSessions: 1})
	_, err := scrpr.scrapeWithFlareSolverr(t.Context(), "first-tracker", http.MethodGet, testFlareSolverrPage, "")
	require.NoError(t, err)
	scrpr.markLoggedIn("first-tracker")

	_, err = scrpr.scrapeWithFlareSolverr(t.Context(), "second-tracker", http.MethodGet, testFlareSolverrPage, "")
	require.NoError(t, err)
	scrpr.markLoggedIn("second-tracker")

	require.False(t, scrpr.loginIsFresh("first-tracker"), "an evicted session's login was kept")
	require.True(t, scrpr.loginIsFresh("second-tracker"))
}

// A tracker uses FlareSolverr only when its own config file opts it in:
// every request through FlareSolverr drives a browser, so a tracker that
// needs no help with an anti-bot challenge is fetched directly and never
// gets a session.
func TestScraperFlareSolverrIsUsedOnlyByOptedInTrackers(t *testing.T) {
	fake := newFakeFlareSolverr(`<div class="row"><a>Solved Release</a></div>`)
	flare := httptest.NewServer(fake)
	defer flare.Close()

	var directHits int
	var directMu sync.Mutex
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		directMu.Lock()
		directHits++
		directMu.Unlock()
		_, _ = w.Write([]byte(`<div class="row"><a>Direct Release</a></div>`))
	}))
	defer direct.Close()

	store := &fakeStore{}

	defsDir := t.TempDir()
	trackerDef := func(id string) string {
		return fmt.Sprintf("id: %s\nname: %s\nlinks:\n  - %s/\nsearch:\n  paths:\n    - path: \"/\"\n  rows:\n    selector: \".row\"\n  fields:\n    title:\n      selector: a\n", id, id, direct.URL)
	}
	solved := loadTestTracker(t, defsDir, "solved-tracker", trackerDef("solved-tracker"))
	plain := loadTestTracker(t, defsDir, "plain-tracker", trackerDef("plain-tracker"))

	scrpr := NewWithOptions(NewConfigStore(""), "", slog.New(slog.DiscardHandler), Options{Sink: store})
	scrpr.useFlareSolverr(t, flare.URL, "solved-tracker")

	require.NoError(t, scrpr.scrapeIndexer(t.Context(), solved, SearchParams{Query: "example"}))
	require.Equal(t, 1, fake.requests, "the opted-in tracker did not go through FlareSolverr")
	require.Equal(t, 1, fake.created)
	directMu.Lock()
	require.Zero(t, directHits, "the opted-in tracker was also fetched directly")
	directMu.Unlock()

	require.NoError(t, scrpr.scrapeIndexer(t.Context(), plain, SearchParams{Query: "example"}))
	require.Equal(t, 1, fake.requests, "a tracker that did not opt in went through FlareSolverr")
	require.Equal(t, 1, fake.created, "a tracker that did not opt in got a session")
	directMu.Lock()
	require.Equal(t, 1, directHits, "the other tracker was not fetched directly")
	directMu.Unlock()
}

// The setting is read from a hand-edited config file as well as the one the
// admin panel writes, so a boolean and the spellings of one both count, and
// anything else, including an endpoint that is not configured, does not.
func TestScraperFlareSolverrSettingValues(t *testing.T) {
	tests := []struct {
		name        string
		content     string
		hasEndpoint bool
		want        bool
	}{
		{name: "a true boolean", content: "flaresolverr: true\n", hasEndpoint: true, want: true},
		{name: "the string true", content: "flaresolverr: \"true\"\n", hasEndpoint: true, want: true},
		{name: "the string True", content: "flaresolverr: \"True\"\n", hasEndpoint: true, want: true},
		{name: "a false boolean", content: "flaresolverr: false\n", hasEndpoint: true},
		{name: "a string that is not a boolean", content: "flaresolverr: \"no\"\n", hasEndpoint: true},
		{name: "a number", content: "flaresolverr: 1\n", hasEndpoint: true},
		{name: "no such setting", content: "username: someone\n", hasEndpoint: true},
		{name: "an unreadable file", content: "flaresolverr: [unterminated\n", hasEndpoint: true},
		{name: "opted in with no endpoint configured", content: "flaresolverr: true\n", hasEndpoint: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "example-tracker.yml"), []byte(tc.content), 0o600))
			scrpr := New(NewConfigStore(dir), "", slog.New(slog.DiscardHandler))
			if tc.hasEndpoint {
				scrpr.flareSolverrURL = "http://flaresolverr.example.invalid"
			}
			require.Equal(t, tc.want, scrpr.usesFlareSolverr(t.Context(), &Tracker{ID: "example-tracker"}))
		})
	}

	t.Run("no config directory", func(t *testing.T) {
		scrpr := New(NewConfigStore(""), "http://flaresolverr.example.invalid", slog.New(slog.DiscardHandler))
		require.False(t, scrpr.usesFlareSolverr(t.Context(), &Tracker{ID: "example-tracker"}))
	})
}

// FlareSolverr's browser holds its own cookies and cannot see Jacklet's, so a
// cookie the operator configured for a tracker has to travel in the request:
// otherwise the tracker is asked without it, answers with its login page,
// and that parses as no results while Jacklet believes it is signed in.
func TestScraper_FlareSolverrRequest_CarriesTheLoginCookie(t *testing.T) {
	fake := newFakeFlareSolverr(`<div class="row"><a>Example Release</a></div>`)
	flare := httptest.NewServer(fake)
	defer flare.Close()

	store := &fakeStore{}

	def := loadTestTracker(t, t.TempDir(), "cookie-tracker", `
id: cookie-tracker
name: cookie-tracker
links:
  - http://example.invalid/
settings:
  - name: cookie
    type: text
login:
  path: login.php
  method: cookie
search:
  paths:
    - path: "/"
  rows:
    selector: ".row"
  fields:
    title:
      selector: "a"
`)
	configDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "cookie-tracker.yml"),
		[]byte("flaresolverr: true\ncookie: \"session=abc; theme=dark\"\n"), 0o600))

	scrpr := NewWithOptions(NewConfigStore(configDir), flare.URL, slog.New(slog.DiscardHandler), Options{Sink: store})
	require.NoError(t, scrpr.scrapeIndexer(t.Context(), def, SearchParams{Query: "example"}))

	fake.mu.Lock()
	defer fake.mu.Unlock()
	require.Len(t, fake.pageRequests, 1)
	require.ElementsMatch(t, []any{
		map[string]any{"name": "session", "value": "abc"},
		map[string]any{"name": "theme", "value": "dark"},
	}, fake.pageRequests[0]["cookies"], "the login cookie was not passed to FlareSolverr")
}

// A tracker with no cookies to present sends none, rather than an empty
// list FlareSolverr would have to make sense of.
func TestScraper_FlareSolverrRequest_WithoutCookiesOmitsThem(t *testing.T) {
	fake := newFakeFlareSolverr("<html></html>")
	flare := httptest.NewServer(fake)
	defer flare.Close()

	scrpr := New(NewConfigStore(""), flare.URL, slog.New(slog.DiscardHandler))
	_, err := scrpr.scrapeWithFlareSolverr(t.Context(), "example-tracker", http.MethodGet, testFlareSolverrPage, "")
	require.NoError(t, err)

	fake.mu.Lock()
	defer fake.mu.Unlock()
	require.NotContains(t, fake.pageRequests[0], "cookies")
}

// A scrape reads the tracker's config file once, and every fetch it makes
// follows that answer, so the file is not parsed again for each login page,
// credential post and search path. Removing the file after the first fetch
// shows which answer the later ones follow.
func TestScraperFlareSolverrChoiceIsMadeOncePerScrape(t *testing.T) {
	fake := newFakeFlareSolverr(`<div class="row"><a>Example Release</a></div>`)
	flare := httptest.NewServer(fake)
	defer flare.Close()

	var directHits int
	var directMu sync.Mutex
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		directMu.Lock()
		directHits++
		directMu.Unlock()
	}))
	defer direct.Close()

	store := &fakeStore{}

	def := loadTestTracker(t, t.TempDir(), "login-tracker", fmt.Sprintf(`
id: login-tracker
name: login-tracker
links:
  - %s/
login:
  path: login.php
  method: get
  inputs:
    username: someone
search:
  paths:
    - path: "/"
  rows:
    selector: ".row"
  fields:
    title:
      selector: "a"
`, direct.URL))

	scrpr := NewWithOptions(NewConfigStore(""), "", slog.New(slog.DiscardHandler), Options{Sink: store})
	scrpr.useFlareSolverr(t, flare.URL, "login-tracker")
	fake.beforePage = func() { _ = os.Remove(filepath.Join(scrpr.config.(*ConfigStore).Dir(), "login-tracker.yml")) }

	require.NoError(t, scrpr.scrapeIndexer(t.Context(), def, SearchParams{Query: "example"}))

	fake.mu.Lock()
	defer fake.mu.Unlock()
	require.Len(t, fake.pageRequests, 2, "both the login and the search should have gone through FlareSolverr")
	directMu.Lock()
	defer directMu.Unlock()
	require.Zero(t, directHits, "a later fetch of the scrape re-read the config and went direct")
}

// setJarCookie puts a session cookie in the Scraper's jar for the test page,
// as a cookie login does.
func setJarCookie(scrpr *Scraper, value string) {
	// These attributes govern how a server tells a browser to store a
	// cookie; this is an outgoing client cookie being loaded into a jar.
	//nolint:gosec // G124: not a Set-Cookie being issued to a client
	scrpr.httpClient.Jar.SetCookies(testFlareSolverrPage, []*http.Cookie{{Name: "session", Value: value}})
}

// pageCookies returns the cookies each page request carried, in order, nil
// for a request that carried none.
func (f *fakeFlareSolverr) pageCookies() []any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]any, len(f.pageRequests))
	for i, req := range f.pageRequests {
		out[i] = req["cookies"]
	}
	return out
}

// FlareSolverr loads a page a second time whenever a request carries
// cookies, and a session keeps the ones it has been given, so they are sent
// once and again only when they change or the session is replaced.
func TestScraperFlareSolverrSendsCookiesOnlyToASessionThatLacksThem(t *testing.T) {
	fake := newFakeFlareSolverr("<html></html>")
	flare := httptest.NewServer(fake)
	defer flare.Close()

	scrpr := New(NewConfigStore(""), flare.URL, slog.New(slog.DiscardHandler))
	setCookie := func(value string) {
		setJarCookie(scrpr, value)
	}
	request := func() {
		_, err := scrpr.scrapeWithFlareSolverr(t.Context(), "example-tracker", http.MethodGet, testFlareSolverrPage, "")
		require.NoError(t, err)
	}
	cookie := func(value string) any {
		return []any{map[string]any{"name": "session", "value": value}}
	}

	setCookie("first")
	request()
	request()
	require.Equal(t, []any{cookie("first"), nil}, fake.pageCookies(), "the second request repeated cookies the session already holds")

	setCookie("second")
	request()
	require.Equal(t, cookie("second"), fake.pageCookies()[2], "a changed cookie was not sent")
	request()
	require.Nil(t, fake.pageCookies()[3])

	// A replaced session starts without the cookies, so they go again.
	tracker := trackerSession(t, scrpr, "example-tracker")
	current, err := scrpr.flareSolverrSession(t.Context(), tracker)
	require.NoError(t, err)
	scrpr.retireFlareSolverrSession(t.Context(), tracker, current)
	request()
	require.Equal(t, cookie("second"), fake.pageCookies()[4], "a new session was not given the cookies")
}

// An evicted session is destroyed with the cookies it held, so the tracker's
// next session must be given them again.
func TestScraperFlareSolverrResendsCookiesAfterEviction(t *testing.T) {
	fake := newFakeFlareSolverr("<html></html>")
	flare := httptest.NewServer(fake)
	defer flare.Close()

	scrpr := NewWithOptions(NewConfigStore(""), flare.URL, slog.New(slog.DiscardHandler), Options{MaxFlareSolverrSessions: 1})
	setJarCookie(scrpr, "abc")
	request := func(trackerID string) {
		_, err := scrpr.scrapeWithFlareSolverr(t.Context(), trackerID, http.MethodGet, testFlareSolverrPage, "")
		require.NoError(t, err)
	}

	request("first-tracker")
	request("second-tracker") // evicts the first tracker's session
	request("first-tracker")  // and evicts the second's

	cookies := fake.pageCookies()
	require.Len(t, cookies, 3)
	require.NotNil(t, cookies[2], "a session created after an eviction was not given the cookies")
}

// scrapeOnce makes one request for the example tracker through FlareSolverr
// and returns its error, for tests that only care what it did to the session.
func scrapeOnce(ctx context.Context, scrpr *Scraper) error {
	_, err := scrpr.scrapeWithFlareSolverr(ctx, "example-tracker", http.MethodGet, testFlareSolverrPage, "")
	return err
}

// FlareSolverr keeps a session's browser after an error, and a single
// failure is more often a challenge that did not solve in time than a
// broken browser. Destroying the session then would throw away the
// clearance it holds, so it survives one failure.
func TestScraperFlareSolverrKeepsASessionThroughOneFailure(t *testing.T) {
	fake := newFakeFlareSolverr("<html></html>")
	fake.failNext = 1
	flare := httptest.NewServer(fake)
	defer flare.Close()

	scrpr := New(NewConfigStore(""), flare.URL, slog.New(slog.DiscardHandler))
	require.Error(t, scrapeOnce(t.Context(), scrpr))
	require.Empty(t, fake.destroyed, "one failure destroyed the session")

	require.NoError(t, scrapeOnce(t.Context(), scrpr))
	require.Equal(t, 1, fake.created, "the session was replaced after a single failure")
}

// A browser that has crashed fails every request until it is destroyed, so
// a session that keeps failing is replaced, and the tracker's cookies went
// with it, so its login is forgotten.
func TestScraperFlareSolverrReplacesASessionAfterConsecutiveFailures(t *testing.T) {
	fake := newFakeFlareSolverr("<html></html>")
	fake.failNext = flareSessionMaxFailures
	flare := httptest.NewServer(fake)
	defer flare.Close()

	scrpr := New(NewConfigStore(""), flare.URL, slog.New(slog.DiscardHandler))
	scrpr.markLoggedIn("example-tracker")
	for range flareSessionMaxFailures {
		require.Error(t, scrapeOnce(t.Context(), scrpr))
	}
	require.Equal(t, []string{"session-1"}, fake.destroyed, "a session that kept failing was not destroyed")
	require.False(t, scrpr.loginIsFresh("example-tracker"), "the destroyed session's login was kept")

	require.NoError(t, scrapeOnce(t.Context(), scrpr))
	require.Equal(t, 2, fake.created)
	require.Equal(t, 1, fake.liveSessions(), "only the replacement should be running")
}

// Only failures in a row count, so a request that works in between starts
// the run again.
func TestScraperFlareSolverrSuccessEndsARunOfFailures(t *testing.T) {
	fake := newFakeFlareSolverr("<html></html>")
	flare := httptest.NewServer(fake)
	defer flare.Close()

	scrpr := New(NewConfigStore(""), flare.URL, slog.New(slog.DiscardHandler))
	for range 3 {
		fake.mu.Lock()
		fake.failNext = 1
		fake.mu.Unlock()
		require.Error(t, scrapeOnce(t.Context(), scrpr))
		require.NoError(t, scrapeOnce(t.Context(), scrpr))
	}
	require.Empty(t, fake.destroyed, "failures with successes between them destroyed the session")
	require.Equal(t, 1, fake.created)
}

// A banned address says nothing about the browser, and a new one cannot
// fix it, so it is not counted against the session.
func TestScraperFlareSolverrBannedAddressDoesNotCountAgainstTheSession(t *testing.T) {
	fake := newFakeFlareSolverr("<html></html>")
	fake.failMessage = "Error: Error solving the challenge. Cloudflare has blocked this request. Probably your IP is banned for this site, check in your web browser."
	fake.failNext = flareSessionMaxFailures + 2
	flare := httptest.NewServer(fake)
	defer flare.Close()

	scrpr := New(NewConfigStore(""), flare.URL, slog.New(slog.DiscardHandler))
	for range flareSessionMaxFailures + 2 {
		require.Error(t, scrapeOnce(t.Context(), scrpr))
	}
	require.Empty(t, fake.destroyed, "a banned address destroyed the session")
	require.Equal(t, 1, fake.created)
}

// FlareSolverr enforces its own timeout and answers with an ordinary error,
// so it is told to stop before the request's deadline does. Abandoning the
// request instead would leave it running in the session's one tab.
func TestScraperFlareSolverrIsToldToStopBeforeTheDeadline(t *testing.T) {
	fake := newFakeFlareSolverr("<html></html>")
	flare := httptest.NewServer(fake)
	defer flare.Close()

	scrpr := New(NewConfigStore(""), flare.URL, slog.New(slog.DiscardHandler))

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	require.NoError(t, scrapeOnce(ctx, scrpr))

	ctx, cancelShort := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancelShort()
	require.NoError(t, scrapeOnce(ctx, scrpr))

	require.NoError(t, scrapeOnce(t.Context(), scrpr))

	fake.mu.Lock()
	defer fake.mu.Unlock()
	require.Len(t, fake.pageRequests, 3)
	timeout, ok := fake.pageRequests[0]["maxTimeout"].(float64)
	require.True(t, ok, "no maxTimeout was sent for a request with a deadline")
	require.InDelta(t, (20*time.Second - flareSolverrTimeoutMargin).Milliseconds(), timeout, 500)
	require.EqualValues(t, flareSolverrMinTimeout.Milliseconds(), fake.pageRequests[1]["maxTimeout"],
		"a nearly spent deadline should not send a value FlareSolverr would ignore")
	require.NotContains(t, fake.pageRequests[2], "maxTimeout", "a request with no deadline should use FlareSolverr's own")
}

// A request that outlived its deadline is not still running: FlareSolverr
// was told to stop before then. Its session is judged like any failure, and
// one failure keeps it.
func TestScraperFlareSolverrKeepsTheSessionWhenADeadlinePasses(t *testing.T) {
	fake := newFakeFlareSolverr("<html></html>")
	fake.delay = 300 * time.Millisecond
	flare := httptest.NewServer(fake)
	defer flare.Close()

	scrpr := New(NewConfigStore(""), flare.URL, slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, scrapeOnce(ctx, scrpr), context.DeadlineExceeded)

	require.Empty(t, fake.destroyed, "a deadline destroyed a working session")
	require.Equal(t, 1, fake.liveSessions())
}

// A request whose context was canceled, by a client that went away, may
// still be running in the session's one tab, and the next request would
// start navigating that same tab. The session is destroyed, which ends the
// abandoned request, and the next request starts on a clean one.
func TestScraperFlareSolverrDestroysTheSessionOfACanceledRequest(t *testing.T) {
	fake := newFakeFlareSolverr("<html></html>")
	fake.delay = 300 * time.Millisecond
	flare := httptest.NewServer(fake)
	defer flare.Close()

	scrpr := New(NewConfigStore(""), flare.URL, slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	require.ErrorIs(t, scrapeOnce(ctx, scrpr), context.Canceled)

	require.Equal(t, []string{"session-1"}, fake.destroyed, "the abandoned request's session was left running")
	require.NoError(t, scrapeOnce(t.Context(), scrpr))
	require.Equal(t, 2, fake.created, "the next request should have started a clean session")
}

// A stale session id retires nothing: when two requests see the same
// session fail, the second finds it already replaced, and destroying the
// replacement would throw away a session that works.
func TestScraperFlareSolverrRetiresOnlyTheCurrentSession(t *testing.T) {
	fake := newFakeFlareSolverr("")
	flare := httptest.NewServer(fake)
	defer flare.Close()

	scrpr := New(NewConfigStore(""), flare.URL, slog.New(slog.DiscardHandler))
	tracker := trackerSession(t, scrpr, "example-tracker")
	first, err := scrpr.flareSolverrSession(t.Context(), tracker)
	require.NoError(t, err)

	scrpr.retireFlareSolverrSession(t.Context(), tracker, first)
	replacement, err := scrpr.flareSolverrSession(t.Context(), tracker)
	require.NoError(t, err)
	require.NotEqual(t, first, replacement)

	scrpr.retireFlareSolverrSession(t.Context(), tracker, first)
	current, err := scrpr.flareSolverrSession(t.Context(), tracker)
	require.NoError(t, err)
	require.Equal(t, replacement, current, "a stale session id retired the current session")
	require.Equal(t, []string{first}, fake.destroyed)
	require.Equal(t, 1, fake.liveSessions())
}

// Each tracker's session holds that tracker's cookies alone, so retiring
// one drops that tracker's login and leaves every other tracker's session
// and login as they were: one slow tracker cannot make the rest start over.
func TestScraperFlareSolverrRetiringASessionAffectsOnlyItsTracker(t *testing.T) {
	fake := newFakeFlareSolverr("")
	flare := httptest.NewServer(fake)
	defer flare.Close()

	scrpr := New(NewConfigStore(""), flare.URL, slog.New(slog.DiscardHandler))
	first := trackerSession(t, scrpr, "first-tracker")
	second := trackerSession(t, scrpr, "second-tracker")
	firstID, err := scrpr.flareSolverrSession(t.Context(), first)
	require.NoError(t, err)
	secondID, err := scrpr.flareSolverrSession(t.Context(), second)
	require.NoError(t, err)
	scrpr.markLoggedIn("first-tracker")
	scrpr.markLoggedIn("second-tracker")

	scrpr.retireFlareSolverrSession(t.Context(), first, firstID)

	require.False(t, scrpr.loginIsFresh("first-tracker"), "the retired session's login was kept")
	require.True(t, scrpr.loginIsFresh("second-tracker"), "another tracker's login was dropped")
	stillSecond, err := scrpr.flareSolverrSession(t.Context(), second)
	require.NoError(t, err)
	require.Equal(t, secondID, stillSecond, "another tracker's session was replaced")
	require.Equal(t, []string{firstID}, fake.destroyed)
}

// A stale session id retires nothing, so it must not drop the login made
// on the session that replaced it.
func TestScraperFlareSolverrStaleRetirementKeepsLogins(t *testing.T) {
	fake := newFakeFlareSolverr("")
	flare := httptest.NewServer(fake)
	defer flare.Close()

	scrpr := New(NewConfigStore(""), flare.URL, slog.New(slog.DiscardHandler))
	tracker := trackerSession(t, scrpr, "example-tracker")
	first, err := scrpr.flareSolverrSession(t.Context(), tracker)
	require.NoError(t, err)
	scrpr.retireFlareSolverrSession(t.Context(), tracker, first)
	_, err = scrpr.flareSolverrSession(t.Context(), tracker)
	require.NoError(t, err)
	scrpr.markLoggedIn("example-tracker")

	scrpr.retireFlareSolverrSession(t.Context(), tracker, first)

	require.True(t, scrpr.loginIsFresh("example-tracker"), "a stale retirement dropped a login made on the current session")
}

// Destroying a retired session is not part of the request that found it
// failing, so it goes out even when that request's context has ended.
func TestScraperFlareSolverrDestroysARetiredSessionAfterTheContextEnds(t *testing.T) {
	fake := newFakeFlareSolverr("")
	flare := httptest.NewServer(fake)
	defer flare.Close()

	scrpr := New(NewConfigStore(""), flare.URL, slog.New(slog.DiscardHandler))
	tracker := trackerSession(t, scrpr, "example-tracker")
	first, err := scrpr.flareSolverrSession(t.Context(), tracker)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	scrpr.retireFlareSolverrSession(ctx, tracker, first)

	require.Equal(t, []string{first}, fake.destroyed, "the retired session was forgotten without being destroyed")
	require.Zero(t, fake.liveSessions())
}

// A request that fails has not delivered its cookies, so the next one
// carries them again: recording them as sent on a failure would leave the
// session without them for good.
func TestScraperFlareSolverrResendsCookiesAfterAFailedRequest(t *testing.T) {
	fake := newFakeFlareSolverr("<html></html>")
	fake.failNext = 1
	flare := httptest.NewServer(fake)
	defer flare.Close()

	scrpr := New(NewConfigStore(""), flare.URL, slog.New(slog.DiscardHandler))
	setJarCookie(scrpr, "abc")

	require.Error(t, scrapeOnce(t.Context(), scrpr))
	require.NoError(t, scrapeOnce(t.Context(), scrpr))

	cookies := fake.pageCookies()
	require.Len(t, cookies, 2)
	require.NotNil(t, cookies[0])
	require.NotNil(t, cookies[1], "the request after a failure did not carry the cookies")
}

// Close destroys the sessions, and a request still running on one may fail
// afterwards. A closed Scraper creates no sessions, or that request would
// leave one behind that nothing destroys.
func TestScraperFlareSolverrCreatesNoSessionAfterClose(t *testing.T) {
	fake := newFakeFlareSolverr("")
	flare := httptest.NewServer(fake)
	defer flare.Close()

	scrpr := New(NewConfigStore(""), flare.URL, slog.New(slog.DiscardHandler))
	tracker := trackerSession(t, scrpr, "example-tracker")
	_, err := scrpr.flareSolverrSession(t.Context(), tracker)
	require.NoError(t, err)
	require.NoError(t, scrpr.Close(t.Context()))

	_, err = scrpr.flareSolverrSession(t.Context(), tracker)
	require.ErrorIs(t, err, errScraperClosed)
	_, err = scrpr.flareSessionFor("another-tracker")
	require.ErrorIs(t, err, errScraperClosed)

	require.Equal(t, 1, fake.created, "a session was created after Close")
	require.Zero(t, fake.liveSessions())
}

// A page over the size cap, or a reply that is not a FlareSolverr answer, is
// no sign of a broken browser: it is deterministic and belongs to the page.
// Counting it would retire a healthy session after two such answers in a
// row and take its clearance and the tracker's login with it.
func TestScraperFlareSolverrUnusableReplyDoesNotCountAgainstTheSession(t *testing.T) {
	tests := []struct {
		name    string
		page    string
		rawText string
		want    string
	}{
		{name: "a page over the size cap", page: strings.Repeat("x", 8192), want: "larger than 4096 bytes"},
		{name: "a reply that is not JSON", rawText: "<html>502 Bad Gateway</html>", want: "unusable reply"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeFlareSolverr(tc.page)
			fake.rawReply = tc.rawText
			flare := httptest.NewServer(fake)
			defer flare.Close()

			scrpr := NewWithOptions(NewConfigStore(""), flare.URL, slog.New(slog.DiscardHandler), Options{MaxResponseBytes: 4096})
			scrpr.markLoggedIn("example-tracker")
			for range flareSessionMaxFailures + 1 {
				require.ErrorContains(t, scrapeOnce(t.Context(), scrpr), tc.want)
			}

			require.Empty(t, fake.destroyed, "an unusable reply destroyed a healthy session")
			require.Equal(t, 1, fake.created)
			require.True(t, scrpr.loginIsFresh("example-tracker"), "an unusable reply dropped the tracker's login")
		})
	}
}

// FlareSolverr accepts a POST only at /v1, so the bare address its own
// documentation gives has to have it added, and one that names it is used as
// it is.
func TestFlareSolverrEndpoint(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{name: "not configured", raw: "", want: ""},
		{name: "a bare address", raw: "http://flaresolverr:8191", want: "http://flaresolverr:8191/v1"},
		{name: "a bare address with a slash", raw: "http://flaresolverr:8191/", want: "http://flaresolverr:8191/v1"},
		{name: "the address with /v1", raw: "http://flaresolverr:8191/v1", want: "http://flaresolverr:8191/v1"},
		{name: "the address with /v1 and a slash", raw: "http://flaresolverr:8191/v1/", want: "http://flaresolverr:8191/v1"},
		{name: "behind a proxy path", raw: "https://example.test/solver/v1", want: "https://example.test/solver/v1"},
		{name: "no scheme", raw: "flaresolverr:8191", wantErr: true},
		{name: "a host and port with no scheme", raw: "localhost:8191", wantErr: true},
		{name: "a scheme that is not http", raw: "ftp://flaresolverr:8191", wantErr: true},
		{name: "no host", raw: "http://", wantErr: true},
		{name: "not an address", raw: "http://[::1", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := FlareSolverrEndpoint(tc.raw)
			if tc.wantErr {
				require.ErrorContains(t, err, "invalid FlareSolverr URL")
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

// v1Only is a FlareSolverr that answers like the real one: a POST at /v1 and
// nothing else, with an HTML error page for any other path.
func v1Only(fake *fakeFlareSolverr) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1" || r.Method != http.MethodPost {
			http.Error(w, "<html><body>Method Not Allowed</body></html>", http.StatusMethodNotAllowed)
			return
		}
		fake.ServeHTTP(w, r)
	})
}

func TestScraperFlareSolverrIsPostedToV1(t *testing.T) {
	for name, suffix := range map[string]string{"the bare address": "", "with a slash": "/", "already naming /v1": "/v1"} {
		t.Run(name, func(t *testing.T) {
			fake := newFakeFlareSolverr("<html></html>")
			flare := httptest.NewServer(v1Only(fake))
			defer flare.Close()

			scrpr := New(NewConfigStore(""), flare.URL+suffix, slog.New(slog.DiscardHandler))
			require.NoError(t, scrapeOnce(t.Context(), scrpr))
			require.Equal(t, 1, fake.requests)
		})
	}

	t.Run("a wrong path says what came back", func(t *testing.T) {
		fake := newFakeFlareSolverr("<html></html>")
		flare := httptest.NewServer(v1Only(fake))
		defer flare.Close()

		scrpr := New(NewConfigStore(""), flare.URL+"/api", slog.New(slog.DiscardHandler))
		err := scrapeOnce(t.Context(), scrpr)
		require.ErrorContains(t, err, "HTTP 405")
		require.ErrorIs(t, err, errFlareSolverrReply)
	})
}

// FlareSolverr makes a session of any name it is asked for, so one lost to a
// restart is replaced without a word, and everything Jacklet tied to the old
// browser goes with it: the cookie it was sent, the login it earned, and what
// was kept for downloads. Asking FlareSolverr which sessions it holds is the
// only way to know.
func TestScraperFlareSolverrNoticesALostSession(t *testing.T) {
	fake := newFakeFlareSolverr("<html></html>")
	fake.solutionCookies = []map[string]any{{"name": "sid", "value": "abc", "domain": "127.0.0.1", "path": "/"}}
	flare := httptest.NewServer(fake)
	defer flare.Close()

	scrpr := New(NewConfigStore(""), flare.URL, slog.New(slog.DiscardHandler))
	setJarCookie(scrpr, "operator")
	scrpr.markLoggedIn("example-tracker")
	require.NoError(t, scrapeOnce(t.Context(), scrpr))
	require.NotNil(t, fake.pageCookies()[0], "the first request should carry the operator's cookie")

	fake.forgetSessions() // FlareSolverr restarts.

	require.NoError(t, scrapeOnce(t.Context(), scrpr))

	require.Equal(t, 2, fake.created, "a session that FlareSolverr had lost was not replaced")
	require.NotNil(t, fake.pageCookies()[1], "the new browser was not given the operator's cookie again")
	require.False(t, scrpr.loginIsFresh("example-tracker"), "the lost session's login was kept")
}

// The question has to come before the request that would recreate the
// session: FlareSolverr makes a fresh one under any name it is asked for, so
// a check made after the first request would find the id and trust an empty
// browser. A session that is still there costs one question per request and
// is otherwise left alone.
func TestScraperFlareSolverrVerifiesASessionBeforeEachRequest(t *testing.T) {
	fake := newFakeFlareSolverr("<html></html>")
	flare := httptest.NewServer(fake)
	defer flare.Close()

	scrpr := New(NewConfigStore(""), flare.URL, slog.New(slog.DiscardHandler))
	scrpr.markLoggedIn("example-tracker")
	require.NoError(t, scrapeOnce(t.Context(), scrpr))
	require.Zero(t, fake.listCalls, "a session that did not exist yet was asked about")
	for range 3 {
		require.NoError(t, scrapeOnce(t.Context(), scrpr))
	}

	require.Equal(t, 3, fake.listCalls, "an existing session was not verified before each request")
	require.Equal(t, 1, fake.created, "a session that was still there was replaced")
	require.True(t, scrpr.loginIsFresh("example-tracker"), "a verified session lost its login")
}

// When FlareSolverr cannot say which sessions it holds, the session is kept
// and the request reports whatever is wrong.
func TestScraperFlareSolverrKeepsASessionItCannotVerify(t *testing.T) {
	fake := newFakeFlareSolverr("<html></html>")
	flare := httptest.NewServer(fake)
	defer flare.Close()

	scrpr := New(NewConfigStore(""), flare.URL, slog.New(slog.DiscardHandler))
	require.NoError(t, scrapeOnce(t.Context(), scrpr))

	fake.mu.Lock()
	fake.shouldFailList = true
	fake.mu.Unlock()

	require.NoError(t, scrapeOnce(t.Context(), scrpr))
	require.Equal(t, 1, fake.created, "a session was replaced because the question could not be answered")
}

// The restart is noticed by the first request after it, whatever comes next:
// the request that names the lost session makes a fresh one, so a check that
// waited would find it there. Two searches in a row after a restart both go
// to the new session, and the cookie was sent to it once.
func TestScraperFlareSolverrNoticesALostSessionOnTheFirstRequest(t *testing.T) {
	fake := newFakeFlareSolverr("<html></html>")
	flare := httptest.NewServer(fake)
	defer flare.Close()

	scrpr := New(NewConfigStore(""), flare.URL, slog.New(slog.DiscardHandler))
	setJarCookie(scrpr, "operator")
	require.NoError(t, scrapeOnce(t.Context(), scrpr))

	fake.forgetSessions()
	require.NoError(t, scrapeOnce(t.Context(), scrpr))
	require.NoError(t, scrapeOnce(t.Context(), scrpr))

	cookies := fake.pageCookies()
	require.Len(t, cookies, 3)
	require.NotNil(t, cookies[0])
	require.NotNil(t, cookies[1], "the first request after a restart did not resend the cookie")
	require.Nil(t, cookies[2], "the cookie was resent to a session that had it")
	require.Equal(t, 2, fake.created)
}

// Sessions are browsers, so closing one to stay within the limit is not
// silent: with more trackers than the limit every request closes a session
// the next one needs, and the operator has to be able to see that and where
// to look.
func TestScraperFlareSolverrLogsEachEviction(t *testing.T) {
	fake := newFakeFlareSolverr("<html></html>")
	flare := httptest.NewServer(fake)
	defer flare.Close()

	var logged bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelInfo}))
	scrpr := NewWithOptions(NewConfigStore(""), flare.URL, logger, Options{MaxFlareSolverrSessions: 1})

	for _, tracker := range []string{"first-tracker", "second-tracker"} {
		_, err := scrpr.scrapeWithFlareSolverr(t.Context(), tracker, http.MethodGet, testFlareSolverrPage, "")
		require.NoError(t, err)
	}

	out := logged.String()
	require.Contains(t, out, "tracker=first-tracker", "the evicted tracker was not named")
	require.Contains(t, out, "limit=1", "the limit was not named")
	require.Equal(t, 1, strings.Count(out, "closing an idle FlareSolverr session"), "one eviction should be one line")
}

// FlareSolverr does not forward a POST: it reads the form as UTF-8, replacing
// every byte that is not, and has the browser submit a new UTF-8 form, so a
// tracker whose encoding is not UTF-8 receives replacement characters where
// the search terms were and answers with its default listing. It also drops a
// field named submit. Jacklet cannot change that, so it says why once.
func TestScraperWarnsWhenFlareSolverrCannotCarryAForm(t *testing.T) {
	tests := []struct {
		name     string
		encoding string
		form     url.Values
		method   string
		want     []string
	}{
		{name: "a legacy encoding", encoding: "windows-1251", form: url.Values{"nm": {"x"}}, method: http.MethodPost, want: []string{"windows-1251", "replacement characters"}},
		{name: "a submit field", encoding: "utf-8", form: url.Values{"nm": {"x"}, "submit": {"Go"}}, method: http.MethodPost, want: []string{"named submit"}},
		{name: "both", encoding: "koi8-r", form: url.Values{"submit": {"Go"}}, method: http.MethodPost, want: []string{"koi8-r", "named submit"}},
		{name: "UTF-8 with nothing dropped", encoding: "utf-8", form: url.Values{"nm": {"x"}}, method: http.MethodPost},
		{name: "no encoding declared", form: url.Values{"nm": {"x"}}, method: http.MethodPost},
		{name: "a GET carries its query as it is", encoding: "windows-1251", form: url.Values{"nm": {"x"}, "submit": {"Go"}}, method: http.MethodGet},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var logged bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelWarn}))
			scrpr := New(NewConfigStore(""), "", logger)
			def := &Tracker{ID: "example-tracker", Encoding: tc.encoding}

			scrpr.warnFlareSolverrForm(def, tc.method, tc.form)
			scrpr.warnFlareSolverrForm(def, tc.method, tc.form)

			out := logged.String()
			if len(tc.want) == 0 {
				require.Empty(t, out)
				return
			}
			require.Equal(t, 1, strings.Count(out, "does not reach the tracker"), "the warning should be one line per tracker")
			for _, want := range tc.want {
				require.Contains(t, out, want)
			}
		})
	}
}

// The warning is on the request path: a legacy-encoded POST search through
// FlareSolverr says so, once, however many searches follow.
func TestScraperFlareSolverrPostWarningIsOnTheRequestPath(t *testing.T) {
	fake := newFakeFlareSolverr(`<div class="row"><a>Example Release</a></div>`)
	flare := httptest.NewServer(fake)
	defer flare.Close()

	store := &fakeStore{}

	def := loadTestTracker(t, t.TempDir(), "legacy-tracker", `
id: legacy-tracker
name: legacy-tracker
encoding: windows-1251
links:
  - http://example.invalid/
search:
  paths:
    - path: "/"
      method: post
  inputs:
    nm: "{{ .Keywords }}"
  rows:
    selector: ".row"
  fields:
    title:
      selector: a
`)
	var logged bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelWarn}))
	scrpr := NewWithOptions(NewConfigStore(""), "", logger, Options{Sink: store})
	scrpr.useFlareSolverr(t, flare.URL, "legacy-tracker")

	// A second search of the tracker waits out the scrape spacing, which is
	// what makes it a second request; resetting the tracker's state to skip
	// that would reset the warning with it.
	for _, query := range []string{"first", "second"} {
		require.NoError(t, scrpr.scrapeIndexer(t.Context(), def, SearchParams{Query: query}))
	}
	require.Equal(t, 1, strings.Count(logged.String(), "does not reach the tracker"))
}

// A GET definition's inputs belong in the query string even when the fetch
// is routed through FlareSolverr, or the tracker never sees them.
func TestScraperFlareSolverrHonorsGetMethod(t *testing.T) {
	var gotCmd, gotURL, gotPostData string

	flare := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&payload))

		if payload["cmd"] == "sessions.create" {
			assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"status": "ok", "session": "s1"}))
			return
		}
		gotCmd, _ = payload["cmd"].(string)
		gotURL, _ = payload["url"].(string)
		gotPostData, _ = payload["postData"].(string)
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"status":   "ok",
			"solution": map[string]any{"response": `<div class="row"><a href="/d/1">Flare Release</a></div>`},
		}))
	}))
	defer flare.Close()

	dir := t.TempDir()
	def := loadTestTracker(t, dir, "flare-get", `
id: flare-get
name: Flare Get
links:
  - http://tracker.example/
search:
  paths:
    - path: search
      method: get
  inputs:
    q: "{{ .Keywords }}"
  rows:
    selector: "div.row"
  fields:
    title:
      selector: "a"
    download:
      selector: "a"
      attribute: "href"
`)

	db := &fakeStore{}

	scrpr := NewWithOptions(NewConfigStore(""), "", slog.New(slog.DiscardHandler), Options{Sink: db})
	scrpr.useFlareSolverr(t, flare.URL, "flare-get")
	require.NoError(t, scrpr.scrapeIndexer(t.Context(), def, SearchParams{Query: "example"}))

	require.Equal(t, "request.get", gotCmd)
	require.Equal(t, "http://tracker.example/search?q=example", gotURL)
	require.Empty(t, gotPostData, "a GET must not carry a POST body")
}

// FlareSolverr's error message may quote the address it was asked to load,
// which for a GET search carries the query and, for some definitions, a
// passkey.
func TestScraperRedactsTheQueryFromAFlareSolverrError(t *testing.T) {
	fake := newFakeFlareSolverr("<html></html>")
	flare := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		if req["cmd"] == "request.get" {
			w.Header().Set("Content-Type", "application/json")
			address, _ := req["url"].(string)
			// The query is quoted on its own, decoded, as a browser that
			// normalized the address would report it.
			_, rawQuery, _ := strings.Cut(address, "?")
			query, _ := url.QueryUnescape(rawQuery)
			writeJSON(w, map[string]any{"status": "error", "message": fmt.Sprintf("Error: timed out loading %s (%s)", address, query)})
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		fake.ServeHTTP(w, r)
	}))
	defer flare.Close()

	scrpr := New(NewConfigStore(""), "", slog.New(slog.DiscardHandler))
	scrpr.useFlareSolverr(t, flare.URL, "example-tracker")

	_, err := scrpr.scrapeWithFlareSolverr(t.Context(), "example-tracker", http.MethodGet, testFlareSolverrPage, "q=sample+release&passkey=SAMPLEKEY")
	require.Error(t, err)
	require.NotContains(t, err.Error(), "SAMPLEKEY", "the tracker's passkey reached the error")
	require.Contains(t, err.Error(), "query redacted")
}
