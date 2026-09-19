// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package torznab

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/torrplay/jacklet/pkg/scraper"
)

// AggregateID is the indexer id that answers from every configured tracker
// at once, so a client can add one endpoint instead of one per definition.
// Jackett exposes the same thing under the same name.
//
// It shadows a definition that uses "all" as its own id: that definition
// still takes part in the aggregate, but is not reachable on its own path.
const AggregateID = "all"

// maxConcurrentScrapes bounds how many trackers the aggregate indexer
// scrapes at once. Fanning out to every definition without a limit would
// turn one client search into as many simultaneous outbound requests as
// there are definition files, which is the kind of burst the scraper's own
// per-tracker spacing exists to avoid.
const maxConcurrentScrapes = 8

// indexer is the set of tracker definitions one request is answered from:
// a single definition for "/api/v2.0/indexers/{id}/...", or every
// configured one for the aggregate id. The search and caps handlers work
// in terms of this, so neither has to ask which kind of request it is
// serving.
type indexer struct {
	// Defs are the definitions to scrape and to answer from, in the order
	// the source returned them.
	Defs []*scraper.Tracker
	// Description, Language and Name describe the feed itself, taken from
	// the single definition or stated for the aggregate.
	Description string
	// ID is the id in the request path, which for a meta indexer is not
	// any definition's own id.
	ID string
	// IsMeta marks an indexer that stands for a set of definitions rather
	// than being one: the aggregate, or a filter expression. An empty set
	// then means nothing was searched, where a single definition with no
	// results has genuinely been searched and found nothing.
	IsMeta bool
	// IsUnconfigured marks a meta indexer built when no tracker definition
	// loaded at all, which is a broken install rather than an empty
	// result, and is reported the same way whether the request addressed
	// the aggregate or a filter. It is recorded when the indexer is built
	// rather than read back from Defs, which a filter or a "Tracker[]"
	// narrowing may since have emptied for reasons that are perfectly
	// ordinary.
	IsUnconfigured bool
	Language       string
	Name           string
}

// singleIndexer answers from one tracker definition.
func singleIndexer(def *scraper.Tracker) *indexer {
	return &indexer{
		Defs:        []*scraper.Tracker{def},
		Description: def.Description,
		ID:          scraper.TrackerID(def),
		Language:    def.Language,
		Name:        def.Name,
	}
}

// aggregateIndexer answers from every configured tracker. defs is the
// slice the tracker source returned; it is not retained beyond the
// pointers taken into it, which the source has already promised not to
// mutate.
func aggregateIndexer(defs []scraper.Tracker) *indexer {
	idx := &indexer{
		Defs:           make([]*scraper.Tracker, len(defs)),
		Description:    fmt.Sprintf("Every indexer Jacklet has a definition for (%d)", len(defs)),
		ID:             AggregateID,
		IsMeta:         true,
		IsUnconfigured: len(defs) == 0,
		// No single language, so the feed declares none rather than
		// claiming one tracker's.
		Name: "All indexers",
	}
	for i := range defs {
		idx.Defs[i] = &defs[i]
	}
	return idx
}

// ids returns the tracker ids this indexer covers, for scoping a store
// query to them.
func (idx *indexer) ids() []string {
	ids := make([]string, len(idx.Defs))
	for i, def := range idx.Defs {
		ids[i] = scraper.TrackerID(def)
	}
	return ids
}

// names maps each covered tracker's id to its display name, for labelling
// a result with the tracker it actually came from — which under the
// aggregate is not the indexer the client asked.
func (idx *indexer) names() map[string]string {
	names := make(map[string]string, len(idx.Defs))
	for _, def := range idx.Defs {
		names[scraper.TrackerID(def)] = def.Name
	}
	return names
}

// categories are the standard Torznab categories this indexer can serve:
// one definition's own, or the union across every definition for the
// aggregate, so a client is not told a category is unavailable merely
// because the first tracker does not carry it.
func (idx *indexer) categories() []scraper.CategoryInfo {
	if len(idx.Defs) == 1 {
		return scraper.SupportedCategories(idx.Defs[0])
	}

	names := make(map[int]string)
	for _, def := range idx.Defs {
		for _, c := range scraper.SupportedCategories(def) {
			names[c.ID] = c.Name
		}
	}

	ids := make([]int, 0, len(names))
	for id := range names {
		ids = append(ids, id)
	}
	sort.Ints(ids)

	categories := make([]scraper.CategoryInfo, len(ids))
	for i, id := range ids {
		categories[i] = scraper.CategoryInfo{ID: id, Name: names[id]}
	}
	return categories
}

