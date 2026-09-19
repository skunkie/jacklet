// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package admin

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/torrplay/jacklet/pkg/database"
	"github.com/torrplay/jacklet/pkg/scraper"
	"github.com/torrplay/jacklet/pkg/torznab"
)

// testTimeout bounds a manual "test this indexer" scrape.
const testTimeout = 60 * time.Second

// testResultLimit and searchResultLimit cap how many rows the panel shows
// after a test scrape and a manual search respectively.
const (
	searchResultLimit = 50
	testResultLimit   = 10
)

// maskedSecret is shown in place of a stored secret and, when submitted
// back unchanged, means "keep the existing value". The real secret is
// never rendered into a page.
const maskedSecret = "••••••••" //nolint:gosec // G101: a row of bullet characters displayed in place of a secret, not a credential

// indexerView is one row of the dashboard.
type indexerView struct {
	Authenticated bool
	BackedOff     bool
	Failures      int
	ID            string
	Language      string
	Name          string
	Newest        time.Time
	NextAllowed   time.Time
	RequiresLogin bool
	Scraped       bool
	Torrents      int
	Type          string
}

// State summarizes an indexer in one word, for the dashboard.
func (v indexerView) State() string {
	switch {
	case v.BackedOff:
		return "backoff"
	case !v.Scraped:
		return "idle"
	case v.Failures > 0:
		return "failing"
	default:
		return "ok"
	}
}

type dashboardData struct {
	CSRFToken string
	Errors    []string
	Healthy   bool
	Indexers  []indexerView
	Total     int
}

func (a *Admin) handleDashboard(w http.ResponseWriter, r *http.Request, sess *session) {
	trackers, defErrs, err := a.trackers.Trackers()
	if err != nil {
		a.logger.Error("failed to load definitions for the admin panel", "error", err)
		http.Error(w, "Failed to load definitions", http.StatusInternalServerError)
		return
	}

	stats, err := a.store.Stats(r.Context())
	if err != nil {
		a.logger.Error("failed to read store statistics", "error", err)
		http.Error(w, "Failed to read statistics", http.StatusInternalServerError)
		return
	}
	total, err := a.store.Total(r.Context())
	if err != nil {
		a.logger.Error("failed to count stored torrents", "error", err)
		http.Error(w, "Failed to read statistics", http.StatusInternalServerError)
		return
	}

	indexers := make([]indexerView, 0, len(trackers))
	for i := range trackers {
		indexers = append(indexers, a.viewFor(&trackers[i], stats))
	}
	sort.Slice(indexers, func(i, j int) bool { return indexers[i].Name < indexers[j].Name })

	errs := make([]string, 0, len(defErrs))
	for _, e := range defErrs {
		errs = append(errs, e.Error())
	}

	a.render(w, "dashboard.html", dashboardData{
		CSRFToken: sess.csrfToken,
		Errors:    errs,
		Healthy:   a.store.Ping(r.Context()) == nil,
		Indexers:  indexers,
		Total:     total,
	})
}

// viewFor combines a definition with its live scrape status and stored
// statistics.
func (a *Admin) viewFor(def *scraper.Tracker, stats map[string]database.TrackerStats) indexerView {
	id := scraper.TrackerID(def)
	status := a.scrpr.Status(id)

	return indexerView{
		Authenticated: status.Authenticated,
		BackedOff:     status.BackedOff(),
		Failures:      status.Failures,
		ID:            id,
		Language:      def.Language,
		Name:          def.Name,
		Newest:        stats[id].Newest,
		NextAllowed:   status.NextAllowed,
		RequiresLogin: def.Login != nil,
		Scraped:       status.Scraped,
		Torrents:      stats[id].Torrents,
		Type:          def.Type,
	}
}

// settingView is one editable setting on an indexer's page.
type settingView struct {
	Label   string
	Name    string
	Options []optionView
	Secret  bool
	Type    string
	Value   string
}

type optionView struct {
	Label    string
	Selected bool
	Value    string
}

type indexerPageData struct {
	CSRFToken   string
	ConfigDir   string
	Indexer     indexerView
	Message     string
	Problem     string
	Settings    []settingView
	TestResults []resultView
}

type resultView struct {
	Category int
	// Download is the panel's own proxy path for this row, empty for a
	// magnet, which needs no fetching on the visitor's behalf.
	Download string
	Link     string
	Magnet   template.URL
	Seeders  int
	Size     int64
	Title    string
}

