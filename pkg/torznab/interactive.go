// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package torznab

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/torrplay/jacklet/pkg/scraper"
)

// The episode patterns Jackett's interactive search reads off the end of a
// query. Each is preceded in Jackett by a word boundary, which .NET draws
// between Unicode word characters and the rest; endingAt checks it, since
// Go's \b knows only ASCII ones.
var (
	trailingSeasonEpisode = regexp.MustCompile(`S(\d{2,4})E(\d{2,4}[A-Za-z]?)$`)
	trailingEpisode       = regexp.MustCompile(`E(\d{2,4}[A-Za-z]?)$`)
	trailingSeason        = regexp.MustCompile(`S(\d{2,4})$`)
)

// imdbIDPattern is the shape Jackett reads an IMDb id in: "tt" and up to
// eight digits, or the digits alone.
var imdbIDPattern = regexp.MustCompile(`^(?:tt)?(\d{1,8})$`)

// Jackett's search-term sanitizing: every run of dashes becomes one "-",
// and every single-quote look-alike becomes "'".
var (
	dashRuns          = regexp.MustCompile(`\p{Pd}+`)
	singleQuoteAlikes = regexp.MustCompile("[`´‘’]")
)

// interactiveSearch returns params as Jackett's interactive search, the
// JSON results endpoint, makes of a request in ApiSearch.ToTorznabQuery.
// The search is a plain one, whatever "t" says. A query ending in an
// episode, as "Sample Show S01E02", "Sample Show E02", "Sample Show S01"
// or "Sample Show S01 E02" do, is searched for the text before it, in that
// season and episode, with the episode's leading zeros dropped. A season
// or episode the request names as its own parameter stands where the query
// names none. A query that is then an IMDb id, "tt0123456", is an IMDb
// lookup instead: it keeps only the categories, season and episode, and
// the paging, and has no mode, which is the query Jackett builds for it.
//
// episodeTerms are the episode as the query wrote it, "S01E02" or "E02"
// and "S01", which the stored rows no search produced are still matched
// on, so taking the episode off into the season and episode narrows them
// as much as the text did. When a season or episode given apart completes
// the one the query wrote, there are none, and search matches the rows on
// the whole episode instead. An IMDb lookup keeps them too, though not the
// id itself, which a stored name never holds: its rows are the ones its
// search produced and the ones stored with that id.
func interactiveSearch(params scraper.SearchParams) (searched scraper.SearchParams, episodeTerms []string) {
	query := strings.TrimSpace(params.Query)
	var isSeasonWritten, isEpisodeWritten bool
	if query != "" {
		if groups, rest, ok := endingAt(trailingSeasonEpisode, query); ok {
			params.Season = seasonNumber(groups[0])
			params.Ep = strings.TrimLeft(groups[1], "0")
			episodeTerms = append(episodeTerms, strings.TrimSpace(query[len(rest):]))
			query = rest
			isSeasonWritten, isEpisodeWritten = true, true
		} else {
			if groups, rest, ok := endingAt(trailingEpisode, query); ok {
				params.Ep = strings.TrimLeft(groups[0], "0")
				episodeTerms = append(episodeTerms, strings.TrimSpace(query[len(rest):]))
				query = rest
				isEpisodeWritten = true
			}
			if groups, rest, ok := endingAt(trailingSeason, query); ok {
				params.Season = seasonNumber(groups[0])
				episodeTerms = append(episodeTerms, strings.TrimSpace(query[len(rest):]))
				query = rest
				isSeasonWritten = true
			}
		}
	}
	if (params.HasSeason() && !isSeasonWritten) || (params.Ep != "" && !isEpisodeWritten) {
		episodeTerms = nil
	}
	params.Query = query
	params.Type = "search"

	if id := imdbQuery(query); id != "" {
		return scraper.SearchParams{
			Categories: params.Categories,
			Ep:         params.Ep,
			IMDBID:     id,
			Limit:      params.Limit,
			Offset:     params.Offset,
			Season:     params.Season,
		}, episodeTerms
	}
	return params, episodeTerms
}

// endingAt matches pattern at the end of query, where the match starts a
// word as .NET's \b reads one, and returns the match's groups and the
// query with the match taken out and trimmed.
func endingAt(pattern *regexp.Regexp, query string) (groups []string, rest string, ok bool) {
	match := pattern.FindStringSubmatchIndex(query)
	if len(match) == 0 || !isWordStart(query, match[0]) {
		return nil, query, false
	}
	for i := 2; i < len(match); i += 2 {
		groups = append(groups, query[match[i]:match[i+1]])
	}
	return groups, strings.TrimSpace(query[:match[0]] + query[match[1]:]), true
}

// isWordStart reports whether the word character at s[i] starts a word,
// that is, whether nothing before it in s is a word character as .NET's
// regular expressions count them at a boundary: a letter, a decimal
// digit, a nonspacing mark, a connector such as "_", or a zero-width
// joiner or non-joiner.
func isWordStart(s string, i int) bool {
	if i == 0 {
		return true
	}
	r, _ := utf8.DecodeLastRuneInString(s[:i])
	return !unicode.IsLetter(r) && !unicode.IsDigit(r) && !unicode.Is(unicode.Mn, r) && !unicode.Is(unicode.Pc, r) &&
		r != '\u200c' && r != '\u200d'
}

// seasonNumber reads a season as Jackett's int.Parse does, so "01" is "1".
// The patterns hand it two to four ASCII digits, which always parse.
func seasonNumber(digits string) string {
	season, _ := strconv.Atoi(digits)
	return strconv.Itoa(season)
}

// imdbQuery returns the IMDb id a query names, in Jackett's "tt" and seven
// digits form, or "" when it names none. As in Jackett, the query is read
// sanitized, must start with "tt" and be at most ten characters long
// there, and an id of zero names none.
func imdbQuery(query string) string {
	term := sanitizedSearchTerm(query)
	if !strings.HasPrefix(term, "tt") || len(utf16.Encode([]rune(term))) > 10 {
		return ""
	}
	match := imdbIDPattern.FindStringSubmatch(term)
	if match == nil {
		return ""
	}
	id, err := strconv.Atoi(match[1])
	if err != nil || id == 0 {
		return ""
	}
	return fmt.Sprintf("tt%07d", id)
}

// sanitizedSearchTerm is Jackett's TorznabQuery.SanitizedSearchTerm:
// dashes and single quotes standardized, then every character dropped but
// letters, digits, white space and - . _ ( ) @ / ' [ ] + %.
func sanitizedSearchTerm(term string) string {
	term = dashRuns.ReplaceAllString(term, "-")
	term = singleQuoteAlikes.ReplaceAllString(term, "'")
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsSpace(r) || strings.ContainsRune("-._()@/'[]+%", r) {
			return r
		}
		return -1
	}, term)
}
