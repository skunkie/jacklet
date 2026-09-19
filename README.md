<!--
SPDX-FileCopyrightText: 2026 TorrPlay

SPDX-License-Identifier: MIT
-->

# <img src="assets/logo-wordmark.svg" alt="Jacklet" width="260" height="64" />

A lightweight Go alternative to [Jackett](https://github.com/Jackett/Jackett): a
single-binary Torznab/Newznab-compatible indexer proxy that scrapes torrent
sites using YAML-defined rules.

Jacklet reads Jackett's own Cardigann-style indexer definitions, so a
definition from Jackett's `Definitions/` directory can be dropped into
`definitions/` and used without modification. It exposes each definition as
its own Torznab endpoint for Sonarr, Radarr, Prowlarr, and anything else that
speaks Torznab.

## Quick start

Jacklet ships no indexer definitions of its own — they describe specific
third-party sites and change independently of this code — so supply some
first:

```bash
mkdir -p definitions
curl -sSLo definitions/<indexer>.yml \
  https://raw.githubusercontent.com/Jackett/Jackett/master/src/Jackett.Common/Definitions/<indexer>.yml
```

Then build and run:

```bash
go build -o jacklet ./cmd/jacklet
JACKLET_API_KEY=$(openssl rand -hex 16) ./jacklet
```

Starting with an empty `definitions/` directory works — Jacklet just has
nothing to search, and says so at startup.

The server listens on port 9117 by default. Browse to `/docs` for an
interactive API reference, or `/api/v2.0/indexers` for the list of configured
indexers.

Point your client at the Torznab URL of a single indexer:

```text
http://localhost:9117/api/v2.0/indexers/<indexer-id>/results/torznab/api?apikey=<key>
```

Or add every configured indexer as one endpoint, using the reserved id
`all`:

```text
http://localhost:9117/api/v2.0/indexers/all/results/torznab/api?apikey=<key>
```

These are Jackett's own URLs, so a client already pointed at a Jackett
instance only needs its host changed. Torznab specifies query parameters
and a response format, not a URL layout, and Jackett's layout is the one
every client is set up with.

The aggregate searches every definition concurrently and returns one
merged feed, advertising the union of their categories and search modes.
An indexer that is down is skipped rather than failing the search, and
each release's download link still points at the indexer it came from.
Adding indexers individually remains the better setup where the client
supports it — Prowlarr, Sonarr and Radarr all do — since it lets the
client see, retry and disable each tracker on its own.

### Prebuilt binaries

Tagged releases publish archives for Linux, macOS and Windows on amd64 and
arm64. Each contains the `jacklet` binary, `README.md`, the `docs/`
directory, `LICENSE` and an empty `definitions/` to drop definitions
into — as above, Jacklet ships none of its own. Keep that directory next
to the binary and Jacklet finds it whatever the working directory is,
so a service manager needs no
`WorkingDirectory=`. `SHA256SUMS` in the release verifies a download.

### Debian and RPM packages

Tagged releases also publish a `.deb` and an `.rpm` for amd64 and arm64:

```bash
sudo dpkg -i jacklet_1.2.3_amd64.deb      # Debian, Ubuntu
sudo rpm -i jacklet-1.2.3-1.x86_64.rpm    # Fedora, RHEL, openSUSE
```

Either installs `/usr/bin/jacklet` and a `jacklet.service` unit that runs
as its own unprivileged `jacklet` system user, and leaves the service
stopped: Jacklet with no `JACKLET_API_KEY` serves every indexer endpoint to
anyone who can reach the port, so it should not reach the network before
you have set one. Set it in `/etc/jacklet/jacklet.env`, add definitions,
then start the service:

```bash
sudo systemctl enable --now jacklet
```

| Path                           | Holds                                                         |
| ------------------------------ | ------------------------------------------------------------- |
| `/etc/jacklet/jacklet.env`     | Settings, as `JACKLET_*` variables. An upgrade keeps your edits. |
| `/var/lib/jacklet/definitions` | Cardigann indexer definitions, which you supply.               |
| `/var/lib/jacklet/config`      | Per-indexer overrides, including tracker credentials.          |
| `/var/lib/jacklet/jacklet.db`  | The scraped-results database.                                  |

Both packages install manual pages: `jacklet(1)` for the command, and
`jacklet-configuration(7)`, `jacklet-definitions(7)`, `jacklet-admin(7)`
and `jacklet-internals(7)` for the documentation above.

Removing the package leaves `/var/lib/jacklet` and the `jacklet` user
alone: the definitions, credentials and database under it are yours, and
the package did not create them.

### Windows installer

Tagged releases also publish an `.msi` for amd64 and arm64. It installs
Jacklet as a Windows service, and its wizard asks for an API key before it
will continue, so the service is configured by the time it starts. The
program goes under `C:\Program Files\Jacklet`, and the definitions,
database, per-indexer credentials and log under
`C:\ProgramData\Jacklet`, which an uninstall leaves alone.

The `.zip` above works too: `jacklet.exe service install` registers the
same service, and `jacklet service config` configures it either way. See
[Windows](docs/windows.md) for both, and for where the logs are.

### Docker

```bash
docker run -p 9117:9117 \
  -e JACKLET_API_KEY=<key> \
  -v /path/to/definitions:/app/definitions:ro \
  -v jacklet-config:/config \
  -v jacklet-data:/data \
  ghcr.io/torrplay/jacklet:latest
```

The image is `linux/amd64` and `linux/arm64`, runs as a non-root user, and
defaults to `JACKLET_DB_PATH=/data/jacklet.db` and
`JACKLET_CONFIG_DIR=/config` so the
database and per-indexer credentials live on volumes rather than in the
container layer, where an upgrade would discard them.

`/app/definitions` is empty in the image — Jacklet ships no definitions —
so the mount over it is what gives the container something to search. Make
it a bind mount, as above, and editing a definition on the host takes
effect on the next request without restarting the container.

| Path               | Holds                                                       |
| ------------------ | ----------------------------------------------------------- |
| `/app/definitions` | Cardigann indexer definitions, which you supply. Read-only. |
| `/config`          | Per-indexer overrides, including tracker credentials.       |
| `/data`            | `jacklet.db`, the scraped-results store.                    |

`/config` holds one `<indexer-id>.yml` per indexer you have customized, and
the admin panel writes them as well as you do, so that mount is read-write.
`/data` also carries the `-wal` and `-shm` files SQLite keeps beside the
database while it is open.

The two differ in what losing them costs. `/data` is a cache: delete it and
the results are scraped again. `/config` is not — losing it means
re-entering every tracker login, and disclosing it means handing those
logins out. Back up `/config`; `/data` need not be.

`docker-compose.yml` in this repository is the same setup as a Compose
file, with the optional settings listed and commented out. It expects
`JACKLET_API_KEY` in the environment or in a `.env` file beside it, and
fails to start rather than quietly serving every indexer endpoint
unauthenticated:

```bash
echo "JACKLET_API_KEY=$(openssl rand -hex 16)" > .env
docker compose up -d
```

## Documentation

- [Configuration](docs/configuration.md) — flags, environment variables,
  secrets, and per-indexer setting overrides
- [Adding an indexer](docs/definitions.md) — where definitions come from,
  the supported Cardigann subset, and private-tracker credentials
- [Admin panel](docs/admin.md) — enabling `/admin`, the password hash, and
  what the panel exposes
- [How it works](docs/internals.md) — the scrape-store-serve pipeline,
  backoff, and retention
- [Windows](docs/windows.md) — the installer, the service, and where its
  settings and logs live
- [Development](docs/development.md) — building, testing, and versioning

## Endpoints

| Endpoint                   | Description                                                  |
| -------------------------- | ------------------------------------------------------------ |
| `/api/v2.0/indexers/{id}/results/torznab/api` | Torznab XML API for one indexer (`t=caps`, `t=search`, `t=tvsearch`, `t=movie`, `t=music`, `t=book`). The trailing `api` is Jackett's action segment and its value is ignored. |
| `/api/v2.0/indexers/all/results/torznab/api` | The same API over every configured indexer at once, plus `t=indexers` for the list of indexers behind it. `all` is reserved: a definition using it as its id is searched only through the aggregate. |
| `/api/v2.0/indexers/{id}/results` | The same search pipeline returned as JSON, in Jackett's own response shape. Jackett's own addition to Torznab, which is XML-only. |
| `/api/v2.0/indexers/{filter}/results/...` | Either endpoint above, over the indexers a filter expression selects — `status:healthy`, `!type:private+lang:en`. |
| `/api/v2.0/indexers/{id}/download/{row}` | The torrent file for one stored result, fetched with Jacklet's tracker session. Jacklet's own: Jackett's equivalent is keyed by a payload only Jackett can mint. |
| `/api/v2.0/indexers`       | JSON directory of every configured indexer, in Jackett's own response shape. |
| `/docs`                    | Interactive API reference.                                   |
| `/healthz`                 | Liveness/readiness probe; 200 when the database is reachable. |
| `/admin`                   | Web administration panel, when `JACKLET_ADMIN_PASSWORD` is set.      |

Search parameters are the Torznab standard ones: `q`, `cat`, `season`, `ep`,
the external ids (`imdbid`, `tvdbid`, `tmdbid`, `tvmazeid`, `traktid`, `rid`,
`doubanid`), `year`, `genre`, `extended`, the music and book terms
(`artist`, `album`, `track`, `label`, `author`, `title`, `publisher`), plus
`limit` (default 50, max 100) and `offset`. A search requires every word
supplied to match, so a multi-word query does not return unrelated stored
releases.

The JSON endpoint additionally accepts the names Jackett binds there —
`Query` for `q`, `Category` (or `Category[]`) for `cat` and `rageid` for
`rid`, matched without regard to case — and answers unpaged, as Jackett's
does, rather than applying the feed's default page size. It also takes
Jackett's `Tracker[]`, which narrows one search to some of the indexers the
addressed one covers. Jackett reads only its own names at that address, so
`q` and `cat` there are Jacklet's addition.

An indexer filter may stand in place of an indexer id in any search path.
It is a `name:value` term, or several joined by `+` (all must match) and
`,` (any may match), each optionally negated with a leading `!`. The names
are `type` (the definition's declared type), `lang` (a language prefix, so
`lang:en` covers `en-US`), `status` (`healthy`, `failing` or `unknown`) and
`test` (`passed` or `failed`). There is no separate test action, so `test`
reads the same state as `status`, and tags are not assigned to definitions,
so `tag:` selects nothing.

Health deliberately differs from Jackett's. Jackett calls an indexer
healthy only inside a validity window opened by a search that succeeded, so
one it has not searched yet is unknown rather than healthy and
`status:healthy` answers a freshly started instance with nothing at all.
Here health is the absence of a recorded scrape failure, so an indexer
nothing has scraped yet is healthy and every configured one is reachable
from the start.

A filter that matches none of the configured indexers returns an empty
result rather than an error: it is a valid expression that happens to
select nothing. A server with no definitions at all is the one case that
reports itself instead, and it reports itself to a filter exactly as it
does to `all`, so a client that addresses this API by filter is told too.

`limit` and `offset` also reach the tracker, for a definition that pages on
the tracker's side by building its request from `{{ .Query.Limit }}` /
`{{ .Query.Offset }}` — which is how Cardigann expresses paging, in Jackett
as well. For every other definition, asking for a later page is answered
from what is already stored rather than re-fetching the same page from the
tracker.

## License

MIT — see [LICENSE](LICENSE).

The repository follows the [REUSE](https://reuse.software/) specification:
every file carries its copyright and licensing in an SPDX header, or is
covered by an annotation in `REUSE.toml` where a header would end up in
served output. `reuse lint` checks this.