// searchModes are the modes this indexer advertises, and the parameters
// each accepts. For the aggregate a mode is available when any definition
// offers it, with the union of that mode's parameters: a request naming a
// parameter only some trackers understand is still answerable, since the
// rest simply fold it into their keyword search.
func (idx *indexer) searchModes() map[string][]string {
	if len(idx.Defs) == 1 {
		return scraper.SearchModes(idx.Defs[0])
	}

	merged := make(map[string][]string)
	for _, def := range idx.Defs {
		for mode, params := range scraper.SearchModes(def) {
			for _, param := range params {
				if !slices.Contains(merged[mode], param) {
					merged[mode] = append(merged[mode], param)
				}
			}
		}
	}
	for mode := range merged {
		sort.Strings(merged[mode])
	}
	return merged
}

// scrapeOutcome is what refreshing one tracker produced: which tracker it
// was, how long it took, and whether it failed. The JSON results endpoint
// reports one of these per indexer, the way Jackett reports a manual
// search.
type scrapeOutcome struct {
	Elapsed   time.Duration
	Err       error
	TrackerID string
}

// scrape refreshes every tracker this indexer covers, concurrently and
// bounded by maxConcurrentScrapes, and returns what each one produced in
// the order the indexer holds its definitions.
//
// It reports an error only when no tracker could be scraped at all. One
// failing tracker must not fail an aggregate search: the point of querying
// every indexer at once is that the reachable ones still answer.
func (t *Torznab) scrape(ctx context.Context, idx *indexer, params scraper.SearchParams) ([]scrapeOutcome, error) {
	outcomes := make([]scrapeOutcome, len(idx.Defs))
	if len(idx.Defs) == 1 {
		outcomes[0] = t.scrapeOne(ctx, idx.Defs[0], params)
		return outcomes, outcomes[0].Err
	}

	sem := make(chan struct{}, maxConcurrentScrapes)
	var wg sync.WaitGroup
	for i, def := range idx.Defs {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			outcomes[i] = t.scrapeOne(ctx, def, params)
		})
	}
	wg.Wait()

	var failed []error
	for _, outcome := range outcomes {
		if outcome.Err != nil {
			failed = append(failed, fmt.Errorf("%s: %w", outcome.TrackerID, outcome.Err))
		}
	}
	if len(failed) == 0 {
		return outcomes, nil
	}
	// Some trackers answered, so the search succeeded; the failures are
	// worth a log line but not an error the client sees.
	if len(failed) < len(idx.Defs) {
		t.logger.Warn("some indexers failed during an aggregate scrape",
			"failed", len(failed), "indexers", len(idx.Defs), "error", errors.Join(failed...))
		return outcomes, nil
	}
	return outcomes, fmt.Errorf("every indexer failed: %w", errors.Join(failed...))
}

// scrapeOne refreshes one tracker and times it, reporting what happened
// rather than returning an error, so a caller assembling a per-indexer
// report has the same record for a tracker that succeeded and one that
// did not.
func (t *Torznab) scrapeOne(ctx context.Context, def *scraper.Tracker, params scraper.SearchParams) scrapeOutcome {
	started := time.Now()
	err := t.scrapeGuarded(def, func() error {
		return t.scrpr.ScrapeIndexer(ctx, def, params)
	})
	return scrapeOutcome{Elapsed: time.Since(started), Err: err, TrackerID: scraper.TrackerID(def)}
}

// scrapeGuarded runs one tracker's scrape and turns a panic into an
// ordinary error, so the caller records it the way it records any other
// per-tracker failure.
//
// The scrape recovers its own panics, where the failure still reaches the
// tracker's backoff and the searches following it. This is the outer net
// for what that cannot cover: a panic raised before the scrape has
// registered its defers, or in anything else this goroutine comes to run. It is here rather than left
// to net/http because these goroutines are not the request's, so nothing
// above them recovers — one panic would end the process instead of the one
// tracker it came from.
func (t *Torznab) scrapeGuarded(def *scraper.Tracker, scrape func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
			t.logger.Error("panic while scraping an indexer",
				"indexer", scraper.TrackerID(def), "panic", r,
				"stack", string(debug.Stack()))
		}
	}()
	return scrape()
}
