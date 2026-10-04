<!--
SPDX-FileCopyrightText: 2026 TorrPlay

SPDX-License-Identifier: MIT
-->

# Embedding Jacklet in a Go program

[← Documentation index](../README.md#documentation)

Jacklet's packages under `pkg/` can be imported by another Go program that
wants indexer scraping, the Torznab API or the admin panel without running
the `jacklet` binary. `pkg/scraper`, `pkg/torznab` and `pkg/admin` import no
storage package, so a program using them carries no SQLite driver unless it
chooses `pkg/database`.

Each package documents its exported identifiers with `go doc`. This page is
about how they fit together.

## Only scraping

A `Scraper` needs no storage. `Scrape` returns the torrents a search found:

```go
logger := slog.Default()
scrpr := scraper.New(scraper.NewConfigStore(""), "", logger)
defer scrpr.Close(ctx)

tracker, err := scraper.NewDefinitionStore("definitions", logger).Find("example-tracker")
if err != nil {
	return err
}
found, err := scrpr.Scrape(ctx, tracker, scraper.SearchParams{Query: "sample"})
```

`scraper.NewConfigStore("")` uses only the settings a definition declares;
give it a directory to layer per-indexer overrides such as credentials on top.
Definitions are Jackett's own YAML files, which Jacklet does not ship: supply
a directory of them, or implement `scraper.TrackerSource` to serve them from
somewhere else.

A search repeated within a few seconds, or against a tracker that is backing
off after a failure, fetches nothing and returns `scraper.ErrThrottled`
rather than an empty result. The caller's own copy of that search is the
answer. See [How it works](internals.md) for the rate limiting.

## The whole stack over SQLite

The scraper, the Torznab API and the admin panel have to share storage: what
the scraper finds must land where the API and the panel read, and a setting
saved in the panel must be the one the scraper reads. `pkg/jacklet` builds all
three from one store and one settings source, so they cannot be given different
ones. `pkg/database` is a SQLite store, and one store plays every role.

```go
store, err := database.Open(ctx, "jacklet.db")
if err != nil {
	return err
}
defer store.Close()

password, err := admin.NewPassword("a-long-admin-password", "")
if err != nil {
	return err
}
stack, err := jacklet.New(
	store,
	scraper.NewConfigStore("config"),
	scraper.NewDefinitionStore("definitions", logger),
	logger,
	jacklet.Options{APIKey: "an-api-key", AdminPassword: password},
)
if err != nil {
	return err
}
defer stack.Close(ctx)

handler, err := stack.Handler()
if err != nil {
	return err
}
return http.ListenAndServe(":9117", handler)
```

`Stack.Handler` serves the API under `/api/v2.0/` and, when a password was
given, the panel under its prefix. To add routes of your own, call
`Stack.Routes` on your own `*http.ServeMux` instead. It returns an error when
one of its routes, under `/api/v2.0/` or the panel's prefix, names a path the
mux already serves; the mux may then hold some of the routes, so discard it
rather than serve from it. Register your own routes before calling it, so a
clash is reported there; registered afterwards, the clash panics in your own
call instead. `Stack` also exposes the `Scraper`, `Torznab` and `Admin` it
built, and each part's own options go in the matching field of
`jacklet.Options`. The scraper's `Sink` is set from the store, so
`Options.Scraper.Sink` must be left unset.

The three can also be built one by one, with `scraper.NewWithOptions`,
`torznab.New` and `admin.New`. Then the same store must be the scraper's
`Options.Sink` and what the other two read, and the scraper and the panel must
be given the same settings source. `torznab.New` and `admin.New` log a warning
when the scraper has no sink at all, but cannot tell that it points somewhere
else, which is what `jacklet.New` rules out.

Keep one `Scraper` for the life of the program and share it. It holds the
cookie jar, the logins and the FlareSolverr sessions that make repeat searches
cheap, and `Close` destroys those sessions on shutdown.

The store is a cache that grows as trackers are re-scraped. Call
`database.Store.Prune` on a schedule to age rows out; nothing does it
automatically.

## Your own storage

These small interfaces stand between the packages and your storage:

| Interface | Package | Methods | Used by |
| --- | --- | --- | --- |
| `Sink` | `scraper` | `UpsertAllForSearch` | the scraper, to hand over each scraped page |
| `Catalog` | `torznab` | `Find`, `Search` | the Torznab handlers, to answer a search or a download |
| `Store` | `admin` | `Catalog` plus `Stats` | the admin panel |
| `ConfigSource` | `scraper` | `Overrides` | the scraper, to read a tracker's setting overrides |
| `Config` | `admin` | `ConfigSource` plus `Enabled`, `Location` and `Save` | the admin panel, to edit them |

`database.Store` implements the first three, and `scraper.ConfigStore`, which
keeps settings as files in a directory, implements the last two. One type can
implement any number of them. The scraper and the panel must be given the same
settings source, or a change saved in the panel never reaches a scrape. An
implementation of `Search` has to honor `scraper.Query` exactly as
documented there: newest release first, a total that counts every match
rather than the page, every term matched case-insensitively (non-ASCII text
included), the category and tracker lists as filters with an empty list
meaning everything, `QueryKey` limiting scraper-managed rows to those a
matching search produced, `IDs` limiting the other rows to those stored
with one of its external ids and contradicting none, and `UnsearchedTerms`
narrowing only those other rows by name. The handlers' stored fallback and the aggregate
indexer's paging depend on all of it.

`catalogtest.Run` in `pkg/torznab/catalogtest` checks an implementation
against that contract. Call it from a test, giving it a way to build an empty
catalog and to fill it:

```go
func TestMyCatalog(t *testing.T) {
	catalogtest.Run(t, func(t *testing.T) catalogtest.Fixture {
		db := newEmptyStore(t)
		return catalogtest.Fixture{
			Add:          db.AddTorrents,
			AddForSearch: db.AddTorrentsForSearch,
			Catalog:      db,
		}
	})
}
```

A type that is also an `admin.Store` is checked with `storetest.Run` in
`pkg/admin/storetest` instead, which runs the same cases and adds the ones for
`Stats`: a count and a newest release date for each tracker that has anything
stored, no entry for one that has not, and a zero date when nothing parses.
Its fixture names the store `Store` where the catalog one names it `Catalog`.

Settings have their own suites. `configtest.Run` in `pkg/scraper/configtest`
checks a `scraper.ConfigSource` and `configtest.Run` in `pkg/admin/configtest`
checks an `admin.Config`, which includes everything the first checks. What they
hold an implementation to is small but easy to get wrong: a tracker with no
overrides has an empty map and no error; a value comes back as it was saved,
so a password such as `123456` stays a string and a checkbox stays a boolean;
the returned map is the caller's own; `Save` replaces what was there, and
saving an empty map clears the tracker; and a disabled config reports nowhere
to write and refuses a save.

## Mounting the handlers

`Torznab.Handler` and `Admin.Handler` return an `http.Handler`, for a program
that routes with something other than a `*http.ServeMux`; `Routes` registers
the same endpoints on a mux.

The Torznab paths are Jackett's own and all begin `/api/v2.0/`, so mount the
handler at that prefix without stripping it: clients are configured with those
addresses. The panel is served under `admin.Options.Prefix`, `/admin` unless
set, and its links, redirects and cookie all follow the prefix, so mount its
handler there without stripping it too. `admin.ParsePrefix` checks a
configured prefix the way the panel will, for a program that reads one from
its own configuration and wants to refuse it early. `Title`, `DocsURL` and
`FaviconURL` in `admin.Options` brand the panel; a link or icon left empty is
omitted.

The panel has one shared password, the one given to `admin.NewPassword`.
Sessions are held in memory and end when the program restarts, and sign-in
attempts are limited per client address. Behind a reverse proxy that is the
proxy's address unless `admin.Options.TrustedProxies` lists it, when the
client its `X-Forwarded-For` header names is used instead; middleware that
already rewrites `Request.RemoteAddr` needs no list. See
[Admin panel](admin.md).
