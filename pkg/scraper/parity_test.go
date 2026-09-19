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
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torrplay/jacklet/pkg/database"
)

// newScrapeFixture serves body at "/" and returns a scraper plus the
// definition loaded from def (with %[1]s substituted for the server URL).
func newScrapeFixture(t *testing.T, id, body, def string) (*Scraper, *Tracker) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, body)
	}))
	t.Cleanup(server.Close)

	dir := t.TempDir()
	tracker := loadTestTracker(t, dir, id, fmt.Sprintf(def, server.URL))

	store, err := database.Open(context.Background(), ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { store.Close() })

	return New(store, NewConfigStore(""), "", testLogger()), tracker
}

// storedTorrents reads back everything a scrape stored, for the single
// tracker these tests use.
func storedTorrents(t *testing.T, s *Scraper, def *Tracker) []database.Torrent {
	t.Helper()
	torrents, err := s.store.Recent(context.Background(), TrackerID(def), 100)
	require.NoError(t, err)
	return torrents
}

// storedTorrent reads back one stored torrent by its exact title.
func storedTorrent(t *testing.T, s *Scraper, def *Tracker, name string) database.Torrent {
	t.Helper()
	stored := storedTorrents(t, s, def)
	for i := range stored {
		if stored[i].Name == name {
			return stored[i]
		}
	}
	require.FailNowf(t, "a stored torrent is missing", "none named %q", name)
	return database.Torrent{}
}

// storedTitles reads back the stored torrent names.
func storedTitles(t *testing.T, s *Scraper, def *Tracker) []string {
	t.Helper()
	torrents := storedTorrents(t, s, def)
	names := make([]string, 0, len(torrents))
	for i := range torrents {
		names = append(names, torrents[i].Name)
	}
	return names
}

func TestScraper_JSONResponse(t *testing.T) {
	const body = `{"data":{"results":[
		{"title":"JSON Release","id":7,"hash":"abc123","seeds":12,"peers":3,"bytes":1048576},
		{"title":"Second Release","id":8,"hash":"def456","seeds":1,"peers":0,"bytes":2097152}]}}`

	scrpr, def := newScrapeFixture(t, "jsonsite", body, `
id: jsonsite
name: JSON Site
type: public
links:
  - %[1]s/
search:
  paths:
    - path: api
      method: get
      response:
        type: json
  rows:
    selector: data.results
  fields:
    title:
      selector: title
    infohash:
      selector: hash
    seeders:
      selector: seeds
    leechers:
      selector: peers
    details:
      text: "/torrent/{{ .Result.title }}"
`)

	require.NoError(t, scrpr.ScrapeIndexer(context.Background(), def, SearchParams{}))
	require.ElementsMatch(t, []string{"JSON Release", "Second Release"}, storedTitles(t, scrpr, def))

	stored := storedTorrent(t, scrpr, def, "JSON Release")
	require.Contains(t, stored.Magnet, "urn:btih:abc123", "an info hash should become a magnet link")
	require.Equal(t, 12, stored.Seeders, "a JSON number must not be scanned as scientific notation")
}

func TestScraper_FieldCaseArms(t *testing.T) {
	const body = `
		<div class="row"><a href="/d/1">Freeleech Release</a><img src="free.png"></div>
		<div class="row"><a href="/d/2">Normal Release</a></div>`

	scrpr, def := newScrapeFixture(t, "cases", body, `
id: cases
name: Cases
links:
  - %[1]s/
search:
  paths:
    - path: "/"
      method: get
  rows:
    selector: div.row
  fields:
    title:
      selector: a
    download:
      selector: a
      attribute: href
    downloadvolumefactor:
      case:
        "img[src$=\"free.png\"]": "0"
        "*": "1"
`)

	require.NoError(t, scrpr.ScrapeIndexer(context.Background(), def, SearchParams{}))

	require.Equal(t, 0.0, storedTorrent(t, scrpr, def, "Freeleech Release").DownloadVolumeFactor)
	require.Equal(t, 1.0, storedTorrent(t, scrpr, def, "Normal Release").DownloadVolumeFactor)
}

