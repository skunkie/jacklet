// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package torznab

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torrplay/jacklet/pkg/scraper"
)

// attrValue returns the named attribute's value, or "" when it was not
// emitted at all.
func attrValue(attrs []Attribute, name string) string {
	for _, attr := range attrs {
		if attr.Name == name {
			return attr.Value
		}
	}
	return ""
}

// The metadata Lidarr, Readarr and Radarr match on has to reach the feed,
// under the attribute names Jackett uses -- notably "coverurl" for the
// poster, which is not what the Cardigann field is called.
func TestAttributes_MediaMetadata(t *testing.T) {
	attrs := attributes(&scraper.Torrent{
		Name: "Some.Release", TMDBID: "12345", TVMazeID: "82", TraktID: "999",
		DoubanID: "26387939", RageID: "7", Genres: "Sci Fi, Action", Year: 2024,
		Poster: "https://tracker.invalid/img/cover.jpg",
		Author: "A. Writer", BookTitle: "The Book", Publisher: "Pub House",
		Artist: "The Band", Album: "The Album", Label: "Some Label", Track: "03",
	})

	for _, tc := range []struct{ name, want string }{
		{name: "tmdbid", want: "12345"},
		{name: "tvmazeid", want: "82"},
		{name: "traktid", want: "999"},
		{name: "doubanid", want: "26387939"},
		{name: "rageid", want: "7"},
		{name: "genre", want: "Sci Fi, Action"},
		{name: "year", want: "2024"},
		{name: "coverurl", want: "https://tracker.invalid/img/cover.jpg"},
		{name: "author", want: "A. Writer"},
		{name: "booktitle", want: "The Book"},
		{name: "publisher", want: "Pub House"},
		{name: "artist", want: "The Band"},
		{name: "album", want: "The Album"},
		{name: "label", want: "Some Label"},
		{name: "track", want: "03"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, attrValue(attrs, tc.name))
		})
	}
}

// An unscraped field is left out rather than advertised as empty, which a
// client would otherwise read as a real value.
func TestAttributes_OmitsUnscrapedMetadata(t *testing.T) {
	attrs := attributes(&scraper.Torrent{Name: "Some.Release"})
	for _, name := range []string{
		"tmdbid", "tvmazeid", "traktid", "doubanid", "rageid", "genre", "year",
		"coverurl", "author", "booktitle", "publisher", "artist", "album", "label", "track",
	} {
		for _, attr := range attrs {
			require.NotEqual(t, name, attr.Name, "%q should not be emitted when unscraped", name)
		}
	}
}

// Jackett emits the IMDb id under two names: "imdb" is the bare number,
// zero-padded, and "imdbid" the canonical "tt"-prefixed form. Clients look
// for one or the other, so a feed carrying only one loses half of them.
func TestAttributes_IMDbBothSpellings(t *testing.T) {
	for _, tc := range []struct {
		stored     string
		wantIMDB   string
		wantIMDBID string
	}{
		{stored: "1234567", wantIMDB: "1234567", wantIMDBID: "tt1234567"},
		{stored: "133093", wantIMDB: "0133093", wantIMDBID: "tt0133093"},
		// Ids have outgrown seven digits; padding never truncates.
		{stored: "10000000", wantIMDB: "10000000", wantIMDBID: "tt10000000"},
	} {
		t.Run(tc.stored, func(t *testing.T) {
			attrs := attributes(&scraper.Torrent{Name: "R", IMDBID: tc.stored})
			require.Equal(t, tc.wantIMDB, attrValue(attrs, "imdb"))
			require.Equal(t, tc.wantIMDBID, attrValue(attrs, "imdbid"))
		})
	}
}

// Nothing scraped means neither attribute, rather than Jackett's
// "tt0000000" -- a client would take that for a real id and match on it.
func TestAttributes_OmitsAbsentIMDb(t *testing.T) {
	attrs := attributes(&scraper.Torrent{Name: "R"})
	for _, attr := range attrs {
		require.NotEqual(t, "imdb", attr.Name)
		require.NotEqual(t, "imdbid", attr.Name)
	}
}

// A row filed under several categories carries each, in order, as an item
// <category> and as a "category" attribute, as Jackett writes them.
func TestToTorznabItem_EveryCategory(t *testing.T) {
	item := toTorznabItem(&scraper.Torrent{Categories: []int{2040, 2045}, Name: "Example Release"}, "https://jacklet.invalid/dl")

	require.Equal(t, []string{"2040", "2045"}, item.Categories)
	var categories []string
	for _, attr := range item.Attributes {
		if attr.Name == "category" {
			categories = append(categories, attr.Value)
		}
	}
	require.Equal(t, []string{"2040", "2045"}, categories)
}

// A JSON result names its categories as Jackett's CategoryDesc does, the
// standard names joined by ", ", and lists them, never as null.
func TestToJackettResult_Categories(t *testing.T) {
	names := map[int]string{2040: "Movies/HD", 2045: "Movies/UHD"}

	result := toJackettResult(&scraper.Torrent{Categories: []int{2040, 100011, 2045}}, "", nil, nil, names, "")
	require.Equal(t, []int{2040, 100011, 2045}, result.Category)
	require.Equal(t, "Movies/HD, Movies/UHD", result.CategoryDesc, "an id with no standard name was not skipped")

	result = toJackettResult(&scraper.Torrent{}, "", nil, nil, names, "")
	require.NotNil(t, result.Category)
	require.Empty(t, result.CategoryDesc)
}
