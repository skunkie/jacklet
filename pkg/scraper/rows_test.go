// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestJSONRowsAndScalarValues(t *testing.T) {
	document := map[string]any{
		"items": []any{
			map[string]any{"name": "Example One"},
			map[string]any{"name": "Example Two"},
		},
	}
	rows, err := jsonRows(document, Rows{}, "items")
	require.NoError(t, err)
	require.Len(t, rows, 2)

	// A JSON case arm is a value compared with what the field selected,
	// not a key to look up.
	name, ok := rows[0].narrow("name")
	require.True(t, ok, "a present key reported as missing")
	require.True(t, name.matches("Example One"), "an arm equal to the selected value did not match")
	require.False(t, name.matches("Example Two"), "an arm unequal to the selected value matched")
	require.False(t, rows[0].matches("name"), "an arm was looked up as a key rather than compared as a value")
	_, ok = rows[0].narrow("missing")
	require.False(t, ok, "a missing key reported as present")

	values := []struct {
		value any
		want  string
	}{
		// Booleans read as .NET renders them, which is what a definition's
		// arms and templates compare against.
		{value: true, want: "True"},
		{value: false, want: "False"},
		{value: float64(12.5), want: "12.5"},
		{value: []any{"Sample", float64(2), true}, want: "Sample,2,True"},
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

	cells, err := goquery.NewDocumentFromReader(strings.NewReader(`<table><tr class="row"><td>Sample Release</td><td>1 GB</td></tr></table>`))
	require.NoError(t, err)
	got, ok = htmlRow{sel: cells.Find(".row")}.lookup(Field{Selector: "td"})
	require.True(t, ok, "a selector matching several cells reported as unmatched")
	require.Equal(t, "Sample Release", got, "a selector matching several cells read more than the first, as Jackett would not")

	rowFromJSON := jsonRow{value: map[string]any{
		"items": []any{map[string]any{"name": "Example"}},
	}}
	got, ok = rowFromJSON.lookup(Field{Attribute: "name", Selector: "items[0]"})
	require.True(t, ok, "JSON attribute lookup reported as unmatched")
	require.Equal(t, "Example", got)

	got, ok = rowFromJSON.lookup(Field{})
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

// A case arm's value is a template, as Jackett renders it, so the common
// "{{ .True }}" and "{{ .False }}" arms yield Cardigann's booleans rather
// than their own text, which every template would read as true.
func TestScraperCaseArmValuesAreTemplates(t *testing.T) {
	const body = `
		<div class="row"><a href="/d/1">Freeleech Release</a><img class="free" src="free.png"></div>
		<div class="row"><a href="/d/2">Normal Release</a></div>`

	scrpr, def := newScrapeFixture(t, "armtemplates", body, `
id: armtemplates
name: Arm Templates
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
    _free:
      case:
        img.free: "{{ .True }}"
        "*": "{{ .False }}"
    description:
      text: "{{ if .Result._free }}Freeleech{{ else }}Normal{{ end }}"
`)

	require.NoError(t, scrpr.scrapeIndexer(t.Context(), def, SearchParams{}))

	require.Equal(t, "Freeleech", storedTorrent(t, scrpr, def, "Freeleech Release").Description)
	require.Equal(t, "Normal", storedTorrent(t, scrpr, def, "Normal Release").Description,
		"an arm's {{ .False }} was kept as text, which a template reads as true")
}

// A field's case arms are tested against what its selector selects, as
// Jackett tests them, so a selector list keeps its alternatives apart and
// an arm can describe the selected element itself.
func TestScraperCaseArmsTestTheFieldsSelection(t *testing.T) {
	const body = `
		<div class="row"><a href="/d/1">Verified Release</a><span class="state-ok"></span></div>
		<div class="row"><a href="/d/2">Unchecked Release</a><span class="state-unchecked"></span></div>`

	scrpr, def := newScrapeFixture(t, "selectedcases", body, `
id: selectedcases
name: Selected Cases
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
    description:
      selector: span.state-ok, span.state-unchecked
      optional: true
      case:
        span.state-ok: verified
        span.state-unchecked: unchecked
    downloadvolumefactor:
      selector: span
      case:
        span.state-ok: "0"
        "*": "1"
`)

	require.NoError(t, scrpr.scrapeIndexer(t.Context(), def, SearchParams{}))

	verified := storedTorrent(t, scrpr, def, "Verified Release")
	unchecked := storedTorrent(t, scrpr, def, "Unchecked Release")
	require.Equal(t, "verified", verified.Description)
	require.Equal(t, "unchecked", unchecked.Description, "a selector list let its first alternative decide every row")
	require.Equal(t, 0.0, verified.DownloadVolumeFactor, "an arm describing the selected element itself did not match it")
	require.Equal(t, 1.0, unchecked.DownloadVolumeFactor)
}

// A field selector the row itself matches selects the row, as Jackett
// resolves it, so a definition whose rows are links can read each link's own
// href, and one whose rows are a single element can read that element's text.
func TestScraperFieldsCanSelectTheRowItself(t *testing.T) {
	const body = `
		<a class="result" href="/d/1"><span class="title">Sample Release</span></a>
		<a class="result" href="/d/2"><span class="title">Another Release</span></a>`

	scrpr, def := newScrapeFixture(t, "selfrows", body, `
id: selfrows
name: Self Rows
links:
  - %[1]s/
search:
  paths:
    - path: "/"
      method: get
  rows:
    selector: a.result
  fields:
    title:
      selector: span.title
    download:
      selector: a.result
      attribute: href
      optional: true
    description:
      selector: a.result
      optional: true
`)

	require.NoError(t, scrpr.scrapeIndexer(t.Context(), def, SearchParams{}))

	stored := storedTorrent(t, scrpr, def, "Sample Release")
	require.Contains(t, stored.DownloadURL, "/d/1", "a field naming the row did not read the row's own attribute")
	require.Equal(t, "Sample Release", strings.TrimSpace(stored.Description), "a field naming the row did not read the row's own text")
	require.Contains(t, storedTorrent(t, scrpr, def, "Another Release").DownloadURL, "/d/2")
}

// A ":root" selector reads from the top of the page rather than from the
// row, as Jackett runs it, so a field or a case arm can see a page-wide
// banner or an info table that sits outside every result row.
func TestScraperFieldsReachTheDocumentRoot(t *testing.T) {
	const body = `
		<ul class="alerts"><li class="alert">Freeleech ends in 2 hours</li></ul>
		<table class="info"><tr><td>Category:</td><td><a href="?cat=7">Sample Category</a></td></tr></table>
		<div class="row"><a href="/d/1">Sample Release</a><span class="size">1 GB</span></div>`

	scrpr, def := newScrapeFixture(t, "rooted", body, `
id: rooted
name: Rooted
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
    description:
      selector: ":root td:contains('Category:') + td a"
      optional: true
    downloadvolumefactor:
      case:
        ":root li.alert:contains(\"Freeleech ends in\")": "0"
        "*": "1"
    uploadvolumefactor:
      selector: span.size
      case:
        ":root li.alert:contains(\"Freeleech ends in\")": "2"
        "*": "1"
`)

	require.NoError(t, scrpr.scrapeIndexer(t.Context(), def, SearchParams{}))

	stored := storedTorrent(t, scrpr, def, "Sample Release")
	require.Equal(t, "Sample Category", stored.Description, "a field selector starting at :root did not reach outside the row")
	require.Equal(t, 0.0, stored.DownloadVolumeFactor, "a case arm starting at :root did not see the page-wide banner")
	require.Equal(t, 2.0, stored.UploadVolumeFactor, "a :root case arm under a field selector was scoped to that selection")
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

	// Jackett treats a description, a poster, a genre and the external ids
	// as optional whatever the definition says.
	t.Run("a field Jackett always treats as optional keeps the row", func(t *testing.T) {
		extra := "      optional: true\n    description:\n      selector: span.missing\n"
		scrpr, def := newScrapeFixture(t, "always-optional", body, fmt.Sprintf(base, extra, "always-optional"))
		require.NoError(t, scrpr.scrapeIndexer(t.Context(), def, SearchParams{}))
		require.ElementsMatch(t, []string{"Complete", "No Seeders"}, storedTitles(t, scrpr, def))
	})
}

// An optional field that comes back empty takes its default, rendered as a
// template against the fields read before it and without the field's own
// filters, as in Jackett; a required field never does, so a row missing
// one is still dropped.
func TestScraperFieldDefault(t *testing.T) {
	const body = `
		<div class="row"><a href="/d/1">Listed Title</a><b>Full Title</b></div>
		<div class="row"><a href="/d/2">Short Title</a></div>
		<div class="row"><b>No Link</b></div>
		<div class="row"><a href="">Empty Link</a></div>`

	scrpr, def := newScrapeFixture(t, "defaults", body, `
id: defaults
name: Defaults
links:
  - %[1]s/
search:
  paths:
    - path: "/"
      method: get
  rows:
    selector: div.row
  fields:
    title_default:
      selector: a
      optional: true
    title:
      selector: b
      optional: true
      default: "{{ .Result.title_default }}"
      filters:
        - name: append
          args: " (filtered)"
    download:
      selector: a
      attribute: href
      default: /never
    description:
      selector: i
      default: "{{ .Result.absent }}"
`)

	require.NoError(t, scrpr.scrapeIndexer(t.Context(), def, SearchParams{}))
	require.ElementsMatch(t, []string{"Full Title (filtered)", "Short Title", "Empty Link"}, storedTitles(t, scrpr, def))
	stored := storedTorrent(t, scrpr, def, "Short Title")
	require.Empty(t, stored.Description, "a default naming a field that does not exist was not left empty")
	stored = storedTorrent(t, scrpr, def, "Empty Link")
	require.NotContains(t, stored.DownloadURL, "/never", "a required field took its default")
}

// Row and field selectors are templates, as in Jackett: a setting can
// narrow the rows, as freeleech-only options do, and a field can pick its
// column by what an earlier field found.
func TestScraperRendersSelectors(t *testing.T) {
	const body = `
		<div class="row free"><a href="/d/1">Free Release</a><i>staff</i><b>1</b><u>2</u></div>
		<div class="row"><a href="/d/2">Paid Release</a><b>3</b></div>`

	scrpr, def := newScrapeFixture(t, "templated-selectors", body, `
id: templated-selectors
name: Templated Selectors
links:
  - %[1]s/
settings:
  - name: freeleech
    type: checkbox
    default: true
search:
  paths:
    - path: "/"
      method: get
  rows:
    selector: div.row{{ if .Config.freeleech }}.free{{ else }}{{ end }}
  fields:
    title:
      selector: a
    download:
      selector: a
      attribute: href
    _staff:
      selector: i
      optional: true
    seeders:
      selector: "{{ if .Result._staff }}u{{ else }}b{{ end }}"
      optional: true
`)

	require.NoError(t, scrpr.scrapeIndexer(t.Context(), def, SearchParams{}))
	require.Equal(t, []string{"Free Release"}, storedTitles(t, scrpr, def), "the row selector's template was not rendered")
	require.Equal(t, 2, storedTorrent(t, scrpr, def, "Free Release").Seeders, "the field selector's template was not rendered against the earlier field")
}

// A JSON answer's rows are shaped as Jackett shapes them: "attribute"
// narrows each element to an object inside it, "multiple" makes each entry
// of that a row of its own, and a field selector starting with ".." reads
// the element the row came from, as a movie's torrents read its title.
func TestScraperShapesJSONRows(t *testing.T) {
	const body = `{"data": {"movie_count": 2, "movies": [
		{"title": "Sample Movie", "torrents": [{"quality": "720p", "url": "/t/1"}, {"quality": "1080p", "url": "/t/2"}]},
		{"title": "Movie Without Torrents"}
	]}}`

	scrpr, def := newScrapeFixture(t, "json-shapes", body, `
id: json-shapes
name: JSON Shapes
links:
  - %[1]s/
search:
  paths:
    - path: "/"
      method: get
      response:
        type: json
  rows:
    selector: data.movies
    attribute: torrents
    multiple: true
    count:
      selector: data.movie_count
  fields:
    _movie:
      selector: ..title
    _quality:
      selector: quality
    title:
      text: "{{ .Result._movie }} {{ .Result._quality }}"
    download:
      selector: url
`)

	require.NoError(t, scrpr.scrapeIndexer(t.Context(), def, SearchParams{}))
	require.ElementsMatch(t, []string{"Sample Movie 720p", "Sample Movie 1080p"}, storedTitles(t, scrpr, def))
}

// An answer whose "rows.count" counts nothing has no results, whatever rows
// it carries, and one lacking the rows key altogether is a failure unless
// the definition says that is how its tracker answers no results.
func TestScraperJSONRowsCountAndMissingRows(t *testing.T) {
	const definition = `
id: %[1]s
name: %[1]s
links:
  - %%[1]s/
search:
  paths:
    - path: "/"
      method: get
      response:
        type: json
  rows:
    selector: items
%[2]s  fields:
    title:
      selector: name
    download:
      selector: url
`

	for _, tc := range []struct {
		name    string
		body    string
		rows    string
		want    []string
		wantErr string
	}{
		{
			name: "a count of zero",
			body: `{"total": 0, "items": [{"name": "Stale Release", "url": "/t/1"}]}`,
			rows: "    count:\n      selector: total\n",
		},
		{
			name: "a count that is not a number",
			body: `{"total": "many", "items": [{"name": "Counted Release", "url": "/t/1"}]}`,
			rows: "    count:\n      selector: total\n",
			want: []string{"Counted Release"},
		},
		{
			name:    "a missing rows key",
			body:    `{"error": "Example failure"}`,
			wantErr: `row selector "items" matched nothing`,
		},
		{
			name: "a missing rows key meaning no results",
			body: `{"error": "Example failure"}`,
			rows: "    missingAttributeEqualsNoResults: true\n",
		},
		{
			name: "null rows",
			body: `{"items": null}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scrpr, def := newScrapeFixture(t, "json-count", tc.body, fmt.Sprintf(definition, "json-count", tc.rows))
			err := scrpr.scrapeIndexer(t.Context(), def, SearchParams{})
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.ElementsMatch(t, tc.want, storedTitles(t, scrpr, def))
		})
	}
}

// A JSON selector's filters are read as Jackett reads them, including a
// filter whose argument is a selector with filters of its own.
func TestJSONSelect(t *testing.T) {
	freeleech := ":not(promotion.down:contains(1)):not(promotion.down:contains(.))"
	for _, tc := range []struct {
		name     string
		selector string
		value    string
		want     bool
	}{
		{name: "a path", selector: "data.items", value: `{"data": {"items": []}}`, want: true},
		{name: "a missing path", selector: "data.items", value: `{"data": {}}`},
		{name: "nested filters keep a free release", selector: freeleech, value: `{"promotion": {"down": 0}}`, want: true},
		{name: "nested filters drop a full-price release", selector: freeleech, value: `{"promotion": {"down": 1}}`},
		{name: "nested filters drop a half-price release", selector: freeleech, value: `{"promotion": {"down": 0.5}}`},
		{name: "contains on a field", selector: "button:contains(Only Upload)", value: `{"button": "Only Upload"}`, want: true},
		{name: "contains that does not hold", selector: "button:contains(Only Upload)", value: `{"button": "Download"}`},
		{name: "has with a filter of its own", selector: "meta.poster:has(:contains(https))", value: `{"meta": {"poster": "https://example.invalid/p.jpg"}}`, want: true},
		{name: "has that does not hold", selector: "meta.poster:has(:contains(https))", value: `{"meta": {"poster": "http://example.invalid/p.jpg"}}`},
		{name: "has on a key", selector: ":has(meta)", value: `{"meta": {}}`, want: true},
		{name: "an unknown filter is ignored", selector: "name:first(1)", value: `{"name": "Example"}`, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var value any
			require.NoError(t, json.Unmarshal([]byte(tc.value), &value))
			_, ok := jsonSelect(value, tc.selector)
			require.Equal(t, tc.want, ok)
		})
	}
}

// A JSON row selector's filters keep only the rows they hold for, as a
// freeleech-only setting narrows an API's results.
func TestScraperFiltersJSONRows(t *testing.T) {
	const body = `{"data": [
		{"name": "Free Release", "url": "/t/1", "promotion": {"down": 0}},
		{"name": "Half Release", "url": "/t/2", "promotion": {"down": 0.5}},
		{"name": "Paid Release", "url": "/t/3", "promotion": {"down": 1}}
	]}`

	scrpr, def := newScrapeFixture(t, "json-filters", body, `
id: json-filters
name: JSON Filters
links:
  - %[1]s/
settings:
  - name: freeleech
    type: checkbox
    default: true
search:
  paths:
    - path: "/"
      method: get
      response:
        type: json
  rows:
    selector: "data{{ if .Config.freeleech }}:not(promotion.down:contains(1)):not(promotion.down:contains(.)){{ else }}{{ end }}"
  fields:
    title:
      selector: name
    download:
      selector: url
`)

	require.NoError(t, scrpr.scrapeIndexer(t.Context(), def, SearchParams{}))
	require.Equal(t, []string{"Free Release"}, storedTitles(t, scrpr, def))
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

// A JSON field's case arms are values compared with what the field
// selected, as Jackett compares them, and a JSON boolean reads as .NET
// renders it. The shape is the one API definitions share: a required flag
// with only "False" and "True" arms and no "*" default, so a row is kept
// only when one of those arms equals the value.
func TestScraperJSONCaseArmsCompareValues(t *testing.T) {
	const body = `{"data":[
		{"title":"Sample Release","hash":"abc123","internal":true,"freeleech":"50%"},
		{"title":"Another Release","hash":"def456","internal":false,"freeleech":"0%"}]}`

	scrpr, def := newScrapeFixture(t, "jsoncases", body, `
id: jsoncases
name: JSON Cases
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
    selector: data
  fields:
    title:
      selector: title
    infohash:
      selector: hash
    _internal:
      selector: internal
      case:
        False: "{{ .False }}"
        True: "{{ .True }}"
    description:
      text: "{{ if .Result._internal }}Internal{{ else }}External{{ end }}"
    downloadvolumefactor:
      selector: freeleech
      case:
        0%%: 1
        50%%: 0.5
        100%%: 0
        "*": 1
`)

	require.NoError(t, scrpr.scrapeIndexer(t.Context(), def, SearchParams{}))
	require.ElementsMatch(t, []string{"Sample Release", "Another Release"}, storedTitles(t, scrpr, def),
		"a required field whose arms are values dropped the rows")

	internal := storedTorrent(t, scrpr, def, "Sample Release")
	require.Equal(t, "Internal", internal.Description, "the JSON boolean true did not match the True arm")
	require.Equal(t, 0.5, internal.DownloadVolumeFactor)
	external := storedTorrent(t, scrpr, def, "Another Release")
	require.Equal(t, "External", external.Description)
	require.Equal(t, 1.0, external.DownloadVolumeFactor)
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

// A field whose own template names a variable never set, such as a field
// declared later or not at all, or an undeclared setting, fails as Jackett
// fails it: a required one drops the row, and an optional one comes back
// empty without taking its default. Its selector, a matched case value,
// its text and its filters' arguments are all its own templates.
func TestExtractFields_VariableNeverSet(t *testing.T) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(`<div class="row"><a href="/d/1">Example Release</a><b>Sample</b></div>`))
	require.NoError(t, err)
	row := htmlRow{sel: doc.Find(".row")}
	title := NamedField{Field: Field{Selector: "a"}, Name: "title"}

	for _, tc := range []struct {
		name    string
		field   Field
		wantRow bool
		wantVal string
	}{
		{name: "text naming a later field", field: Field{Text: "{{ .Result.later }}"}},
		{name: "text naming an undeclared setting", field: Field{Text: "{{ .Config.undeclared }}"}},
		{name: "a selector", field: Field{Selector: "{{ .Result.later }}"}},
		{name: "a matched case value", field: Field{Case: CaseList{{Selector: "b", Value: "{{ .Result.later }}"}}}},
		{
			name:  "a filter argument",
			field: Field{Filters: []Filter{{Args: "{{ .Result.later }}", Name: "prepend"}}, Selector: "b"},
		},
		{name: "an earlier field", field: Field{Text: "{{ .Result.title }}!"}, wantRow: true, wantVal: "Example Release!"},
		{name: "an unmatched case arm's value", field: Field{Case: CaseList{
			{Selector: "i", Value: "{{ .Result.later }}"},
			{Selector: "b", Value: "bold"},
		}}, wantRow: true, wantVal: "bold"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			later := NamedField{Field: Field{Selector: "b"}, Name: "later"}

			required := &Tracker{Search: Search{Fields: FieldList{title, {Field: tc.field, Name: "subject"}, later}}}
			got, ok := extractFields(required, row, templateData{Config: map[string]any{}}, slog.New(slog.DiscardHandler))
			require.Equal(t, tc.wantRow, ok, "whether the row was kept")
			if tc.wantRow {
				require.Equal(t, tc.wantVal, got["subject"])
				return
			}

			optional := tc.field
			optional.Optional = true
			optional.Default = "fallback"
			def := &Tracker{Search: Search{Fields: FieldList{title, {Field: optional, Name: "subject"}, later}}}
			got, ok = extractFields(def, row, templateData{Config: map[string]any{}}, slog.New(slog.DiscardHandler))
			require.True(t, ok, "an optional field dropped the row")
			require.Empty(t, got["subject"], "the failed optional field was not empty, or took its default")
			require.Equal(t, "Sample", got["later"], "a later field was not read after the failed one")
		})
	}
}

// Only a missing variable fails a field's template; one that fails for any
// other reason, such as a parse error or a field templateData lacks, is
// left as written, as renderTemplate leaves it.
func TestRenderFieldTemplate(t *testing.T) {
	data := templateData{Config: map[string]any{"set": "yes"}, Result: map[string]string{"title": "Example"}}
	for _, tc := range []struct {
		in           string
		want         string
		wantResolved bool
	}{
		{in: "plain", want: "plain", wantResolved: true},
		{in: "{{ .Result.title }}/{{ .Config.set }}", want: "Example/yes", wantResolved: true},
		{in: "{{ .Result.absent }}", want: "", wantResolved: false},
		{in: "{{ .Config.absent }}", want: "", wantResolved: false},
		{in: "{{ if }}", want: "{{ if }}", wantResolved: true},
		{in: "{{ .Nope }}", want: "{{ .Nope }}", wantResolved: true},
	} {
		t.Run(tc.in, func(t *testing.T) {
			got, isResolved := renderFieldTemplate(tc.in, data, slog.New(slog.DiscardHandler))
			require.Equal(t, tc.wantResolved, isResolved)
			require.Equal(t, tc.want, got)
		})
	}
}

// A field key carries modifiers after its name, as Jackett reads it:
// "optional" makes the field optional, "append" adds a title or
// description to the one an earlier field read and replaces any other
// field as usual, and the name alone is what
// the row and ".Result" know the field by, so "categorydesc|append" maps
// a row's category as "categorydesc" does.
func TestExtractFields_KeyModifiers(t *testing.T) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(`<div class="row">
		<a href="/d/1">Example Release</a><b>Remux</b><i>Sample Notes</i><u>Films</u>
	</div>`))
	require.NoError(t, err)
	row := htmlRow{sel: doc.Find(".row")}

	var def Tracker
	require.NoError(t, yaml.Unmarshal([]byte(`
search:
  fields:
    title:
      selector: a
    title|append:
      selector: b
      filters:
        - name: prepend
          args: " "
    title|optional|append:
      selector: s
    description:
      selector: i
    description|append:
      text: " ({{ .Result.title }})"
    imdbid|optional:
      selector: s
    categorydesc|append:
      selector: u
    summary:
      text: "{{ .Result.title }}"
    size:
      text: "1 GB"
    size|append:
      text: "2 GB"
`), &def))
	require.Equal(t, []string{"optional", "append"}, def.Search.Fields[2].Modifiers)
	require.Equal(t, "title", def.Search.Fields[2].Name)

	got, ok := extractFields(&def, row, templateData{}, slog.New(slog.DiscardHandler))
	require.True(t, ok)
	require.Equal(t, "Example Release Remux", got["title"], "an empty optional append changed the title")
	require.Equal(t, "Sample Notes (Example Release Remux)", got["description"])
	require.Equal(t, "Films", got["categorydesc"])
	require.Equal(t, "Example Release Remux", got["summary"], ".Result did not carry the appended title")
	require.NotContains(t, got, "categorydesc|append")
	require.Equal(t, "2 GB", got["size"], "a field other than a title or description appended")
}

// Jackett looks its always-optional fields up by the whole key, so a field
// carrying a modifier is optional only when it says so.
func TestNamedField_IsOptional(t *testing.T) {
	for _, tc := range []struct {
		name  string
		field NamedField
		want  bool
	}{
		{name: "an always-optional field", field: NamedField{Name: "description"}, want: true},
		{name: "an always-optional field with a modifier", field: NamedField{Modifiers: []string{"append"}, Name: "description"}},
		{name: "the optional modifier", field: NamedField{Modifiers: []string{"optional"}, Name: "title"}, want: true},
		{name: "the optional key", field: NamedField{Field: Field{Optional: true}, Name: "title"}, want: true},
		{name: "a plain field", field: NamedField{Name: "title"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, tc.field.isOptional())
		})
	}
}
