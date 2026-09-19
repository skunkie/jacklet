<!--
SPDX-FileCopyrightText: 2026 TorrPlay

SPDX-License-Identifier: MIT
-->

# Adding an indexer

[← Documentation index](../README.md#documentation)

Drop a Cardigann YAML definition into `definitions/` — no restart or code
change needed. Definitions are cached and re-read only when a file actually
changes, so adding, editing, or removing one takes effect on the next
request without re-parsing the whole directory each time. The directory is
git-ignored: definitions are yours, and Jacklet tracks none of its own.

Definitions come from [Jackett's `Definitions/`
directory](https://github.com/Jackett/Jackett/tree/master/src/Jackett.Common/Definitions)
and are used unmodified.

A file that fails to parse is skipped, and logged with its path and the
parse error, rather than taking down the rest. If it parsed successfully
before, the last working version keeps being served until the file is
fixed, so a syntax error saved mid-edit doesn't take a live indexer down.

The supported subset of the Cardigann format covers:

- `search.paths` (per-path `method`, `inputs`, `inheritinputs`,
  `categories`, `followredirect`, and `response.type` of `json` or `xml`),
  `search.inputs` (including `$raw`),
  `search.headers`, `search.keywordsfilters`, `search.preprocessingfilters`,
  and `search.error`
- `rows` with `selector`, `after`, `remove`, and `dateheaders`
- `fields` with `selector`, `attribute`, `text`, `filters`, `case`,
  `remove`, and `optional`
- `caps.modes`, `caps.categorymappings`, and `caps.allowrawsearch`
- `login` with the `form`, `post`, `get`, and `cookie` methods, including
  `test`, `error`, and hidden form fields such as CSRF tokens
- the `join` and `re_replace` template functions
- the template variables `{{ .Keywords }}`, `{{ .Categories }}`,
  `{{ .Config.* }}` (including `sitelink`, which Jacklet supplies),
  `{{ .Query.* }}` (including `Keywords`, the terms before this
  definition's `keywordsfilters` rewrote them), `{{ .Result.* }}`,
  `{{ .Today }}` / `{{ .Today.Year }}`,
  and `{{ .True }}` / `{{ .False }}`
- the `re_replace`, `replace`, `regexp`, `querystring`, `trim`,
  `tolower`, `toupper`, `append`, `prepend`, `split`, `urlencode`,
  `urldecode`, `htmldecode`, `htmlencode`, `validate`, `validfilename`,
  `diacritics`, `reverse`, `jsonjoinarray`, `hexdump`, `strdump`,
  `dateparse`, `timeparse`, `timeago`, `reltime`, and `fuzzytime`
  filters, each following Jackett's semantics so that a definition
  written against Jackett yields the same values here

Cardigann's `andmatch` is a **row** filter rather than a value filter, so
it belongs under `search.rows.filters` and not in a field's `filters`.
Both of Jackett's row filters are supported there:

- `andmatch` drops a row whose title does not contain every word of the
  query, for a tracker whose own search ORs the terms together. Its
  optional argument limits how many characters of the query are compared,
  for a tracker that lists truncated titles. It stands down when the
  search was by an external id the definition advertises.
- `strdump` logs the row as the tracker served it — markup, JSON or XML — and keeps it, for working out why a selector matches nothing.

Downloads are fetched directly rather than through FlareSolverr even when
one is configured, since FlareSolverr returns a rendered page as text and
would corrupt a torrent file.

With FlareSolverr configured, a definition's `search.headers` do not reach
the tracker: the request is made by a real browser, which supplies its own,
and FlareSolverr no longer accepts overrides. Jacklet logs a warning naming
the headers and the indexer. Downloads, being fetched directly, still send
them.

The target is Jackett's definition set whole, not a curated subset of it:
a definition that fails to parse, or that leaves a template expression
unrendered, is a bug in Jacklet rather than an unsupported file. Whether
any given definition parses is visible in the log, which names the file
and the parse error as the definition is read. The admin panel lists the
same failures under "Definition errors", but it is served only when an
admin password is configured, so on a headless install the log is the
only surface.

As in Jackett, a search does not follow redirects unless its path sets
`followredirect: true`. A tracker whose session has lapsed answers by
redirecting to its login page, and following that would scrape the login
page as if it were results.

CAPTCHA-gated logins are not supported either: a definition declaring one
is reported as unusable rather than failing with a confusing credential
error.

## Private trackers

A definition with a `login:` block needs credentials, which come from its
own `username`/`password` settings. Put them in
`<JACKLET_CONFIG_DIR>/<indexer-id>.yml` (see
[Configuration](configuration.md)):

```yaml
username: your-account
password: your-password
```

A definition using `method: cookie` instead takes a `cookie:` value there.
Sessions are established once and reused; Jacklet re-authenticates when a
session lapses.
