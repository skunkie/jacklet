// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package torznab_test

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/torrplay/jacklet/pkg/scraper"
	"github.com/torrplay/jacklet/pkg/torznab"
)

// feedItem is the subset of a Torznab <item> these tests assert on.
type feedItem struct {
	Attrs []struct {
		Name  string `xml:"name,attr"`
		Value string `xml:"value,attr"`
	} `xml:"attr"`
	Comments  string `xml:"comments"`
	Enclosure struct {
		URL string `xml:"url,attr"`
	} `xml:"enclosure"`
	GUID struct {
		IsPermaLink string `xml:"isPermaLink,attr"`
		Value       string `xml:",chardata"`
	} `xml:"guid"`
	Link    string `xml:"link"`
	PubDate string `xml:"pubDate"`
	Title   string `xml:"title"`
}

type feed struct {
	Channel struct {
		Items []feedItem `xml:"item"`
	} `xml:"channel"`
}

// searchFeed runs a search against the test handler and decodes the feed.
func searchFeed(t *testing.T, mux *http.ServeMux, query string) feed {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v2.0/indexers/"+testIndexerID+"/results/torznab/api?t=search&"+query, http.NoBody)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var f feed
	require.NoError(t, xml.Unmarshal(rec.Body.Bytes(), &f), "failed to decode feed:\n%s", rec.Body.String())
	return f
}

// An item's pubDate must come from the stored release date, not from the
// moment the feed happens to be generated.
func TestTorznabHandlerPubDateFromStoredPublishedDate(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newIndexerDef(t, dir)

	const published = "2024-03-05T14:30:00Z"
	storeTorrent(t, db, scraper.Torrent{
		Category:    2000,
		DetailsURL:  "http://tracker.example/details/1",
		DownloadURL: "magnet:?xt=1",
		Leechers:    1,
		Name:        "Dated Release",
		Published:   published,
		Seeders:     5,
		Size:        100,
	})

	items := searchFeed(t, newTestHandler(t, db, dir, ""), "q=Dated").Channel.Items
	require.Len(t, items, 1)

	got, err := time.Parse(time.RFC1123Z, items[0].PubDate)
	require.NoError(t, err, "pubDate %q is not RFC1123Z", items[0].PubDate)
	want, _ := time.Parse(time.RFC3339, published)
	require.True(t, got.Equal(want), "pubDate = %v, want %v", got, want)
}

// The scraped details page is the item's permalink and comments URL.
func TestTorznabHandlerServesDetailsURL(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newIndexerDef(t, dir)

	const details = "http://tracker.example/details/42"
	storeTorrent(t, db, scraper.Torrent{
		Category:    2000,
		DetailsURL:  details,
		DownloadURL: "magnet:?xt=1",
		Leechers:    1,
		Name:        "Detailed Release",
		Published:   "2024-01-01T00:00:00Z",
		Seeders:     5,
		Size:        100,
	})

	items := searchFeed(t, newTestHandler(t, db, dir, ""), "q=Detailed").Channel.Items
	require.Len(t, items, 1)
	require.Equal(t, details, items[0].GUID.Value, "guid")
	require.Equal(t, "true", items[0].GUID.IsPermaLink, "isPermaLink")
	require.Equal(t, details, items[0].Comments, "comments")
}

// A row with no details URL must still produce a guid, flagged as not
// being a permalink.
func TestTorznabHandlerGUIDFallsBackToTitle(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newIndexerDef(t, dir)

	storeTorrent(t, db, scraper.Torrent{
		Category:    2000,
		DownloadURL: "magnet:?xt=1",
		Leechers:    1,
		Name:        "Bare Release",
		Seeders:     5,
		Size:        100,
	})

	items := searchFeed(t, newTestHandler(t, db, dir, ""), "q=Bare").Channel.Items
	require.Len(t, items, 1)
	require.Equal(t, "Bare Release", items[0].GUID.Value, "the guid must fall back to the title")
	require.Equal(t, "false", items[0].GUID.IsPermaLink, "isPermaLink")
}

