// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package torznab

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torrplay/jacklet/pkg/scraper"
)

func TestInteractiveSearch(t *testing.T) {
	for _, tc := range []struct {
		name      string
		in        scraper.SearchParams
		want      scraper.SearchParams
		wantTerms []string
	}{
		{
			name: "a plain query is a plain search",
			in:   scraper.SearchParams{Query: "  Sample Show  ", Type: "tvsearch"},
			want: scraper.SearchParams{Query: "Sample Show", Type: "search"},
		},
		{
			name:      "a trailing season and episode become the search's",
			in:        scraper.SearchParams{Query: "Sample Show S01E02"},
			want:      scraper.SearchParams{Ep: "2", Query: "Sample Show", Season: "1", Type: "search"},
			wantTerms: []string{"S01E02"},
		},
		{
			name:      "a long season and a lettered episode are read whole",
			in:        scraper.SearchParams{Query: "Sample Show S2024E105a"},
			want:      scraper.SearchParams{Ep: "105a", Query: "Sample Show", Season: "2024", Type: "search"},
			wantTerms: []string{"S2024E105a"},
		},
		{
			name:      "an episode alone is read",
			in:        scraper.SearchParams{Query: "Sample Show E05"},
			want:      scraper.SearchParams{Ep: "5", Query: "Sample Show", Type: "search"},
			wantTerms: []string{"E05"},
		},
		{
			name:      "a season alone is read",
			in:        scraper.SearchParams{Query: "Sample Show S03"},
			want:      scraper.SearchParams{Query: "Sample Show", Season: "3", Type: "search"},
			wantTerms: []string{"S03"},
		},
		{
			// The episode is read first, which leaves the season trailing.
			name:      "a season and an episode written apart are both read",
			in:        scraper.SearchParams{Query: "Sample Show S01 E02"},
			want:      scraper.SearchParams{Ep: "2", Query: "Sample Show", Season: "1", Type: "search"},
			wantTerms: []string{"E02", "S01"},
		},
		{
			name:      "an episode of zeros is no episode",
			in:        scraper.SearchParams{Query: "Sample Show S01E00"},
			want:      scraper.SearchParams{Query: "Sample Show", Season: "1", Type: "search"},
			wantTerms: []string{"S01E00"},
		},
		{
			name: "an episode run into the title is left in it",
			in:   scraper.SearchParams{Query: "Sample ShowS01E02"},
			want: scraper.SearchParams{Query: "Sample ShowS01E02", Type: "search"},
		},
		{
			// .NET's word boundary counts Cyrillic letters as word
			// characters, so there is no boundary before the S.
			name: "an episode run into a non-latin title is left in it",
			in:   scraper.SearchParams{Query: "ТестовыйS01E02"},
			want: scraper.SearchParams{Query: "ТестовыйS01E02", Type: "search"},
		},
		{
			name: "an episode after a zero-width joiner is left in the query",
			in:   scraper.SearchParams{Query: "Тестовый\u200dS01E02"},
			want: scraper.SearchParams{Query: "Тестовый\u200dS01E02", Type: "search"},
		},
		{
			name:      "an episode after a non-latin title is read",
			in:        scraper.SearchParams{Query: "Тестовый Релиз S01E02"},
			want:      scraper.SearchParams{Ep: "2", Query: "Тестовый Релиз", Season: "1", Type: "search"},
			wantTerms: []string{"S01E02"},
		},
		{
			name: "a lowercase episode is left in the query",
			in:   scraper.SearchParams{Query: "Sample Show s01e02"},
			want: scraper.SearchParams{Query: "Sample Show s01e02", Type: "search"},
		},
		{
			name: "an episode given apart completes the season the query wrote",
			in:   scraper.SearchParams{Ep: "2", Query: "Sample Show S01"},
			want: scraper.SearchParams{Ep: "2", Query: "Sample Show", Season: "1", Type: "search"},
		},
		{
			name: "a season given apart completes the episode the query wrote",
			in:   scraper.SearchParams{Query: "Sample Show E02", Season: "3"},
			want: scraper.SearchParams{Ep: "2", Query: "Sample Show", Season: "3", Type: "search"},
		},
		{
			name:      "a season of zero given apart leaves the episode the query wrote",
			in:        scraper.SearchParams{Query: "Sample Show E02", Season: "0"},
			want:      scraper.SearchParams{Ep: "2", Query: "Sample Show", Season: "0", Type: "search"},
			wantTerms: []string{"E02"},
		},
		{
			name: "a season given apart stays when the query names none",
			in:   scraper.SearchParams{Query: "Sample Show", Season: "4"},
			want: scraper.SearchParams{Query: "Sample Show", Season: "4", Type: "search"},
		},
		{
			name: "an imdb id is an imdb lookup with no mode",
			in: scraper.SearchParams{
				Categories: []string{"5000"}, Limit: 1000, Query: "tt0123456", TVDBID: "77", Title: "Sample", Type: "search",
			},
			want: scraper.SearchParams{Categories: []string{"5000"}, IMDBID: "tt0123456", Limit: 1000},
		},
		{
			name: "a short imdb id is padded to seven digits",
			in:   scraper.SearchParams{Query: "tt123"},
			want: scraper.SearchParams{IMDBID: "tt0000123"},
		},
		{
			name:      "an imdb id keeps the episode written after it",
			in:        scraper.SearchParams{Query: "tt0123456 S01E02"},
			want:      scraper.SearchParams{Ep: "2", IMDBID: "tt0123456", Season: "1"},
			wantTerms: []string{"S01E02"},
		},
		{
			name: "an imdb id of zero is text",
			in:   scraper.SearchParams{Query: "tt0"},
			want: scraper.SearchParams{Query: "tt0", Type: "search"},
		},
		{
			name: "a query past ten characters is text",
			in:   scraper.SearchParams{Query: "tt012345678"},
			want: scraper.SearchParams{Query: "tt012345678", Type: "search"},
		},
		{
			name: "an uppercase prefix is text",
			in:   scraper.SearchParams{Query: "TT0123456"},
			want: scraper.SearchParams{Query: "TT0123456", Type: "search"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, terms := interactiveSearch(tc.in)
			require.Equal(t, tc.want, got)
			require.Equal(t, tc.wantTerms, terms,
				"the stored rows no search produced are matched on the episode as the query wrote it, and never on an IMDb id")
		})
	}
}
