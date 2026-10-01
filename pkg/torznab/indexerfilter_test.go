// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package torznab

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/torrplay/jacklet/pkg/scraper"
)

// TestIsIndexerFilter covers what separates a filter expression from a
// definition's own id, which is the only thing standing between a client
// asking for "status:healthy" and a 404.
func TestIsIndexerFilter(t *testing.T) {
	require.True(t, isIndexerFilter("status:healthy"))
	require.True(t, isIndexerFilter("!type:private+lang:en"))
	require.False(t, isIndexerFilter("example-tracker"))
	require.False(t, isIndexerFilter(AggregateID))
}

// TestParseIndexerFilter covers each filter name and each operator against
// the same two definitions, so a term's meaning and the way terms combine
// are both pinned.
func TestParseIndexerFilter(t *testing.T) {
	public := &scraper.Tracker{ID: "public-tracker", Language: "en-US", Type: "public"}
	private := &scraper.Tracker{ID: "private-tracker", Language: "de-DE", Type: "private"}

	healthy := scraper.IndexerStatus{Scraped: true}
	failing := scraper.IndexerStatus{Failures: 3, Scraped: true}
	untouched := scraper.IndexerStatus{}

	for _, tc := range []struct {
		def        *scraper.Tracker
		expression string
		status     scraper.IndexerStatus
		want       bool
	}{
		{public, "type:public", healthy, true},
		{public, "type:private", healthy, false},
		{public, "type:PUBLIC", healthy, true},
		{public, "lang:en", healthy, true},
		{private, "lang:en", healthy, false},
		{private, "lang:de-DE", healthy, true},
		{public, "status:healthy", healthy, true},
		{public, "status:healthy", failing, false},
		{public, "status:failing", failing, true},
		{public, "status:unknown", untouched, true},
		{public, "status:unknown", healthy, false},
		{public, "test:passed", healthy, true},
		{public, "test:failed", failing, true},
		{public, "tag:anything", healthy, false},
		{public, "!type:private", healthy, true},
		{public, "!type:public", healthy, false},
		{public, "type:public+lang:en", healthy, true},
		{public, "type:public+lang:de", healthy, false},
		{public, "type:private,lang:en", healthy, true},
		{public, "type:private,lang:fr", healthy, false},
		{public, "!type:private+status:healthy", healthy, true},
		{public, "!type:private+status:healthy", failing, false},
	} {
		match, err := parseIndexerFilter(tc.expression)
		require.NoError(t, err, "parseIndexerFilter(%q)", tc.expression)
		require.Equal(t, tc.want, match(tc.def, tc.status), "%q against %s", tc.expression, tc.def.ID)
	}
}

// TestParseIndexerFilter_Unsupported covers the expressions that must be
// refused rather than silently matching nothing: a client is better told
// its filter is not served than handed an empty search it reads as "no
// indexer is healthy".
func TestParseIndexerFilter_Unsupported(t *testing.T) {
	for _, expression := range []string{
		"",
		"healthy",
		"kind:public",
		"status:excellent",
		"test:pending",
		"type:public+nonsense",
	} {
		_, err := parseIndexerFilter(expression)
		require.ErrorIs(t, err, errUnsupportedFilter, "parseIndexerFilter(%q)", expression)
	}
}

// TestFilterIndexerSelectsMatchingDefinitions covers the indexer a filter
// resolves to: only the definitions it covers, marked as a meta indexer so
// an expression matching nothing is reported as nothing searched rather
// than as an empty result.
func TestFilterIndexerSelectsMatchingDefinitions(t *testing.T) {
	tz := &Torznab{scrpr: &scraper.Scraper{}}
	defs := []scraper.Tracker{
		{ID: "public-tracker", Type: "public"},
		{ID: "private-tracker", Type: "private"},
	}

	match, err := parseIndexerFilter("type:public")
	require.NoError(t, err)
	idx := tz.filterIndexer(defs, "type:public", match)

	require.True(t, idx.IsMeta)
	require.Equal(t, "type:public", idx.ID)
	require.Equal(t, []string{"public-tracker"}, idx.ids())
	require.False(t, idx.IsUnconfigured)

	match, err = parseIndexerFilter("type:semi-private")
	require.NoError(t, err)
	unmatched := tz.filterIndexer(defs, "type:semi-private", match)
	require.Empty(t, unmatched.Defs, "no definition is of that type")
	require.False(t, unmatched.IsUnconfigured,
		"a filter that matched nothing is an empty answer, not a broken install")

	// The same expression against an install where nothing loaded covers
	// nothing for a different reason, and says so. It is the definitions
	// handed in rather than the ones matched that tells the two apart.
	require.True(t, tz.filterIndexer(nil, "type:public", match).IsUnconfigured,
		"a filter over no definitions at all is a broken install, not an empty answer")
}