// Clients score releases on the volume factors and swarm attributes, so
// they must be present on every item.
func TestTorznabHandlerEmitsFullAttributeSet(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newIndexerDef(t, dir)

	storeTorrent(t, db, scraper.Torrent{
		Category:             2000,
		DetailsURL:           "http://x/d/1",
		DownloadURL:          "magnet:?xt=1",
		DownloadVolumeFactor: 0.0,
		Files:                7,
		Grabs:                42,
		InfoHash:             "abc123",
		Leechers:             2,
		Name:                 "Freeleech Release",
		Seeders:              9,
		Size:                 100,
		UploadVolumeFactor:   2.0,
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v2.0/indexers/"+testIndexerID+"/results/torznab/api?t=search&q=Freeleech", http.NoBody)
	rec := httptest.NewRecorder()
	newTestHandler(t, db, dir, "").ServeHTTP(rec, req)

	var feed struct {
		Channel struct {
			Items []struct {
				Attributes []struct {
					Name  string `xml:"name,attr"`
					Value string `xml:"value,attr"`
				} `xml:"attr"`
			} `xml:"item"`
		} `xml:"channel"`
	}
	require.NoError(t, xml.Unmarshal(rec.Body.Bytes(), &feed), "failed to decode feed")
	require.Len(t, feed.Channel.Items, 1)

	got := map[string]string{}
	for _, a := range feed.Channel.Items[0].Attributes {
		got[a.Name] = a.Value
	}
	want := map[string]string{
		"seeders": "9", "leechers": "2", "peers": "11", "size": "100",
		"grabs": "42", "files": "7", "category": "2000",
		"downloadvolumefactor": "0", "uploadvolumefactor": "2", "infohash": "abc123",
	}
	for name, value := range want {
		require.Equal(t, value, got[name], "attr %s", name)
	}
}

// A row written without the optional columns takes the schema's defaults,
// so it serves as zeros rather than failing the query.
func TestTorznabHandlerServesRowWrittenWithDefaults(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newIndexerDef(t, dir)

	// Only the columns a minimal writer would set.
	storeTorrent(t, db, scraper.Torrent{DownloadURL: "magnet:?xt=1", Name: "Sparse Release"})

	items := searchFeed(t, newTestHandler(t, db, dir, ""), "q=Sparse").Channel.Items
	require.Len(t, items, 1)
	require.Equal(t, "Sparse Release", items[0].Title)
}

// A tracker's own HTTP download link must be rewritten to Jacklet's proxy
// endpoint. A Torznab client has no account on a private tracker, so
// following the tracker's link directly gives it a 403 or a login page
// saved as a ".torrent".
func TestTorznabHandlerRewritesHTTPDownloadsThroughTheProxy(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newIndexerDef(t, dir)

	rowID := storeTorrent(t, db, scraper.Torrent{
		Category:    2000,
		DetailsURL:  "http://tracker.example/details/1",
		DownloadURL: "http://tracker.example/download/1",
		Leechers:    1,
		Name:        "Private Release",
		Published:   "2024-03-05T14:30:00Z",
		Seeders:     5,
		Size:        100,
	})

	items := searchFeed(t, newTestHandler(t, db, dir, ""), "q=Private").Channel.Items
	require.Len(t, items, 1)

	want := fmt.Sprintf("/api/v2.0/indexers/%s/download/%d", testIndexerID, rowID)
	require.True(t, strings.HasSuffix(items[0].Link, want), "link = %q, want it to end in %q", items[0].Link, want)
	require.True(t, strings.HasSuffix(items[0].Enclosure.URL, want),
		"enclosure url = %q, want it to end in %q", items[0].Enclosure.URL, want)
}

// A magnet needs no proxying: a client resolves one itself and there is
// nothing to authenticate.
func TestTorznabHandlerLeavesMagnetLinksAlone(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newIndexerDef(t, dir)

	const magnet = "magnet:?xt=urn:btih:abcdef"
	storeTorrent(t, db, scraper.Torrent{Category: 2000, DownloadURL: magnet, Name: "Magnet Release"})

	items := searchFeed(t, newTestHandler(t, db, dir, ""), "q=Magnet").Channel.Items
	require.Len(t, items, 1)
	require.Equal(t, magnet, items[0].Link, "the magnet must be left alone")
}

func TestTorznabHandlerProtectedFeedCarriesAPIKeyInDownloadLink(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newIndexerDef(t, dir)
	storeTorrent(t, db, scraper.Torrent{
		DownloadURL: "https://tracker.test/download/1.torrent",
		Name:        "Protected Example",
	})

	items := searchFeed(t, newTestHandler(t, db, dir, "secret value"), "q=Protected&apikey=secret+value").Channel.Items
	require.Len(t, items, 1)
	parsed, err := url.Parse(items[0].Link)
	require.NoError(t, err)
	require.Equal(t, "secret value", parsed.Query().Get("apikey"), "the download link dropped the apikey")
}

func TestTorznabHandlerConfiguredBaseURLControlsGeneratedLinks(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newIndexerDef(t, dir)
	storeTorrent(t, db, scraper.Torrent{
		DownloadURL: "https://tracker.test/download/1.torrent",
		Name:        "Origin Example",
	})

	mux := newTestHandlerWithOptions(t, db, dir, "", torznab.Options{BaseURL: "https://public.example"})
	items := searchFeed(t, mux, "q=Origin").Channel.Items
	require.Len(t, items, 1)
	require.True(t, strings.HasPrefix(items[0].Link, "https://public.example/"),
		"generated link = %q, want the configured base URL", items[0].Link)
}

// TestTorznabHandlerMagnetBesideTheTorrent covers a release offering both
// a torrent file and a magnet. The link stays the proxied torrent, which
// is what a private tracker needs, and the magnet rides along as an
// attribute for a client that prefers one.
func TestTorznabHandlerMagnetBesideTheTorrent(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newIndexerDef(t, dir)

	const magnet = "magnet:?xt=urn:btih:abcdef"
	storeTorrent(t, db, scraper.Torrent{
		Category:    2000,
		DownloadURL: "https://tracker.test/dl/1.torrent",
		Magnet:      magnet,
		Name:        "Both Release",
	})

	items := searchFeed(t, newTestHandler(t, db, dir, ""), "q=Both").Channel.Items
	require.Len(t, items, 1)

	require.Contains(t, items[0].Link, "/download/", "want the proxied torrent as the link")

	var got string
	for _, attr := range items[0].Attrs {
		if attr.Name == "magneturl" {
			got = attr.Value
		}
	}
	require.Equal(t, magnet, got, "magneturl")
}

// TestTorznabHandlerMagnetOnlyRow covers a release with no torrent file:
// the magnet becomes the link, and the download endpoint hands it back
// rather than trying to fetch it from the tracker.
func TestTorznabHandlerMagnetOnlyRow(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newIndexerDef(t, dir)

	const magnet = "magnet:?xt=urn:btih:abcdef"
	storeTorrent(t, db, scraper.Torrent{Category: 2000, Magnet: magnet, Name: "Magnet Only"})

	handler := newTestHandler(t, db, dir, "")
	items := searchFeed(t, handler, "q=Magnet").Channel.Items
	require.Len(t, items, 1)
	require.Equal(t, magnet, items[0].Link, "want the magnet as the link")

	req := httptest.NewRequest(http.MethodGet, "/api/v2.0/indexers/"+testIndexerID+"/download/1", http.NoBody)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusFound, rec.Code)
	require.Equal(t, magnet, rec.Header().Get("Location"), "Location")
}
