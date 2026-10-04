<!--
SPDX-FileCopyrightText: 2026 TorrPlay

SPDX-License-Identifier: MIT
-->

# Admin panel

[← Documentation index](../README.md#documentation)

Setting `JACKLET_ADMIN_PASSWORD`, or `JACKLET_ADMIN_PASSWORD_HASH`,
enables a web panel at `/admin`, or at the path `JACKLET_ADMIN_PREFIX` names, for:

- indexer health — scrape state, consecutive failures, backoff, login session
- how many torrents are stored per indexer and how recent they are
- definitions that failed to parse
- editing each indexer's settings and credentials, written to
  `JACKLET_CONFIG_DIR` (see [Configuration](configuration.md)), including
  whether the indexer is fetched through FlareSolverr when an endpoint is
  configured
- a manual search, to check that a definition still matches the site, with
  a Torrent and a Magnet column per result. A torrent is fetched through
  Jacklet, using its tracker session, so the link works on a private
  tracker where your browser has no account; a magnet goes straight to
  your client

With neither of those set the panel is not served at all and every
`/admin` path returns 404, so a deployment that has not configured one
never exposes a way to read or write tracker credentials.

## Admin password

Prefer a hash, so the password itself never appears in a compose file,
a systemd unit, or a shell history:

```bash
jacklet hash-password
```

From a terminal it asks for the password twice without showing it; from a
pipe it reads one line, for a script that already holds the password. It
prints the hash on one line, in the form
`pbkdf2-sha256$<iterations>$<salt>$<key>`, which goes in
`JACKLET_ADMIN_PASSWORD_HASH`. The hash is salted, so the same password produces a
different hash each time, and it carries its own iteration count so an
existing hash keeps working if the default cost changes later.

The `$` between its fields is what shells and Docker Compose use to expand
a variable, so protect the hash from expansion. Single-quote it in a shell or
a Compose `.env` file, and write each `$` as `$$` in a compose file itself;
a hash that lost a field this way stops startup with a hint saying so.
`docker-compose.yml` in the repository reads it from `.env`.

`JACKLET_ADMIN_PASSWORD` accepts a plaintext password instead, which is easier to
get started with but leaves the password in your deployment configuration;
Jacklet logs a warning at startup when it is used. Setting both uses the
hash. A hash that cannot be parsed stops startup rather than silently
falling back.

The panel signs in with a session cookie (`HttpOnly`, `SameSite=Lax`,
`Secure` over TLS, and scoped to the panel's path) rather than the `JACKLET_API_KEY`
query parameter, and every state-changing request carries a CSRF token.
Sessions live in memory and
last 12 hours, or 1 hour idle, so a restart or a long pause ends them; the
next request of any kind lands on the sign-in form, and signing in returns
you to the page you asked for. Stored passwords are never
rendered back into a page: a secret is shown masked, and submitting the
mask unchanged keeps the value already stored. Config files are written
with owner-only permissions.

Sign-in attempts are spaced by half a second per client address: a second
attempt inside that window is refused with a 429 without its password being
checked, so guesses cannot be issued in parallel to get around it, and one
client's failures never hold up anyone else's sign-in. An IPv6 client counts
by its `/48`, since a network can hand a client any address in its allocation,
and signing in successfully clears the address's window.

Behind a reverse proxy every client arrives from the proxy's address, so they
would all share one window and one client's guessing would keep everyone else
waiting. Set `JACKLET_TRUSTED_PROXIES` to the proxy's address, such as
`127.0.0.1,::1` for one on the same machine, and the window is kept per
client named in the proxy's `X-Forwarded-For` header instead. The header is
read from the right, skipping the proxies listed, so a client cannot choose
the address it is counted under by sending one of its own; a request that
does not come from a listed proxy has its header ignored. List only the
proxies' own addresses, never a range clients also sit in, such as a whole
container network: a client inside a listed range is skipped like a proxy,
so it can choose its address again. In a container network, give the proxy
a fixed address and list that. The proxy has to add the address it was
reached from, appending to the header or replacing it, rather than pass a
client's header through unchanged; nginx does so with
`$proxy_add_x_forwarded_for`, and Caddy and Traefik do by default.

The panel is separate from `JACKLET_API_KEY`, which still guards the Torznab
endpoints for Sonarr and friends.

When a reverse proxy terminates TLS, set `JACKLET_BASE_URL` to the public
HTTPS origin. Generated download links then use that origin and panel session
cookies remain `Secure` even though the proxy connects to Jacklet over HTTP.
