// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/torrplay/jacklet/pkg/database"
)

// exampleTrackerDefinition describes testdata/example-tracker.html. It
// uses the same Cardigann features a real definition does — a row filter,
// an optional field, a case arm, and per-field filters — so the fixture
// exercises the extraction path end to end rather than a reduced version
// of it.
const exampleTrackerDefinition = `
id: example-tracker
name: Example Tracker
links:
  - %s/
search:
  paths:
    - path: browse.php
      method: get
  rows:
    selector: tr.row
    after: 1
    remove: .promoted
  fields:
    title:
      selector: td.name a
    details:
      selector: td.name a
      attribute: href
    download:
      selector: td.dl a
      attribute: href
    size:
      selector: td.size
    seeders:
      selector: td.seeds
    leechers:
      selector: td.leech
    grabs:
      selector: td.grabs
    date:
      selector: td.added
    description:
      selector: td.nonexistent
      optional: true
    downloadvolumefactor:
      case:
        "td.flags img[src$=\"freeleech.png\"]": "0"
        "*": "1"
`

// TestScraper_ExampleTrackerFixture scrapes the checked-in fixture through
// the whole pipeline and asserts what each row produced.
func TestScraper_ExampleTrackerFixture(t *testing.T) {
	page, err := os.ReadFile("testdata/example-tracker.html")
	require.NoError(t, err)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(page)
	}))
	defer server.Close()

	dir := t.TempDir()
	def := loadTestTracker(t, dir, "example-tracker",
		fmt.Sprintf(exampleTrackerDefinition, server.URL))

	store, err := database.Open(context.Background(), ":memory:")
	require.NoError(t, err)
	defer store.Close()

	scrpr := New(store, NewConfigStore(""), "", testLogger())
	require.NoError(t, scrpr.ScrapeIndexer(context.Background(), def, SearchParams{}))

	stored, err := store.Recent(context.Background(), TrackerID(def), 100)
	require.NoError(t, err)

	read := func(t *testing.T, title string) database.Torrent {
		t.Helper()
		for _, torrent := range stored {
			if torrent.Name == title {
				return torrent
			}
		}
		require.FailNowf(t, "a stored torrent is missing", "none named %q", title)
		return database.Torrent{}
	}

	t.Run("skips the header row and the promoted row", func(t *testing.T) {
		require.Len(t, stored, 3)
		for _, torrent := range stored {
			require.NotEqual(t, "Sponsored Listing", torrent.Name,
				"rows.remove should have dropped the promoted row")
		}
	})

	t.Run("parses a raw byte count and a freeleech marker", func(t *testing.T) {
		got := read(t, "Example Release (2024) 1080p")
		require.Equal(t, int64(1932735283), got.Size)
		require.Equal(t, 120, got.Seeders)
		require.Equal(t, 8, got.Leechers)
		require.Equal(t, 340, got.Grabs)
		require.Equal(t, 0.0, got.DownloadVolumeFactor, "the freeleech row should have a zero download factor")
		require.Equal(t, server.URL+"/download.php?id=1001", got.DownloadURL)
	})

	t.Run("parses a human-readable size and no freeleech marker", func(t *testing.T) {
		got := read(t, "Sample Show S01E02")
		require.Equal(t, int64(700*1024*1024), got.Size)
		require.Equal(t, 1.0, got.DownloadVolumeFactor)
	})

	t.Run("keeps a non-ASCII title intact", func(t *testing.T) {
		got := read(t, "Тестовый Релиз (2024)")
		require.Equal(t, int64(3221225472), got.Size)
		require.Equal(t, 7, got.Seeders)
	})

	t.Run("stores the parsed release date", func(t *testing.T) {
		// The fixture's date carries no offset, so it is the tracker's own
		// wall clock: read as local time and stored normalized to UTC.
		want := time.Date(2024, time.March, 15, 0, 0, 0, 0, time.Local).UTC().Format(time.RFC3339) //nolint:gosmopolitan // asserting the local-time behaviour under test, on whatever host runs it
		require.Equal(t, want, read(t, "Example Release (2024) 1080p").Published)
	})
}
