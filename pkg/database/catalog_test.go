// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package database

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torrplay/jacklet/pkg/admin/storetest"
	"github.com/torrplay/jacklet/pkg/scraper"
)

// Store is the store Jacklet ships, so it has to meet the same contract an
// embedding program's implementation is held to.
func TestStoreMeetsTheStoreContract(t *testing.T) {
	storetest.Run(t, func(t *testing.T) storetest.Fixture {
		t.Helper()
		store, err := Open(t.Context(), ":memory:")
		require.NoError(t, err)
		t.Cleanup(func() { store.Close() })
		return storetest.Fixture{
			Add: func(ctx context.Context, torrents []scraper.Torrent) error {
				_, err := store.UpsertAll(ctx, torrents)
				return err
			},
			AddForSearch: func(ctx context.Context, torrents []scraper.Torrent, searchKey string) error {
				_, err := store.UpsertAllForSearch(ctx, torrents, searchKey)
				return err
			},
			Store: store,
		}
	})
}
