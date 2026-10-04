<!--
SPDX-FileCopyrightText: 2026 TorrPlay

SPDX-License-Identifier: MIT
-->

# jacklet 1 "@DATE@" "jacklet @VERSION@" "User Commands"

## NAME

jacklet - Torznab/Newznab indexer proxy

## SYNOPSIS

**jacklet** [_options_]

**jacklet hash-password**

**jacklet service** [_verb_]

**jacklet version**

## DESCRIPTION

**jacklet** scrapes torrent sites described by Cardigann YAML definitions
and serves them over a Torznab/Newznab API, for Sonarr, Radarr, Prowlarr
and anything else that speaks Torznab. Each definition becomes its own
endpoint, and the reserved id **all** searches every one of them at once
and returns a merged feed.

It ships no definitions of its own: they describe specific third-party
sites and change independently of the program. Starting with none works,
and is reported at startup.

A search re-scrapes the indexer live, stores what it finds in SQLite, and
answers from that store, which also serves as the fallback when a tracker
is unreachable.

## COMMANDS

**hash-password**
    Read a password from standard input and print a hash for
    **JACKLET_ADMIN_PASSWORD_HASH**, so the password itself need not appear
    in a deployment configuration. From a terminal it asks twice without
    echoing what is typed; otherwise it reads the first line, so the
    password can be piped in.

**service** [_verb_]
    Manage the Windows service, on Windows. **install** registers the
    service and its event log source; **uninstall** removes both and the
    registry settings while leaving the state directory in place. **start**,
    **stop** and **status** control and report the service, and **config**
    prints the configured settings with credentials masked. **config set**
    _NAME_[=_VALUE_] changes one setting, reading the value from standard
    input when none is given, and **config unset** _NAME_ removes one.
    Each _NAME_ is the Windows-style PascalCase name the setting goes by in
    the registry, such as **ApiKey**, **Port** or **LogFile**. Settings are
    stored under _HKLM\SOFTWARE\Jacklet\Settings_ and read by the
    service itself when it next starts, which is when a change takes
    effect. Run the verb with no arguments for the full list.

**version**
    Print the version, revision and build time.

## OPTIONS

Every option defaults to the matching **JACKLET_** variable described under
ENVIRONMENT, and falls back to a built-in default. Credentials have no
option: an argument is readable by every other process on the machine for
as long as the server runs, and lands in shell history besides.

**-admin-prefix** _path_
    Path the web administration panel is served under, **/admin** by
    default. It must begin with a slash and must not end with one, and may
    contain only letters, digits and **.**, **_**, **~**, **-** and **/**;
    anything else, or a path the server already serves such as **/docs**,
    fails startup. Use it to serve the panel behind a reverse
    proxy at a path of its own.

**-base-url** _url_
    External HTTP(S) origin used in generated links. Set this behind a
    TLS-terminating reverse proxy.

**-config-dir** _directory_
    Directory of per-indexer setting overrides.

**-contact-email** _address_
    Operator address advertised as the Torznab caps server email and the
    feed's **webMaster**, which some clients show to the end user. Empty,
    the default, omits both. It must be a bare address, with no display
    name, since the caps attribute has nowhere to put one; a malformed
    address fails startup.

**-db-path** _file_
    SQLite database file.

**-definitions-dir** _directory_
    Directory of Cardigann indexer definitions. Unset, it resolves next to
    the executable.

**-flaresolverr-sessions** _count_
    How many FlareSolverr browser sessions are kept alive at once, 8 by
    default. Each is a browser on the FlareSolverr host. It should cover
    the trackers that use FlareSolverr: with more of them than this, an
    aggregate search closes the session the next tracker needs, and each
    solves its challenge and signs in again on every search. It must be at
    least 1.

**-flaresolverr-url** _url_
    FlareSolverr address, such as **http://localhost:8191**, for trackers
    behind an anti-bot challenge. **/v1** is added to an address with no
    path; one with a path, such as behind a reverse proxy, is used as it is
    and must end in **/v1**. A tracker uses it once its own
    **flaresolverr** setting is on, in its file under the configuration
    directory. Its credentials and cookies then pass through FlareSolverr,
    so run it on a trusted network or behind TLS.

**-log-file** _file_
    File to write logs to. Empty, the default, logs to standard output, so
    that a service manager or container runtime owns log routing and
    retention. A named file is capped and rotated in place, for a
    deployment where nothing else would do it.

