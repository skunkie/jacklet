<!--
SPDX-FileCopyrightText: 2026 TorrPlay

SPDX-License-Identifier: MIT
-->

# Admin panel

[← Documentation index](../README.md#documentation)

Setting `JACKLET_ADMIN_PASSWORD`, or `JACKLET_ADMIN_PASSWORD_HASH`,
enables a web panel at `/admin` for:

- indexer health — scrape state, consecutive failures, backoff, login session
- how many torrents are stored per indexer and how recent they are
- definitions that failed to parse
- editing each indexer's settings and credentials, written to
  `JACKLET_CONFIG_DIR` (see [Configuration](configuration.md))
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
read -rs PASSWORD && printf '%s' "$PASSWORD" | jacklet hash-password
```

That prints a line like
`pbkdf2-sha256$600000$SGVsbG8gdGhlcmU$…`, which goes in
`JACKLET_ADMIN_PASSWORD_HASH`. The hash is salted, so the same password produces a
different hash each time, and it carries its own iteration count so an
existing hash keeps working if the default cost changes later.

`JACKLET_ADMIN_PASSWORD` accepts a plaintext password instead, which is easier to
get started with but leaves the password in your deployment configuration;
Jacklet logs a warning at startup when it is used. Setting both uses the
hash. A hash that cannot be parsed stops startup rather than silently
falling back.

The panel signs in with a session cookie (`HttpOnly`, `SameSite=Lax`,
`Secure` over TLS, and scoped to `/admin`) rather than the `JACKLET_API_KEY`
query parameter, and every state-changing request carries a CSRF token.
Sessions live in memory and
last 12 hours, or 1 hour idle, so a restart or a long pause ends them; the
next request of any kind lands on the sign-in form, and signing in returns
you to the page you asked for. Stored passwords are never
rendered back into a page: a secret is shown masked, and submitting the
mask unchanged keeps the value already stored. Config files are written
with owner-only permissions.

The panel is separate from `JACKLET_API_KEY`, which still guards the Torznab
endpoints for Sonarr and friends.

When a reverse proxy terminates TLS, set `JACKLET_BASE_URL` to the public
HTTPS origin. Generated download links then use that origin and panel session
cookies remain `Secure` even though the proxy connects to Jacklet over HTTP.
