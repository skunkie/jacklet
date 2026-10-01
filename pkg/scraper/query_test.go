// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// These tests cover the ".Query.*" template context, which is how a
// Cardigann definition reaches the raw search parameters — including the
// client's paging, since Cardigann has no page loop of its own and a
// definition that pages does it by building the request from
// ".Query.Offset"/".Query.Limit".

// newFormRecorder serves an empty result page and records the query string
// and form body of every request, so a test can assert what a definition's
// templates rendered into the tracker request.
func newFormRecorder(t *testing.T) (server *httptest.Server, requests func() []url.Values) {
	t.Helper()
	var (
		mu  sync.Mutex
		got []url.Values
	)
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(body))
		for key, values := range r.URL.Query() {
			form[key] = append(form[key], values...)
		}
		mu.Lock()
		got = append(got, form)
		mu.Unlock()
		w.Write([]byte(`<table></table>`))
	}))
	t.Cleanup(server.Close)

	return server, func() []url.Values {
		mu.Lock()
		defer mu.Unlock()
		return append([]url.Values{}, got...)
	}
}

// newQueryTracker loads a definition whose inputs are the given template
// strings, against server.
func newQueryTracker(t *testing.T, id, serverURL, inputs string) (*Scraper, *Tracker) {
	t.Helper()
	def := fmt.Sprintf(`
id: %[1]s
name: %[1]s
links:
  - %[2]s/
search:
  paths:
    - path: "/"
  inputs:
%[3]s
  rows:
    selector: ".row"
  fields:
    title:
      selector: "a"
`, id, serverURL, inputs)

	tracker := loadTestTracker(t, t.TempDir(), id, def)
	store := &fakeStore{}

	return NewWithOptions(NewConfigStore(""), "", slog.New(slog.DiscardHandler), Options{Sink: store}), tracker
}

// TestScraperQueryPagingReachesTheTracker covers the paging variables a
// definition pages with.
func TestScraperQueryPagingReachesTheTracker(t *testing.T) {
	server, requests := newFormRecorder(t)
	scrpr, def := newQueryTracker(t, "paging-tracker", server.URL, `    page: "{{ .Query.Offset }}"
    perpage: "{{ .Query.Limit }}"`)

	require.NoError(t, scrpr.scrapeIndexer(t.Context(), def,
		SearchParams{Limit: 50, Offset: 100, Query: "test", Type: "search"}))

	got := requests()
	require.Len(t, got, 1)
	require.Equal(t, "100", got[0].Get("page"))
	require.Equal(t, "50", got[0].Get("perpage"))
}

// TestScraperQueryPagingIsEmptyWithoutAPage checks offset zero renders as
// empty rather than "0", so a definition's "{{ if .Query.Offset }}" is
// false for an unpaged search — which is how Jackett's null behaves.
func TestScraperQueryPagingIsEmptyWithoutAPage(t *testing.T) {
	server, requests := newFormRecorder(t)
	scrpr, def := newQueryTracker(t, "unpaged-tracker", server.URL,
		`    page: "{{ if .Query.Offset }}{{ .Query.Offset }}{{ else }}first{{ end }}"`)

	require.NoError(t, scrpr.scrapeIndexer(t.Context(), def,
		SearchParams{Limit: 50, Offset: 0, Query: "test", Type: "search"}))

	got := requests()
	require.Len(t, got, 1)
	require.Equal(t, "first", got[0].Get("page"))
}

// TestScraperPagingDefeatsTheDeduplicationWindow is the point of
// pagesServerSide: a client asking for page 2 right after page 1 must
// reach the tracker, since for a paging definition that is a different
// request, not a repeat.
func TestScraperPagingDefeatsTheDeduplicationWindow(t *testing.T) {
	server, requests := newFormRecorder(t)
	scrpr, def := newQueryTracker(t, "paged-dedup", server.URL, `    page: "{{ .Query.Offset }}"`)

	ctx := t.Context()
	base := SearchParams{Limit: 50, Query: "test", Type: "search"}
	first := base
	second := base
	second.Offset = 50

	require.NoError(t, scrpr.scrapeIndexer(ctx, def, first))
	require.NoError(t, scrpr.scrapeIndexer(ctx, def, second))

	got := requests()
	require.Len(t, got, 2, "expected the second page to be fetched rather than de-duplicated")
	require.Empty(t, got[0].Get("page"))
	require.Equal(t, "50", got[1].Get("page"))
}

