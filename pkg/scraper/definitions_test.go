// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSearchModes(t *testing.T) {
	t.Run("uses the definition's declared modes", func(t *testing.T) {
		def := &Tracker{Caps: Caps{Modes: map[string][]string{
			"search":      {"q"},
			"book-search": {"q", "author"},
		}}}
		modes := SearchModes(def)
		require.Equal(t, []string{"q", "author"}, modes["book-search"])
		require.NotContains(t, modes, "tv-search")
		modes["book-search"][0] = "mutated"
		require.Equal(t, []string{"q", "author"}, def.Caps.Modes["book-search"])
	})

	t.Run("falls back when no modes are declared", func(t *testing.T) {
		modes := SearchModes(&Tracker{})
		require.Equal(t, defaultSearchModes, modes)
		modes["search"][0] = "mutated"
		delete(modes, "tv-search")
		require.Equal(t, []string{"q"}, SearchModes(&Tracker{})["search"])
		require.Contains(t, SearchModes(&Tracker{}), "tv-search")
	})
}

// A setting with no value must render as an empty string, not as the text
// Go's default formatting gives a nil interface.
func TestMergedConfig_NilValues(t *testing.T) {
	def := &Tracker{Settings: []Setting{
		{Name: "password", Type: "password"},
		{Default: "added", Name: "sort", Type: "select"},
	}}

	t.Run("a setting with no default", func(t *testing.T) {
		require.Empty(t, defaultConfig(def)["password"])
	})

	t.Run("an override with no value", func(t *testing.T) {
		cfg := mergedConfig(def, map[string]any{"password": nil, "sort": nil})
		require.Empty(t, cfg["password"])
		require.Empty(t, cfg["sort"])
	})
}

func TestIsCheckboxChecked(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
		want  bool
	}{
		{name: "true boolean", value: true, want: true},
		{name: "false boolean", value: false, want: false},
		{name: "cardigann true", value: "True", want: true},
		{name: "lowercase true", value: "true", want: true},
		{name: "cardigann false", value: "", want: false},
		{name: "false string", value: "False", want: false},
		{name: "not a boolean", value: "yes please", want: false},
		{name: "a number", value: 1, want: false},
		{name: "absent", value: nil, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, IsCheckboxChecked(tc.value))
		})
	}
}

// A checkbox override written by hand as a string reaches the template as
// Cardigann's own values, as a boolean override does.
func TestMergedConfig_CheckboxStrings(t *testing.T) {
	def := &Tracker{Settings: []Setting{{Default: false, Name: "freeleech", Type: "checkbox"}}}
	require.Equal(t, cardigannTrue, mergedConfig(def, map[string]any{"freeleech": "True"})["freeleech"])
	require.Equal(t, cardigannTrue, mergedConfig(def, map[string]any{"freeleech": "true"})["freeleech"])
	require.Equal(t, cardigannFalse, mergedConfig(def, map[string]any{"freeleech": "False"})["freeleech"])
	require.Equal(t, cardigannFalse, mergedConfig(def, nil)["freeleech"])
}
