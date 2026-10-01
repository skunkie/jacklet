// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"fmt"
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"
	"github.com/stretchr/testify/require"
)

func TestJSONRowsAndScalarValues(t *testing.T) {
	document := map[string]any{
		"items": []any{
			map[string]any{"name": "Example One"},
			map[string]any{"name": "Example Two"},
		},
	}
	rows := jsonRows(document, "items")
	require.Len(t, rows, 2)
	require.True(t, rows[0].matches("name"), "a present key reported as missing")
	require.False(t, rows[0].matches("missing"), "a missing key reported as present")

	values := []struct {
		value any
		want  string
	}{
		{value: true, want: "true"},
		{value: float64(12.5), want: "12.5"},
		{value: nil, want: ""},
	}
	for _, tc := range values {
		require.Equal(t, tc.want, jsonScalar(tc.value), "jsonScalar(%#v)", tc.value)
	}
}

func TestResultRow_Lookup(t *testing.T) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(`<div class="row"><a href="/item">Example</a><span>Hidden</span></div>`))
	require.NoError(t, err)

	row := htmlRow{sel: doc.Find(".row")}
	require.True(t, row.matches("a"), "a descendant selector reported as unmatched")
	require.True(t, row.matches(".row"), "the row's own selector reported as unmatched")
	require.False(t, row.matches("missing"), "a missing selector reported as matched")

	got, ok := row.lookup(Field{Attribute: "href", Selector: "a"})
	require.True(t, ok, "attribute lookup reported as unmatched")
	require.Equal(t, "/item", got)

	got, ok = row.lookup(Field{Remove: "span"})
	require.True(t, ok, "remove lookup reported as unmatched")
	require.Equal(t, "Example", strings.TrimSpace(got))

	_, ok = row.lookup(Field{Selector: "missing"})
	require.False(t, ok, "missing HTML selector reported as matched")

	json := jsonRow{value: map[string]any{
		"items": []any{map[string]any{"name": "Example"}},
	}}
	got, ok = json.lookup(Field{Attribute: "name", Selector: "items[0]"})
	require.True(t, ok, "JSON attribute lookup reported as unmatched")
	require.Equal(t, "Example", got)

	got, ok = json.lookup(Field{})
	require.True(t, ok, "JSON root lookup reported as unmatched")
	require.NotEmpty(t, got)
}

func TestScraperJSONResponse(t *testing.T) {
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

	require.NoError(t, scrpr.scrapeIndexer(t.Context(), def, SearchParams{}))
	require.ElementsMatch(t, []string{"JSON Release", "Second Release"}, storedTitles(t, scrpr, def))

	stored := storedTorrent(t, scrpr, def, "JSON Release")
	require.Contains(t, stored.Magnet, "urn:btih:abc123", "an info hash should become a magnet link")
	require.Equal(t, 12, stored.Seeders, "a JSON number must not be scanned as scientific notation")
}

func TestScraperFieldCaseArms(t *testing.T) {
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

	require.NoError(t, scrpr.scrapeIndexer(t.Context(), def, SearchParams{}))

	require.Equal(t, 0.0, storedTorrent(t, scrpr, def, "Freeleech Release").DownloadVolumeFactor)
	require.Equal(t, 1.0, storedTorrent(t, scrpr, def, "Normal Release").DownloadVolumeFactor)
}

func TestScraperRequiredAndOptionalFields(t *testing.T) {
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
		require.NoError(t, scrpr.scrapeIndexer(t.Context(), def, SearchParams{}))
		require.Equal(t, []string{"Complete"}, storedTitles(t, scrpr, def))
	})

	t.Run("an optional field keeps the row", func(t *testing.T) {
		scrpr, def := newScrapeFixture(t, "optional", body, fmt.Sprintf(base, "      optional: true\n", "optional"))
		require.NoError(t, scrpr.scrapeIndexer(t.Context(), def, SearchParams{}))
		require.ElementsMatch(t, []string{"Complete", "No Seeders"}, storedTitles(t, scrpr, def))
	})
}

func TestScraperRowsAfterAndRemove(t *testing.T) {
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

	require.NoError(t, scrpr.scrapeIndexer(t.Context(), def, SearchParams{}))
	require.Equal(t, []string{"Real Release"}, storedTitles(t, scrpr, def))
}

func TestScraperFieldRemoveStripsNestedMarkup(t *testing.T) {
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

	require.NoError(t, scrpr.scrapeIndexer(t.Context(), def, SearchParams{}))
	// The removal must not leak into the next field reading the same node.
	require.Equal(t, []string{"Real Title"}, storedTitles(t, scrpr, def))
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
