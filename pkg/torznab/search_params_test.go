// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package torznab

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCanonicalSearchType covers the normalization definitions rely on:
// ServeHTTP accepts every spelling real clients send, so a definition
// branching on ".Query.Type" must see one canonical value per mode rather
// than whichever alias the client happened to use.
func TestCanonicalSearchType(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", "search"},
		{"search", "search"},
		{"tvsearch", "tvsearch"},
		{"tv-search", "tvsearch"},
		{"movie", "movie"},
		{"moviesearch", "movie"},
		{"movie-search", "movie"},
		{"music", "music"},
		{"audio", "music"},
		{"audio-search", "music"},
		{"book", "book"},
		{"book-search", "book"},
		{"caps", "caps"},
	} {
		require.Equal(t, tc.want, canonicalSearchType(tc.in), "canonicalSearchType(%q)", tc.in)
	}
}

// TestSearchParamsFromQuery_CarriesPagingAndType checks the parameters
// that reach the scraper: paging is carried on the params as well as
// returned for the store, and the search type is normalized.
func TestSearchParamsFromQuery_CarriesPagingAndType(t *testing.T) {
	params, limit, offset := searchParamsFromQuery(url.Values{
		"t":        {"tv-search"},
		"limit":    {"25"},
		"offset":   {"50"},
		"tvmazeid": {"77"},
		"rid":      {"88"},
	}, torznabLimits)

	require.Equal(t, 25, limit, "store paging limit")
	require.Equal(t, 50, offset, "store paging offset")
	require.Equal(t, 25, params.Limit, "tracker paging limit")
	require.Equal(t, 50, params.Offset, "tracker paging offset")
	require.Equal(t, "tvsearch", params.Type)
	require.Equal(t, "77", params.TVMazeID)
	require.Equal(t, "88", params.TVRageID)

	// A limit above the advertised maximum is capped, not honored.
	capped, _, _ := searchParamsFromQuery(url.Values{"limit": {"5000"}}, torznabLimits)
	require.Equal(t, maxLimit, capped.Limit, "the limit sent to the tracker was not capped")
}

// A numeric category reaches the tracker's mappings and the store as they
// write it, however the client spelled it, so every consumer reads the
// same id; one that is not a number passes through as it is.
func TestSearchParamsFromQuery_CategorySpelledAnotherWay(t *testing.T) {
	params, _, _ := searchParamsFromQuery(url.Values{"cat": {"02000,+5000,100011,junk"}}, torznabLimits)
	require.Equal(t, []string{"2000", "5000", "100011", "junk"}, params.Categories)
}
