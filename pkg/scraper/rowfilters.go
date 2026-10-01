// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// Cardigann has two kinds of filter and they are not interchangeable. The
// ones in filterRegistry rewrite a single field's value; the ones here,
// declared under "search.rows.filters", decide whether a scraped row is
// kept at all. Jackett implements the second kind in ParseRowFilters and
// supports exactly two names there.

// andMatchCommonWords are the words Jackett ignores when AND-matching a
// title, being too common to carry meaning.
var andMatchCommonWords = []string{"and", "the", "an"}

// skipRow reports whether a definition's row filters reject this row.
//
// Every filter runs even once one has rejected the row, because rejection
// is not the end of the list: a definition pairing "andmatch" with a
// following "strdump" is asking to see the rows that were dropped, which
// is exactly when the dump is worth having. Jackett records the rejection
// in a flag and keeps going for the same reason.
func skipRow(def *Tracker, row resultRow, title string, td templateData, logger *slog.Logger) bool {
	skip := false
	for _, filter := range def.Search.Rows.Filters {
		switch filter.Name {
		case "andmatch":
			if skipByAndMatch(def, filter, title, td, logger) {
				skip = true
			}
		case "strdump":
			// Debugging only; Jackett logs the row as served and keeps it.
			// The point is the raw markup or JSON, so that an author can
			// see what their selectors are addressing -- logging the
			// extracted title instead would remove the only information
			// the filter exists to provide.
			logger.Debug("row strdump", "row", row.debugString())
		default:
			logger.Error("unsupported rows filter", "filter", filter.Name)
		}
	}
	return skip
}

// skipByAndMatch reports whether "andmatch" rejects the row. The filter
// exists for trackers whose own search ORs the terms together: it re-applies
// the AND within Jacklet, so a query for two words does not come back with
// every release matching either one.
//
// Its optional argument limits how many characters of the query are
// compared, for a tracker that lists a truncated title.
func skipByAndMatch(def *Tracker, filter Filter, title string, td templateData, logger *slog.Logger) bool {
	// A search by external id was answered by the tracker matching that id,
	// not the keywords, so the title has no reason to contain them. Jackett
	// skips the filter whenever the id it was given is one the definition
	// advertises.
	if idSearchSupported(def, td.Query) {
		return false
	}

	limit := 0
	if args := filterArgsAsStrings(filter.Args); len(args) > 0 && args[0] != "" {
		parsed, err := strconv.Atoi(strings.TrimSpace(args[0]))
		if err != nil {
			logger.Debug("ignoring unparseable andmatch limit", "args", args[0], "error", err)
		} else {
			limit = parsed
		}
	}

	if matchQueryStringAND(title, td.Keywords, limit) {
		return false
	}
	logger.Debug("skipping row: andmatch filter", "title", title, "keywords", td.Keywords)
	return true
}

// idSearchSupported reports whether the request carried an external id
// that the definition advertises, meaning the tracker searched by that id
// and its titles have no reason to carry the keywords.
//
// The gates are Jackett's, quirks included: a douban or trakt search is
// gated on the *imdb* capability, and a tvmaze or rage search on the
// tv-search imdb capability, none of which is the id being searched for.
// That reads as copy-paste rather than intent, but a definition Jackett
// filters and one Jacklet filters have to be the same set of definitions,
// so it is reproduced rather than tidied.
//
// The one flag that is not a search parameter is tv-search imdb, which
// Jackett takes straight from "caps.allowtvsearchimdb" rather than from
// the tv-search parameter list.
func idSearchSupported(def *Tracker, query queryParams) bool {
	// Jackett parses the declared modes and nothing else, so a definition
	// declaring none advertises no id at all. SearchModes' fallback exists
	// to advertise a generic keyword search and must not widen these gates.
	declared := def.Caps.Modes
	hasParam := func(mode, param string) bool {
		return slices.Contains(declared[mode], param)
	}

	movieIMDB := hasParam("movie-search", "imdbid")
	tvIMDB := def.Caps.AllowTVSearchIMDB
	anyIMDB := movieIMDB || tvIMDB

	switch {
	case query.IMDBID != "" && anyIMDB:
		return true
	case query.TMDBID != "" && (hasParam("movie-search", "tmdbid") || hasParam("tv-search", "tmdbid")):
		return true
	case query.TVDBID != "" && hasParam("tv-search", "tvdbid"):
		return true
	case query.DoubanID != "" && anyIMDB:
		return true
	case query.TraktID != "" && anyIMDB:
		return true
	case query.TVMazeID != "" && tvIMDB:
		return true
	case query.TVRageID != "" && tvIMDB:
		return true
	default:
		return false
	}
}

// matchQueryStringAND reports whether title contains every significant word
// of the query, as Jackett's TorznabQuery.MatchQueryStringAND does: words
// are split on non-word characters, single characters and the common words
// are ignored, and the comparison is case-insensitive.
//
// A query with nothing significant in it matches everything, which is what
// makes an RSS-style request unaffected by the filter.
func matchQueryStringAND(title, query string, limit int) bool {
	if limit > 0 {
		if runes := []rune(query); limit < len(runes) {
			query = string(runes[:limit])
		}
	}

	lowered := strings.ToLower(title)
	for _, word := range splitQueryWords(query) {
		if len([]rune(word)) <= 1 || containsFold(andMatchCommonWords, word) {
			continue
		}
		if !strings.Contains(lowered, strings.ToLower(word)) {
			return false
		}
	}
	return true
}

// splitQueryWords splits on everything that is not a word character.
// Jackett's "[^\w]+" is Unicode-aware in .NET, so this cannot use Go's
// "\w", which matches ASCII only and would split a Cyrillic title into
// single characters.
func splitQueryWords(query string) []string {
	return strings.FieldsFunc(query, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_'
	})
}

// containsFold reports whether values contains value, ignoring case.
func containsFold(values []string, value string) bool {
	return slices.ContainsFunc(values, func(v string) bool {
		return strings.EqualFold(v, value)
	})
}
