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

- `search.paths` (per-path `path`, a template whose keywords are encoded
  into the address, `method`, which is GET unless it says `post`,
  `inputs`, `inheritinputs`, `categories`, `followredirect`, and
  `response.type` of `json` or `xml`), and an older definition's single
  `search.path`, searched as one more path,
  `search.inputs` (including `$raw`; an input that renders empty is left
  out unless `search.allowEmptyInputs` is set),
  `search.headers`, `search.keywordsfilters`, `search.preprocessingfilters`,
  and `search.error`
- `rows` with `selector`, `after`, `remove`, and `dateheaders`, and for a
  JSON answer `attribute`, `multiple`, `count`, and
  `missingAttributeEqualsNoResults`
- `fields` with `selector`, `attribute`, `text`, `filters`, `case`,
  `remove`, `optional`, and `default`, and a field key's `|optional` and
  `|append` modifiers, the latter adding a title or description to an
  earlier one
- `caps.modes`, `caps.categorymappings` (including each mapping's `desc`,
  which a row's `categorydesc` field is matched against and which makes
  the tracker's own custom category, numbered as Jackett numbers it), the
  older `caps.categories` map, `caps.allowtvsearchimdb`, and
  `caps.allowrawsearch`, which marks the caps `searchEngine="raw"`
- `login` with the `form`, `post`, `get`, and `cookie` methods, including
  `test`, `error`, hidden form fields such as CSRF tokens, a form
  login's `selectorinputs` and `selectors`, and the `cookies` a post or
  form login sends
- `download` with `selectors`, `before` (including `pathselector`),
  `infohash`, and `method`, and the top-level `testlinktorrent`, for a
  tracker whose results link to a details page rather than to the torrent
  file. Every request the block makes must name one of the tracker's own
  hosts, since each carries its session: a selector whose link points
  elsewhere is a refused download.
- `replaces`, the ids a renamed tracker had before: a request naming one
  reaches the tracker and logs a warning naming the new id, and the
  settings saved under one apply until they are saved in the admin panel,
  which moves them to the tracker's own id
- `certificates`, the SHA-1 fingerprints of a tracker's own certificates,
  trusted on its own hosts when they fail the standard verification
- `requestDelay`, the seconds left between the tracker's answer to one
  request and the sending of the next
- the `join` and `re_replace` template functions
- the template variables `{{ .Keywords }}`, `{{ .Categories }}`,
  `{{ .Config.* }}` (including `sitelink`, which Jacklet supplies),
  `{{ .Query.* }}` (including `Keywords`, the terms before this
  definition's `keywordsfilters` rewrote them), `{{ .Result.* }}`,
  `{{ .DownloadUri.* }}` in a `download` block,
  `{{ .Today }}` / `{{ .Today.Year }}`,
  and `{{ .True }}` / `{{ .False }}`
- the `re_replace`, `replace`, `regexp`, `querystring`, `trim`,
  `tolower`, `tolowercase`, `toupper`, `touppercase`, `append`, `prepend`, `split`, `urlencode`,
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

A torrent file is fetched directly rather than through FlareSolverr even
when one is configured, since FlareSolverr returns a rendered page as text
and would corrupt it. For a tracker that uses FlareSolverr the download
presents the cookies and user agent its browser last held, so a login made
through FlareSolverr, and the clearance it earned, carry over. The pages a
`download` block reads on the way, such as a details page, go through
FlareSolverr like a search's.

A tracker goes through FlareSolverr only when an endpoint is configured and
the tracker's own `flaresolverr` setting is on (see
[Configuration](configuration.md)); the rest are fetched directly.

Through FlareSolverr the tracker's HTTP status is not visible: a tracker that
answers with an error status and no page at all is reported as a failure, but an
error page that has content is read like any other page and matches no rows.

A POST search through FlareSolverr does not arrive as the definition wrote
it. FlareSolverr reads the form as UTF-8, replacing every byte that is not,
and has the browser submit a new UTF-8 form, so a tracker whose `encoding` is
not UTF-8 receives replacement characters where the search terms were and
answers with its default listing. It also drops a form field named `submit`.
A search by GET is unaffected, since its query string goes to the browser as
it is. Jacklet logs a warning once per tracker when this applies; for a tracker
that needs a legacy encoding and a POST, turn off its `flaresolverr` setting.

For a tracker that uses FlareSolverr, a definition's `search.headers` do
not reach the tracker: the request is made by a real browser, which
supplies its own, and FlareSolverr accepts no header overrides. Jacklet
logs a warning naming the headers and the indexer. Torrent files, being
fetched directly, still send them.

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

A definition-level `followredirect`, which Jackett uses to rewrite the
configured site URL when a login test is redirected to another domain, is
not implemented: Jacklet answers a moved site with mirror failover
instead of editing your configuration.

CAPTCHA-gated logins are not supported: a definition declaring one
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