func TestScraper_RequiredAndOptionalFields(t *testing.T) {
	const body = `
		<div class="row"><a href="/d/1">Complete</a><span class="s">5</span></div>
		<div class="row"><a href="/d/2">No Seeders</a></div>`

	base := `
id: %[2]s
name: %[2]s
links:
  - %%[1]s/
search:
  paths:
    - path: "/"
      method: get
  rows:
    selector: div.row
  fields:
    title:
      selector: a
    download:
      selector: a
      attribute: href
    seeders:
      selector: span.s
%[1]s`

	t.Run("a missing required field drops the row", func(t *testing.T) {
		scrpr, def := newScrapeFixture(t, "required", body, fmt.Sprintf(base, "", "required"))
		require.NoError(t, scrpr.ScrapeIndexer(context.Background(), def, SearchParams{}))
		require.Equal(t, []string{"Complete"}, storedTitles(t, scrpr, def))
	})

	t.Run("an optional field keeps the row", func(t *testing.T) {
		scrpr, def := newScrapeFixture(t, "optional", body, fmt.Sprintf(base, "      optional: true\n", "optional"))
		require.NoError(t, scrpr.ScrapeIndexer(context.Background(), def, SearchParams{}))
		require.ElementsMatch(t, []string{"Complete", "No Seeders"}, storedTitles(t, scrpr, def))
	})
}

func TestScraper_RowsAfterAndRemove(t *testing.T) {
	const body = `
		<div class="row header">Column headings</div>
		<div class="row"><a href="/d/1">Real Release</a></div>
		<div class="row promo"><a href="/d/2">Sponsored</a></div>`

	scrpr, def := newScrapeFixture(t, "rowspec", body, `
id: rowspec
name: Row Spec
links:
  - %[1]s/
search:
  paths:
    - path: "/"
      method: get
  rows:
    selector: div.row
    after: 1
    remove: .promo
  fields:
    title:
      selector: a
    download:
      selector: a
      attribute: href
`)

	require.NoError(t, scrpr.ScrapeIndexer(context.Background(), def, SearchParams{}))
	require.Equal(t, []string{"Real Release"}, storedTitles(t, scrpr, def))
}

func TestScraper_FieldRemoveStripsNestedMarkup(t *testing.T) {
	const body = `<div class="row"><a href="/d/1">Real Title<span class="tag">[PROMO]</span></a></div>`

	scrpr, def := newScrapeFixture(t, "removal", body, `
id: removal
name: Removal
links:
  - %[1]s/
search:
  paths:
    - path: "/"
      method: get
  rows:
    selector: div.row
  fields:
    title:
      selector: a
      remove: .tag
    download:
      selector: a
      attribute: href
    tagged:
      selector: a
`)

	require.NoError(t, scrpr.ScrapeIndexer(context.Background(), def, SearchParams{}))
	// The removal must not leak into the next field reading the same node.
	require.Equal(t, []string{"Real Title"}, storedTitles(t, scrpr, def))
}

func TestScraper_SearchErrorIsReported(t *testing.T) {
	const body = `<div class="err">Flood protection: wait 60 seconds</div>`

	scrpr, def := newScrapeFixture(t, "errsite", body, `
id: errsite
name: Error Site
links:
  - %[1]s/
search:
  paths:
    - path: "/"
      method: get
  error:
    - selector: div.err
      message:
        selector: div.err
  rows:
    selector: div.row
  fields:
    title:
      selector: a
`)

	err := scrpr.ScrapeIndexer(context.Background(), def, SearchParams{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "Flood protection")
}

func TestScraper_PerPathInputsAndHeaders(t *testing.T) {
	var gotQuery, gotHeader string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		gotHeader = r.Header.Get("X-Requested-With")
		fmt.Fprint(w, `<div class="row"><a href="/d/1">Release</a></div>`)
	}))
	defer server.Close()

	dir := t.TempDir()
	def := loadTestTracker(t, dir, "paths", `
id: paths
name: Paths
links:
  - `+server.URL+`/
search:
  headers:
    X-Requested-With: XMLHttpRequest
  inputs:
    shared: "yes"
    q: "{{ .Keywords }}"
  paths:
    - path: "/"
      method: get
      inputs:
        extra: "1"
  rows:
    selector: div.row
  fields:
    title:
      selector: a
    download:
      selector: a
      attribute: href
`)

	db, err := database.Open(context.Background(), ":memory:")
	require.NoError(t, err)
	defer db.Close()

	scrpr := New(db, NewConfigStore(""), "", testLogger())
	require.NoError(t, scrpr.ScrapeIndexer(context.Background(), def, SearchParams{Query: "example"}))

	require.Contains(t, gotQuery, "shared=yes", "shared inputs should be inherited")
	require.Contains(t, gotQuery, "extra=1", "a path's own inputs should be submitted")
	require.Contains(t, gotQuery, "q=example")
	require.Equal(t, "XMLHttpRequest", gotHeader)
}

