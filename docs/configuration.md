<!--
SPDX-FileCopyrightText: 2026 TorrPlay

SPDX-License-Identifier: MIT
-->

# Configuration

[← Documentation index](../README.md#documentation)

All settings are optional, and each is read from a command-line flag, then
the environment, then a built-in default. `jacklet --help` lists them.

```bash
jacklet -port 9117 -definitions-dir /var/lib/jacklet/definitions
```

Every variable is named `JACKLET_<NAME>`: a shared environment — several
services in one compose file, or one systemd `EnvironmentFile=` — is a
shared namespace, where a bare `API_KEY` or `DB_PATH` is liable to be
another program's. The proxy variables, described under "Outbound proxy"
below, are the one exception: they are the standard names every HTTP
client reads, with no flag, so sharing them across services is usually
the point.

The three credentials have no flag: an argument is readable by every other
process on the machine for as long as the server runs, and lands in shell
history besides. Each also reads a `_FILE` variant naming a file to take
the value from, which is how Docker and Kubernetes deliver a secret and
keeps it out of the environment entirely:

```bash
docker run -e JACKLET_API_KEY_FILE=/run/secrets/jacklet-api-key ...
```

Setting both a credential and its `_FILE` form is an error.

| Variable (`JACKLET_` prefix) | Flag       | Default       | Purpose                                                        |
| ------------------ | -------------------- | ------------- | -------------------------------------------------------------- |
| `ADMIN_PASSWORD`   | *(none)*             | *(unset)*     | Plaintext password for the web admin panel.                    |
| `ADMIN_PASSWORD_HASH` | *(none)*          | *(unset)*     | Hashed admin password; takes precedence over the plaintext one. |
| `ADMIN_PREFIX`     | `-admin-prefix`      | `/admin`      | Path the admin panel is served under. It must begin with a slash and not end with one, and may contain only letters, digits and `.`, `_`, `~`, `-` and `/`; anything else, or a path the server already serves such as `/docs`, fails startup. |
| `API_KEY`          | *(none)*             | *(unset)*     | Required `apikey` query parameter. Unset leaves the API open.  |
| `BASE_URL`         | `-base-url`          | *(request origin)* | External HTTP(S) origin used in generated links; set this behind a TLS-terminating reverse proxy. |
| `CONFIG_DIR`       | `-config-dir`        | `config`      | Directory of per-indexer setting overrides.                    |
| `CONTACT_EMAIL`    | `-contact-email`     | *(unset)*     | Operator address advertised as the Torznab caps server email and the feed's `webMaster`. Must be a bare address, with no display name; unset omits both. |
| `DB_PATH`          | `-db-path`           | `jacklet.db`  | SQLite database file.                                          |
| `DEFINITIONS_DIR`  | `-definitions-dir`   | `definitions` beside the binary | Directory of Cardigann `*.yml` indexer definitions. Unset, it resolves next to the executable. |
| `FLARESOLVERR_SESSIONS` | `-flaresolverr-sessions` | `8` | FlareSolverr browser sessions kept alive at once. It should cover the trackers that use FlareSolverr (see below). |
| `FLARESOLVERR_URL` | `-flaresolverr-url`  | *(unset)*     | FlareSolverr address, such as `http://localhost:8191`. `/v1` is added to an address with no path; one with a path, such as behind a reverse proxy, is used as it is and must end in `/v1`. For trackers behind an anti-bot challenge. Setting it changes nothing by itself: a tracker uses it once its own `flaresolverr` setting is on (see below). |
| `LOG_FILE`         | `-log-file`          | *(stdout)*    | File to write logs to. Empty logs to standard output.          |
| `LOG_LEVEL`        | `-log-level`         | `info`        | Least severe level logged: `debug`, `info`, `warn` or `error`. Any other value fails startup. See [Logging](#logging). |
| `MAGNET_TRACKERS`  | `-magnet-trackers`   | *(unset)*     | Comma-separated announce URLs (`udp`, `http`, `https` or `wss`) added as `tr=` parameters to a magnet built from an info hash, so a client finds peers through them as well as the DHT. Unset builds trackerless magnets. A magnet a tracker supplied is left as it is. Magnets are served to every client, so use only public announce URLs, and percent-encode a comma inside one. A change applies to rows scraped afterwards. |
| `PORT`             | `-port`              | `9117`        | Port to listen on, from 1 to 65535.                            |
| `RETENTION_DAYS`   | `-retention-days`    | `30`          | Days to keep a scraped torrent. `0` keeps everything.          |
| `TRUSTED_PROXIES`  | `-trusted-proxies`   | *(unset)*     | Comma-separated addresses or CIDR ranges of reverse proxies whose `X-Forwarded-For` header names the client signing in to the admin panel, so sign-in attempts are spaced per client rather than per proxy. List only the proxies' own addresses, never a range clients also sit in, since a client inside one can choose the address it is counted under. Unset trusts no header. Anything that is not an address or range fails startup. |
| `USER_AGENT`       | `-user-agent`        | *(a desktop browser's)* | User-Agent sent to trackers. Set it when a tracker refuses the built-in one as out of date. A definition that declares its own still sends that, and a tracker using FlareSolverr presents the browser's. A value containing a control character fails startup. |

Leaving `JACKLET_API_KEY` unset logs a warning at startup and leaves every indexer
endpoint unauthenticated.

The Debian and RPM packages carry these as an `EnvironmentFile=` at
`/etc/jacklet/jacklet.env`, which the unit reads at startup, so
`systemctl restart jacklet` is what applies an edit. That file already
points `DEFINITIONS_DIR`, `CONFIG_DIR` and `DB_PATH` under
`/var/lib/jacklet`; the rest are commented out at their defaults.

`DEFINITIONS_DIR` is covered further in [Adding an
indexer](definitions.md), and the two `ADMIN_PASSWORD` variables in
[Admin panel](admin.md).

## Windows

A Windows service reads these same settings from the registry, because a
service has no environment a shell set up for it. There they carry
Windows-style PascalCase names — `ApiKey`, `Port`, `FlareSolverrUrl` —
which Jacklet translates to the `JACKLET_*` variables above at startup. A
variable that is set still wins, so nothing about running Jacklet from a
terminal changes. `jacklet service config` reads and writes them. The [Windows documentation](windows.md) describes
that, the name each setting goes by, and the paths an installed service
uses.

## Logging

Jacklet writes structured JSON to standard output, the shape container runtimes and
log aggregators expect, so the deployment environment owns routing and retention:
`journald` under systemd, the logging driver under Docker.

Set `JACKLET_LOG_FILE` where nothing plays that role. Jacklet then caps the file and
rotates it in place, keeping a few older generations beside it, so a process that
runs for months does not fill the disk.

`JACKLET_LOG_LEVEL` sets how much is written. `info`, the default, records each
search and failure; `debug` adds why each row was skipped, the waits between
requests, and whatever a definition dumps with its `strdump` or `hexdump`
filter. That is what to turn on while working out why a definition finds
nothing, and what to turn off afterwards: a dumped value can be a tracker's
download link with its passkey in it.

## Per-indexer settings

A definition's `settings:` block supplies defaults for its `{{ .Config.* }}`
template values. To override one, create `<JACKLET_CONFIG_DIR>/<indexer-id>.yml`
containing a flat mapping of setting name to value:

```yaml
sort: 10
type: desc
```

A missing file simply means no overrides.

### FlareSolverr

`JACKLET_FLARESOLVERR_URL` names a FlareSolverr endpoint, and a tracker uses
it only when its own file says so:

```yaml
flaresolverr: true
```

Every request through FlareSolverr drives a browser, which is slow and takes
memory on the FlareSolverr host, so a tracker that needs no help with an
anti-bot challenge is fetched directly. The setting belongs to your
deployment rather than to the definition, so it is not declared in one; the
admin panel offers it as a "Use FlareSolverr" checkbox on an indexer's page
whenever an endpoint is configured. It has no effect without an endpoint.

Each tracker gets a FlareSolverr session of its own, so a challenge that
fails or a login that lapses on one tracker leaves the others alone. The
sessions are browsers, so only a limited number are kept alive at once,
`JACKLET_FLARESOLVERR_SESSIONS`, 8 by default: when a new one is needed the
least recently used idle session is closed, and that tracker solves its
challenge and signs in again on its next search. Set it to at least the
number of trackers that use FlareSolverr. An aggregate search visits them in
the same order every time, so with more of them than the limit each one finds
its session closed for the one before it, and every search solves every
challenge again; Jacklet logs each session it closes for this reason.

For a tracker that uses FlareSolverr, its credentials and cookies pass
through FlareSolverr: the login form is sent to it, a configured cookie goes
in the request, and its replies carry the browser's session cookies back.
Run it on a trusted network or behind TLS; an `https` address is accepted.

## Outbound proxy

Jacklet sends its requests to trackers — searches, logins and torrent
downloads — through the proxy named by the standard variables, which Go's
HTTP client reads rather than a `JACKLET_*` setting:

```bash
HTTPS_PROXY=http://proxy.example.org:3128
HTTP_PROXY=http://proxy.example.org:3128
NO_PROXY=flaresolverr
```

`HTTPS_PROXY` covers the many trackers served over HTTPS and `HTTP_PROXY`
the rest. A `socks5://` address works too, and credentials go in the
address, as in `http://user:password@proxy.example.org:3128`. Requests to
`localhost` and loopback addresses are never proxied, and `NO_PROXY` lists
further hosts to reach directly. The variables are read once, so a change
takes effect on restart.

A tracker that uses FlareSolverr is fetched by FlareSolverr's browser, which
ignores these variables. Give FlareSolverr the same proxy through its own
`PROXY_URL`, with `PROXY_USERNAME` and `PROXY_PASSWORD` for a proxy that
needs them; it applies to every session Jacklet opens. Use the same proxy
for both, because the torrent file is still downloaded by Jacklet, presenting
the clearance the browser earned, and a clearance is usually bound to the
address that earned it. When FlareSolverr is reached by a hostname, as in
`docker-compose.yml`, list that host in `NO_PROXY`, or Jacklet sends its own
calls to FlareSolverr through the proxy as well.

The Windows service takes its settings from the registry, which covers
only Jacklet's own settings, so `jacklet service config` cannot set a
proxy.
