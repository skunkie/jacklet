// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package torznab_test

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/torrplay/jacklet/pkg/database"
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
func TestTorznabHandler_PubDateFromStoredPublishedDate(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newIndexerDef(t, dir)

	const published = "2024-03-05T14:30:00Z"
	storeTorrent(t, db, database.Torrent{
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
func TestTorznabHandler_ServesDetailsURL(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newIndexerDef(t, dir)

	const details = "http://tracker.example/details/42"
	storeTorrent(t, db, database.Torrent{
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
func TestTorznabHandler_GUIDFallsBackToTitle(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newIndexerDef(t, dir)

	storeTorrent(t, db, database.Torrent{
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

// newUnreachableIndexerDef writes a definition whose only link refuses
// connections, so every scrape of it fails.
func newUnreachableIndexerDef(t *testing.T, dir string) {
	t.Helper()
	def := fmt.Sprintf(`
id: %[1]s
name: %[1]s
links:
  - http://127.0.0.1:1/
search:
  paths:
    - path: "/"
      method: get
  rows:
    selector: ".torrent_row"
  fields:
    title:
      selector: "a"
`, testIndexerID)
	require.NoError(t, os.WriteFile(dir+"/"+testIndexerID+".yml", []byte(def), 0o600),
		"failed to write indexer definition")
}

// The store exists so a tracker outage degrades to stale results rather
// than to no results at all.
func TestTorznabHandler_ServesStoredResultsWhenScrapeFails(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newUnreachableIndexerDef(t, dir)

	storeTorrent(t, db, database.Torrent{
		Category:    2000,
		DownloadURL: "magnet:?xt=1",
		Leechers:    1,
		Name:        "Cached Release",
		Published:   "2024-01-01T00:00:00Z",
		Seeders:     5,
		Size:        100,
	})

	mux := newTestHandler(t, db, dir, "")

	items := searchFeed(t, mux, "q=Cached").Channel.Items
	require.Len(t, items, 1, "want the stored release served")
	require.Equal(t, "Cached Release", items[0].Title)

	// The JSON endpoint shares the pipeline and must behave the same way.
	req := httptest.NewRequest(http.MethodGet, "/api/v2.0/indexers/"+testIndexerID+"/results?q=Cached", http.NoBody)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "results: %s", rec.Body.String())
}

// With nothing stored to fall back on, an unreachable tracker is still an
// error rather than a silently empty feed.
func TestTorznabHandler_FailsWhenScrapeFailsWithNothingStored(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newUnreachableIndexerDef(t, dir)

	req := httptest.NewRequest(http.MethodGet, "/api/v2.0/indexers/"+testIndexerID+"/results/torznab/api?t=search&q=anything", http.NoBody)
	rec := httptest.NewRecorder()
	newTestHandler(t, db, dir, "").ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadGateway, rec.Code, "body: %s", rec.Body.String())
}

// A "%" in the query is a literal character to match, not a wildcard.
func TestTorznabHandler_EscapesLikeWildcards(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newIndexerDef(t, dir)

	for _, name := range []string{"100% Example", "Other Example"} {
		storeTorrent(t, db, database.Torrent{
			Category:    2000,
			DownloadURL: "magnet:?xt=1",
			Leechers:    1,
			Name:        name,
			Seeders:     5,
			Size:        100,
		})
	}

	mux := newTestHandler(t, db, dir, "")

	items := searchFeed(t, mux, "q=100%25+Example").Channel.Items
	require.Len(t, items, 1, "want only the literal match")
	require.Equal(t, "100% Example", items[0].Title)

	// A bare "%" must not match every row.
	require.Len(t, searchFeed(t, mux, "q=%25").Channel.Items, 1, "a literal %% matched the wrong number of rows")
}

// Asking for a top-level category returns results filed under any of its
// subcategories, as Jackett does.
func TestTorznabHandler_ParentCategoryMatchesSubcategories(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newIndexerDef(t, dir)

	for name, cat := range map[string]int{"HD Movie": 2040, "SD Movie": 2030, "A TV Show": 5040} {
		storeTorrent(t, db, database.Torrent{
			Category:    cat,
			DownloadURL: "magnet:?xt=1",
			Leechers:    1,
			Name:        name,
			Seeders:     5,
			Size:        100,
		})
	}

	items := searchFeed(t, newTestHandler(t, db, dir, ""), "cat=2000").Channel.Items
	require.Len(t, items, 2, "want both movie subcategories")
	for _, item := range items {
		require.NotContains(t, item.Title, "TV", "a TV result leaked into a Movies query")
	}
}

// Caps reflect what the definition declares rather than a fixed set.
func TestTorznabHandler_CapsFromDefinitionModes(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()

	def := fmt.Sprintf(`
id: %[1]s
name: %[1]s
links:
  - http://tracker.example/
caps:
  modes:
    search: [q]
    book-search: [q, author]
  categorymappings:
    - {id: 1, cat: "Books/EBook"}
search:
  paths:
    - path: "/"
  rows:
    selector: ".row"
  fields:
    title:
      selector: "a"
`, testIndexerID)
	require.NoError(t, os.WriteFile(dir+"/"+testIndexerID+".yml", []byte(def), 0o600),
		"failed to write definition")

	req := httptest.NewRequest(http.MethodGet, "/api/v2.0/indexers/"+testIndexerID+"/results/torznab/api?t=caps", http.NoBody)
	rec := httptest.NewRecorder()
	newTestHandler(t, db, dir, "").ServeHTTP(rec, req)

	var caps struct {
		Categories struct {
			Category []struct {
				ID int `xml:"id,attr"`
			} `xml:"category"`
		} `xml:"categories"`
		Searching struct {
			BookSearch struct {
				Available       string `xml:"available,attr"`
				SupportedParams string `xml:"supportedParams,attr"`
			} `xml:"book-search"`
			TVSearch struct {
				Available string `xml:"available,attr"`
			} `xml:"tv-search"`
		} `xml:"searching"`
	}
	require.NoError(t, xml.Unmarshal(rec.Body.Bytes(), &caps), "failed to decode caps")

	require.Equal(t, "yes", caps.Searching.BookSearch.Available, "book-search available")
	require.Equal(t, "q,author", caps.Searching.BookSearch.SupportedParams, "book-search params")
	require.Equal(t, "no", caps.Searching.TVSearch.Available, "tv-search available")
	require.Len(t, caps.Categories.Category, 1, "want only the Books/EBook subcategory")
	require.Equal(t, 7020, caps.Categories.Category[0].ID, "want the Books/EBook subcategory")
}

// The JSON endpoint carries the details URL and publish date too.
func TestTorznabHandler_ResultsJSONIncludesDetailsAndDate(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newIndexerDef(t, dir)

	const details = "http://tracker.example/details/7"
	const published = "2024-05-06T07:08:09Z"
	storeTorrent(t, db, database.Torrent{
		Category:    2000,
		DetailsURL:  details,
		DownloadURL: "magnet:?xt=1",
		Leechers:    1,
		Name:        "JSON Release",
		Published:   published,
		Seeders:     5,
		Size:        100,
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v2.0/indexers/"+testIndexerID+"/results?q=JSON", http.NoBody)
	rec := httptest.NewRecorder()
	newTestHandler(t, db, dir, "").ServeHTTP(rec, req)

	var body struct {
		Results []struct {
			Details     string `json:"details"`
			GUID        string `json:"guid"`
			PublishDate string `json:"publishDate"`
		} `json:"results"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), "failed to decode results")
	require.Len(t, body.Results, 1)
	require.Equal(t, details, body.Results[0].Details, "details")
	require.Equal(t, details, body.Results[0].GUID, "guid")
	require.Equal(t, published, body.Results[0].PublishDate, "publishDate")
}

// Every mode handleCaps advertises as available must be accepted by the
// search endpoint, or a client takes Jacklet up on an offer it rejects.
func TestTorznabHandler_AcceptsEveryAdvertisedMode(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()

	def := fmt.Sprintf(`
id: %[1]s
name: %[1]s
links:
  - http://tracker.example/
caps:
  modes:
    search: [q]
    tv-search: [q, season, ep]
    movie-search: [q]
    music-search: [q]
    book-search: [q]
  categorymappings:
    - {id: 1, cat: "Movies"}
search:
  paths:
    - path: "/"
  rows:
    selector: ".row"
  fields:
    title:
      selector: "a"
`, testIndexerID)
	require.NoError(t, os.WriteFile(dir+"/"+testIndexerID+".yml", []byte(def), 0o600),
		"failed to write definition")

	// A stored row keeps an unreachable tracker from turning into a 502,
	// leaving the response status to reflect the "t" handling alone.
	storeTorrent(t, db, database.Torrent{
		Category:    2000,
		DownloadURL: "magnet:?xt=1",
		Leechers:    1,
		Name:        "Stored",
		Seeders:     1,
		Size:        1,
	})

	mux := newTestHandler(t, db, dir, "")
	for _, mode := range []string{"search", "tvsearch", "movie", "music", "book", "audio"} {
		t.Run(mode, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v2.0/indexers/"+testIndexerID+"/results/torznab/api?t="+mode, http.NoBody)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			require.Equal(t, http.StatusOK, rec.Code, "t=%s: %s", mode, rec.Body.String())
		})
	}
}

// Clients score releases on the volume factors and swarm attributes, so
// they must be present on every item.
func TestTorznabHandler_EmitsFullAttributeSet(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newIndexerDef(t, dir)

	storeTorrent(t, db, database.Torrent{
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

// A multi-word query must require every word, so unrelated stored releases
// do not leak into a result set.
func TestTorznabHandler_RequiresEveryQueryTerm(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newIndexerDef(t, dir)

	for _, name := range []string{"Example Series S01E01", "Other Series S01E01", "Example Series Soundtrack"} {
		storeTorrent(t, db, database.Torrent{
			Category:    5000,
			DownloadURL: "magnet:?xt=1",
			Leechers:    1,
			Name:        name,
			Seeders:     1,
			Size:        1,
		})
	}

	items := searchFeed(t, newTestHandler(t, db, dir, ""), "q=Example+Series").Channel.Items
	require.Len(t, items, 2, "want the two Example Series releases")
	for _, item := range items {
		require.Contains(t, item.Title, "Example Series", "an unrelated release matched")
	}
}

// staticTrackerSource serves definitions from memory, standing in for a
// consumer of the scraper package that keeps its definitions somewhere
// other than a directory of YAML files.
type staticTrackerSource struct {
	defs []scraper.Tracker
}

func (s staticTrackerSource) Find(id string) (*scraper.Tracker, error) {
	for i := range s.defs {
		if scraper.TrackerID(&s.defs[i]) == id {
			found := s.defs[i]
			return &found, nil
		}
	}
	return nil, fmt.Errorf("%w: %q", scraper.ErrTrackerNotFound, id)
}

func (s staticTrackerSource) Trackers() ([]scraper.Tracker, []scraper.DefinitionError, error) {
	return s.defs, nil, nil
}

// The handler must work against any TrackerSource, not just the
// directory-backed store.
func TestTorznabHandler_AcceptsACustomTrackerSource(t *testing.T) {
	db := newTestDB(t)

	source := staticTrackerSource{defs: []scraper.Tracker{{
		Caps: scraper.Caps{CategoryMappings: []scraper.CategoryMapping{{Cat: "Movies", ID: "1"}}},
		ID:   testIndexerID,
		Name: "In-Memory Tracker",
	}}}

	logger := slog.New(slog.DiscardHandler)
	scrpr := scraper.New(db, scraper.NewConfigStore(""), "", logger)
	handler := torznab.New(db, scrpr, source, "", logger)

	mux := http.NewServeMux()
	handler.Routes(mux)

	t.Run("serves caps", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v2.0/indexers/"+testIndexerID+"/results/torznab/api?t=caps", http.NoBody)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
		require.Contains(t, rec.Body.String(), `id="2000"`, "want the Movies category in caps")
	})

	t.Run("lists the indexer", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v2.0/indexers", http.NoBody)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		var body []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), "failed to decode")
		require.Len(t, body, 1)
		require.Equal(t, "In-Memory Tracker", body[0].Name)
	})

	t.Run("reports an unknown id", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v2.0/indexers/absent/results/torznab/api?t=caps", http.NoBody)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		require.Equal(t, http.StatusNotFound, rec.Code)
	})
}

// A search must match a non-ASCII title regardless of case. SQLite's LIKE
// folds case for ASCII only, which would make a Cyrillic query silently
// return nothing on a Russian-language tracker.
func TestTorznabHandler_MatchesNonASCIICaseInsensitively(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newIndexerDef(t, dir)

	for _, name := range []string{"Тестовый Релиз (2009) BDRip", "Тёмный Образец (1977)"} {
		storeTorrent(t, db, database.Torrent{
			Category:    2000,
			DownloadURL: "magnet:?xt=1",
			Leechers:    1,
			Name:        name,
			Seeders:     1,
			Size:        1,
		})
	}

	mux := newTestHandler(t, db, dir, "")

	tests := []struct {
		name  string
		query string
		want  string
	}{
		{name: "lowercase query, capitalized title", query: "q=%D1%82%D0%B5%D1%81%D1%82%D0%BE%D0%B2%D1%8B%D0%B9", want: "Тестовый Релиз (2009) BDRip"},
		{name: "matching case", query: "q=%D0%A2%D0%B5%D1%81%D1%82%D0%BE%D0%B2%D1%8B%D0%B9", want: "Тестовый Релиз (2009) BDRip"},
		{name: "term containing ё", query: "q=%D1%82%D1%91%D0%BC%D0%BD%D1%8B%D0%B9", want: "Тёмный Образец (1977)"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			items := searchFeed(t, mux, tc.query).Channel.Items
			require.Len(t, items, 1)
			require.Equal(t, tc.want, items[0].Title)
		})
	}
}

// A row written without the optional columns takes the schema's defaults,
// so it serves as zeros rather than failing the query.
func TestTorznabHandler_ServesRowWrittenWithDefaults(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newIndexerDef(t, dir)

	// Only the columns a minimal writer would set.
	storeTorrent(t, db, database.Torrent{DownloadURL: "magnet:?xt=1", Name: "Sparse Release"})

	items := searchFeed(t, newTestHandler(t, db, dir, ""), "q=Sparse").Channel.Items
	require.Len(t, items, 1)
	require.Equal(t, "Sparse Release", items[0].Title)
}

// A tracker's own HTTP download link must be rewritten to Jacklet's proxy
// endpoint. A Torznab client has no account on a private tracker, so
// following the tracker's link directly gives it a 403 or a login page
// saved as a ".torrent".
func TestTorznabHandler_RewritesHTTPDownloadsThroughTheProxy(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newIndexerDef(t, dir)

	rowID := storeTorrent(t, db, database.Torrent{
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
func TestTorznabHandler_LeavesMagnetLinksAlone(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newIndexerDef(t, dir)

	const magnet = "magnet:?xt=urn:btih:abcdef"
	storeTorrent(t, db, database.Torrent{Category: 2000, DownloadURL: magnet, Name: "Magnet Release"})

	items := searchFeed(t, newTestHandler(t, db, dir, ""), "q=Magnet").Channel.Items
	require.Len(t, items, 1)
	require.Equal(t, magnet, items[0].Link, "the magnet must be left alone")
}

// The proxy fetches the torrent with Jacklet's tracker session and streams
// it back, rather than handing the client a link it cannot authenticate.
func TestTorznabHandler_DownloadFetchesWithTheTrackerSession(t *testing.T) {
	const torrent = "d8:announce7:exampleeee"

	var gotCookie, gotUserAgent string
	tracker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			// A stand-in tracker handing out a session cookie, not a
			// cookie Jacklet issues to a browser.
			//nolint:gosec // G124: the test tracker, not Jacklet, sets this
			http.SetCookie(w, &http.Cookie{Name: "session", Path: "/", Value: "granted"})
			w.Write([]byte(`<html><body>ok</body></html>`))
			return
		}
		if c, err := r.Cookie("session"); err == nil {
			gotCookie = c.Value
		}
		gotUserAgent = r.UserAgent()
		w.Header().Set("Content-Type", "application/x-bittorrent")
		w.Write([]byte(torrent))
	}))
	defer tracker.Close()

	dir := t.TempDir()
	def := fmt.Sprintf(`
id: %[1]s
name: %[1]s
links:
  - %[2]s/
login:
  path: /login
  method: post
  inputs:
    username: user
    password: pass
search:
  paths:
    - path: "/"
  rows:
    selector: ".torrent_row"
  fields:
    title:
      selector: "a"
`, testIndexerID, tracker.URL)
	require.NoError(t, os.WriteFile(dir+"/"+testIndexerID+".yml", []byte(def), 0o600),
		"failed to write indexer definition")

	db := newTestDB(t)
	rowID := storeTorrent(t, db, database.Torrent{
		DownloadURL: tracker.URL + "/download/1",
		Name:        "Some/Release: 2024",
	})

	rec := httptest.NewRecorder()
	newTestHandler(t, db, dir, "").ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/api/v2.0/indexers/%s/download/%d", testIndexerID, rowID), http.NoBody))

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	require.Equal(t, torrent, rec.Body.String(), "want the torrent streamed back verbatim")
	require.Equal(t, "granted", gotCookie, "want the session cookie login established")
	require.Contains(t, gotUserAgent, "Mozilla/5.0", "want a browser user agent")
	require.Equal(t, "application/x-bittorrent", rec.Header().Get("Content-Type"))

	// The filename comes from a third-party page, so path separators and
	// punctuation must not reach it.
	cd := rec.Header().Get("Content-Disposition")
	require.Contains(t, cd, `filename=`)
	require.NotContains(t, cd, "/", "a path separator reached the filename")
}

// A tracker that has lost the session answers with a login page rather
// than a 401. Saving that as a ".torrent" is the failure this endpoint
// exists to avoid, so it is reported instead.
func TestTorznabHandler_DownloadRejectsAWebPage(t *testing.T) {
	tracker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<html><body>Please log in</body></html>`))
	}))
	defer tracker.Close()

	dir := t.TempDir()
	def := fmt.Sprintf("id: %[1]s\nname: %[1]s\nlinks:\n  - %[2]s/\nsearch:\n  paths:\n    - path: \"/\"\n  rows:\n    selector: .r\n  fields:\n    title:\n      selector: a\n",
		testIndexerID, tracker.URL)
	require.NoError(t, os.WriteFile(dir+"/"+testIndexerID+".yml", []byte(def), 0o600),
		"failed to write indexer definition")

	db := newTestDB(t)
	rowID := storeTorrent(t, db, database.Torrent{
		DownloadURL: tracker.URL + "/download/1",
		Name:        "Release",
	})

	rec := httptest.NewRecorder()
	newTestHandler(t, db, dir, "").ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/api/v2.0/indexers/%s/download/%d", testIndexerID, rowID), http.NoBody))

	require.Equal(t, http.StatusBadGateway, rec.Code, "body: %s", rec.Body.String())
}

// The endpoint takes a row id, never a URL, so it cannot be turned into an
// open proxy. A row whose link points somewhere other than the tracker is
// refused rather than fetched with that tracker's credentials.
func TestTorznabHandler_DownloadRefusesAForeignLink(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newIndexerDef(t, dir)

	rowID := storeTorrent(t, db, database.Torrent{
		DownloadURL: "http://attacker.example/steal",
		Name:        "Elsewhere",
	})

	mux := newTestHandler(t, db, dir, "")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/api/v2.0/indexers/%s/download/%d", testIndexerID, rowID), http.NoBody))
	require.Equal(t, http.StatusBadRequest, rec.Code, "foreign link: %s", rec.Body.String())

	// An unknown row is a 404, not an attempt to fetch anything.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/api/v2.0/indexers/"+testIndexerID+"/download/999999", http.NoBody))
	require.Equal(t, http.StatusNotFound, rec.Code, "unknown row")
}

// A tracker cannot turn its download proxy into a redirect-following open
// proxy: redirects to another host are rejected before the response is
// returned to the client.
func TestTorznabHandler_DownloadRefusesForeignRedirect(t *testing.T) {
	var targetRequests int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetRequests++
		w.Header().Set("Content-Type", "application/x-bittorrent")
		_, _ = w.Write([]byte("not a tracker torrent"))
	}))
	defer target.Close()

	tracker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/download/1" {
			http.Redirect(w, r, target.URL+"/secret", http.StatusFound)
			return
		}
		w.Write([]byte(`<html></html>`))
	}))
	defer tracker.Close()

	dir := t.TempDir()
	def := fmt.Sprintf(`
id: %[1]s
name: %[1]s
links:
  - %[2]s/
search:
  paths:
    - path: "/"
  rows:
    selector: ".torrent_row"
  fields:
    title:
      selector: "a"
`, testIndexerID, tracker.URL)
	require.NoError(t, os.WriteFile(dir+"/"+testIndexerID+".yml", []byte(def), 0o600),
		"failed to write indexer definition")

	db := newTestDB(t)
	rowID := storeTorrent(t, db, database.Torrent{
		DownloadURL: tracker.URL + "/download/1",
		Name:        "Redirected Release",
	})

	rec := httptest.NewRecorder()
	newTestHandler(t, db, dir, "").ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/api/v2.0/indexers/%s/download/%d", testIndexerID, rowID), http.NoBody))

	require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	require.Zero(t, targetRequests, "the foreign redirect target was requested")
}

