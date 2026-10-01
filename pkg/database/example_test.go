// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package database_test

import (
	"context"
	"fmt"

	"github.com/torrplay/jacklet/pkg/database"
	"github.com/torrplay/jacklet/pkg/scraper"
)

func ExampleOpen() {
	// A file path works too; ":memory:" keeps this example self-contained.
	store, err := database.Open(context.Background(), ":memory:")
	if err != nil {
		fmt.Println(err)
		return
	}
	defer store.Close()

	ctx := context.Background()
	if err := store.Upsert(ctx, scraper.Torrent{
		InfoHash:  "abc123",
		Name:      "Example Release",
		Published: "2024-03-05T14:30:00Z",
		Seeders:   12,
		Size:      1 << 30,
		Tracker:   "example",
	}); err != nil {
		fmt.Println(err)
		return
	}

	torrents, total, err := store.Search(ctx, scraper.Query{
		Limit:    10,
		Terms:    []string{"example"},
		Trackers: []string{"example"},
	})
	if err != nil {
		fmt.Println(err)
		return
	}

	fmt.Println(total, torrents[0].Name, torrents[0].Seeders)

	// Output: 1 Example Release 12
}
