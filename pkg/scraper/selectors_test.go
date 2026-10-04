// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"
	"github.com/stretchr/testify/require"
)

// A selector AngleSharp accepts and cascadia does not is adapted, and
// every other selector is left as written.
func TestCSSSelector(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{in: "table[border=1] > tr", want: `table[border="1"] > tr`},
		{in: `table[width="100%"][border=1][cellspacing=0]`, want: `table[width="100%"][border="1"][cellspacing="0"]`},
		{in: "a[href^=details.php]", want: `a[href^="details.php"]`},
		{in: "free_button:contains(Only Upload)", want: `free_button:contains("Only Upload")`},
		{in: `span:contains("ago")`, want: `span:contains("ago")`},
		{in: "a[href='x']", want: "a[href='x']"},
		{in: "a[data-x=y i]", want: "a[data-x=y i]"},
		{in: "td:nth-child(2) > a", want: "td:nth-child(2) > a"},
	} {
		t.Run(tc.in, func(t *testing.T) {
			require.Equal(t, tc.want, cssSelector(tc.in))
		})
	}
}

// A field's ":scope" stands for its row, as in the QuerySelector Jackett
// runs from a row, so ":scope > span > a" reads a link in one of the row's
// own spans and not one nested deeper; the mark that makes it work is gone
// from the page afterwards.
func TestHTMLRow_Lookup_Scope(t *testing.T) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(`<div class="row">
		<div><span><a href="/nested">Nested</a></span></div>
		<span><a href="/own">Example Release</a></span>
		<span>12</span>
	</div>`))
	require.NoError(t, err)
	row := htmlRow{sel: doc.Find(".row")}

	got, ok := row.lookup(Field{Selector: ":scope > span > a"})
	require.True(t, ok)
	require.Equal(t, "Example Release", got)

	got, ok = row.lookup(Field{Selector: ":scope > span:nth-of-type(2)"})
	require.True(t, ok)
	require.Equal(t, "12", got)

	got, ok = row.lookup(Field{Attribute: "class", Selector: ":scope"})
	require.True(t, ok, "a bare :scope did not select the row")
	require.Equal(t, "row", got)

	require.True(t, row.matches(":scope > span"), "a case arm's :scope did not stand for the row")

	html, err := goquery.OuterHtml(doc.Selection)
	require.NoError(t, err)
	require.NotContains(t, html, scopeAttribute, "the row kept the mark")
}

// A row selector with an unquoted attribute value finds its rows, as it
// does in Jackett.
func TestFindIn_UnquotedAttribute(t *testing.T) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(
		`<table border="1"><tbody><tr><td>Example Release</td></tr></tbody></table><table><tbody><tr><td>Other</td></tr></tbody></table>`))
	require.NoError(t, err)

	rows := findIn(doc.Selection, "table[border=1] > tbody > tr")
	require.Equal(t, 1, rows.Length())
	require.Equal(t, "Example Release", rows.Text())
}

