// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"

	"github.com/torrplay/jacklet/pkg/database"
	"github.com/torrplay/jacklet/pkg/scraper"
)

func ExampleDefinitionStore_Trackers() {
	dir, err := os.MkdirTemp("", "jacklet-example")
	if err != nil {
		fmt.Println(err)
		return
	}
	defer os.RemoveAll(dir)

	const def = `
id: example-tracker
name: Example Tracker
`
	if err := os.WriteFile(dir+"/example-tracker.yml", []byte(def), 0o600); err != nil {
		fmt.Println(err)
		return
	}

	store := scraper.NewDefinitionStore(dir, slog.New(slog.DiscardHandler))

	trackers, defErrs, err := store.Trackers()
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(len(trackers), "trackers,", len(defErrs), "errors")
	fmt.Println(trackers[0].Name)

	// Output:
	// 1 trackers, 0 errors
	// Example Tracker
}

// ExampleScraper_ScrapeIndexer scrapes a single indexer and stores its
// results, the same pipeline Jacklet's HTTP handlers run on every search.
func ExampleScraper_ScrapeIndexer() {
	const html = `
		<table>
			<tr class="torrent_row">
				<td><a href="/download/123">Ubuntu 24.04 ISO</a></td>
				<td>4.5 GB</td>
				<td>100</td>
				<td>10</td>
				<td>2024-01-01</td>
			</tr>
		</table>
	`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, html)
	}))
	defer server.Close()

	dir, err := os.MkdirTemp("", "jacklet-example")
	if err != nil {
		fmt.Println(err)
		return
	}
	defer os.RemoveAll(dir)

	def := fmt.Sprintf(`
id: example-tracker
name: Example Tracker
links:
  - %s/
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
    size:
      selector: "td:nth-child(2)"
    seeders:
      selector: "td:nth-child(3)"
    leechers:
      selector: "td:nth-child(4)"
    date:
      selector: "td:nth-child(5)"
`, server.URL)
	if err := os.WriteFile(dir+"/example-tracker.yml", []byte(def), 0o600); err != nil {
		fmt.Println(err)
		return
	}

	logger := slog.New(slog.DiscardHandler)
	tracker, err := scraper.NewDefinitionStore(dir, logger).Find("example-tracker")
	if err != nil {
		fmt.Println(err)
		return
	}

	store, err := database.Open(context.Background(), ":memory:")
	if err != nil {
		fmt.Println(err)
		return
	}
	defer store.Close()

	s := scraper.New(store, scraper.NewConfigStore(""), "", logger)
	if err := s.ScrapeIndexer(context.Background(), tracker, scraper.SearchParams{Query: "ubuntu"}); err != nil {
		fmt.Println(err)
		return
	}

	stored, err := store.Recent(context.Background(), scraper.TrackerID(tracker), 1)
	if err != nil || len(stored) == 0 {
		fmt.Println("nothing was stored:", err)
		return
	}
	fmt.Println(stored[0].Name, stored[0].Seeders)

	// Output: Ubuntu 24.04 ISO 100
}

// memoryTrackerSource serves definitions held in memory rather than read
// from a directory of YAML files.
type memoryTrackerSource struct {
	defs []scraper.Tracker
}

func (m memoryTrackerSource) Find(id string) (*scraper.Tracker, error) {
	for i := range m.defs {
		if scraper.TrackerID(&m.defs[i]) == id {
			found := m.defs[i]
			return &found, nil
		}
	}
	return nil, fmt.Errorf("%w: %q", scraper.ErrTrackerNotFound, id)
}

func (m memoryTrackerSource) Trackers() ([]scraper.Tracker, []scraper.DefinitionError, error) {
	return m.defs, nil, nil
}

// A program embedding this package can supply definitions from anywhere by
// implementing TrackerSource, instead of using the directory-backed
// DefinitionStore.
func ExampleTrackerSource() {
	var source scraper.TrackerSource = memoryTrackerSource{
		defs: []scraper.Tracker{{ID: "in-memory", Name: "In-Memory Tracker"}},
	}

	def, err := source.Find("in-memory")
	if err != nil {
		panic(err)
	}
	fmt.Println(def.Name)

	_, err = source.Find("absent")
	fmt.Println(errors.Is(err, scraper.ErrTrackerNotFound))

	// Output:
	// In-Memory Tracker
	// true
}
