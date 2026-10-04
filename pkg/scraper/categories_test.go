// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"errors"
	"strconv"
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
func TestMapCategories_SubcategoryNames(t *testing.T) {
	def := &Tracker{Caps: Caps{CategoryMappings: []CategoryMapping{
		{Cat: "Movies/HD", ID: "11"},
		{Cat: "TV/Anime", ID: "22"},
		{Cat: "PC/Games", ID: "33"},
		{Cat: "Nonsense/Unknown", ID: "44"},
		// Jackett spells the console names "XBox", and matches a name
		// exactly.
		{Cat: "Console/XBox 360", ID: "55"},
	}}}

	require.Equal(t, []int{2040}, mapCategories(def, "11"))
	require.Equal(t, []int{5070}, mapCategories(def, "22"))
	require.Equal(t, []int{4050}, mapCategories(def, "33"))
	require.Empty(t, mapCategories(def, "44"), "a mapping naming no standard category was not skipped")
	require.Equal(t, []int{1050}, mapCategories(def, "55"))
}

// Cardigann's older "categories" map, a site id to a standard category
// name, counts as category mappings, ahead of any "categorymappings" list,
// as Jackett adds them: caps advertise it, rows map through it, and a
// search sends its site ids.
func TestCaps_UnmarshalYAML(t *testing.T) {
	def := loadTestTracker(t, t.TempDir(), "old-categories", `
id: old-categories
name: Old Categories
links:
  - https://example.invalid/
caps:
  categories:
    7: Movies/HD
    tv: TV
  categorymappings:
    - {id: 7, cat: Audio}
    - {id: 9, cat: Books}
search:
  paths:
    - path: /
  rows:
    selector: tr
  fields:
    title:
      selector: a
`)

	require.Equal(t, []CategoryMapping{
		{Cat: "Movies/HD", ID: "7"},
		{Cat: "TV", ID: "tv"},
		{Cat: "Audio", ID: "7"},
		{Cat: "Books", ID: "9"},
	}, def.Caps.CategoryMappings)
	require.Equal(t, []int{2040, 3000}, mapCategories(def, "7"), "the map's entry did not come first")
	require.Equal(t, []int{5000}, mapCategories(def, "tv"))
	require.Equal(t, []string{"tv"}, siteCategoryIDs(def, []string{"5000"}))
	require.Contains(t, SupportedCategories(def), CategoryInfo{ID: 2040, Name: "Movies/HD"})
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
	require.Equal(t, []int{5000}, mapCategories(&def, "tv"))
	require.Equal(t, []int{2040}, mapCategories(&def, "48"))
	// A row's scraped category is matched as text, whitespace trimmed and
	// case ignored, as Jackett matches it.
	require.Equal(t, []int{5070}, mapCategories(&def, " 22 "))
	require.Equal(t, []int{5000}, mapCategories(&def, "TV"))
	require.Empty(t, mapCategories(&def, "nosuch"))

	// The same opaque ids are what a search's ".Categories" carries. A
	// request for the TV parent selects the TV/Anime mapping as well.
	require.Equal(t, []string{"22", "tv"}, siteCategoryIDs(&def, []string{"5000"}))
}

// A tracker category mapped to several standard categories is filed under
// each, once, in the order its mappings are written, a described mapping's
// custom category after its standard one, as Jackett files it; a row's
// "categorydesc" is matched against the mappings' descriptions the same
// way.
func TestMapCategories_EveryMapping(t *testing.T) {
	def := &Tracker{Caps: Caps{CategoryMappings: []CategoryMapping{
		{Cat: "Movies/HD", Desc: "Films HD", ID: "1"},
		{Cat: "Movies/UHD", Desc: "Films HD", ID: "1"},
		{Cat: "Movies/HD", Desc: "Films", ID: "1"},
		{Cat: "TV", Desc: "Series", ID: "2"},
	}}}

	require.Equal(t, []int{2040, 100001, 2045}, mapCategories(def, "1"))
	require.Equal(t, []int{2040, 100001, 2045}, mapCategoryDescs(def, "films hd"))
	require.Equal(t, []int{5000, 100002}, mapCategoryDescs(def, " Series "))
	require.Empty(t, mapCategoryDescs(def, ""), "an empty description matched a mapping")
	require.Empty(t, mapCategoryDescs(def, "2"), "a description matched a mapping's id")
}

