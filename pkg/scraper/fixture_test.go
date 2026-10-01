// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
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

// TestScraperExampleTrackerFixture scrapes the checked-in fixture through
// the whole pipeline and asserts what each row produced.
func TestScraperExampleTrackerFixture(t *testing.T) {
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

	store := &fakeStore{}

	scrpr := NewWithOptions(NewConfigStore(""), "", slog.New(slog.DiscardHandler), Options{Sink: store})
	require.NoError(t, scrpr.scrapeIndexer(t.Context(), def, SearchParams{}))

	stored, err := store.Recent(t.Context(), TrackerID(def), 100)
	require.NoError(t, err)

	read := func(t *testing.T, title string) Torrent {
		t.Helper()
		for _, torrent := range stored {
			if torrent.Name == title {
				return torrent
			}
		}
		require.FailNowf(t, "a stored torrent is missing", "none named %q", title)
		return Torrent{}
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

	store := &fakeStore{}

	return NewWithOptions(NewConfigStore(""), "", slog.New(slog.DiscardHandler), Options{Sink: store}), tracker
}

// storedTorrents reads back everything a scrape stored, for the single
// tracker these tests use.
func storedTorrents(t *testing.T, s *Scraper, def *Tracker) []Torrent {
	t.Helper()
	torrents, err := s.sink.(*fakeStore).Recent(t.Context(), TrackerID(def), 100)
	require.NoError(t, err)
	return torrents
}

// storedTorrent reads back one stored torrent by its exact title.
func storedTorrent(t *testing.T, s *Scraper, def *Tracker, name string) Torrent {
	t.Helper()
	stored := storedTorrents(t, s, def)
	for i := range stored {
		if stored[i].Name == name {
			return stored[i]
		}
	}
	require.FailNowf(t, "a stored torrent is missing", "none named %q", name)
	return Torrent{}
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
