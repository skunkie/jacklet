// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"
	"github.com/stretchr/testify/require"
	"github.com/torrplay/jacklet/pkg/database"
)

// stubRow stands in where a filter only needs a row to exist.
type stubRow struct{ raw string }

func (r stubRow) debugString() string         { return r.raw }
func (r stubRow) lookup(Field) (string, bool) { return "", false }
func (r stubRow) matches(string) bool         { return false }

// Jackett's MatchQueryStringAND splits on non-word characters, ignores
// single characters and the common words, and compares case-insensitively.
func TestMatchQueryStringAND(t *testing.T) {
	for _, tc := range []struct {
		name, title, query string
		limit              int
		want               bool
	}{
		{name: "all words present", title: "Some.Show.S01E02.1080p", query: "some show", want: true},
		{name: "a word missing", title: "Other.Show.S01E02", query: "some show", want: false},
		{name: "case insensitive", title: "SOME SHOW", query: "some show", want: true},
		{name: "punctuation splits both sides", title: "Some.Show-2024", query: "some/show 2024", want: true},
		{name: "common words ignored", title: "Show.Name", query: "the show and name", want: true},
		{name: "single characters ignored", title: "Show.Name", query: "a show n name", want: true},
		{name: "empty query matches anything", title: "Whatever", query: "", want: true},
		// A tracker listing a truncated title compares only the first N
		// characters of the query.
		{name: "limit truncates the query", title: "Some.Show", query: "some show extra", limit: 9, want: true},
		{name: "without the limit it would fail", title: "Some.Show", query: "some show extra", want: false},
		{name: "limit longer than the query is harmless", title: "Some.Show", query: "some show", limit: 500, want: true},
		// Go's \w is ASCII-only, so a Cyrillic query must not be split
		// into single characters, which would then all be ignored.
		{name: "cyrillic words are words", title: "Большой Фильм 2024", query: "большой фильм", want: true},
		{name: "cyrillic mismatch is rejected", title: "Другой Фильм", query: "большой фильм", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, matchQueryStringAND(tc.title, tc.query, tc.limit))
		})
	}
}

// andmatch is a row filter, not a value filter: it rejects a whole row.
func TestSkipRow_AndMatch(t *testing.T) {
	def := &Tracker{ID: "t", Name: "T"}
	def.Search.Rows.Filters = []Filter{{Name: "andmatch"}}
	td := templateData{Keywords: "some show"}

	require.False(t, skipRow(def, stubRow{}, "Some.Show.S01E02", td, testLogger()))
	require.True(t, skipRow(def, stubRow{}, "Unrelated.Release", td, testLogger()),
		"a row whose title lacks a query word must be dropped")
}

// The optional argument caps how many characters of the query are compared.
func TestSkipRow_AndMatchCharacterLimit(t *testing.T) {
	def := &Tracker{ID: "t", Name: "T"}
	def.Search.Rows.Filters = []Filter{{Args: "9", Name: "andmatch"}}
	td := templateData{Keywords: "some show extra"}

	require.False(t, skipRow(def, stubRow{}, "Some.Show", td, testLogger()),
		"only the first 9 characters of the query should be compared")
}

// A search the tracker answered by external id has no reason to carry the
// keywords in its titles, so the filter stands down -- but only when the
// definition actually advertises that id.
func TestSkipRow_AndMatchSkippedForIDSearch(t *testing.T) {
	td := templateData{Keywords: "some show", Query: queryParams{IMDBID: "tt1234567"}}

	t.Run("definition advertises imdbid", func(t *testing.T) {
		def := &Tracker{ID: "t", Name: "T"}
		def.Caps.Modes = map[string][]string{"movie-search": {"q", "imdbid"}}
		def.Search.Rows.Filters = []Filter{{Name: "andmatch"}}
		require.False(t, skipRow(def, stubRow{}, "Unrelated.Release", td, testLogger()))
	})

	t.Run("definition does not advertise it", func(t *testing.T) {
		def := &Tracker{ID: "t", Name: "T"}
		def.Caps.Modes = map[string][]string{"search": {"q"}}
		def.Search.Rows.Filters = []Filter{{Name: "andmatch"}}
		require.True(t, skipRow(def, stubRow{}, "Unrelated.Release", td, testLogger()),
			"the tracker was searched by keywords, so the filter still applies")
	})
}

// strdump is debugging only, and an unknown row filter must not silently
// reject every row.
func TestSkipRow_OtherFilters(t *testing.T) {
	def := &Tracker{ID: "t", Name: "T"}
	def.Search.Rows.Filters = []Filter{{Name: "strdump"}, {Name: "not_a_filter"}}
	require.False(t, skipRow(def, stubRow{}, "Anything", templateData{Keywords: "nope"}, testLogger()))
}