// A search reads its rows and fields through the same adaptation, so a
// definition whose row selector, rows remove or field remove carries an
// unquoted attribute value scrapes as it does in Jackett.
func TestScraperAdaptsADefinitionsSelectors(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<table border="1"><tbody>
			<tr><td><a href="/d/1">Example Release<i data-noise=1> (noise)</i></a></td></tr>
			<tr data-sticky=1><td><a href="/d/2">Sticky Notice</a></td></tr>
		</tbody></table>`)
	}))
	defer site.Close()
	def := loadTestTracker(t, t.TempDir(), "unquoted", `
id: unquoted
name: Unquoted
links:
  - `+site.URL+`/
search:
  paths:
    - path: /
  rows:
    selector: table[border=1] > tbody > tr
    remove: tr[data-sticky=1]
  fields:
    title:
      selector: a
      remove: i[data-noise=1]
    download:
      selector: a
      attribute: href
`)
	scrpr := NewWithOptions(NewConfigStore(""), "", slog.New(slog.DiscardHandler), Options{Sink: &fakeStore{}})
	torrents, err := scrpr.Scrape(t.Context(), def, SearchParams{})
	require.NoError(t, err)
	require.Len(t, torrents, 1)
	require.Equal(t, "Example Release", torrents[0].Name)
}

// From the whole document, as a row selector or a login test runs,
// ":scope" is the root element, as in a document's QuerySelectorAll.
func TestFindIn_ScopeFromTheDocument(t *testing.T) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(`<html><body><table><tr><td>Example Release</td></tr></table></body></html>`))
	require.NoError(t, err)

	rows := findIn(doc.Selection, ":scope > body tr")
	require.Equal(t, 1, rows.Length())
	require.Equal(t, "Example Release", rows.Text())

	html, err := goquery.OuterHtml(doc.Selection)
	require.NoError(t, err)
	require.NotContains(t, html, scopeAttribute, "the root element kept the mark")
}

// A group alternative ending in a relative :has, which AngleSharp accepts
// and cascadia cannot compile, matches as AngleSharp matches it, and the
// group's matches stay in document order whichever alternative made them:
// a definition pairing each post with the entry after it relies on both.
func TestFindIn_RelativeHas(t *testing.T) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(`<div id="content">
		<div class="poststuff" id="p1"></div><div class="entry" id="e1"><a class="download">1</a></div>
		<div class="poststuff" id="p2"></div><div class="entry" id="e2"></div>
		<div class="poststuff" id="p3"></div><div class="entry" id="e3"><a class="download">3</a></div>
		<div class="poststuff" id="p4"><div class="entry"><a class="download">nested</a></div></div>
	</div>`))
	require.NoError(t, err)
	ids := func(sel *goquery.Selection) []string {
		return sel.Map(func(_ int, s *goquery.Selection) string { return s.AttrOr("id", "") })
	}

	for _, tc := range []struct {
		selector string
		want     []string
	}{
		{
			selector: "div#content > div.poststuff:has(~ div.entry a.download), div#content > div.poststuff ~ div.entry:has(a.download)",
			want:     []string{"p1", "e1", "p2", "p3", "e3"},
		},
		{selector: "div.poststuff:has(+ div.entry a.download)", want: []string{"p1", "p3"}},
		{selector: "div.poststuff:has(> div.entry)", want: []string{"p4"}},
		{selector: ":has(+ div.entry a.download)", want: []string{"p1", "p3"}},
		{selector: "div#content > :has(~ div#e3)", want: []string{"p1", "e1", "p2", "e2", "p3"}},
		{selector: "div#content :has(+ div#e2)", want: []string{"p2"}},
		{selector: "div.poststuff:has(~ div.nothing)"},
		{selector: "div.poststuff:has(~ ), div.entry"},
	} {
		t.Run(tc.selector, func(t *testing.T) {
			got := ids(findIn(doc.Selection, tc.selector))
			if tc.want == nil {
				require.Empty(t, got)
				return
			}
			require.Equal(t, tc.want, got)
		})
	}

	html, err := goquery.OuterHtml(doc.Selection)
	require.NoError(t, err)
	require.NotContains(t, html, hasAttribute, "an element kept the mark")
}

// A group splits only at commas outside brackets, parentheses and quotes,
// and a bracket or parenthesis inside quotes does not count as one.
func TestSplitSelectorGroup(t *testing.T) {
	require.Equal(t, []string{"a:has(~ b, c)", ` d[title="x,y"]`, ` e:contains("f, g")`},
		splitSelectorGroup(`a:has(~ b, c), d[title="x,y"], e:contains("f, g")`))
	require.Equal(t, []string{`a:contains(")")`, ` b:contains('\'(')`, " c"},
		splitSelectorGroup(`a:contains(")"), b:contains('\'('), c`))
}