// The download endpoint is behind the same apikey as every other one.
func TestTorznabHandler_DownloadRequiresTheAPIKey(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newIndexerDef(t, dir)

	rec := httptest.NewRecorder()
	newTestHandler(t, db, dir, "secret").ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/api/v2.0/indexers/"+testIndexerID+"/download/1", http.NoBody))
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestTorznabHandler_ProtectedFeedCarriesAPIKeyInDownloadLink(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newIndexerDef(t, dir)
	storeTorrent(t, db, database.Torrent{
		DownloadURL: "https://tracker.test/download/1.torrent",
		Name:        "Protected Example",
	})

	items := searchFeed(t, newTestHandler(t, db, dir, "secret value"), "q=Protected&apikey=secret+value").Channel.Items
	require.Len(t, items, 1)
	parsed, err := url.Parse(items[0].Link)
	require.NoError(t, err)
	require.Equal(t, "secret value", parsed.Query().Get("apikey"), "the download link dropped the apikey")
}

func TestTorznabHandler_ConfiguredBaseURLControlsGeneratedLinks(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newIndexerDef(t, dir)
	storeTorrent(t, db, database.Torrent{
		DownloadURL: "https://tracker.test/download/1.torrent",
		Name:        "Origin Example",
	})

	mux := newTestHandlerWithOptions(t, db, dir, "", torznab.Options{BaseURL: "https://public.example"})
	items := searchFeed(t, mux, "q=Origin").Channel.Items
	require.Len(t, items, 1)
	require.True(t, strings.HasPrefix(items[0].Link, "https://public.example/"),
		"generated link = %q, want the configured base URL", items[0].Link)
}

func TestTorznabHandler_ScrapeDeadlineStillAllowsStoredFallback(t *testing.T) {
	tracker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer tracker.Close()

	dir := t.TempDir()
	def := fmt.Sprintf("id: %[1]s\nname: %[1]s\nlinks:\n  - %[2]s/\nsearch:\n  paths:\n    - path: /\n  rows:\n    selector: .row\n  fields:\n    title:\n      selector: a\n", testIndexerID, tracker.URL)
	require.NoError(t, os.WriteFile(dir+"/"+testIndexerID+".yml", []byte(def), 0o600))

	db := newTestDB(t)
	storeTorrent(t, db, database.Torrent{DownloadURL: "magnet:?xt=1", Name: "Fallback Example"})
	mux := newTestHandlerWithOptions(t, db, dir, "", torznab.Options{ScrapeTimeout: 20 * time.Millisecond})
	items := searchFeed(t, mux, "q=Fallback").Channel.Items
	require.Len(t, items, 1)
	require.Equal(t, "Fallback Example", items[0].Title)
}

func TestTorznabHandler_FailedScrapeAllowsPagePastLastResult(t *testing.T) {
	tracker := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	trackerURL := tracker.URL
	tracker.Close()

	dir := t.TempDir()
	def := fmt.Sprintf("id: %[1]s\nname: %[1]s\nlinks:\n  - %[2]s/\nsearch:\n  paths:\n    - path: /\n  rows:\n    selector: .row\n  fields:\n    title:\n      selector: a\n", testIndexerID, trackerURL)
	require.NoError(t, os.WriteFile(dir+"/"+testIndexerID+".yml", []byte(def), 0o600))

	db := newTestDB(t)
	storeTorrent(t, db, database.Torrent{DownloadURL: "magnet:?xt=1", Name: "Paged Example"})
	feed := searchFeed(t, newTestHandler(t, db, dir, ""), "q=Paged&offset=10")
	require.Empty(t, feed.Channel.Items, "items were served past the last page")
}

// TestTorznabHandler_MagnetBesideTheTorrent covers a release offering both
// a torrent file and a magnet. The link stays the proxied torrent, which
// is what a private tracker needs, and the magnet rides along as an
// attribute for a client that prefers one.
func TestTorznabHandler_MagnetBesideTheTorrent(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newIndexerDef(t, dir)

	const magnet = "magnet:?xt=urn:btih:abcdef"
	storeTorrent(t, db, database.Torrent{
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

// TestTorznabHandler_MagnetOnlyRow covers a release with no torrent file:
// the magnet becomes the link, and the download endpoint hands it back
// rather than trying to fetch it from the tracker.
func TestTorznabHandler_MagnetOnlyRow(t *testing.T) {
	db := newTestDB(t)
	dir := t.TempDir()
	newIndexerDef(t, dir)

	const magnet = "magnet:?xt=urn:btih:abcdef"
	storeTorrent(t, db, database.Torrent{Category: 2000, Magnet: magnet, Name: "Magnet Only"})

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

// A tracker that breaks off mid-download must not look like a success to
// the client: a .torrent is saved to disk, so an empty or truncated file
// arriving under a 200 is the failure that gets noticed only later, when
// the torrent client rejects it.
func TestTorznabHandler_DownloadFailureIsNotAQuietSuccess(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write int
	}{
		{name: "before any bytes", write: 0},
		{name: "part way through", write: 32},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tracker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/download/1" {
					_, _ = w.Write([]byte(`<html></html>`))
					return
				}
				// Promising more than is delivered and then dropping the
				// connection is how a tracker fails part way through.
				w.Header().Set("Content-Length", "4096")
				w.Header().Set("Content-Type", "application/x-bittorrent")
				// The headers are flushed first so the response itself
				// succeeds and only the body fails, which is what puts the
				// failure inside the proxy's copy rather than its request.
				w.WriteHeader(http.StatusOK)
				if tc.write > 0 {
					_, _ = w.Write(make([]byte, tc.write))
				}
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
				panic(http.ErrAbortHandler)
			}))
			defer tracker.Close()

			dir := t.TempDir()
			def := fmt.Sprintf(`
id: %[1]s
name: %[1]s
links:
  - %[2]s/
search:
  paths:
    - path: "/"
  rows:
    selector: ".torrent_row"
  fields:
    title:
      selector: "a"
`, testIndexerID, tracker.URL)
			require.NoError(t, os.WriteFile(dir+"/"+testIndexerID+".yml", []byte(def), 0o600),
				"failed to write indexer definition")

			db := newTestDB(t)
			rowID := storeTorrent(t, db, database.Torrent{
				DownloadURL: tracker.URL + "/download/1",
				Name:        "Interrupted Release",
			})

			jacklet := httptest.NewServer(newTestHandler(t, db, dir, ""))
			defer jacklet.Close()

			resp, err := jacklet.Client().Get(fmt.Sprintf("%s/api/v2.0/indexers/%s/download/%d", jacklet.URL, testIndexerID, rowID))
			if err != nil {
				// The connection was broken after the headers went out,
				// which is the truncated case reported as a failure.
				return
			}
			defer resp.Body.Close()
			body, readErr := io.ReadAll(resp.Body)
			require.False(t, resp.StatusCode == http.StatusOK && readErr == nil,
				"a failed download returned 200 with %d bytes and no error", len(body))
		})
	}
}

