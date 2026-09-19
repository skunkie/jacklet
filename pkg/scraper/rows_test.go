// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"
	"github.com/stretchr/testify/require"
)

func TestJSONRowsAndScalarValues(t *testing.T) {
	document := map[string]any{
		"items": []any{
			map[string]any{"name": "Example One"},
			map[string]any{"name": "Example Two"},
		},
	}
	rows := jsonRows(document, "items")
	require.Len(t, rows, 2)
	require.True(t, rows[0].matches("name"), "a present key reported as missing")
	require.False(t, rows[0].matches("missing"), "a missing key reported as present")

	values := []struct {
		value any
		want  string
	}{
		{value: true, want: "true"},
		{value: float64(12.5), want: "12.5"},
		{value: nil, want: ""},
	}
	for _, tc := range values {
		require.Equal(t, tc.want, jsonScalar(tc.value), "jsonScalar(%#v)", tc.value)
	}
}

func TestRowsLookupVariants(t *testing.T) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(`<div class="row"><a href="/item">Example</a><span>Hidden</span></div>`))
	require.NoError(t, err)

	row := htmlRow{sel: doc.Find(".row")}
	require.True(t, row.matches("a"), "a descendant selector reported as unmatched")
	require.True(t, row.matches(".row"), "the row's own selector reported as unmatched")
	require.False(t, row.matches("missing"), "a missing selector reported as matched")

	got, ok := row.lookup(Field{Attribute: "href", Selector: "a"})
	require.True(t, ok, "attribute lookup reported as unmatched")
	require.Equal(t, "/item", got)

	got, ok = row.lookup(Field{Remove: "span"})
	require.True(t, ok, "remove lookup reported as unmatched")
	require.Equal(t, "Example", strings.TrimSpace(got))

	_, ok = row.lookup(Field{Selector: "missing"})
	require.False(t, ok, "missing HTML selector reported as matched")

	json := jsonRow{value: map[string]any{
		"items": []any{map[string]any{"name": "Example"}},
	}}
	got, ok = json.lookup(Field{Attribute: "name", Selector: "items[0]"})
	require.True(t, ok, "JSON attribute lookup reported as unmatched")
	require.Equal(t, "Example", got)

	got, ok = json.lookup(Field{})
	require.True(t, ok, "JSON root lookup reported as unmatched")
	require.NotEmpty(t, got)
}
