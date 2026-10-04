// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package torznab

import (
	"errors"
	"fmt"
	"strings"

	"github.com/torrplay/jacklet/pkg/scraper"
)

// This file implements Jackett's indexer filter expressions, which stand
// in for an indexer id in the request path: "status:healthy" searches
// every healthy indexer and "!type:private+lang:en" every public English
// one. A client offering its user "all working indexers" rather than a
// list of them sends one of these where an id would otherwise go, so the
// expression has to resolve here for that client to search at all.

// The operators of a filter expression: "," is OR, "+" is AND, and a
// leading "!" negates one term.
const (
	filterAnd = "+"
	filterNot = "!"
	filterOr  = ","
)

// errUnsupportedFilter reports an expression that is shaped like a filter
// but names something Jacklet cannot select on.
var errUnsupportedFilter = errors.New("unsupported indexer filter")

// indexerFilter reports whether one definition is covered by a filter
// expression. status is the scraper's view of that tracker, which the
// health filters read and the rest ignore.
type indexerFilter func(def *scraper.Tracker, status scraper.IndexerStatus) bool

// isIndexerFilter reports whether an id in the request path is a filter
// expression rather than a definition's own id. A definition id is a slug
// and never contains a colon, so the colon that every filter term
// requires is what tells the two apart.
func isIndexerFilter(id string) bool {
	return strings.Contains(id, ":")
}

// parseIndexerFilter compiles a filter expression into a predicate over
// the configured definitions.
func parseIndexerFilter(expression string) (indexerFilter, error) {
	groups := strings.Split(expression, filterOr)
	alternatives := make([]indexerFilter, len(groups))
	for i, group := range groups {
		parts := strings.Split(group, filterAnd)
		terms := make([]indexerFilter, len(parts))
		for j, part := range parts {
			term, err := parseFilterTerm(part)
			if err != nil {
				return nil, err
			}
			terms[j] = term
		}
		alternatives[i] = allOf(terms)
	}
	return anyOf(alternatives), nil
}

// parseFilterTerm compiles one "name:value" term, with its optional
// negation.
func parseFilterTerm(term string) (indexerFilter, error) {
	term = strings.TrimSpace(term)
	isNegated := strings.HasPrefix(term, filterNot)
	if isNegated {
		term = strings.TrimSpace(strings.TrimPrefix(term, filterNot))
	}

	name, value, found := strings.Cut(term, ":")
	if !found {
		return nil, fmt.Errorf("%w: %q names no filter", errUnsupportedFilter, term)
	}
	match, err := filterTerm(
		strings.ToLower(strings.TrimSpace(name)),
		strings.ToLower(strings.TrimSpace(value)),
	)
	if err != nil {
		return nil, err
	}
	if !isNegated {
		return match, nil
	}
	return func(def *scraper.Tracker, status scraper.IndexerStatus) bool {
		return !match(def, status)
	}, nil
}

// filterTerm compiles one filter by name.
func filterTerm(name, value string) (indexerFilter, error) {
	switch name {
	case "lang":
		// A language is matched by prefix, so "lang:en" covers a
		// definition declaring "en-US" as well as one declaring "en-GB".
		return func(def *scraper.Tracker, _ scraper.IndexerStatus) bool {
			return strings.HasPrefix(strings.ToLower(def.Language), value)
		}, nil
	case "status", "test":
		return healthTerm(name, value)
	case "tag":
		// Tags are a user's own labels on an indexer. A definition here
		// is a file in a directory with nothing to hang one on, so no
		// definition carries any tag and a tag filter selects nothing --
		// the same answer as a tag that has been assigned to no indexer.
		return func(*scraper.Tracker, scraper.IndexerStatus) bool { return false }, nil
	case "type":
		return func(def *scraper.Tracker, _ scraper.IndexerStatus) bool {
			return strings.EqualFold(def.Type, value)
		}, nil
	default:
		return nil, fmt.Errorf("%w: %q", errUnsupportedFilter, name)
	}
}

// healthTerm compiles the filters that read a tracker's recent behavior
// rather than its definition. "test" is a separate manual action in
// Jackett, reporting whether that action last left an error; Jacklet has
// no such action, so both names read the one thing it knows, which is
// whether recent scrapes of that tracker failed.
//
// Health here deliberately differs from Jackett's. Jackett calls an
// indexer healthy only inside a validity window opened by a search that
// succeeded, so one it has not searched yet is neither healthy nor
// failing but unknown, and a filter for the healthy ones answers a
// freshly started instance with nothing at all. A client that offers its
// user "all working indexers" and nothing else would then be unable to
// search until something else had already searched. Health is the absence
// of a recorded failure instead, so a tracker nothing has scraped yet is
// healthy and every configured indexer is reachable from the start. Such
// a tracker is also "status:unknown", which is the one filter that asks
// whether a scrape has run at all; unlike Jackett's, that answer does not
// come back once a successful scrape goes stale, because nothing here
// expires a tracker's recorded state.
func healthTerm(name, value string) (indexerFilter, error) {
	switch {
	case name == "status" && value == "healthy", name == "test" && value == "passed":
		return func(_ *scraper.Tracker, status scraper.IndexerStatus) bool {
			return status.Failures == 0
		}, nil
	case name == "status" && value == "failing", name == "test" && value == "failed":
		return func(_ *scraper.Tracker, status scraper.IndexerStatus) bool {
			return status.Failures > 0
		}, nil
	case name == "status" && value == "unknown":
		return func(_ *scraper.Tracker, status scraper.IndexerStatus) bool {
			return !status.Scraped
		}, nil
	default:
		return nil, fmt.Errorf("%w: %q has no value %q", errUnsupportedFilter, name, value)
	}
}

// allOf matches a definition every term matches, which is one "+"-joined
// group of an expression.
func allOf(terms []indexerFilter) indexerFilter {
	return func(def *scraper.Tracker, status scraper.IndexerStatus) bool {
		for _, term := range terms {
			if !term(def, status) {
				return false
			}
		}
		return true
	}
}

// anyOf matches a definition any group matches, which is the expression
// as a whole.
func anyOf(groups []indexerFilter) indexerFilter {
	return func(def *scraper.Tracker, status scraper.IndexerStatus) bool {
		for _, group := range groups {
			if group(def, status) {
				return true
			}
		}
		return false
	}
}

// filterIndexer answers from every configured tracker an expression
// covers. Like the aggregate it is a meta indexer over a set of
// definitions, addressed by the expression itself rather than by an id of
// its own.
//
// defs is everything that loaded, not what the expression selected, which
// is what lets the two empty cases stay apart: an expression matching
// none of several definitions is an empty answer, while one addressed to
// an install where nothing loaded reports the install, exactly as the
// aggregate does for the same request.
func (t *Torznab) filterIndexer(defs []scraper.Tracker, expression string, match indexerFilter) *indexer {
	idx := &indexer{
		ID:             expression,
		IsMeta:         true,
		IsUnconfigured: len(defs) == 0,
		Name:           "Filtered indexers",
	}
	for i := range defs {
		def := &defs[i]
		if match(def, t.scrpr.Status(scraper.TrackerID(def))) {
			idx.Defs = append(idx.Defs, def)
		}
	}
	idx.Description = fmt.Sprintf("Every indexer matching %q (%d)", expression, len(idx.Defs))
	return idx
}
