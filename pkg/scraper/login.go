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
	"slices"
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

	state, ok := s.loginState[trackerID]
	return ok && time.Now().Before(state.validUntil)
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
	return findIn(doc.Selection, def.Login.Test.Selector).Length() > 0, nil
}

// performLogin submits credentials using whichever method the definition
// declares.
func (s *Scraper) performLogin(ctx context.Context, def *Tracker, baseURL *url.URL, cfg map[string]any) error {
	login := def.Login
	data := templateData{
		Config: withSiteLink(cfg, baseURL),
		False:  cardigannFalse,
		Today:  newTodayVars(time.Now()),
		True:   cardigannTrue,
	}

	inputs := url.Values{}
	for key, value := range login.Inputs {
		inputs.Set(key, renderTemplate(value, data, s.logger))
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
		s.setLoginCookies(def, baseURL)
		return s.submitLogin(ctx, def, baseURL, login.Path, http.MethodPost, inputs)
	case "form", "":
		s.setLoginCookies(def, baseURL)
		return s.loginWithForm(ctx, def, baseURL, inputs, data)
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

	cookies := parseCookiePairs(raw)
	if len(cookies) == 0 {
		return fmt.Errorf("tracker %s: configured cookie is empty", def.Name)
	}

	s.httpClient.Jar.SetCookies(baseURL, cookies)
	return nil
}

// setLoginCookies loads the cookies a definition's login declares into the
// jar before a post or form login, as Jackett sends them with its login
// requests: "JAVA=OK", for example, gets past a tracker's check that the
// browser runs scripts, a check that otherwise redirects the login page
// away.
func (s *Scraper) setLoginCookies(def *Tracker, baseURL *url.URL) {
	if cookies := parseCookiePairs(strings.Join(def.Login.Cookies, "; ")); len(cookies) > 0 {
		s.httpClient.Jar.SetCookies(baseURL, cookies)
	}
}

// parseCookiePairs reads "name=value; name=value" into cookies, skipping
// any pair without a name.
func parseCookiePairs(raw string) []*http.Cookie {
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
	return cookies
}

// loginWithForm fetches the login page, adopts the hidden fields the
// server put in the form (CSRF tokens and the like), overlays the
// definition's own inputs and the values its selector inputs read off the
// page, and submits the result to the form's action.
func (s *Scraper) loginWithForm(ctx context.Context, def *Tracker, baseURL *url.URL, inputs url.Values, data templateData) error {
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

	// A definition naming no form logs in through the page's first one, as
	// in Jackett, whose action is where the credentials go.
	formSelector := login.Form
	if formSelector == "" {
		formSelector = "form"
	}
	form := findIn(doc.Selection, formSelector).First()
	if form.Length() == 0 {
		return fmt.Errorf("tracker %s: login form %q not found", def.Name, formSelector)
	}

	submitted := url.Values{}
	form.Find("input").Each(func(_ int, sel *goquery.Selection) {
		if name, ok := sel.Attr("name"); ok && name != "" && isSubmitted(sel) {
			value, _ := sel.Attr("value")
			submitted.Set(name, value)
		}
	})
	if login.Selectors {
		if inputs, err = inputsBySelector(doc, def, inputs); err != nil {
			return err
		}
	}
	maps.Copy(submitted, inputs)
	if err := s.readSelectorInputs(doc, def, data, submitted); err != nil {
		return err
	}

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

// inputsBySelector renames a form login's inputs whose keys are CSS
// selectors to the names of the inputs they select on the login page, as
// Jackett does when the login sets "selectors", for a page whose input
// names change while their ids or placeholders do not. A selector that
// selects nothing, or an input without a name, fails the login. The
// credential check runs again on the names, which the keys did not show.
func inputsBySelector(doc *goquery.Document, def *Tracker, inputs url.Values) (url.Values, error) {
	named := make(url.Values, len(inputs))
	for selector, values := range inputs {
		input := findIn(doc.Selection, selector).First()
		if input.Length() == 0 {
			return nil, fmt.Errorf("tracker %s: login input %q matched nothing on the login page", def.Name, selector)
		}
		name := input.AttrOr("name", "")
		if name == "" {
			return nil, fmt.Errorf("tracker %s: login input %q has no name", def.Name, selector)
		}
		named[name] = values
	}
	if err := requireCredentials(def, named); err != nil {
		return nil, err
	}
	return named, nil
}

// isSubmitted reports whether a browser would submit a form's input, as
// Jackett decides it: not when it is disabled, and for a checkbox or radio
// button only when it is checked.
func isSubmitted(input *goquery.Selection) bool {
	if _, isDisabled := input.Attr("disabled"); isDisabled {
		return false
	}
	switch strings.ToLower(input.AttrOr("type", "")) {
	case "checkbox", "radio":
		_, isChecked := input.Attr("checked")
		return isChecked
	}
	return true
}

// readSelectorInputs sets each of the login's selector inputs to the value
// it reads off the login page, as Jackett does: from the whole page, as a
// field reads its row, with the page's root element standing for the row.
// A required one that matches nothing fails the login, since the form
// would go out without a token the tracker checks; an optional one is
// left out. One whose template names a variable never set fails the login
// whether optional or not, as in Jackett.
func (s *Scraper) readSelectorInputs(doc *goquery.Document, def *Tracker, data templateData, submitted url.Values) error {
	page := htmlRow{sel: doc.Children().First()}
	for _, name := range slices.Sorted(maps.Keys(def.Login.SelectorInputs)) {
		field := def.Login.SelectorInputs[name]
		value, isMatched, isResolved := readField(page, field, data, s.logger)
		if !isResolved {
			return fmt.Errorf("tracker %s: login selector input %q names a variable never set", def.Name, name)
		}
		if !isMatched {
			if field.Optional {
				continue
			}
			return fmt.Errorf("tracker %s: login selector input %q matched nothing on the login page", def.Name, name)
		}
		submitted.Set(name, value)
	}
	return nil
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

		sel := findIn(doc.Selection, selector)
		if sel.Length() == 0 {
			continue
		}

		message := strings.TrimSpace(sel.Text())
		if block.Message.Selector != "" && block.Message.Selector != selector {
			message = strings.TrimSpace(findIn(sel, block.Message.Selector).Text())
		}
		message = applyFilters(message, block.Message.Filters, templateData{encoding: def.Encoding}, logger)
		if message == "" {
			message = "tracker reported an error"
		}
		return fmt.Errorf("tracker %s: %s", def.Name, message)
	}
	return nil
}
