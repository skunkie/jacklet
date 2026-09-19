// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torrplay/jacklet/pkg/database"
)

// A search path may declare the tracker categories it serves, so a
// definition can keep movies on one page and music on another. Without it
// every search fetches every page — extra requests to the tracker and
// irrelevant rows in the store.

func TestSearchPath_SelectCategories(t *testing.T) {
	for _, tc := range []struct {
		declared []CategoryID
		mapped   []string
		name     string
		run      bool
		want     []string
	}{
		{
			mapped: []string{"401", "402"},
			name:   "a path declaring nothing runs over every category",
			run:    true,
			want:   []string{"401", "402"},
		},
		{
			declared: []CategoryID{"401", "402"},
			mapped:   []string{"402", "601"},
			name:     "a path runs narrowed to what it shares with the search",
			run:      true,
			want:     []string{"402"},
		},
		{
			declared: []CategoryID{"401", "402"},
			mapped:   []string{"601"},
			name:     "a path sharing no category is skipped",
			run:      false,
		},
		{
			declared: []CategoryID{"401"},
			mapped:   nil,
			name:     "a search naming no category skips a path that declares one",
			run:      false,
		},
		{
			declared: []CategoryID{"!", "401"},
			mapped:   []string{"401", "601"},
			name:     "a negated path is skipped when it does share a category",
			run:      false,
		},
		{
			// Jackett's quirk, replicated: the categories it hands a
			// negated path that runs are the intersection it just found
			// empty, not the difference.
			declared: []CategoryID{"!", "401"},
			mapped:   []string{"601"},
			name:     "a negated path runs with no categories of its own",
			run:      true,
			want:     nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, run := SearchPath{Categories: tc.declared}.selectCategories(tc.mapped)
			require.Equal(t, tc.run, run)
			if tc.run {
				require.Equal(t, tc.want, got)
			}
		})
	}
}

func TestDefaultCategoryIDs(t *testing.T) {
	def := &Tracker{Caps: Caps{CategoryMappings: []CategoryMapping{
		{Cat: "Movies", Default: true, ID: "401"},
		{Cat: "TV", ID: "402"},
		{Cat: "Audio", Default: true, ID: "403"},
		{Cat: "Movies/HD", Default: true, ID: "401"}, // the same id mapped twice
	}}}
	require.Equal(t, []string{"401", "403"}, DefaultCategoryIDs(def))

	// Most definitions mark none, and then there is no fallback to make.
	require.Empty(t, DefaultCategoryIDs(&Tracker{Caps: Caps{CategoryMappings: []CategoryMapping{{Cat: "TV", ID: "1"}}}}))
}

// fetchedPath is one request a scrape made: which page, and the inputs it
// carried. A definition's inputs travel in the form body unless the path
// declares "method: get", so both are merged here.
type fetchedPath struct {
	Form url.Values
	Path string
}

// newPathRecorder serves an empty page from every path and records which
// paths were asked for, and with what.
func newPathRecorder(t *testing.T) (server *httptest.Server, fetched func() []fetchedPath) {
	t.Helper()
	var (
		mu   sync.Mutex
		seen []fetchedPath
	)
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(body))
		for key, values := range r.URL.Query() {
			form[key] = append(form[key], values...)
		}
		mu.Lock()
		seen = append(seen, fetchedPath{Form: form, Path: r.URL.Path})
		mu.Unlock()
		w.Write([]byte(`<table></table>`))
	}))
	t.Cleanup(server.Close)

	return server, func() []fetchedPath {
		mu.Lock()
		defer mu.Unlock()
		return append([]fetchedPath{}, seen...)
	}
}

// newCategorizedPathTracker builds a definition with a movies page and a
// music page, each declaring the tracker categories it serves.
func newCategorizedPathTracker(t *testing.T, serverURL string) (*Scraper, *Tracker) {
	t.Helper()
	def := fmt.Sprintf(`
id: path-cats
name: path-cats
links:
  - %s/
caps:
  categorymappings:
    - {id: 401, cat: Movies, default: true}
    - {id: 601, cat: Audio}
search:
  paths:
    - path: /movies.php
      categories: [401]
    - path: /music.php
      categories: [601]
    - path: /all.php
  inputs:
    cats: "{{ range .Categories }}{{.}},{{ end }}"
  rows:
    selector: ".row"
  fields:
    title:
      selector: "a"
`, serverURL)

	tracker := loadTestTracker(t, t.TempDir(), "path-cats", def)
	store, err := database.Open(context.Background(), ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { store.Close() })

	return New(store, NewConfigStore(""), "", testLogger()), tracker
}