func (a *Admin) handleIndexer(w http.ResponseWriter, r *http.Request, sess *session) {
	def, ok := a.lookupIndexer(w, r)
	if !ok {
		return
	}

	a.renderIndexer(r.Context(), w, sess, def, indexerPageData{
		Message: r.URL.Query().Get("message"),
		Problem: r.URL.Query().Get("problem"),
	})
}

// renderIndexer draws one indexer's page, filling in the parts derived
// from the definition and its stored configuration.
func (a *Admin) renderIndexer(ctx context.Context, w http.ResponseWriter, sess *session, def *scraper.Tracker, data indexerPageData) {
	id := scraper.TrackerID(def)

	overrides, err := a.config.Overrides(id)
	if err != nil {
		a.logger.Warn("failed to read tracker configuration", "indexer", id, "error", err)
		overrides = map[string]any{}
		if data.Problem == "" {
			data.Problem = "Could not read the saved configuration: " + err.Error()
		}
	}

	stats, err := a.store.Stats(ctx)
	if err != nil {
		a.logger.Error("failed to read store statistics", "error", err)
		stats = map[string]database.TrackerStats{}
	}

	data.CSRFToken = sess.csrfToken
	data.ConfigDir = a.config.Dir()
	data.Indexer = a.viewFor(def, stats)
	data.Settings = settingViews(def, overrides)

	a.render(w, "indexer.html", data)
}

// settingViews renders a definition's settings for editing, substituting a
// mask for any stored secret so it is never sent to the browser.
func settingViews(def *scraper.Tracker, overrides map[string]any) []settingView {
	views := make([]settingView, 0, len(def.Settings))
	for _, s := range def.Settings {
		view := settingView{
			Label:  s.Label,
			Name:   s.Name,
			Secret: isSecret(s),
			Type:   s.Type,
		}
		if view.Label == "" {
			view.Label = s.Name
		}

		value := s.Default
		if v, ok := overrides[s.Name]; ok {
			value = v
		}

		switch {
		case view.Secret:
			if hasValue(overrides[s.Name]) {
				view.Value = maskedSecret
			}
		case s.Type == "checkbox":
			if b, _ := value.(bool); b {
				view.Value = "on"
			}
		default:
			view.Value = scalarString(value)
		}

		for _, option := range sortedOptions(s.Options) {
			view.Options = append(view.Options, optionView{
				Label:    s.Options[option],
				Selected: option == view.Value,
				Value:    option,
			})
		}

		views = append(views, view)
	}
	return views
}

