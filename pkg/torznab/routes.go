// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package torznab

import "net/http"

// Routes registers every endpoint this package serves on mux.
//
// The paths are Jackett's own, so a client configured against a Jackett
// instance reaches the same handler here without being reconfigured.
// Torznab itself specifies query parameters and a response format, not a
// URL layout, so there is no spec-blessed path to prefer instead — and
// Jackett's layout is what every client (Sonarr, Radarr, Lidarr,
// Prowlarr) is set up with, since an operator configures one by pasting a
// Jackett URL.
//
// One of these has no Jackett counterpart and is marked as Jacklet's own
// below. Everything else is reachable at the address Jackett would serve
// it from.
func (t *Torznab) Routes(mux *http.ServeMux) {
	// Jackett's ResultsController is routed at
	// "api/v2.0/indexers/{indexerId}/results" with the torznab action at
	// "[action]/{ignored?}", so the trailing segment is present, absent,
	// or arbitrary and means nothing either way. Clients send "api".
	//
	// The "{$}" pattern is the trailing-slash form, and it is not
	// hypothetical: Jackett's own UI hands the operator this feed URL
	// ending in a slash, so it is the string most likely to be pasted
	// somewhere. ASP.NET matches it as the same route; Go's "{ignored}"
	// requires a non-empty segment and would answer 404.
	mux.Handle("GET /api/v2.0/indexers/{id}/results/torznab", t)
	mux.Handle("GET /api/v2.0/indexers/{id}/results/torznab/{$}", t)
	mux.Handle("GET /api/v2.0/indexers/{id}/results/torznab/{ignored}", t)
	// JSON alternative to the XML feed above, on the same pipeline.
	// Torznab has no JSON variant; this is Jackett's own addition to it.
	mux.HandleFunc("GET /api/v2.0/indexers/{id}/results", t.Results)
	mux.HandleFunc("GET /api/v2.0/indexers/{id}/results/{$}", t.Results)
	// Jacklet's own: the torrent file for one stored result, fetched with
	// Jacklet's tracker session. Jackett serves the equivalent from
	// "/dl/{indexerId}", keyed by an encrypted "path" parameter carrying
	// the tracker's link -- a payload only Jackett can mint, so there is
	// nothing to be compatible with. A client never builds this address
	// anyway; it follows the one in the feed.
	mux.HandleFunc("GET /api/v2.0/indexers/{id}/download/{row}", t.Download)
	// Jackett's indexer directory, in Jackett's own JSON shape.
	mux.HandleFunc("GET /api/v2.0/indexers", t.JackettIndexers)
	mux.HandleFunc("GET /api/v2.0/indexers/{$}", t.JackettIndexers)
}
