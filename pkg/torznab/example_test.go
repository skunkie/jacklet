// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package torznab_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"

	"github.com/torrplay/jacklet/pkg/database"
	"github.com/torrplay/jacklet/pkg/scraper"
	"github.com/torrplay/jacklet/pkg/torznab"
)

// ExampleNew wires a Scraper and a Torznab handler together into an
// HTTP server exposing a single indexer, the same way cmd/jacklet does.
func ExampleNew() {
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
	trackerSite := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, html)
	}))
	defer trackerSite.Close()

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
`, trackerSite.URL)
	if err := os.WriteFile(dir+"/example-tracker.yml", []byte(def), 0o600); err != nil {
		fmt.Println(err)
		return
	}

	logger := slog.New(slog.DiscardHandler)
	db, err := database.Open(context.Background(), ":memory:")
	if err != nil {
		fmt.Println(err)
		return
	}
	defer db.Close()

	scrpr := scraper.New(db, scraper.NewConfigStore(""), "", logger)
	handler := torznab.New(db, scrpr, scraper.NewDefinitionStore(dir, logger), "", logger)

	mux := http.NewServeMux()
	handler.Routes(mux)

	api := httptest.NewServer(mux)
	defer api.Close()

	resp, err := http.Get(api.URL + "/api/v2.0/indexers/example-tracker/results?q=ubuntu")
	if err != nil {
		fmt.Println(err)
		return
	}
	defer resp.Body.Close()

	var results struct {
		Results []struct {
			Title string `json:"Title"`
		} `json:"Results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(len(results.Results), results.Results[0].Title)

	// Output: 1 Ubuntu 24.04 ISO
}