// TestScraper_PathCategoriesSelectThePage is the point of the feature: a
// movie search must not also fetch the music page.
func TestScraper_PathCategoriesSelectThePage(t *testing.T) {
	server, fetched := newPathRecorder(t)
	scrpr, def := newCategorizedPathTracker(t, server.URL)

	// Standard Torznab 2000 (Movies) maps to this tracker's 401.
	require.NoError(t, scrpr.ScrapeIndexer(context.Background(), def,
		SearchParams{Categories: []string{"2000"}, Query: "test", Type: "movie"}))

	got := fetched()
	require.Len(t, got, 2, "expected the movies page and the unrestricted one, not music")
	require.Equal(t, "/movies.php", got[0].Path)
	require.Equal(t, "/all.php", got[1].Path)
}

// TestScraper_PathCategoriesNarrowTheTemplateVariable checks each page is
// told only about the categories it serves, which is what a definition's
// "{{ range .Categories }}" writes into the request.
func TestScraper_PathCategoriesNarrowTheTemplateVariable(t *testing.T) {
	server, fetched := newPathRecorder(t)
	scrpr, def := newCategorizedPathTracker(t, server.URL)

	// Both categories at once: 2000 (Movies) -> 401, 3000 (Audio) -> 601.
	require.NoError(t, scrpr.ScrapeIndexer(context.Background(), def,
		SearchParams{Categories: []string{"2000", "3000"}, Query: "test", Type: "search"}))

	got := fetched()
	require.Len(t, got, 3, "every page serves one of the requested categories")

	byPath := map[string]string{}
	for _, req := range got {
		byPath[req.Path] = req.Form.Get("cats")
	}
	require.Equal(t, "401,", byPath["/movies.php"], "the movies page hears only about movies")
	require.Equal(t, "601,", byPath["/music.php"], "the music page hears only about music")
	require.Equal(t, "401,601,", byPath["/all.php"], "a path declaring nothing hears about both")
}

// TestScraper_PathCategoriesFallBackToTheDefaults covers a search naming no
// category, which would otherwise match none of the declared paths. Jackett
// falls back to the categories the definition marked "default: true".
func TestScraper_PathCategoriesFallBackToTheDefaults(t *testing.T) {
	server, fetched := newPathRecorder(t)
	scrpr, def := newCategorizedPathTracker(t, server.URL)

	require.NoError(t, scrpr.ScrapeIndexer(context.Background(), def,
		SearchParams{Query: "test", Type: "search"}))

	got := fetched()
	// 401 is the only default-flagged category, so the music page stays
	// out of it.
	require.Len(t, got, 2)
	require.Equal(t, "/movies.php", got[0].Path)
	require.Equal(t, "/all.php", got[1].Path)
}

// TestScraper_AllPathsSkippedIsNotAFailure guards the reachability rule: a
// mirror that was never asked anything has not failed, and reporting the
// tracker as down would turn "this search matches no page of this site"
// into an error the client sees.
func TestScraper_AllPathsSkippedIsNotAFailure(t *testing.T) {
	server, fetched := newPathRecorder(t)
	def := fmt.Sprintf(`
id: no-match
name: no-match
links:
  - %s/
caps:
  categorymappings:
    - {id: 401, cat: Movies}
search:
  paths:
    - path: /movies.php
      categories: [401]
  rows:
    selector: ".row"
  fields:
    title:
      selector: "a"
`, server.URL)

	tracker := loadTestTracker(t, t.TempDir(), "no-match", def)
	store, err := database.Open(context.Background(), ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { store.Close() })
	scrpr := New(store, NewConfigStore(""), "", testLogger())

	// An audio search of a movies-only tracker: nothing to ask.
	require.NoError(t, scrpr.ScrapeIndexer(context.Background(), tracker,
		SearchParams{Categories: []string{"3000"}, Query: "test", Type: "music"}))
	require.Empty(t, fetched(), "no page serves that category, so none is fetched")
}