func TestJSONLookup(t *testing.T) {
	document := map[string]any{
		"data": map[string]any{
			"items": []any{
				map[string]any{"name": "first"},
				map[string]any{"name": "second"},
			},
		},
	}

	tests := []struct {
		path string
		want string
	}{
		{path: "data.items[0].name", want: "first"},
		{path: "$.data.items[1].name", want: "second"},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			v, ok := jsonLookup(document, tc.path)
			require.True(t, ok)
			require.Equal(t, tc.want, jsonScalar(v))
		})
	}

	t.Run("reports a missing path", func(t *testing.T) {
		_, ok := jsonLookup(document, "data.missing.name")
		require.False(t, ok)
	})
}

func TestApplyFilter_AddedFilters(t *testing.T) {
	logger := testLogger()
	run := func(name string, args any, value string) (string, error) {
		return applyFilter(value, Filter{Args: args, Name: name}, templateData{}, logger)
	}

	t.Run("replace", func(t *testing.T) {
		got, err := run("replace", []any{"-", " "}, "a-b-c")
		require.NoError(t, err)
		require.Equal(t, "a b c", got)
	})

	t.Run("htmldecode", func(t *testing.T) {
		got, err := run("htmldecode", nil, "Tom &amp; Jerry")
		require.NoError(t, err)
		require.Equal(t, "Tom & Jerry", got)
	})

	t.Run("regexp captures the first group", func(t *testing.T) {
		got, err := run("regexp", `id=(\d+)`, "?x=1&id=4321&y=2")
		require.NoError(t, err)
		require.Equal(t, "4321", got)
	})

	t.Run("validate drops an unexpected value", func(t *testing.T) {
		// Jackett intersects tokens, so nothing in common yields "".
		got, err := run("validate", []any{"a", "b"}, "c")
		require.NoError(t, err)
		require.Empty(t, got)
	})

	t.Run("diacritics folds accents", func(t *testing.T) {
		// "replace" is the only operation Jackett implements, and every
		// definition using this filter passes it.
		got, err := run("diacritics", "replace", "Tëst Ćafé")
		require.NoError(t, err)
		require.Equal(t, "Test Cafe", got)
	})

	t.Run("diacritics rejects any other argument", func(t *testing.T) {
		_, err := run("diacritics", "strip", "Tëst")
		require.Error(t, err)

		_, err = run("diacritics", nil, "Tëst")
		require.Error(t, err, "the argument is required")
	})

	t.Run("reverse", func(t *testing.T) {
		got, err := run("reverse", nil, "abc")
		require.NoError(t, err)
		require.Equal(t, "cba", got)
	})

	t.Run("fuzzytime handles both relative and absolute text", func(t *testing.T) {
		relative, err := run("fuzzytime", nil, "2 hours ago")
		require.NoError(t, err)
		require.NotEmpty(t, relative)

		absolute, err := run("fuzzytime", nil, "2024-03-05")
		require.NoError(t, err)
		require.True(t, strings.HasPrefix(absolute, "2024-03-05"))
	})
}

func TestSearchParams_KeywordsFoldsMediaTerms(t *testing.T) {
	got := SearchParams{Album: "Test Album", Artist: "Test Artist"}.keywords()
	require.Equal(t, "Test Artist Test Album", got)
}

