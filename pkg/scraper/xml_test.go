// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torrplay/jacklet/pkg/database"
)

// A definition may declare "response: {type: xml}", which in practice
// always means an RSS feed. These cannot go through goquery: an HTML
// parser lowercases element names, so "pubDate" would never match, and
// <link> is void in HTML, so an RSS <link>…</link> loses its text.

// rssFeed is the shape the real definitions select against: a Torznab-ish
// RSS feed, with the namespaced attribute elements they read swarm counts
// from.
const rssFeed = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:torznab="http://torznab.com/schemas/2015/feed">
  <channel>
    <title>Example Tracker</title>
    <item>
      <title>Some.Release.2024.1080p</title>
      <link>https://tracker.example/details/1</link>
      <guid>https://tracker.example/details/1</guid>
      <pubDate>Sat, 18 Jul 2026 07:33:19 +0000</pubDate>
      <enclosure url="https://tracker.example/dl/1.torrent" length="1234567890"
                 type="application/x-bittorrent"/>
      <torznab:attr name="seeders" value="100"/>
      <torznab:attr name="leechers" value="7"/>
      <torznab:attr name="downloadvolumefactor" value="0"/>
    </item>
    <item>
      <title>Another.Release.2023.720p</title>
      <link>https://tracker.example/details/2</link>
      <guid>https://tracker.example/details/2</guid>
      <pubDate>Fri, 17 Jul 2026 07:33:19 +0000</pubDate>
      <enclosure url="https://tracker.example/dl/2.torrent" length="42"/>
      <torznab:attr name="seeders" value="3"/>
      <torznab:attr name="leechers" value="0"/>
    </item>
  </channel>