// andmatch must not be reachable as a value filter: applied to a field it
// would rewrite the value instead of judging the row.
func TestAndMatchIsNotAValueFilter(t *testing.T) {
	_, ok := filterRegistry["andmatch"]
	require.False(t, ok, "andmatch belongs to the row filters, not filterRegistry")
}

// A definition declaring "search.rows.filters" must have them applied to a
// real scrape. Definitions such as ilcorsaroblu and girotorrent rely on
// this: their trackers OR the query terms together, and without the filter
// every unrelated release comes back as a result.
func TestScraper_RowFiltersDropUnrelatedReleases(t *testing.T) {
	const testHTML = `
		<table>
			<tr class="torrent_row">
				<td><a href="/dl/1">Some.Show.S01E02.1080p</a></td>
				<td>1.5 GB</td><td>10</td><td>2</td>
			</tr>
			<tr class="torrent_row">
				<td><a href="/dl/2">Totally.Unrelated.Release</a></td>
				<td>2.0 GB</td><td>20</td><td>3</td>
			</tr>
		</table>
	`

	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(testHTML))
	}))
	defer testServer.Close()

	testDef := fmt.Sprintf(`
id: andmatch-tracker
name: andmatch-tracker
links:
  - %s/
search:
  paths:
    - path: "/"
      method: get
  rows:
    selector: ".torrent_row"
    filters:
      - name: andmatch
  fields:
    title:
      selector: "td:nth-child(1) a"
    download:
      selector: "td:nth-child(1) a"
      attribute: "href"
    size:
      selector: "td:nth-child(2)"
    seeders:
      selector: "td:nth-child(3)"
    leechers:
      selector: "td:nth-child(4)"
`, testServer.URL)

	store, err := database.Open(context.Background(), ":memory:")
	require.NoError(t, err)
	defer store.Close()

	def := loadTestTracker(t, t.TempDir(), "andmatch-tracker", testDef)
	scraper := New(store, NewConfigStore(""), "", testLogger())
	require.NoError(t, scraper.ScrapeIndexer(context.Background(), def, SearchParams{Query: "some show"}))

	stored, _, err := store.Search(context.Background(), database.Search{Limit: 10})
	require.NoError(t, err)
	require.Len(t, stored, 1, "the unrelated release should have been filtered out")
	require.Equal(t, "Some.Show.S01E02.1080p", stored[0].Name)
}

// strdump exists to show what a selector is being written against, so it
// has to log the row as served, not the values already extracted from it.
func TestSkipRow_StrdumpLogsTheRow(t *testing.T) {
	var logged bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))

	def := &Tracker{ID: "t", Name: "T"}
	def.Search.Rows.Filters = []Filter{{Name: "strdump"}}

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(
		`<table><tr class="row"><td><a href="/dl/1">Some.Release</a></td></tr></table>`))
	require.NoError(t, err)
	row := htmlRow{sel: doc.Find(".row")}

	require.False(t, skipRow(def, row, "Some.Release", templateData{}, logger))

	dumped := logged.String()
	require.Contains(t, dumped, "href", "the row's markup should be logged")
	require.Contains(t, dumped, "/dl/1", "the row's markup should be logged")
}

// Each row type renders itself in the form its selectors address.
func TestResultRowDebugString(t *testing.T) {
	t.Run("html", func(t *testing.T) {
		doc, err := goquery.NewDocumentFromReader(strings.NewReader(`<div class="r"><a href="/x">T</a></div>`))
		require.NoError(t, err)
		require.Equal(t, `<div class="r"><a href="/x">T</a></div>`, htmlRow{sel: doc.Find(".r")}.debugString())
	})

	t.Run("json", func(t *testing.T) {
		require.JSONEq(t, `{"title":"T"}`, jsonRow{value: map[string]any{"title": "T"}}.debugString())
	})

	t.Run("xml", func(t *testing.T) {
		root, err := parseXML(`<item id="1"><title>T</title></item>`)
		require.NoError(t, err)
		require.Equal(t, `<item id="1"><title>T</title></item>`, xmlRow{node: root.Children[0]}.debugString())
	})

	// A row carrying encoded markup is exactly the row worth dumping, so
	// its text and attributes have to come back out escaped rather than
	// as markup that never existed.
	t.Run("xml escapes text and attributes", func(t *testing.T) {
		root, err := parseXML(`<item u="a&amp;b &lt;c&gt;"><t>Tom &amp; Jerry &lt;b&gt;</t></item>`)
		require.NoError(t, err)

		dumped := xmlRow{node: root.Children[0]}.debugString()
		require.Contains(t, dumped, "Tom &amp; Jerry &lt;b&gt;")
		require.Contains(t, dumped, `u="a&amp;b &lt;c&gt;"`)
		require.NotContains(t, dumped, "<b>", "decoded markup must not reappear as real elements")

		// What comes out is well-formed, which is the point: it can be
		// pasted back into a parser.
		reparsed, err := parseXML(dumped)
		require.NoError(t, err)
		require.Equal(t, "Tom & Jerry <b>", reparsed.Children[0].text(nil))
	})
}