func sortedOptions(options map[string]string) []string {
	keys := make([]string, 0, len(options))
	for k := range options {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// isSecret reports whether a setting holds a credential that must not be
// rendered back into a page.
func isSecret(s scraper.Setting) bool {
	if strings.EqualFold(s.Type, "password") {
		return true
	}
	name := strings.ToLower(s.Name)
	return strings.Contains(name, "password") || strings.Contains(name, "passkey") ||
		strings.Contains(name, "apikey") || strings.Contains(name, "cookie") ||
		strings.Contains(name, "token") || strings.Contains(name, "secret")
}

func hasValue(v any) bool {
	return scalarString(v) != ""
}

func scalarString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

// lookupIndexer resolves the {id} path value to a definition, writing the
// error response itself when it cannot.
func (a *Admin) lookupIndexer(w http.ResponseWriter, r *http.Request) (*scraper.Tracker, bool) {
	def, err := a.trackers.Find(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Indexer not found", http.StatusNotFound)
		return nil, false
	}
	return def, true
}

// downloadTimeout bounds a proxied torrent fetch.
const downloadTimeout = 60 * time.Second

// handleDownload serves the torrent for one stored result, fetched with
// Jacklet's own tracker session.
//
// The panel links results here rather than at the tracker: a private
// tracker answers a browser that has no account with a login page, saved
// as a ".torrent". The row id resolves to a stored result, so the endpoint
// cannot be aimed at an address of the caller's choosing.
func (a *Admin) handleDownload(w http.ResponseWriter, r *http.Request, _ *session) {
	def, ok := a.lookupIndexer(w, r)
	if !ok {
		return
	}

	rowID, err := strconv.ParseInt(r.PathValue("row"), 10, 64)
	if err != nil {
		http.Error(w, "Invalid row id", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), downloadTimeout)
	defer cancel()

	// Scoped to the indexer in the path, so one indexer's page cannot
	// fetch another's row.
	indexerID := scraper.TrackerID(def)
	row, err := a.store.Find(ctx, indexerID, rowID)
	if errors.Is(err, database.ErrNotFound) {
		http.Error(w, "No such result", http.StatusNotFound)
		return
	}
	if err != nil {
		a.logger.Error("failed to look up a download", "indexer", indexerID, "row", rowID, "error", err)
		http.Error(w, "Failed to look up the result", http.StatusInternalServerError)
		return
	}

	torznab.ServeTorrent(ctx, w, r, a.scrpr, def, row, a.logger)
}

// handleSaveSettings writes a tracker's settings and credentials. A secret
// left at its mask keeps the value already stored, so saving the form
// without retyping a password does not wipe it.
func (a *Admin) handleSaveSettings(w http.ResponseWriter, r *http.Request, sess *session) {
	def, ok := a.lookupIndexer(w, r)
	if !ok {
		return
	}
	id := scraper.TrackerID(def)

	if !a.config.Enabled() {
		a.renderIndexer(r.Context(), w, sess, def, indexerPageData{
			Problem: "No config directory is configured, so settings cannot be saved. Set JACKLET_CONFIG_DIR or -config-dir.",
		})
		return
	}

	existing, err := a.config.Overrides(id)
	if err != nil {
		a.logger.Warn("failed to read tracker configuration before saving", "indexer", id, "error", err)
		existing = map[string]any{}
	}

	overrides := make(map[string]any, len(def.Settings))
	for _, s := range def.Settings {
		submitted := r.PostFormValue(s.Name)

		switch {
		case isSecret(s):
			if submitted == maskedSecret {
				// Unchanged: keep whatever is already stored.
				if v, ok := existing[s.Name]; ok {
					overrides[s.Name] = v
				}
				continue
			}
			if submitted != "" {
				overrides[s.Name] = submitted
			}

		case s.Type == "checkbox":
			overrides[s.Name] = submitted != ""

		default:
			if submitted != "" {
				overrides[s.Name] = submitted
			}
		}
	}

	if err := a.config.Save(id, overrides); err != nil {
		a.logger.Error("failed to save tracker configuration", "indexer", id, "error", err)
		a.renderIndexer(r.Context(), w, sess, def, indexerPageData{Problem: "Could not save: " + err.Error()})
		return
	}
	// The credentials may have just changed, so a session established with
	// the old ones must not be trusted.
	a.scrpr.InvalidateLogin(id)

	a.logger.Info("saved tracker configuration", "indexer", id, "settings", len(overrides))
	// Redirect after a successful POST so a refresh does not resubmit.
	redirect := url.URL{
		Path:     "/admin/indexers/" + url.PathEscape(id),
		RawQuery: url.Values{"message": {"Settings saved"}}.Encode(),
	}
	http.Redirect(w, r, redirect.String(), http.StatusSeeOther)
}

// handleTest runs a live search against an indexer so an operator can see
// whether its definition and credentials actually work.
func (a *Admin) handleTest(w http.ResponseWriter, r *http.Request, sess *session) {
	def, ok := a.lookupIndexer(w, r)
	if !ok {
		return
	}
	id := scraper.TrackerID(def)

	scrapeCtx, cancel := context.WithTimeout(r.Context(), testTimeout)

	query := strings.TrimSpace(r.PostFormValue("query"))
	data := indexerPageData{}

	if err := a.scrpr.ScrapeIndexer(scrapeCtx, def, scraper.SearchParams{Query: query, Type: "search"}); err != nil {
		cancel()
		a.logger.Warn("admin indexer test failed", "indexer", id, "error", err)
		data.Problem = "Test failed: " + err.Error()
		a.renderIndexer(r.Context(), w, sess, def, data)
		return
	}
	cancel()

	results, err := a.recentResults(r.Context(), id)
	if err != nil {
		a.logger.Error("failed to read test results", "indexer", id, "error", err)
		data.Problem = "The scrape succeeded but its results could not be read: " + err.Error()
		a.renderIndexer(r.Context(), w, sess, def, data)
		return
	}

	data.TestResults = results
	switch {
	case len(results) > 0:
		data.Message = "Test succeeded."
	default:
		data.Message = "The tracker responded, but nothing is stored for it yet. " +
			"That can mean the search matched nothing, or that the definition's row selector no longer matches the site."
	}
	a.renderIndexer(r.Context(), w, sess, def, data)
}

// recentResults reads the most recently stored torrents for one tracker,
// to show what a test scrape produced.
func (a *Admin) recentResults(ctx context.Context, trackerID string) ([]resultView, error) {
	torrents, err := a.store.Recent(ctx, trackerID, testResultLimit)
	if err != nil {
		return nil, err
	}
	return resultViews(torrents), nil
}

type searchPageData struct {
	CSRFToken string
	Indexer   string
	Indexers  []indexerView
	Problem   string
	Query     string
	Results   []resultView
	Searched  bool
}

// handleSearchForm shows the empty manual-search page.
func (a *Admin) handleSearchForm(w http.ResponseWriter, r *http.Request, sess *session) {
	data, ok := a.searchData(w, sess)
	if !ok {
		return
	}
	a.render(w, "search.html", data)
}

// searchData assembles the parts of the search page that do not depend on
// a query.
func (a *Admin) searchData(w http.ResponseWriter, sess *session) (searchPageData, bool) {
	trackers, _, err := a.trackers.Trackers()
	if err != nil {
		http.Error(w, "Failed to load definitions", http.StatusInternalServerError)
		return searchPageData{}, false
	}

	stats := map[string]database.TrackerStats{}
	indexers := make([]indexerView, 0, len(trackers))
	for i := range trackers {
		indexers = append(indexers, a.viewFor(&trackers[i], stats))
	}
	sort.Slice(indexers, func(i, j int) bool { return indexers[i].Name < indexers[j].Name })

	return searchPageData{CSRFToken: sess.csrfToken, Indexers: indexers}, true
}

// handleSearch runs a manual search against one indexer and shows the
// parsed rows, for checking a definition by eye. It scrapes the tracker
// live, which is why it is a POST rather than a bookmarkable GET.
func (a *Admin) handleSearch(w http.ResponseWriter, r *http.Request, sess *session) {
	data, ok := a.searchData(w, sess)
	if !ok {
		return
	}
	data.Indexer = r.PostFormValue("indexer")
	data.Query = r.PostFormValue("q")

	if data.Indexer == "" {
		data.Problem = "Choose an indexer to search."
		a.render(w, "search.html", data)
		return
	}

	def, err := a.trackers.Find(data.Indexer)
	if err != nil {
		data.Problem = "Unknown indexer."
		a.render(w, "search.html", data)
		return
	}

	data.Searched = true
	scrapeCtx, cancel := context.WithTimeout(r.Context(), testTimeout)
	params := scraper.SearchParams{Query: data.Query, Type: "search"}

	if err := a.scrpr.ScrapeIndexer(scrapeCtx, def, params); err != nil {
		data.Problem = "Scrape failed: " + err.Error()
	}
	cancel()

	results, err := a.matchingResults(r.Context(), data.Indexer, params)
	if err != nil {
		data.Problem = "Could not read results: " + err.Error()
		a.render(w, "search.html", data)
		return
	}
	data.Results = results

	a.render(w, "search.html", data)
}

// matchingResults reads stored torrents for a tracker whose name contains
// every word of the query.
func (a *Admin) matchingResults(ctx context.Context, trackerID string, params scraper.SearchParams) ([]resultView, error) {
	torrents, _, err := a.store.Search(ctx, database.Search{
		Limit:    searchResultLimit,
		QueryKey: params.ResultKey(),
		Terms:    strings.Fields(params.Query),
		Trackers: []string{trackerID},
	})
	if err != nil {
		return nil, err
	}
	return resultViews(torrents), nil
}

// magnetHref returns raw as a URL the template may write into an href,
// for a magnet link and nothing else.
//
// html/template writes "#ZgotmplZ" in place of any scheme it does not
// recognise, and magnet is one of those, so a magnet link needs the typed
// form to survive. That type also skips the sanitiser, so the scheme is
// checked here: a definition is third-party data, and its download field
// is whatever the site put there.
func magnetHref(raw string) template.URL {
	parsed, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(parsed.Scheme, "magnet") {
		return ""
	}
	// The typed form skips the scheme filter, which the check above has
	// already made. Everything else about the value is still escaped for
	// the attribute it lands in.
	//nolint:gosec // G203: the scheme is checked above, and a magnet URI carries no script.
	return template.URL(raw)
}

// downloadPath is where the panel serves row's torrent from, or "" when
// there is nothing to proxy.
func downloadPath(row database.Torrent) string {
	if row.DownloadURL == "" {
		return ""
	}
	return fmt.Sprintf("%s/indexers/%s/download/%d", adminPrefix, url.PathEscape(row.Tracker), row.ID)
}

// resultViews renders stored torrents for the panel's result tables.
func resultViews(torrents []database.Torrent) []resultView {
	views := make([]resultView, len(torrents))
	for i := range torrents {
		views[i] = resultView{
			Category: torrents[i].Category,
			Download: downloadPath(torrents[i]),
			Link:     torrents[i].DownloadURL,
			Magnet:   magnetHref(torrents[i].Magnet),
			Seeders:  torrents[i].Seeders,
			Size:     torrents[i].Size,
			Title:    torrents[i].Name,
		}
	}
	return views
}