</rss>`

func parsedFeed(t *testing.T) *xmlNode {
	t.Helper()
	root, err := parseXML(rssFeed)
	require.NoError(t, err)
	return root
}

func TestSelectXML(t *testing.T) {
	root := parsedFeed(t)

	require.Len(t, selectXML([]*xmlNode{root}, "rss > channel > item"), 2)
	require.Len(t, selectXML([]*xmlNode{root}, "channel > item"), 2, "a chain may start below the root")
	require.Len(t, selectXML([]*xmlNode{root}, "item"), 2, "a bare name is a descendant search")
	require.Len(t, selectXML([]*xmlNode{root}, "rss item"), 2, "whitespace is the descendant combinator")

	// A child combinator is not a descendant one.
	require.Empty(t, selectXML([]*xmlNode{root}, "rss > item"))
	require.Empty(t, selectXML([]*xmlNode{root}, "nonesuch"))
	require.Empty(t, selectXML([]*xmlNode{root}, ""))

	// Element names are matched case-sensitively, as XML defines them —
	// the reason an HTML parser cannot stand in here.
	require.Len(t, selectXML([]*xmlNode{root}, "pubDate"), 2)
	require.Empty(t, selectXML([]*xmlNode{root}, "pubdate"))
}

func TestSelectXML_AttributePredicates(t *testing.T) {
	items := selectXML([]*xmlNode{parsedFeed(t)}, "rss > channel > item")
	require.Len(t, items, 2)
	first := []*xmlNode{items[0]}

	seeders := selectXML(first, "[name=seeders]")
	require.Len(t, seeders, 1)
	require.Equal(t, "100", seeders[0].Attrs["value"])

	// A namespaced element is reachable by its local name, and its
	// attributes by theirs.
	require.Len(t, selectXML(first, "attr"), 3)
	require.Len(t, selectXML(first, "attr[name]"), 3, "presence alone")
	require.Empty(t, selectXML(first, "[name=nonesuch]"))

	// Quoted values are accepted either way, as definitions write both.
	require.Len(t, selectXML(first, `[name="seeders"]`), 1)
	require.Len(t, selectXML(first, "[name='seeders']"), 1)
}

func TestXMLRow_Lookup(t *testing.T) {
	items := selectXML([]*xmlNode{parsedFeed(t)}, "rss > channel > item")
	row := xmlRow{node: items[0]}

	for _, tc := range []struct {
		field Field
		name  string
		want  string
	}{
		{field: Field{Selector: "title"}, name: "element text", want: "Some.Release.2024.1080p"},
		{
			// Void in HTML, ordinary in XML: this is the case that makes
			// an HTML parser unusable here.
			field: Field{Selector: "link"},
			name:  "a link keeps its text",
			want:  "https://tracker.example/details/1",
		},
		{field: Field{Selector: "pubDate"}, name: "case-sensitive name", want: "Sat, 18 Jul 2026 07:33:19 +0000"},
		{
			field: Field{Attribute: "url", Selector: "enclosure"},
			name:  "an attribute of a selected element",
			want:  "https://tracker.example/dl/1.torrent",
		},
		{
			field: Field{Attribute: "value", Selector: "[name=seeders]"},
			name:  "an attribute predicate with an attribute value",
			want:  "100",
		},
		{
			field: Field{Attribute: "value", Selector: "[name=downloadvolumefactor]"},
			name:  "a zero that is present, not missing",
			want:  "0",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := row.lookup(tc.field)
			require.True(t, ok)
			require.Equal(t, tc.want, got)
		})
	}

	// A selector matching nothing is reported as unmatched, which is what
	// makes a required field drop its row and an optional one fall back.
	_, ok := row.lookup(Field{Selector: "nonesuch"})
	require.False(t, ok)

	// So is an absent attribute on an element that does match.
	_, ok = row.lookup(Field{Attribute: "nonesuch", Selector: "enclosure"})
	require.False(t, ok)

	// The second item has no downloadvolumefactor at all.
	_, ok = xmlRow{node: items[1]}.lookup(Field{Attribute: "value", Selector: "[name=downloadvolumefactor]"})
	require.False(t, ok)
}

func TestXMLRow_Matches(t *testing.T) {
	items := selectXML([]*xmlNode{parsedFeed(t)}, "rss > channel > item")
	row := xmlRow{node: items[0]}

	require.True(t, row.matches(""), "an empty selector addresses the row itself")
	require.True(t, row.matches("*"))
	require.True(t, row.matches("title"), "a descendant")
	require.True(t, row.matches("item"), "the row itself")
	require.True(t, row.matches("[name=seeders]"))
	require.False(t, row.matches("nonesuch"))
}

// fetch has already decoded the body to UTF-8 using the definition's
// encoding, so a declaration naming the original charset must not stop
// the parse.
func TestParseXML_TolerateADeclaredEncoding(t *testing.T) {
	root, err := parseXML(`<?xml version="1.0" encoding="windows-1251"?><rss><channel><item><title>Тест</title></item></channel></rss>`)
	require.NoError(t, err)

	titles := selectXML([]*xmlNode{root}, "item > title")
	require.Len(t, titles, 1)
	require.Equal(t, "Тест", titles[0].text(nil))
}

// TestScraper_XMLResponse is the end-to-end check, against a definition
// shaped like the real ones: an RSS feed read with CSS selectors.
func TestScraper_XMLResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		fmt.Fprint(w, rssFeed)
	}))
	t.Cleanup(server.Close)

	def := fmt.Sprintf(`
id: xml-tracker
name: xml-tracker
links:
  - %s/
search:
  paths:
    - path: /rss
      response:
        type: xml
  rows:
    selector: rss > channel > item
  fields:
    title:
      selector: title
    details:
      selector: link
    download:
      selector: enclosure
      attribute: url
    size:
      selector: enclosure
      attribute: length
    date:
      selector: pubDate
    seeders:
      selector: "[name=seeders]"
      attribute: value
    leechers:
      selector: "[name=leechers]"
      attribute: value
`, server.URL)

	tracker := loadTestTracker(t, t.TempDir(), "xml-tracker", def)
	store, err := database.Open(context.Background(), ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { store.Close() })

	scrpr := New(store, NewConfigStore(""), "", testLogger())
	require.NoError(t, scrpr.ScrapeIndexer(context.Background(), tracker,
		SearchParams{Query: "release", Type: "search"}))

	stored := storedTorrents(t, scrpr, tracker)
	require.Len(t, stored, 2)

	byName := map[string]database.Torrent{}
	for _, row := range stored {
		byName[row.Name] = row
	}

	first := byName["Some.Release.2024.1080p"]
	require.Equal(t, "https://tracker.example/details/1", first.DetailsURL)
	require.Equal(t, "https://tracker.example/dl/1.torrent", first.DownloadURL)
	require.Equal(t, int64(1234567890), first.Size)
	require.Equal(t, 100, first.Seeders)
	require.Equal(t, 7, first.Leechers)
	require.Equal(t, "2026-07-18T07:33:19Z", first.Published)

	require.Equal(t, 3, byName["Another.Release.2023.720p"].Seeders)
}
