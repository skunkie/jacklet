// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package torznab

import (
	"encoding/xml"
	"net/http"
	"strings"

	"github.com/torrplay/jacklet/pkg/scraper"
)

// handleIndexerList serves "t=indexers": the indexers behind a meta
// indexer, each with its own capabilities, so a client that added the
// aggregate can discover what it is actually searching without asking
// each definition separately.
//
// Jackett answers this only for a meta indexer and returns error 203 for
// any other, which is the shape a client is written against: the
// aggregate and a filter expression are the meta indexers here, and a
// single definition is not one.
func (t *Torznab) handleIndexerList(w http.ResponseWriter, r *http.Request, idx *indexer) {
	if !idx.IsMeta {
		t.writeTorznabError(w, http.StatusBadRequest, errMetaIndexerOnly,
			"Function Not Available: this isn't a meta indexer")
		return
	}

	// Jackett filters the list on "configured", which every definition
	// Jacklet loaded is. Asking for the unconfigured ones is therefore a
	// well-formed request with an empty answer, not an error.
	defs := idx.Defs
	if strings.EqualFold(r.URL.Query().Get("configured"), "false") {
		defs = nil
	}

	baseURL := t.getBaseURL(r)
	entries := make([]indexerEntry, len(defs))
	for i, def := range defs {
		// Each entry carries the definition's own capabilities, not the
		// aggregate's merged ones: the point of the list is what each
		// indexer behind it can do on its own.
		caps := t.capsFor(singleIndexer(def), baseURL)
		entries[i] = indexerEntry{
			Caps:        &caps,
			Configured:  true,
			Description: def.Description,
			ID:          scraper.TrackerID(def),
			Language:    def.Language,
			Link:        siteLink(def),
			Title:       def.Name,
			Type:        def.Type,
		}
	}

	t.writeXML(w, indexerListDocument{Indexers: entries})
}

// indexerListDocument is the "t=indexers" response. Its element and
// attribute names are Jackett's, which a client reads by position in the
// document rather than by any Jacklet convention.
type indexerListDocument struct {
	Indexers []indexerEntry `xml:"indexer"`
	XMLName  xml.Name       `xml:"indexers"`
}

// indexerEntry is one indexer behind the meta indexer.
//
// Its fields are in document order rather than alphabetical, which is the
// one place in this package that departs from the ordering convention:
// encoding/xml emits elements in field order, so the field order *is* the
// document, and this document's order is Jackett's. Sorting these would
// silently reshape the response.
type indexerEntry struct {
	Configured  bool   `xml:"configured,attr"`
	ID          string `xml:"id,attr"`
	Title       string `xml:"title"`
	Description string `xml:"description"`
	Link        string `xml:"link"`
	Language    string `xml:"language"`
	Type        string `xml:"type"`
	Caps        *Caps  `xml:"caps"`
}