// TestScraperPagingDoesNotDefeatDeduplicationForOtherDefinitions is the
// other half: a definition that ignores paging must keep being protected
// by the de-duplication window, or a client walking pages would re-scrape
// the same tracker request once per page.
func TestScraperPagingDoesNotDefeatDeduplicationForOtherDefinitions(t *testing.T) {
	server, requests := newFormRecorder(t)
	scrpr, def := newQueryTracker(t, "unpaged-dedup", server.URL, `    nm: "{{ .Keywords }}"`)

	ctx := t.Context()
	base := SearchParams{Limit: 50, Query: "test", Type: "search"}
	second := base
	second.Offset = 50

	require.NoError(t, scrpr.scrapeIndexer(ctx, def, base))
	require.NoError(t, scrpr.scrapeIndexer(ctx, def, second))

	require.Len(t, requests(), 1, "expected the second page to be answered from the store")
}

// TestScraperQueryVariables covers the rest of the ".Query.*" surface a
// Jackett definition may reference. A missing field is a template error,
// which renderTemplate reports by leaving the string unresolved — so a
// gap here would send the tracker a literal "{{ .Query.IsTVSearch }}".
func TestScraperQueryVariables(t *testing.T) {
	server, requests := newFormRecorder(t)
	scrpr, def := newQueryTracker(t, "vars-tracker", server.URL, `    type: "{{ .Query.Type }}"
    q: "{{ .Query.Q }}"
    ep: "{{ .Query.Episode }}"
    imdb: "{{ .Query.IMDBIDShort }}"
    tvsearch: "{{ if .Query.IsTVSearch }}yes{{ else }}no{{ end }}"
    idsearch: "{{ if .Query.IsIdSearch }}yes{{ else }}no{{ end }}"
    imdbquery: "{{ if .Query.IsImdbQuery }}yes{{ else }}no{{ end }}"
    rss: "{{ if .Query.IsRssSearch }}yes{{ else }}no{{ end }}"
    movie: "{{ if .Query.IsMovieSearch }}yes{{ else }}no{{ end }}"
    series: "[{{ .Query.Series }}{{ .Query.Movie }}]"
    trakt: "{{ .Query.TraktID }}"`)

	require.NoError(t, scrpr.scrapeIndexer(t.Context(), def, SearchParams{
		Ep: "2", IMDBID: "tt1234567", Query: "some show", Season: "1", TraktID: "42", Type: "tvsearch",
	}))

	got := requests()
	require.Len(t, got, 1)
	require.Equal(t, "tvsearch", got[0].Get("type"))
	// .Query.Q is the raw parameter, unlike .Keywords, which has the
	// season/episode folded in.
	require.Equal(t, "some show", got[0].Get("q"))
	require.Equal(t, "S01E02", got[0].Get("ep"))
	require.Equal(t, "1234567", got[0].Get("imdb"))
	require.Equal(t, "yes", got[0].Get("tvsearch"))
	require.Equal(t, "yes", got[0].Get("idsearch"))
	require.Equal(t, "yes", got[0].Get("imdbquery"))
	require.Equal(t, "no", got[0].Get("rss"), "a request with a query is not an RSS-style one")
	require.Equal(t, "no", got[0].Get("movie"))
	require.Equal(t, "42", got[0].Get("trakt"))
	// Always empty, as they are in Jackett.
	require.Equal(t, "[]", got[0].Get("series"))
}

// TestScraperQueryIsRssSearch covers the RSS-style request: no terms and
// no ids, which is what a client's periodic "latest releases" poll sends.
func TestScraperQueryIsRssSearch(t *testing.T) {
	server, requests := newFormRecorder(t)
	scrpr, def := newQueryTracker(t, "rss-tracker", server.URL,
		`    rss: "{{ if .Query.IsRssSearch }}yes{{ else }}no{{ end }}"`)

	require.NoError(t, scrpr.scrapeIndexer(t.Context(), def, SearchParams{Type: "search"}))

	got := requests()
	require.Len(t, got, 1)
	require.Equal(t, "yes", got[0].Get("rss"))
}

