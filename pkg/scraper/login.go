// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// loginValidity is how long a successful login is trusted before the
// session is tested again. Re-testing on every scrape would double the
// request count against a tracker that rate limits aggressively.
const loginValidity = 30 * time.Minute

// ErrCaptchaRequired is returned when a definition's login is gated by a
// CAPTCHA. Jacklet has no interface for a human to solve one, so such a
// tracker cannot be used rather than failing with a confusing credential
// error.
var ErrCaptchaRequired = errors.New("login requires solving a CAPTCHA, which Jacklet cannot do")

// ErrCredentialsMissing is returned when a definition needs credentials
// that no configuration supplies.
var ErrCredentialsMissing = errors.New("login requires credentials; set them in the tracker's config file")

// loginState records when a tracker's session was last confirmed good.
type loginState struct {
	validUntil time.Time
}

// ensureLoggedIn authenticates against baseURL when the definition
// declares a login block and the current session is not known to be
// valid. Session cookies live in the Scraper's shared cookie jar, so one
// login serves every later request to that tracker.
func (s *Scraper) ensureLoggedIn(ctx context.Context, def *Tracker, baseURL *url.URL, cfg map[string]any) error {
	if def.Login == nil {
		return nil
	}
	if def.Login.Captcha != nil {
		return fmt.Errorf("tracker %s: %w", def.Name, ErrCaptchaRequired)
	}

	trackerID := TrackerID(def)
	if s.loginIsFresh(trackerID) {
		return nil
	}

	logger := s.logger.With("tracker", def.Name)

	// An existing session (from the cookie jar, or a "cookie" login) may
	// still be good, which saves submitting credentials again.
	if ok, err := s.loginTestPasses(ctx, def, baseURL); err == nil && ok {
		s.markLoggedIn(trackerID)
		return nil
	}

	logger.Info("authenticating")
	if err := s.performLogin(ctx, def, baseURL, cfg); err != nil {
		return err
	}

	// A tracker that answers the login request with 200 but rejects the
	// credentials is only detectable by re-running the test.
	if def.Login.Test != nil {
		ok, err := s.loginTestPasses(ctx, def, baseURL)
		if err != nil {
			return fmt.Errorf("tracker %s: could not verify login: %w", def.Name, err)
		}
		if !ok {
			return fmt.Errorf("tracker %s: login was rejected", def.Name)
		}
	}

	s.markLoggedIn(trackerID)
	logger.Info("authenticated")
	return nil
}

func (s *Scraper) loginIsFresh(trackerID string) bool {
	s.loginStateMu.Lock()
	defer s.loginStateMu.Unlock()

	st, ok := s.loginState[trackerID]
	return ok && time.Now().Before(st.validUntil)
}

func (s *Scraper) markLoggedIn(trackerID string) {
	s.loginStateMu.Lock()
	defer s.loginStateMu.Unlock()

	s.loginState[trackerID] = &loginState{validUntil: time.Now().Add(loginValidity)}
}

// invalidateLogin forces the next scrape of a tracker to authenticate
// again, used when a response suggests the session has lapsed.
func (s *Scraper) invalidateLogin(trackerID string) {
	s.loginStateMu.Lock()
	defer s.loginStateMu.Unlock()

	delete(s.loginState, trackerID)
}

// loginTestPasses reports whether the definition's test selector is
// present at its test path, which is how Cardigann definitions recognize
// an authenticated session.
func (s *Scraper) loginTestPasses(ctx context.Context, def *Tracker, baseURL *url.URL) (bool, error) {
	if def.Login.Test == nil {
		return false, nil
	}

	testURL := baseURL.ResolveReference(&url.URL{Path: def.Login.Test.Path})
	body, err := s.fetch(ctx, def, &SearchPath{Method: http.MethodGet}, testURL, url.Values{})
	if err != nil {
		return false, err
	}
	if def.Login.Test.Selector == "" {
		return true, nil
	}

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(body))
	if err != nil {
		return false, err
	}
	return doc.Find(def.Login.Test.Selector).Length() > 0, nil
}

// performLogin submits credentials using whichever method the definition
// declares.
func (s *Scraper) performLogin(ctx context.Context, def *Tracker, baseURL *url.URL, cfg map[string]any) error {
	login := def.Login
	td := templateData{
		Config: withSiteLink(cfg, baseURL),
		False:  cardigannFalse,
		Today:  newTodayVars(time.Now()),
		True:   cardigannTrue,
	}

	inputs := url.Values{}
	for key, value := range login.Inputs {
		inputs.Set(key, renderTemplate(value, td, s.logger))
	}
	if err := requireCredentials(def, inputs); err != nil {
		return err
	}

	switch strings.ToLower(login.Method) {
	case "cookie":
		return s.loginWithCookie(def, baseURL, cfg)
	case "get":
		return s.submitLogin(ctx, def, baseURL, login.Path, http.MethodGet, inputs)
	case "post":
		return s.submitLogin(ctx, def, baseURL, login.Path, http.MethodPost, inputs)
	case "form", "":
		return s.loginWithForm(ctx, def, baseURL, inputs)
	default:
		return fmt.Errorf("tracker %s: unsupported login method %q", def.Name, login.Method)
	}
}

