// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package torznab

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/torrplay/jacklet/pkg/scraper"
)

// jackettIndexer is one entry of Jackett's "GET /api/v2.0/indexers"
// response, field for field. The names are lowercase and underscored
// because they are Jackett's C# property names serialized as they stand
// (Jackett.Common/Models/DTO/Indexer.cs), not because they follow this
// repository's JSON conventions -- a consumer written against Jackett
// reads these keys, so they are copied rather than tidied.
type jackettIndexer struct {
	AlternativeSiteLinks []string            `json:"alternativesitelinks"`
	Caps                 []jackettCapability `json:"caps"`
	Configured           bool                `json:"configured"`
	Description          string              `json:"description"`
	ID                   string              `json:"id"`
	Language             string              `json:"language"`
	LastError            string              `json:"last_error"`
	Name                 string              `json:"name"`
	PotatoEnabled        bool                `json:"potatoenabled"`
	SiteLink             string              `json:"site_link"`
	Tags                 []string            `json:"tags"`
	Type                 string              `json:"type"`
}

// jackettCapability is one advertised category. Jackett capitalizes these
// two keys where it lowercases every key of the object containing them,
// and serializes the category id as a string; both are reproduced here.
type jackettCapability struct {
	ID   string `json:"ID"`
	Name string `json:"Name"`
}

// JackettIndexers serves "GET /api/v2.0/indexers": the configured indexer
// directory, in the JSON shape Jackett returns from the same address, so a
// script or dashboard written against Jackett can enumerate Jacklet's
// indexers unmodified.
//
// Jackett reads "?configured" here as a boolean and filters only when it
// is true, returning everything otherwise. Every definition Jacklet can
// load is configured -- a definition lives in definitions/ or it does
// not, and there is no separate step that turns one on -- so both
// branches answer with the same list and the parameter is accepted and
// ignored.
//
// Do not reconcile this with handleIndexerList, which treats
// "configured=false" as a request for the unconfigured ones and answers
// empty. Jackett's two endpoints genuinely differ -- one parses a bool,
// the other compares the string to "true" and "false" -- and each is
// reproduced as it stands.
//
// Definitions that failed to parse cannot be reported here: Jackett's
// response is a bare array with nowhere to put them. DefinitionStore logs
// each one as it reads it, which on a headless install is the only
// surface; the admin panel lists them when one is configured.
//
// The aggregate indexer (AggregateID) is omitted too. It is an endpoint
// over the listed definitions rather than another definition alongside
// them, and a client walking this list to add every indexer would
// otherwise add all of them twice.
func (t *Torznab) JackettIndexers(w http.ResponseWriter, r *http.Request) {
	// A JSON endpoint fails in JSON. The Torznab error document belongs
	// to the XML API, but the alternative to it here is the shape the
	// sibling results endpoint already answers with, not plain text.
	if !t.authorized(r) {
		t.writeJSONError(w, http.StatusUnauthorized, errInvalidAPIKey, "Invalid API Key")
		return
	}

	trackers, _, err := t.trackers.Trackers()
	if err != nil {
		t.logger.Error("failed to load tracker definitions", "error", err)
		t.writeJSONError(w, http.StatusInternalServerError, errUnknown, "Failed to load tracker definitions")
		return
	}

	indexers := make([]jackettIndexer, 0, len(trackers))
	for i := range trackers {
		def := &trackers[i]

		categories := scraper.SupportedCategories(def)
		caps := make([]jackettCapability, len(categories))
		for i, c := range categories {
			caps[i] = jackettCapability{ID: strconv.Itoa(c.ID), Name: c.Name}
		}

		// A definition's first link is the site; the rest are the
		// alternatives Jackett advertises under its own name for them.
		alternatives := []string{}
		if len(def.Links) > 1 {
			alternatives = append(alternatives, def.Links[1:]...)
		}

		indexers = append(indexers, jackettIndexer{
			AlternativeSiteLinks: alternatives,
			Caps:                 caps,
			Configured:           true,
			Description:          def.Description,
			ID:                   scraper.TrackerID(def),
			Language:             def.Language,
			Name:                 def.Name,
			SiteLink:             siteLink(def),
			// Jackett's tags are a user's own labels on an indexer and
			// its potato flag is CouchPotato support, which Jacklet does
			// not serve. Both are reported empty rather than dropped: a
			// consumer reading the field would otherwise see null where
			// Jackett gives it a value.
			Tags: []string{},
			Type: def.Type,
		})
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(indexers); err != nil {
		t.logger.Error("failed to encode the Jackett indexer list", "error", err)
	}
}

// siteLink is the tracker's own address: a definition's first link, which
// is the one it is reached at. Jackett reports the rest separately, as
// alternative site links.
func siteLink(def *scraper.Tracker) string {
	if len(def.Links) == 0 {
		return ""
	}
	return def.Links[0]
}
