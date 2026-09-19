// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"sort"
	"strconv"
	"strings"
)

// standardCategoryIDs maps a Cardigann standard category name (the "cat"
// field of a categorymappings entry) to its Torznab/Newznab numeric
// category ID. It mirrors Jackett's own standard category list, so a
// definition written for Jackett maps its categories identically here
// rather than silently collapsing to Other.
var standardCategoryIDs = map[string]int{
	"Console":              1000,
	"Console/NDS":          1010,
	"Console/PSP":          1020,
	"Console/Wii":          1030,
	"Console/Xbox":         1040,
	"Console/Xbox 360":     1050,
	"Console/Wiiware":      1060,
	"Console/Xbox 360 DLC": 1070,
	"Console/PS3":          1080,
	"Console/Other":        1090,
	"Console/3DS":          1110,
	"Console/PS Vita":      1120,
	"Console/WiiU":         1130,
	"Console/Xbox One":     1140,
	"Console/PS4":          1180,

	"Movies":         2000,
	"Movies/Foreign": 2010,
	"Movies/Other":   2020,
	"Movies/SD":      2030,
	"Movies/HD":      2040,
	"Movies/UHD":     2045,
	"Movies/BluRay":  2050,
	"Movies/3D":      2060,
	"Movies/DVD":     2070,
	"Movies/WEB-DL":  2080,

	"Audio":           3000,
	"Audio/MP3":       3010,
	"Audio/Video":     3020,
	"Audio/Audiobook": 3030,
	"Audio/Lossless":  3040,
	"Audio/Other":     3050,
	"Audio/Foreign":   3060,

	"PC":                4000,
	"PC/0day":           4010,
	"PC/ISO":            4020,
	"PC/Mac":            4030,
	"PC/Mobile-Other":   4040,
	"PC/Games":          4050,
	"PC/Mobile-iOS":     4060,
	"PC/Mobile-Android": 4070,

	"TV":             5000,
	"TV/WEB-DL":      5010,
	"TV/Foreign":     5020,
	"TV/SD":          5030,
	"TV/HD":          5040,
	"TV/UHD":         5045,
	"TV/Other":       5050,
	"TV/Sport":       5060,
	"TV/Anime":       5070,
	"TV/Documentary": 5080,

	"XXX":          6000,
	"XXX/DVD":      6010,
	"XXX/WMV":      6020,
	"XXX/XviD":     6030,
	"XXX/x264":     6040,
	"XXX/UHD":      6045,
	"XXX/Pack":     6050,
	"XXX/ImageSet": 6060,
	"XXX/Other":    6070,
	"XXX/SD":       6080,
	"XXX/WEB-DL":   6090,

	"Books":           7000,
	"Books/Mags":      7010,
	"Books/EBook":     7020,
	"Books/Comics":    7030,
	"Books/Technical": 7040,
	"Books/Other":     7050,
	"Books/Foreign":   7060,

	"Other":        8000,
	"Other/Misc":   8010,
	"Other/Hashed": 8020,
}

// defaultCategoryID is used when a definition has no usable category
// information for a row, matching Torznab/Newznab's "Other" category.
const defaultCategoryID = 8000

// categoryGroupSize is the spacing between Torznab top-level categories:
// a parent's ID is a multiple of it, and its children occupy the IDs up to
// the next multiple.
const categoryGroupSize = 1000

// ExpandCategoryIDs returns requested plus, for every top-level category
// among them (an ID that is a multiple of 1000, e.g. 2000 "Movies"), all
// of its known subcategory IDs (2010 "Movies/Foreign", 2040 "Movies/HD",
// ...). A client asking for a parent category means the whole tree, which
// is how Jackett treats it; matching only the exact ID would drop every
// result a definition filed under a more specific subcategory. The result
// is sorted and free of duplicates.
func ExpandCategoryIDs(requested []string) []string {
	if len(requested) == 0 {
		return nil
	}

	expanded := make(map[string]bool, len(requested))
	for _, c := range requested {
		expanded[c] = true

		parent, err := strconv.Atoi(c)
		if err != nil || parent%categoryGroupSize != 0 {
			continue
		}
		for _, id := range standardCategoryIDs {
			if id > parent && id < parent+categoryGroupSize {
				expanded[strconv.Itoa(id)] = true
			}
		}
	}

	ids := make([]string, 0, len(expanded))
	for id := range expanded {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// siteCategoryIDs translates Torznab standard category IDs (as requested
// by a client, e.g. via ?cat=2000) into this tracker's own site-specific
// category IDs, using caps.categorymappings in reverse. Used to populate
// the ".Categories" template variable so a definition's search inputs
// (typically a "$raw" f[]=<id> loop) can request matching categories from
// the site itself. A requested top-level category also selects the
// tracker's categories mapped to any of its subcategories.
func siteCategoryIDs(def *Tracker, standardCatIDs []string) []string {
	if len(standardCatIDs) == 0 {
		return nil
	}

	wanted := make(map[string]bool)
	for _, c := range ExpandCategoryIDs(standardCatIDs) {
		wanted[c] = true
	}

	var ids []string
	seen := make(map[CategoryID]bool)
	for _, mapping := range def.Caps.CategoryMappings {
		stdID, ok := standardCategoryIDs[mapping.Cat]
		if !ok {
			continue
		}
		if wanted[strconv.Itoa(stdID)] && !seen[mapping.ID] {
			seen[mapping.ID] = true
			ids = append(ids, string(mapping.ID))
		}
	}
	return ids
}

// CategoryInfo describes one standard Torznab category a tracker supports.
type CategoryInfo struct {
	ID   int
	Name string
}

// SupportedCategories returns the distinct standard Torznab categories a
// tracker's caps.categorymappings map to, sorted by ID, for advertising in
// that tracker's own Torznab caps response.
func SupportedCategories(def *Tracker) []CategoryInfo {
	names := make(map[int]string)
	for _, mapping := range def.Caps.CategoryMappings {
		if id, ok := standardCategoryIDs[mapping.Cat]; ok {
			names[id] = mapping.Cat
		}
	}

	ids := make([]int, 0, len(names))
	for id := range names {
		ids = append(ids, id)
	}
	sort.Ints(ids)

	result := make([]CategoryInfo, len(ids))
	for i, id := range ids {
		result[i] = CategoryInfo{ID: id, Name: names[id]}
	}
	return result
}

// mapCategory resolves a tracker's own raw category identifier (as
// extracted from the scraped row, typically the site's numeric category
// ID) to a standard Torznab category ID, using the definition's
// caps.categorymappings. The comparison is textual, since a site category
// identifier is opaque and need not be numeric (see CategoryID).
func mapCategory(def *Tracker, raw string) int {
	rawID := CategoryID(strings.TrimSpace(raw))
	if rawID == "" {
		return defaultCategoryID
	}

	for _, mapping := range def.Caps.CategoryMappings {
		if mapping.ID == rawID {
			if id, ok := standardCategoryIDs[mapping.Cat]; ok {
				return id
			}
			return defaultCategoryID
		}
	}

	return defaultCategoryID
}