// requireCredentials rejects a login whose credential inputs rendered
// empty, so the failure names the real cause instead of surfacing as a
// rejected password.
func requireCredentials(def *Tracker, inputs url.Values) error {
	for _, key := range []string{"username", "password", "login", "email"} {
		if v, ok := inputs[key]; ok && strings.TrimSpace(strings.Join(v, "")) == "" {
			return fmt.Errorf("tracker %s: %w", def.Name, ErrCredentialsMissing)
		}
	}
	return nil
}

// loginWithCookie installs a cookie string supplied by configuration,
// for trackers whose session cannot be established by posting credentials.
func (s *Scraper) loginWithCookie(def *Tracker, baseURL *url.URL, cfg map[string]any) error {
	raw, _ := cfg["cookie"].(string)
	if raw == "" {
		return fmt.Errorf("tracker %s: %w", def.Name, ErrCredentialsMissing)
	}

	var cookies []*http.Cookie
	for pair := range strings.SplitSeq(raw, ";") {
		name, value, found := strings.Cut(strings.TrimSpace(pair), "=")
		if !found || name == "" {
			continue
		}
		// These attributes govern how a server instructs a browser to store
		// a cookie; this is an outgoing client cookie being loaded into a
		// jar, where they carry no meaning.
		//nolint:gosec // G124: not a Set-Cookie being issued to a client
		cookies = append(cookies, &http.Cookie{Name: name, Value: value})
	}
	if len(cookies) == 0 {
		return fmt.Errorf("tracker %s: configured cookie is empty", def.Name)
	}

	s.httpClient.Jar.SetCookies(baseURL, cookies)
	return nil
}

// loginWithForm fetches the login page, adopts the hidden fields the
// server put in the form (CSRF tokens and the like), overlays the
// definition's own inputs, and submits the result to the form's action.
func (s *Scraper) loginWithForm(ctx context.Context, def *Tracker, baseURL *url.URL, inputs url.Values) error {
	login := def.Login
	pageURL := baseURL.ResolveReference(&url.URL{Path: login.Path})

	body, err := s.fetch(ctx, def, &SearchPath{Method: http.MethodGet}, pageURL, url.Values{})
	if err != nil {
		return fmt.Errorf("tracker %s: failed to load login page: %w", def.Name, err)
	}

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(body))
	if err != nil {
		return err
	}

	form := doc.Selection
	if login.Form != "" {
		form = doc.Find(login.Form)
		if form.Length() == 0 {
			return fmt.Errorf("tracker %s: login form %q not found", def.Name, login.Form)
		}
	}

	submitted := url.Values{}
	form.Find("input").Each(func(_ int, sel *goquery.Selection) {
		name, ok := sel.Attr("name")
		if !ok || name == "" {
			return
		}
		value, _ := sel.Attr("value")
		submitted.Set(name, value)
	})
	maps.Copy(submitted, inputs)

	action := login.SubmitPath
	if action == "" {
		if attr, ok := form.Attr("action"); ok && attr != "" {
			action = attr
		} else {
			action = login.Path
		}
	}

	actionURL, err := url.Parse(action)
	if err != nil {
		return fmt.Errorf("tracker %s: invalid login action %q: %w", def.Name, action, err)
	}

	return s.postLogin(ctx, def, pageURL.ResolveReference(actionURL), http.MethodPost, submitted)
}

// submitLogin resolves a login path against the tracker's base URL and
// submits the credentials.
func (s *Scraper) submitLogin(ctx context.Context, def *Tracker, baseURL *url.URL, path, method string, inputs url.Values) error {
	target := baseURL.ResolveReference(&url.URL{Path: path})
	return s.postLogin(ctx, def, target, method, inputs)
}

// postLogin performs the credential request and checks the response for a
// site-reported login error.
func (s *Scraper) postLogin(ctx context.Context, def *Tracker, target *url.URL, method string, inputs url.Values) error {
	body, err := s.fetch(ctx, def, &SearchPath{Method: method}, target, inputs)
	if err != nil {
		return fmt.Errorf("tracker %s: login request failed: %w", def.Name, err)
	}

	if len(def.Login.Error) == 0 {
		return nil
	}

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(body))
	if err != nil {
		return err
	}
	return checkErrorBlocks(def, doc, def.Login.Error, s.logger)
}

// checkErrorBlocks reports the first site-declared error message present in
// doc, shared by the login and search error blocks.
func checkErrorBlocks(def *Tracker, doc *goquery.Document, blocks []ErrorBlock, logger *slog.Logger) error {
	for i := range blocks {
		block := &blocks[i]

		selector := block.Selector
		if selector == "" {
			selector = block.Message.Selector
		}
		if selector == "" {
			continue
		}

		sel := doc.Find(selector)
		if sel.Length() == 0 {
			continue
		}

		message := strings.TrimSpace(sel.Text())
		if block.Message.Selector != "" && block.Message.Selector != selector {
			message = strings.TrimSpace(sel.Find(block.Message.Selector).Text())
		}
		message = applyFilters(message, block.Message.Filters, templateData{encoding: def.Encoding}, logger)
		if message == "" {
			message = "tracker reported an error"
		}
		return fmt.Errorf("tracker %s: %s", def.Name, message)
	}
	return nil
}
