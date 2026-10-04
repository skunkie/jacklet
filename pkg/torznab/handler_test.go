// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package torznab_test

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torrplay/jacklet/pkg/scraper"
	"github.com/torrplay/jacklet/pkg/torznab"
)

// Handler must serve the same endpoints as Routes, at the same paths, so a
// program with its own router can mount the API without a *http.ServeMux.
func TestTorznab_Handler_ServesTheRoutes(t *testing.T) {
	dir := t.TempDir()
	newIndexerDef(t, dir)
	logger := slog.New(slog.DiscardHandler)
	handler := torznab.New(&fakeCatalog{}, scraper.New(scraper.NewConfigStore(""), "", logger), scraper.NewDefinitionStore(dir, logger), "", logger).Handler()

	for _, target := range []string{
		"/api/v2.0/indexers",
		"/api/v2.0/indexers/" + testIndexerID + "/results/torznab/api?t=caps",
		"/api/v2.0/indexers/" + testIndexerID + "/results?q=sample",
	} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, http.NoBody))
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", target, rec.Body.String())
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/elsewhere", http.NoBody))
	require.Equal(t, http.StatusNotFound, rec.Code, "the handler answered a path outside the API")
}