// A setting with no value must render as an empty string, not as the text
// Go's default formatting gives a nil interface.
func TestMergedConfig_NilValues(t *testing.T) {
	def := &Tracker{Settings: []Setting{
		{Name: "password", Type: "password"},
		{Default: "added", Name: "sort", Type: "select"},
	}}

	t.Run("a setting with no default", func(t *testing.T) {
		require.Empty(t, defaultConfig(def)["password"])
	})

	t.Run("an override with no value", func(t *testing.T) {
		cfg := mergedConfig(def, map[string]any{"password": nil, "sort": nil})
		require.Empty(t, cfg["password"])
		require.Empty(t, cfg["sort"])
	})
}

// A tracker that serves windows-1251 expects its search terms in
// windows-1251 too. Submitting UTF-8 does not fail loudly — the site reads
// the query as mojibake and returns its default listing — so this is
// pinned by asserting the bytes actually sent.
func TestScraper_EncodesFormInTrackerCharset(t *testing.T) {
	var gotBody, gotContentType string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		gotContentType = r.Header.Get("Content-Type")
		fmt.Fprint(w, `<div class="row"><a href="/d/1">Result</a></div>`)
	}))
	defer server.Close()

	dir := t.TempDir()
	def := loadTestTracker(t, dir, "cp1251", `
id: cp1251
name: CP1251 Site
encoding: windows-1251
links:
  - `+server.URL+`/
search:
  paths:
    - path: "/"
  inputs:
    nm: "{{ .Keywords }}"
  rows:
    selector: div.row
  fields:
    title:
      selector: a
    download:
      selector: a
      attribute: href
`)

	db, err := database.Open(context.Background(), ":memory:")
	require.NoError(t, err)
	defer db.Close()

	scrpr := New(db, NewConfigStore(""), "", testLogger())
	require.NoError(t, scrpr.ScrapeIndexer(context.Background(), def, SearchParams{Query: "тестовый"}))

	// "тестовый" in windows-1251 is 8 single bytes.
	require.Contains(t, gotBody, "nm=%F2%E5%F1%F2%EE%E2%FB%E9")
	require.NotContains(t, gotBody, "%D1%82", "the term was submitted as UTF-8")
	require.Contains(t, gotContentType, "charset=windows-1251")
}

// A definition with no declared encoding, or an explicit UTF-8 one, must
// still submit UTF-8.
func TestScraper_DefaultsToUTF8Form(t *testing.T) {
	for _, encoding := range []string{"", "utf-8", "UTF-8"} {
		t.Run("encoding="+encoding, func(t *testing.T) {
			var gotBody string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				gotBody = string(body)
				fmt.Fprint(w, `<div class="row"><a href="/d/1">Result</a></div>`)
			}))
			defer server.Close()

			dir := t.TempDir()
			encodingLine := ""
			if encoding != "" {
				encodingLine = "encoding: " + encoding + "\n"
			}
			def := loadTestTracker(t, dir, "utf8site", `
id: utf8site
name: UTF8 Site
`+encodingLine+`links:
  - `+server.URL+`/
search:
  paths:
    - path: "/"
  inputs:
    nm: "{{ .Keywords }}"
  rows:
    selector: div.row
  fields:
    title:
      selector: a
    download:
      selector: a
      attribute: href
`)

			db, err := database.Open(context.Background(), ":memory:")
			require.NoError(t, err)
			defer db.Close()

			scrpr := New(db, NewConfigStore(""), "", testLogger())
			require.NoError(t, scrpr.ScrapeIndexer(context.Background(), def, SearchParams{Query: "тестовый"}))
			require.Contains(t, gotBody, "nm=%D1%82%D0%B5%D1%81%D1%82%D0%BE%D0%B2%D1%8B%D0%B9")
		})
	}
}

func TestEncodeForm(t *testing.T) {
	form := url.Values{"nm": {"тестовый"}, "z": {"a b"}}

	t.Run("transcodes to the declared charset", func(t *testing.T) {
		got, err := encodeForm(form, "windows-1251")
		require.NoError(t, err)
		require.Equal(t, "nm=%F2%E5%F1%F2%EE%E2%FB%E9&z=a+b", got)
	})

	t.Run("leaves utf-8 alone", func(t *testing.T) {
		got, err := encodeForm(form, "")
		require.NoError(t, err)
		require.Equal(t, form.Encode(), got)
	})

	t.Run("reports an unknown charset", func(t *testing.T) {
		_, err := encodeForm(form, "not-a-charset")
		require.Error(t, err)
	})
}