// A rejection is not the end of the filter list. A definition pairing
// andmatch with a following strdump is asking to see the rows that were
// dropped -- which is exactly when the dump is worth having.
func TestSkipRow_ContinuesAfterRejection(t *testing.T) {
	var logged bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))

	def := &Tracker{ID: "t", Name: "T"}
	def.Search.Rows.Filters = []Filter{{Name: "andmatch"}, {Name: "strdump"}}

	skipped := skipRow(def, stubRow{raw: "<tr>the row</tr>"}, "Unrelated.Release",
		templateData{Keywords: "some show"}, logger)

	require.True(t, skipped, "the row is still rejected")
	require.Contains(t, logged.String(), "the row",
		"strdump after andmatch must still run for a rejected row")
}

// The capability gates are Jackett's, including the ones that check a
// capability for a different id than the one being searched for.
func TestIDSearchSupported_JackettGates(t *testing.T) {
	withCaps := func(modes map[string][]string, allowTVIMDB bool) *Tracker {
		def := &Tracker{ID: "t", Name: "T"}
		def.Caps.Modes = modes
		def.Caps.AllowTVSearchIMDB = allowTVIMDB
		return def
	}
	movieIMDB := map[string][]string{"movie-search": {"q", "imdbid"}}
	keywordsOnly := map[string][]string{"search": {"q"}}

	for _, tc := range []struct {
		name        string
		modes       map[string][]string
		allowTVIMDB bool
		query       queryParams
		want        bool
	}{
		{name: "imdb via movie caps", modes: movieIMDB, query: queryParams{IMDBID: "tt1"}, want: true},
		{name: "imdb via allowtvsearchimdb", modes: keywordsOnly, allowTVIMDB: true, query: queryParams{IMDBID: "tt1"}, want: true},
		{name: "imdb unadvertised", modes: keywordsOnly, query: queryParams{IMDBID: "tt1"}, want: false},

		{name: "tmdb via movie caps", modes: map[string][]string{"movie-search": {"tmdbid"}}, query: queryParams{TMDBID: "1"}, want: true},
		{name: "tmdb via tv caps", modes: map[string][]string{"tv-search": {"tmdbid"}}, query: queryParams{TMDBID: "1"}, want: true},
		{name: "tvdb via tv caps", modes: map[string][]string{"tv-search": {"tvdbid"}}, query: queryParams{TVDBID: "1"}, want: true},
		{name: "tvdb is not gated on movie caps", modes: map[string][]string{"movie-search": {"tvdbid"}}, query: queryParams{TVDBID: "1"}, want: false},

		// Jackett gates these on the imdb capability rather than their own,
		// which is reproduced rather than tidied.
		{name: "douban rides on imdb caps", modes: movieIMDB, query: queryParams{DoubanID: "1"}, want: true},
		{name: "trakt rides on imdb caps", modes: movieIMDB, query: queryParams{TraktID: "1"}, want: true},
		{name: "douban with its own param is still gated on imdb", modes: map[string][]string{"movie-search": {"doubanid"}}, query: queryParams{DoubanID: "1"}, want: false},

		// These two ride on the tv-search imdb flag specifically, so movie
		// imdb caps are not enough.
		{name: "tvmaze needs allowtvsearchimdb", modes: keywordsOnly, allowTVIMDB: true, query: queryParams{TVMazeID: "1"}, want: true},
		{name: "tvmaze not satisfied by movie imdb", modes: movieIMDB, query: queryParams{TVMazeID: "1"}, want: false},
		{name: "rage needs allowtvsearchimdb", modes: keywordsOnly, allowTVIMDB: true, query: queryParams{TVRageID: "1"}, want: true},
		{name: "rage not satisfied by movie imdb", modes: movieIMDB, query: queryParams{TVRageID: "1"}, want: false},

		{name: "no id at all", modes: movieIMDB, query: queryParams{}, want: false},
		// A definition declaring no modes advertises no id, as in Jackett;
		// SearchModes' generic fallback must not widen the gates.
		{name: "no declared modes", modes: nil, query: queryParams{IMDBID: "tt1"}, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, idSearchSupported(withCaps(tc.modes, tc.allowTVIMDB), tc.query))
		})
	}
}