// A described mapping's custom category is numbered as Jackett numbers it:
// past 100000 by the tracker's id when that is a 32-bit integer, and
// otherwise by the first two bytes of the id's SHA-1, little-endian, so a
// client configured against Jackett asks for the same number here.
func TestCategoryMapping_CustomCategoryID(t *testing.T) {
	for _, tc := range []struct {
		name   string
		id     CategoryID
		wantID int
		wantOK bool
	}{
		{name: "an integer id", id: "925", wantID: 100925, wantOK: true},
		{name: "a signed integer id", id: "+7", wantID: 100007, wantOK: true},
		{name: "a name", id: "tv", wantID: 103583, wantOK: true},
		{name: "a hyphenated name", id: "movies-hd", wantID: 112136, wantOK: true},
		{name: "an integer past 32 bits", id: "3000000000", wantID: 112409, wantOK: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := (&CategoryMapping{Cat: "TV", Desc: "Sample", ID: tc.id}).customCategoryID()
			require.Equal(t, tc.wantOK, ok)
			require.Equal(t, tc.wantID, got)
		})
	}

	_, ok := (&CategoryMapping{Cat: "TV", ID: "5"}).customCategoryID()
	require.False(t, ok, "a mapping without a description made a custom category")
}

// A tracker's custom categories are listed by name, each id once under its
// first description, and only for a mapping Jackett keeps: one naming a
// standard category and carrying a description.
func TestCustomCategories(t *testing.T) {
	def := &Tracker{Caps: Caps{CategoryMappings: []CategoryMapping{
		{Cat: "TV", Desc: "Sample Series", ID: "2"},
		{Cat: "Movies/HD", Desc: "Sample Films", ID: "1"},
		{Cat: "Movies/UHD", Desc: "Sample Films Again", ID: "1"},
		{Cat: "Nonsense/Unknown", Desc: "Sample Lost", ID: "3"},
		{Cat: "Audio", ID: "4"},
	}}}

	require.Equal(t, []CategoryInfo{
		{ID: 100001, Name: "Sample Films"},
		{ID: 100002, Name: "Sample Series"},
	}, CustomCategories(def))
}

// A search for a tracker's custom category asks the tracker for the
// category that made it, beside whatever a standard category selects.
func TestSiteCategoryIDs_CustomCategory(t *testing.T) {
	def := &Tracker{Caps: Caps{CategoryMappings: []CategoryMapping{
		{Cat: "Movies/HD", Desc: "Sample Films", ID: "1"},
		{Cat: "Movies/HD", Desc: "Sample Remux", ID: "tv"},
		{Cat: "TV", ID: "2"},
	}}}

	require.Equal(t, []string{"1"}, siteCategoryIDs(def, []string{"100001"}))
	require.Equal(t, []string{"tv"}, siteCategoryIDs(def, []string{"103583"}))
	require.Equal(t, []string{"1", "2"}, siteCategoryIDs(def, []string{"100001", "5000"}))
	require.Empty(t, siteCategoryIDs(def, []string{"100002"}), "a mapping without a description answered for a custom category")
}

// Custom categories are listed in Jackett's order rather than byte order:
// case is ignored, a name differing only in case comes lowercase first,
// and Cyrillic follows Latin.
func TestCustomCategories_Order(t *testing.T) {
	descs := []string{"Чай", "Banana", "арбуз", "Apple", "apple"}
	mappings := make([]CategoryMapping, len(descs))
	for i, desc := range descs {
		mappings[i] = CategoryMapping{Cat: "TV", Desc: desc, ID: CategoryID(strconv.Itoa(i))}
	}

	custom := CustomCategories(&Tracker{Caps: Caps{CategoryMappings: mappings}})
	got := make([]string, len(custom))
	for i, category := range custom {
		got[i] = category.Name
	}
	require.Equal(t, []string{"apple", "Apple", "Banana", "арбуз", "Чай"}, got)
}
