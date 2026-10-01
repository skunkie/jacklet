<!--
SPDX-FileCopyrightText: 2026 TorrPlay

SPDX-License-Identifier: MIT
-->

# Windows

[← Documentation index](../README.md#documentation)

Jacklet runs as a Windows service. The installer is the usual way in; the
archive works too, and configures the same service the same way.

## Install

Download `jacklet_<version>_windows_amd64.msi` (or `_arm64` for a Windows
on ARM machine) from the releases page and run it.

The wizard asks for:

- **An API key**, which is required. Clients send it as the `apikey` query
  parameter. Without one, every indexer endpoint answers anyone who can
  reach the port, which is why the wizard will not continue until it has
  one. Any long random string will do.
- **A port**, `9117` by default.
- **An admin password**, which is optional and enables the web
  administration panel at `/admin`. The installer stores it where only
  administrators and the service can read it; the first start replaces it
  with a hash and deletes it. So it exists in plaintext on disk between
  the install and the first start, and in a verbose installer log if one
  was asked for. To avoid both, leave it empty here and set the hash
  afterwards with `jacklet service config set AdminPasswordHash`,
  which reads the value from standard input.
- **A firewall rule**, off by default. Leave it off unless something on
  your network needs to reach Jacklet; a client on the same machine does
  not.

The service is installed, started, and set to start with Windows. Neither
the installer nor the binary is signed, which is why Windows objects to
both; [Unsigned downloads](#unsigned-downloads) below is what that looks
like and what to do about it.

To install without the wizard, supply at least the API key:

```powershell
msiexec /i jacklet_v1.2.3_windows_amd64.msi /qn JACKLETAPIKEY=<key>
```

A silent install with no `JACKLETAPIKEY` is refused rather than producing
an unauthenticated proxy. `JACKLETADMINPASSWORD`, `JACKLETPORT` and
`ADDFIREWALLRULE=1` are also accepted, and a silent install accepts the
licence, as every silent install does.

An installer log written with `/l*v` records every value the installer
stores, so it contains both the API key and the admin password. Treat
such a log as a credential: delete it once it has served its purpose, and
redact it before attaching it to a bug report. Nothing about the install
makes this avoidable, so the only log that holds no credentials is the
one you did not ask for.

### Unsigned downloads

Jacklet is published unsigned, so an ordinary install runs into two
separate Windows defences.

**SmartScreen** shows a blue "Windows protected your PC" panel before the
installer runs. *More info* reveals the *Run anyway* button.

**Microsoft Defender** may quarantine `jacklet.exe` or the installer
outright, usually as `Trojan:Win32/Sabsik.FL.A!ml`. The `!ml` suffix says
this is a machine-learning verdict rather than a match against a known
sample: nothing was recognised, something was guessed. A statically
linked Go binary that no one has signed and that almost no one has
downloaded is the shape it guesses from, so a clean build of this
project trips it much as any other would.

There is no version of Jacklet that avoids this while it stays unsigned,
and a detection that is reversed today can come back on the next
release, because the verdict attaches to a file nobody has seen before.
What is available instead:

1. **Check the download first.** Every release publishes `SHA256SUMS`.
   A file that matches is the one the build produced; a file that does
   not is a reason to stop, whatever Defender says about it.

   ```powershell
   (Get-FileHash .\jacklet_v1.2.3_windows_amd64.msi -Algorithm SHA256).Hash
   ```

2. **Report it to Microsoft** at
   <https://www.microsoft.com/en-us/wdsi/filesubmission>, as a software
   developer or as a customer, marking it a false positive. Submissions
   are usually answered within a day or two, and a reversal reaches every
   machine through the cloud rather than needing anything done locally.
   This is worth doing even if you are about to exclude the file, since
   it is the only route that helps the next person.

3. **Exclude it locally**, if you need the service running now. Prefer a
   path exclusion over restoring from quarantine, and keep it narrow:

   ```powershell
   Add-MpPreference -ExclusionPath 'C:\Program Files\Jacklet\jacklet.exe'
   ```

   An exclusion turns the scanner off for that path permanently, so it
   is worth removing once a submission comes back.

The durable fix is an Authenticode signature. That is a decision about
money and identity rather than about code, and it has not been taken
yet.

### Upgrading

Installing a newer version over an older one keeps the settings already in
place, and so does not ask for them again: the wizard goes straight from
the install folder to the confirmation, and a silent upgrade needs no
`JACKLETAPIKEY`. Change settings with `jacklet service config`, below,
rather than by reinstalling.

What decides this is whether an API key is configured, not whether the
install is an upgrade. An upgrade over an installation that has none is
asked for one, which is what makes an installation that somehow lost its
settings recoverable by installing over it.

Removing Jacklet removes its settings, including the admin password hash.
A later install is then a fresh one: it asks again, and an empty password
field really does leave the panel disabled rather than quietly keeping
the previous one.

## Where things are

| Path | Holds |
| ---- | ----- |
| `C:\Program Files\Jacklet\` | `jacklet.exe` and the documentation. Replaced on an upgrade. |
| `C:\ProgramData\Jacklet\definitions\` | Cardigann indexer definitions. Jacklet ships none; put them here. |
| `C:\ProgramData\Jacklet\config\` | Per-indexer setting overrides, which hold tracker credentials in plaintext. |
| `C:\ProgramData\Jacklet\jacklet.db` | The scraped-results database. |
| `C:\ProgramData\Jacklet\logs\jacklet.log` | The log, capped and rotated in place. |

Uninstalling removes the program and the service and leaves
`C:\ProgramData\Jacklet` alone: the database, the definitions and the
tracker credentials are yours.

The service runs as `NT AUTHORITY\LocalService`, an unprivileged built-in
account with no rights beyond making outbound requests and writing the
directories above.

## Settings

The service reads its settings from `HKLM\SOFTWARE\Jacklet\Settings`,
one value per setting using Windows-style PascalCase names. At startup
Jacklet translates those values to the corresponding `JACKLET_*`
configuration variables; an environment variable, where there is one,
still wins over the registry value.

Read them back with:

```powershell
jacklet service config
```

Credentials are shown masked. To change one, from an elevated prompt:

```powershell
jacklet service config set Port=9118
jacklet service config unset FlareSolverrUrl
```

Written with no value, the value is read from standard input instead, so a
credential stays out of the process list and the command history:

```powershell
jacklet service config set ApiKey
```

A change takes effect when the service next starts, which is when the
settings are read:

```powershell
Restart-Service Jacklet
```

To replace the admin password, hash it first and store the hash:

```powershell
jacklet hash-password
jacklet service config set AdminPasswordHash
```

## Logs

Two places, for two purposes.

The full log is `C:\ProgramData\Jacklet\logs\jacklet.log`, one JSON record
per line, capped and rotated in place with a few older generations beside
it. That is where a search that went wrong is explained.

Lifecycle events — started, stopped, failed to start — also go to the
Application event log under the source `Jacklet`, which is where to look
when the service will not start at all:

```powershell
Get-WinEvent -LogName Application -MaxEvents 20 |
  Where-Object ProviderName -eq Jacklet
```

## From the archive

`jacklet_<version>_windows_<arch>.zip` carries the same executable without
the installer. Unpack it somewhere it can stay, then from an elevated
prompt:

```powershell
.\jacklet.exe service install
.\jacklet.exe service config set ApiKey
.\jacklet.exe service start
```

`service install` registers the service and its event log source, creates
`C:\ProgramData\Jacklet` with the permissions the service account needs,
and records the same absolute paths the installer does. Unlike the
installer it leaves the service stopped and set to start manually, because
nothing has asked for an API key yet.

`service status` reports the state, `service stop` and `service uninstall`
do what they say, and `service` on its own lists every verb. Uninstalling
removes the registry settings, including credentials, while leaving the
state under `C:\ProgramData\Jacklet` in place.

`service status` only queries, and asks the Service Control Manager for no
more than that, so it runs from an ordinary prompt. The verbs that change
something need an elevated one.

Run `jacklet.exe` from a terminal and it serves in the foreground, logs
to standard output, and takes its settings from flags and the
environment. The registry settings belong to the installed service, and
are read only when Jacklet starts as one.