// The contact address advertised in caps and in the feed is the operator's
// to set: an unconfigured deployment must advertise none at all, rather
// than a placeholder address that clients show to their users.
func TestTorznabHandler_ContactEmailIsConfigurable(t *testing.T) {
	body := func(t *testing.T, options torznab.Options, path string) string {
		t.Helper()
		db := newTestDB(t)
		dir := t.TempDir()
		newIndexerDef(t, dir)

		req := httptest.NewRequest(http.MethodGet, path, http.NoBody)
		rec := httptest.NewRecorder()
		newTestHandlerWithOptions(t, db, dir, "", options).ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
		return rec.Body.String()
	}

	capsPath := "/api/v2.0/indexers/" + testIndexerID + "/results/torznab/api?t=caps"
	feedPath := "/api/v2.0/indexers/" + testIndexerID + "/results/torznab/api?t=search"

	t.Run("omitted by default", func(t *testing.T) {
		// Each absence is paired with what must still be there, so a
		// response that lost the whole element — or never rendered —
		// cannot pass for an omitted address.
		caps := body(t, torznab.Options{}, capsPath)
		require.Contains(t, caps, "<server ", "caps did not render a server element at all")
		require.NotContains(t, caps, "email=", "caps advertised an email nobody configured")

		feed := body(t, torznab.Options{}, feedPath)
		require.Contains(t, feed, "<channel>", "the feed did not render a channel at all")
		require.NotContains(t, feed, "webMaster", "the feed advertised a webMaster nobody configured")
	})

	t.Run("advertised when configured", func(t *testing.T) {
		options := torznab.Options{ContactEmail: "ops@tracker.invalid"}
		require.Contains(t, body(t, options, capsPath), `email="ops@tracker.invalid"`, "caps dropped the configured address")
		require.Contains(t, body(t, options, feedPath), "<webMaster>ops@tracker.invalid</webMaster>", "the feed dropped the configured address")
	})
}
