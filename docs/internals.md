<!--
SPDX-FileCopyrightText: 2026 TorrPlay

SPDX-License-Identifier: MIT
-->

# How it works

[← Documentation index](../README.md#documentation)

A search request re-scrapes the indexer live, stores the results in SQLite,
and answers from the store. That store is also the fallback: if the tracker is
unreachable, previously stored results are served rather than an error.

Each result's link points back at Jacklet rather than at the tracker, and
Jacklet fetches the torrent file with its own tracker session when the client
asks for it. Sonarr and your BitTorrent client have no account on a private
tracker, so the tracker's own link would hand them a 403 or a login page saved
as a `.torrent`. Magnet links are passed through untouched, since a client
resolves one itself.

Repeating the *same* search within a few seconds skips the live scrape and
answers from the store, so frequent polling (an RSS sync, say) cannot hammer
a tracker. A different search of that indexer still reaches the site — the
store holds the previous search's results, not this one's — and is merely
spaced a second behind it.

A tracker that keeps failing is backed off exponentially, up to ten minutes,
and searches of it are answered from the store without waiting.

Stored torrents are swept out hourly once their release date is older than
`JACKLET_RETENTION_DAYS` (30 by default). The cutoff is measured against the release
date scraped from the tracker, not against when Jacklet stored the row, so a
tracker listing an old release will not keep it around. Releases whose date
could not be parsed age from their internal `last_seen` timestamp instead,
preventing undated listings from growing the cache forever. Setting
`JACKLET_RETENTION_DAYS=0` disables pruning entirely.
