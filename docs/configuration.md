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
another program's.

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
| `BASE_URL`         | `-base-url`          | *(request origin)* | External HTTP(S) origin used in generated links; set this behind a TLS-terminating reverse proxy. |
| `CONTACT_EMAIL`    | `-contact-email`     | *(unset)*     | Operator address advertised as the Torznab caps server email and the feed's `webMaster`. Must be a bare address, with no display name; unset omits both. |
| `PORT`             | `-port`              | `9117`        | Port to listen on.                                             |
| `API_KEY`          | *(none)*             | *(unset)*     | Required `apikey` query parameter. Unset leaves the API open.  |
| `DEFINITIONS_DIR`  | `-definitions-dir`   | `definitions` beside the binary | Directory of Cardigann `*.yml` indexer definitions. Unset, it resolves next to the executable. |
| `CONFIG_DIR`       | `-config-dir`        | `config`      | Directory of per-indexer setting overrides.                    |
| `DB_PATH`          | `-db-path`           | `jacklet.db`  | SQLite database file.                                          |
| `FLARESOLVERR_URL` | `-flaresolverr-url`  | *(unset)*     | FlareSolverr endpoint, for trackers behind an anti-bot challenge. |
| `RETENTION_DAYS`   | `-retention-days`    | `30`          | Days to keep a scraped torrent. `0` keeps everything.          |
| `LOG_FILE`         | `-log-file`          | *(stdout)*    | File to write logs to. Empty logs to standard output.          |
| `ADMIN_PASSWORD`   | *(none)*             | *(unset)*     | Plaintext password for the web admin panel.                    |
| `ADMIN_PASSWORD_HASH` | *(none)*          | *(unset)*     | Hashed admin password; takes precedence over the plaintext one. |

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

## Per-indexer settings

A definition's `settings:` block supplies defaults for its `{{ .Config.* }}`
template values. To override one, create `<JACKLET_CONFIG_DIR>/<indexer-id>.yml`
containing a flat mapping of setting name to value:

```yaml
sort: 10
type: desc
```

A missing file simply means no overrides.
