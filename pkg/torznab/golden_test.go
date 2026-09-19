// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package torznab_test

import (
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torrplay/jacklet/pkg/database"
	"github.com/torrplay/jacklet/pkg/torznab"
)

// updateGolden rewrites the checked-in documents instead of comparing
// against them: `go test ./pkg/torznab -update-golden`. Review the diff it
// produces, since it is the whole point of the test — a field that
// disappeared shows up there and nowhere else.
var updateGolden = flag.Bool("update-golden", false, "rewrite the golden Torznab documents")

// goldenBaseURL fixes the origin the documents are rendered against, so
// nothing in them depends on the port the test server happened to get.
const goldenBaseURL = "https://jacklet.example"

// newGoldenDef writes the definition the golden documents are rendered
// from: named categories and declared search modes, so caps has something
// of each to render, backed by a test site that returns no rows so the
// unconditional re-scrape leaves the stored row alone.
func newGoldenDef(t *testing.T, dir string) {
	t.Helper()
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`<html></html>`))
	}))
	t.Cleanup(site.Close)

	def := fmt.Sprintf(`
id: %[1]s
name: Golden Tracker
description: A tracker used to pin the rendered documents
language: en-US
links:
  - %[2]s/
caps:
  categorymappings:
    - {id: "1", cat: Movies/HD}
    - {id: "2", cat: TV/HD}
  modes:
    search: [q]
    tv-search: [q, season, ep]
    movie-search: [q, imdbid]
search:
  paths:
    - path: "/"
  rows:
    selector: ".torrent_row"
  fields:
    title:
      selector: "a"
    download:
      selector: "a"
      attribute: "href"
`, testIndexerID, site.URL)
	require.NoError(t, os.WriteFile(filepath.Join(dir, testIndexerID+".yml"), []byte(def), 0o600),
		"failed to write the golden indexer definition")
}

// The caps document and the feed are a wire format clients parse by field
// name, so the whole envelope matters, not only the parts a given test
// happens to assert. Comparing a rendered document against a checked-in
// one catches a field that silently disappears — an element dropped from a
// struct, or an `omitempty` that starts firing when it should not — which
// no assertion about search results would notice.
func TestGoldenDocuments(t *testing.T) {
	tests := []struct {
		name  string
		query string
		file  string
	}{
		{name: "caps", query: "t=caps", file: "caps.xml"},
		{name: "feed", query: "t=search&q=Golden", file: "feed.xml"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := newTestDB(t)
			dir := t.TempDir()
			newGoldenDef(t, dir)

			// The row carries every field a definition can scrape, so
			// that each optional torznab:attr renders and a field that
			// stops being emitted shows up as a diff. No real release is
			// a movie and a book and an album at once: this row is a
			// rendering fixture, not a plausible search result. Nothing
			// is left at its zero value by accident — the volume factors
			// are the scraper's own defaults (pkg/scraper defaults both
			// to 1), not the 0 that would advertise freeleech.
			id := storeTorrent(t, db, database.Torrent{
				Album:                "Golden Album",
				Artist:               "Golden Artist",
				Author:               "Golden Author",
				BookTitle:            "The Golden Book",
				Category:             2040,
				Description:          "A release used to pin the rendered feed",
				DetailsURL:           "https://tracker.invalid/details/1",
				DoubanID:             "26387939",
				DownloadURL:          "https://tracker.invalid/download/1",
				DownloadVolumeFactor: 1,
				Files:                7,
				Genres:               "Action,Sci-Fi",
				Grabs:                42,
				IMDBID:               "1375666",
				InfoHash:             "0123456789abcdef0123456789abcdef01234567",
				Label:                "Golden Records",
				Leechers:             2,
				Magnet:               "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567",
				MinimumRatio:         1,
				MinimumSeedTime:      172800,
				Name:                 "Golden.Movie.2024.1080p.BluRay.x264-TEST",
				Poster:               "https://tracker.invalid/covers/1.jpg",
				Published:            "2024-03-15T12:00:00Z",
				Publisher:            "Golden Press",
				RageID:               "12345",
				Seeders:              9,
				Size:                 1234567890,
				TMDBID:               "27205",
				TVDBID:               "81189",
				TVMazeID:             "169",
				Track:                "Golden Track",
				TraktID:              "1390",
				UploadVolumeFactor:   1,
				Year:                 2024,
			})
			// The id is part of the rendered download link, and is 1 in a
			// database this test just made.
			require.EqualValues(t, 1, id, "the golden download link embeds row id 1")

			options := torznab.Options{BaseURL: goldenBaseURL, ContactEmail: "ops@example.org"}
			req := httptest.NewRequest(http.MethodGet, "/api/v2.0/indexers/"+testIndexerID+"/results/torznab/api?"+tc.query, http.NoBody)
			rec := httptest.NewRecorder()
			newTestHandlerWithOptions(t, db, dir, "", options).ServeHTTP(rec, req)
			require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

			path := filepath.Join("testdata", tc.file)
			if *updateGolden {
				require.NoError(t, os.MkdirAll("testdata", 0o750))
				require.NoError(t, os.WriteFile(path, rec.Body.Bytes(), 0o600))
				return
			}
			want, err := os.ReadFile(path)
			require.NoError(t, err, "missing golden document; regenerate with -update-golden")
			require.Equal(t, string(want), rec.Body.String(),
				"the rendered document changed; review the diff, then regenerate with -update-golden if it is intended")
		})
	}
}