**-log-level** _level_
    Least severe level written to the log: **debug**, **info**, **warn** or
    **error**, **info** by default. **debug** adds why each row was skipped,
    the waits between requests, and whatever a definition dumps with its
    **strdump** or **hexdump** filter, which can include a tracker's links
    and the passkey in them, so it is for diagnosing a definition rather
    than for leaving on. Any other value fails startup.

**-magnet-trackers** _urls_
    Comma-separated announce URLs (**udp**, **http**, **https** or **wss**)
    added to a magnet built from an info hash, so a client finds peers
    through them as well as through the DHT. Empty, the default, builds
    trackerless magnets. A magnet a tracker supplied is left as it is.
    Magnets are served to every client, so use only public announce URLs,
    and percent-encode a comma inside one. A change applies to rows
    scraped afterwards.

**-port** _port_
    Port to listen on, from 1 to 65535.

**-retention-days** _days_
    Days to keep a scraped torrent. **0** keeps everything.

**-trusted-proxies** _addresses_
    Comma-separated addresses or CIDR ranges of reverse proxies whose
    **X-Forwarded-For** header names the client signing in to the admin
    panel, so sign-in attempts are spaced per client rather than per
    proxy. The header is read from the right, skipping the proxies listed,
    and is ignored on a request from any other address. List only the
    proxies' own addresses, never a range clients also sit in, since a
    client inside one can choose the address it is counted under. Empty, the
    default, trusts no header. Anything that is not an address or range
    fails startup.

**-user-agent** _string_
    User-Agent sent to trackers. Empty, the default, sends a desktop
    browser's built into the release; set it when a tracker refuses that
    one as out of date. A definition that declares its own still sends
    that, and a tracker using FlareSolverr presents the browser's. A value
    containing a control character fails startup.

**-help**, **-version**
    Print the full help, or the version, and exit.

## ENVIRONMENT

Every variable is named **JACKLET_**_NAME_. A shared environment -- several
services in one compose file, or one systemd **EnvironmentFile=** -- is a
shared namespace, where a bare **API_KEY** or **DB_PATH** is liable to be
another program's.

**JACKLET_API_KEY**
    Required **apikey** query parameter. Unset leaves every indexer
    endpoint open to anyone who can reach the port.

**JACKLET_ADMIN_PASSWORD**, **JACKLET_ADMIN_PASSWORD_HASH**
    Enable the web administration panel, at **/admin** unless
    **-admin-prefix** says otherwise. The hash, from
    **jacklet hash-password**, takes precedence over the plaintext form.

Each credential also reads a **_FILE** variant naming a file to take the
value from, which is how Docker and Kubernetes deliver a secret and keeps
it out of the environment entirely. Setting both a credential and its
**_FILE** form is an error.

The remaining variables mirror the options above:
**JACKLET_ADMIN_PREFIX**, **JACKLET_BASE_URL**, **JACKLET_CONFIG_DIR**, **JACKLET_CONTACT_EMAIL**,
**JACKLET_DB_PATH**, **JACKLET_DEFINITIONS_DIR**,
**JACKLET_FLARESOLVERR_SESSIONS**, **JACKLET_FLARESOLVERR_URL**,
**JACKLET_LOG_FILE**, **JACKLET_LOG_LEVEL**, **JACKLET_MAGNET_TRACKERS**,
**JACKLET_PORT**, **JACKLET_RETENTION_DAYS**, **JACKLET_TRUSTED_PROXIES**
and **JACKLET_USER_AGENT**.

## FILES

These are the paths the Debian and RPM packages configure; a build from
source resolves definitions next to the executable and writes its database
into the working directory.

_/etc/jacklet/jacklet.env_
    Settings for the **jacklet** service, read by systemd as an
    **EnvironmentFile=**. A package upgrade preserves it.

_/var/lib/jacklet/definitions_
    Cardigann indexer definitions.

_/var/lib/jacklet/config_
    Per-indexer setting overrides, which hold tracker credentials in
    plaintext.

_/var/lib/jacklet/jacklet.db_
    The scraped-results database.

## EXIT STATUS

**jacklet** exits 0 on a clean shutdown, and non-zero when the
configuration is invalid or the server cannot start.

## SEE ALSO

jacklet-configuration(7), jacklet-definitions(7), jacklet-admin(7),
jacklet-internals(7)

The **/docs** endpoint serves an interactive API reference, and
**/api/v2.0/indexers** lists every configured indexer.
