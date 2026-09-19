// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package database

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// The media metadata columns have to survive a write and a read: a field
// missing from either the upsert or the scan would silently store nothing
// while every other layer looked correct.
func TestTorrentMetadataRoundTrip(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "metadata.db"))
	require.NoError(t, err)
	defer store.Close()

	want := Torrent{
		Name: "Some.Release", Tracker: "t", InfoHash: "abc123",
		TMDBID: "12345", TVMazeID: "82", TraktID: "999", DoubanID: "26387939",
		RageID: "7", Genres: "Sci Fi, Action", Year: 2024,
		Poster: "https://tracker.invalid/img/cover.jpg",
		Author: "A. Writer", BookTitle: "The Book", Publisher: "Pub House",
		Artist: "The Band", Album: "The Album", Label: "Some Label", Track: "03",
	}
	require.NoError(t, store.Upsert(ctx, want))

	got, _, err := store.Search(ctx, Search{Limit: 10})
	require.NoError(t, err)
	require.Len(t, got, 1)

	stored := got[0]
	require.Equal(t, want.TMDBID, stored.TMDBID)
	require.Equal(t, want.TVMazeID, stored.TVMazeID)
	require.Equal(t, want.TraktID, stored.TraktID)
	require.Equal(t, want.DoubanID, stored.DoubanID)
	require.Equal(t, want.RageID, stored.RageID)
	require.Equal(t, want.Genres, stored.Genres)
	require.Equal(t, want.Year, stored.Year)
	require.Equal(t, want.Poster, stored.Poster)
	require.Equal(t, want.Author, stored.Author)
	require.Equal(t, want.BookTitle, stored.BookTitle)
	require.Equal(t, want.Publisher, stored.Publisher)
	require.Equal(t, want.Artist, stored.Artist)
	require.Equal(t, want.Album, stored.Album)
	require.Equal(t, want.Label, stored.Label)
	require.Equal(t, want.Track, stored.Track)
}

// A re-scrape refreshes the metadata rather than keeping the first-seen
// values, as it does for every other column.
func TestTorrentMetadataRefreshedOnRescrape(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "refresh.db"))
	require.NoError(t, err)
	defer store.Close()

	base := Torrent{Name: "R", Tracker: "t", InfoHash: "abc123", Artist: "Old", Year: 2000}
	require.NoError(t, store.Upsert(ctx, base))

	base.Artist, base.Year = "New", 2024
	require.NoError(t, store.Upsert(ctx, base))

	got, _, err := store.Search(ctx, Search{Limit: 10})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "New", got[0].Artist)
	require.Equal(t, 2024, got[0].Year)
}
