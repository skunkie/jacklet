// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package jacklet_test

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"

	"github.com/torrplay/jacklet/pkg/admin"
	"github.com/torrplay/jacklet/pkg/database"
	"github.com/torrplay/jacklet/pkg/jacklet"
	"github.com/torrplay/jacklet/pkg/scraper"
)

// Example builds the whole stack over one SQLite store: the scraper writes
// into it, and the Torznab API and the admin panel read from it.
func Example() {
	ctx := context.Background()
	logger := slog.New(slog.DiscardHandler)

	dir, err := os.MkdirTemp("", "jacklet-embedding")
	if err != nil {
		fmt.Println(err)
		return
	}
	defer os.RemoveAll(dir)

	store, err := database.Open(ctx, ":memory:")
	if err != nil {
		fmt.Println(err)
		return
	}
	defer store.Close()

	password, err := admin.NewPassword("a-long-admin-password", "")
	if err != nil {
		fmt.Println(err)
		return
	}

	// One store and one settings source go in, and every part comes out
	// wired to them.
	stack, err := jacklet.New(store, scraper.NewConfigStore(dir), scraper.NewDefinitionStore(dir, logger), logger, jacklet.Options{
		APIKey:        "an-api-key",
		AdminPassword: password,
	})
	if err != nil {
		fmt.Println(err)
		return
	}
	defer stack.Close(ctx)

	handler, err := stack.Handler()
	if err != nil {
		fmt.Println(err)
		return
	}

	for _, target := range []string{"/api/v2.0/indexers?apikey=an-api-key", admin.DefaultPrefix + "/login"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, http.NoBody))
		fmt.Println(target, rec.Code)
	}

	// Output:
	// /api/v2.0/indexers?apikey=an-api-key 200
	// /admin/login 200
}
