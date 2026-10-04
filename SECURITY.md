<!--
SPDX-FileCopyrightText: 2026 TorrPlay

SPDX-License-Identifier: MIT
-->

# Security policy

## Supported versions

Security fixes go into the next release. Run the latest
[release](https://github.com/torrplay/jacklet/releases) to receive them, since
an older release is not patched separately.

## Reporting a vulnerability

Report a vulnerability privately, through GitHub's [private vulnerability
reporting](https://github.com/torrplay/jacklet/security/advisories/new), not
in a public issue, pull request or discussion. If that form is unavailable to
you, contact a maintainer directly through the contact details on their GitHub
profile, or open an issue that asks for a private channel and leaves the
details out of it.

A useful report includes:

- the version (`jacklet version`) and how it is deployed: the binary, the
  container image, a package, or the Windows service;
- the settings involved, with every secret removed;
- the request that triggers the problem and what came back;
- what an attacker gains, and what they need first, such as the API key, an
  admin session, or a crafted definition.

Leave out tracker credentials, cookies and API keys, yours or anyone else's.
If a definition is involved, a minimal fictional one that reproduces the
problem is more useful than a real site's.

You will get an acknowledgment, then an assessment of whether the report is
accepted. An accepted report is fixed in a release and disclosed in a GitHub
security advisory, which credits you unless you ask otherwise. Please keep the
details private until that advisory is published.

## Scope

In scope is anything that lets a request, or a definition, do more than
Jacklet's own rules allow. Definitions are third-party data, copied from
Jackett's repository or elsewhere, so a definition is held to what a template
is given and to its tracker's own hosts, not trusted with the process. For
example:

- reaching the indexer endpoints without the configured API key, or the admin
  panel without signing in, or bypassing its CSRF check or sign-in throttling;
- making the download endpoint, or anything else, send a request to a host
  that is not one of the tracker's own;
- a stored secret, such as a tracker password, the API key or the admin
  password, appearing in a response, a page, a log line or a file readable by
  other users;
- a definition reading a value it is not given, such as the environment, the
  API key or another tracker's settings, or reaching a file outside what it
  describes;
- a response or download that exhausts memory or disk despite the size caps;
- path traversal through a tracker id, a setting or the config directory.

Out of scope:

- an instance running without `JACKLET_API_KEY`, whose indexer endpoints are
  unauthenticated by design and say so at startup;
- an attacker who already controls the config directory, the environment or
  the database file, all of which the operator is trusted to own;
- a definition sending its own tracker's requests where it points them, which
  is what a definition is for;
- vulnerabilities in a tracker site, in FlareSolverr, or in Jackett's
  definitions themselves, which belong to their own projects;
- findings from automated scanners with no demonstrated impact;
- anything about the content a tracker indexes.