func TestPagesServerSide(t *testing.T) {
	for _, tc := range []struct {
		name string
		def  Tracker
		want bool
	}{
		{name: "no paging", def: Tracker{Search: Search{Inputs: map[string]string{"nm": "{{ .Keywords }}"}}}},
		{
			name: "shared input",
			def:  Tracker{Search: Search{Inputs: map[string]string{"page": "{{ .Query.Offset }}"}}},
			want: true,
		},
		{
			name: "path input",
			def:  Tracker{Search: Search{Paths: []SearchPath{{Inputs: map[string]string{"n": "{{ .Query.Limit }}"}}}}},
			want: true,
		},
		{
			name: "path itself",
			def:  Tracker{Search: Search{Paths: []SearchPath{{Path: "browse/{{ .Query.Offset }}"}}}},
			want: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, pagesServerSide(&tc.def))
		})
	}
}

// TestScraperQueryKeywordsIsUnfiltered covers ".Query.Keywords", which is
// the search text as the client sent it, where the top-level ".Keywords"
// is the same value after the definition's keywordsfilters have rewritten
// it for the tracker's own search box. A definition that needs both has no
// other way to reach the original.
func TestScraperQueryKeywordsIsUnfiltered(t *testing.T) {
	server, requests := newFormRecorder(t)
	scrpr, def := newQueryTracker(t, "rawkeywords-tracker", server.URL,
		`    raw: "{{ .Query.Keywords }}"
    filtered: "{{ .Keywords }}"`)
	// A filter that rewrites the query, so the two variables differ.
	def.Search.KeywordsFilters = []Filter{{Args: []any{`[\s]+`, "."}, Name: "re_replace"}}

	require.NoError(t, scrpr.scrapeIndexer(t.Context(), def,
		SearchParams{Ep: "2", Query: "some show", Season: "1", Type: "tvsearch"}))

	got := requests()
	require.Len(t, got, 1)
	// Season and episode are folded in before the filters run, which is
	// exactly what those filters exist to rewrite.
	require.Equal(t, "some show S01E02", got[0].Get("raw"))
	require.Equal(t, "some.show.S01E02", got[0].Get("filtered"))
}

// TestScraperQueryCategoriesAreTheClientsOwn pins the difference between
// the two category variables, which is easy to conflate: ".Categories" is
// the tracker's own ids, reverse-mapped through caps.categorymappings,
// while ".Query.Categories" is what the client asked for — as Jackett
// sets them.
func TestScraperQueryCategoriesAreTheClientsOwn(t *testing.T) {
	server, requests := newFormRecorder(t)
	scrpr, def := newQueryTracker(t, "catvars-tracker", server.URL,
		`    site: "{{ range .Categories }}{{.}},{{ end }}"
    asked: "{{ range .Query.Categories }}{{.}},{{ end }}"`)
	def.Caps.CategoryMappings = []CategoryMapping{{Cat: "Movies", ID: "401"}}

	require.NoError(t, scrpr.scrapeIndexer(t.Context(), def,
		SearchParams{Categories: []string{"2000"}, Query: "test", Type: "movie"}))

	got := requests()
	require.Len(t, got, 1)
	require.Equal(t, "401,", got[0].Get("site"), "the tracker's own category id")
	require.Equal(t, "2000,", got[0].Get("asked"), "the standard Torznab id the client sent")
}

func TestSearchParams_ResultKey_CoversSemanticsButNotPaging(t *testing.T) {
	base := SearchParams{Categories: []string{"2000", "5000"}, Query: "Example", Season: "1", Type: "tvsearch"}
	reordered := base
	reordered.Categories = []string{"5000", "2000"}
	reordered.Limit = 10
	reordered.Offset = 40
	require.Equal(t, base.ResultKey(), reordered.ResultKey(),
		"category order or paging changed the logical result key")

	changed := base
	changed.TVDBID = "123"
	require.NotEqual(t, base.ResultKey(), changed.ResultKey(),
		"a search identifier did not change the logical result key")
}

func TestSearchParams_Keywords_FoldsMediaTerms(t *testing.T) {
	got := SearchParams{Album: "Test Album", Artist: "Test Artist"}.keywords()
	require.Equal(t, "Test Artist Test Album", got)
}
