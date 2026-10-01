// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestSupportedCategories(t *testing.T) {
	def := &Tracker{Caps: Caps{CategoryMappings: []CategoryMapping{
		{Cat: "Movies/HD", ID: "hd"},
		{Cat: "Movies", ID: "movies"},
		{Cat: "Movies/HD", ID: "duplicate"},
		{Cat: "Unknown", ID: "unknown"},
	}}}

	got := SupportedCategories(def)
	require.Len(t, got, 2, "want Movies and Movies/HD, deduplicated")
	require.Equal(t, 2000, got[0].ID, "want Movies sorted first")
	require.Equal(t, 2040, got[1].ID, "want Movies/HD sorted second")
}

func TestDefinitionError_Unwrap(t *testing.T) {
	want := errors.New("invalid definition")
	got := DefinitionError{Err: want, Path: "example.yml"}
	require.ErrorIs(t, got, want, "DefinitionError does not unwrap its cause")
	require.EqualError(t, got, "example.yml: invalid definition")
}

func TestExpandCategoryIDs(t *testing.T) {
	t.Run("expands a parent into its subcategories", func(t *testing.T) {
		got := ExpandCategoryIDs([]string{"2000"})
		require.Contains(t, got, "2000")
		require.Contains(t, got, "2040") // Movies/HD
		require.Contains(t, got, "2080") // Movies/WEB-DL
		require.NotContains(t, got, "5040")
	})

	t.Run("leaves a subcategory alone", func(t *testing.T) {
		require.Equal(t, []string{"5040"}, ExpandCategoryIDs([]string{"5040"}))
	})

	t.Run("ignores a non-numeric category", func(t *testing.T) {
		require.Equal(t, []string{"junk"}, ExpandCategoryIDs([]string{"junk"}))
	})

	t.Run("returns nil for no categories", func(t *testing.T) {
		require.Nil(t, ExpandCategoryIDs(nil))
	})
}

// A Jackett definition using a subcategory name must map to that
// subcategory's ID rather than collapsing to Other.
func TestMapCategory_SubcategoryNames(t *testing.T) {
	def := &Tracker{Caps: Caps{CategoryMappings: []CategoryMapping{
		{Cat: "Movies/HD", ID: "11"},
		{Cat: "TV/Anime", ID: "22"},
		{Cat: "PC/Games", ID: "33"},
		{Cat: "Nonsense/Unknown", ID: "44"},
	}}}

	require.Equal(t, 2040, mapCategory(def, "11"))
	require.Equal(t, 5070, mapCategory(def, "22"))
	require.Equal(t, 4050, mapCategory(def, "33"))
	require.Equal(t, defaultCategoryID, mapCategory(def, "44"))
}

// Jackett's own definitions spell a site category id as a bare number, a
// quoted number, or a non-numeric name. Every one must decode, because a
// rejected id fails the whole definition file, not just the mapping.
func TestCategoryMappingsAcceptEverySpellingOfAnID(t *testing.T) {
	const definition = `
id: example
name: Example
caps:
  categorymappings:
    - {id: 48, cat: Movies/HD}
    - {id: "22", cat: TV/Anime}
    - {id: tv, cat: TV}
    - {id: 1.5, cat: PC/Games}
`

	var def Tracker
	require.NoError(t, yaml.Unmarshal([]byte(definition), &def))
	require.Equal(t, []CategoryMapping{
		{Cat: "Movies/HD", ID: "48"},
		{Cat: "TV/Anime", ID: "22"},
		{Cat: "TV", ID: "tv"},
		{Cat: "PC/Games", ID: "1.5"},
	}, def.Caps.CategoryMappings)

	// A non-numeric site category maps like any other.
	require.Equal(t, 5000, mapCategory(&def, "tv"))
	require.Equal(t, 2040, mapCategory(&def, "48"))
	// A row's scraped category is matched as text, whitespace trimmed.
	require.Equal(t, 5070, mapCategory(&def, " 22 "))
	require.Equal(t, defaultCategoryID, mapCategory(&def, "nosuch"))

	// The same opaque ids are what a search's ".Categories" carries. A
	// request for the TV parent selects the TV/Anime mapping as well.
	require.Equal(t, []string{"22", "tv"}, siteCategoryIDs(&def, []string{"5000"}))
}
