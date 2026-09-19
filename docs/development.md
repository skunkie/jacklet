<!--
SPDX-FileCopyrightText: 2026 TorrPlay

SPDX-License-Identifier: MIT
-->

# Development

[← Documentation index](../README.md#documentation)

```bash
go build ./...
go test ./...
golangci-lint run
reuse lint
```

## Windows

Jacklet runs as a Windows service, which is code a Linux build never
compiles and a Linux lint run never sees. Cross-build and cross-lint it
before committing:

```bash
GOOS=windows GOARCH=amd64 go build ./...
GOOS=windows GOARCH=arm64 go vet ./...
GOOS=windows golangci-lint run
```

That covers everything but behavior that genuinely differs. Renaming a
file that is still open fails on Windows and succeeds on Linux, and the
service and registry code cannot run at all here, so CI runs the test
suite on a Windows runner too; a change to any of it is verified there.

Definitions are read from `definitions/` next to the executable, so
building into the repository root (as in the [quick
start](../README.md#quick-start)) puts them where Jacklet looks. `go run`
builds into a temporary directory, so point `JACKLET_DEFINITIONS_DIR` at
the checkout to use it:

```bash
JACKLET_DEFINITIONS_DIR=definitions go run ./cmd/jacklet
```

The release workflow builds the `.deb` and the `.rpm` with
[nfpm](https://nfpm.goreleaser.com/), from `packaging/nfpm.yaml`. The
packages install manual pages, which `packaging/mkman` renders first:
`jacklet.1` from `packaging/jacklet.1.md`, and a section 7 page from each
of the `docs/` pages that describes a running Jacklet. Those docs are
adapted as they are read rather than edited, so nothing in `docs/` has to
carry manual page markup.

`packaging/mkman` is a module of its own, which keeps the markdown
renderer out of Jacklet's dependencies. So are `packaging/mkico`, which
keeps a rasterizer out of them, and `packaging/wixcheck`. None is covered
by `go test ./...` or `golangci-lint run` at the root, so run those there
as well:

```bash
go -C packaging/mkman test ./...
(cd packaging/mkman && golangci-lint run)
go -C packaging/mkico test ./...
(cd packaging/mkico && golangci-lint run)
go -C packaging/wixcheck test ./...
(cd packaging/wixcheck && golangci-lint run)
```

`packaging/wixcheck` reads the Windows installer's authoring and reports
what nothing else does. The toolset rejects authoring that is malformed;
these are the mistakes that build, install, and then do something other
than they read as — an assignment that can never fire, a dialog nothing
references and so is silently dropped, a check box that renders ticked
while the thing it offers is off. Each one reached a release before it
was noticed. It reads the source, not a package, so it needs no Windows:

```bash
go -C packaging/wixcheck run . ../windows/jacklet.wxs ../windows/jacklet.ui.wxs
```

One thing it cannot judge from the source alone is whether the wizard's
navigation is reachable, because half of the wizard comes from the
toolset's own dialog library and the two only meet once a package has been
linked. Given a built intermediate it reads that table instead:

```bash
go -C packaging/wixcheck run . -ipl jacklet.wixipl
```

CI builds one for this, which costs no more than the package it already
builds.

## Icon and installer artwork

`assets/logo.svg` is the master logo. `assets/logo-wordmark.svg` sets the
same mark beside the product name for the README, and repeats its paths
rather than referencing them, so a test in `cmd/jacklet` compares the two
and fails when an edit to the master leaves the wordmark behind.
`packaging/mkico` derives everything else from the master — the Windows
icon, the resource object that carries that icon and the version details
into `jacklet.exe`, the installer's two bitmaps, and the documentation
PNGs — so nothing derived is committed and an icon cannot disagree with
the logo it came from. The wizard's colour is derived as well: the
generator reads the badge's fill out of the master and lightens it for
the panel beside the mark, rather than keeping a second copy of the value
that an edit to the logo would leave behind:

```bash
go -C packaging/mkico run . --out dist --syso dist/resource_windows_amd64.syso
```

A plain `go build` therefore produces an executable with no icon, which is
intended: branding is a release-build concern, and the ordinary
development loop needs no rasterizer. The release workflow generates the
resource object into `cmd/jacklet/` before the Windows builds, where the
linker picks it up by its `GOOS` and `GOARCH` suffix.

The adaptation holds the `docs/` pages to the shape they have now. A link
it cannot turn into a manual page reference — one with an anchor, or to a
page that is not installed — and a `docs/` page it has never been told
about are both errors, so that a documentation change either converts or
fails. CI renders the pages on every pull request and checks them with
`groff -ww`, which is where such a failure surfaces.

To build a package locally, render the pages, put the binary where
`nfpm.yaml` expects it, and supply the three variables the workflow
supplies — an unset one expands to nothing rather than failing:

```bash
go -C packaging/mkman run . --version v1.2.3
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o dist/jacklet ./cmd/jacklet
ARCH=arm64 VERSION=v1.2.3 REPO_URL=https://github.com/torrplay/jacklet \
  go run github.com/goreleaser/nfpm/v2/cmd/nfpm@v2.47.0 pkg \
  --config packaging/nfpm.yaml --packager deb --target out/
```

`man -l man/man1/jacklet.1.gz` reads a rendered page without installing it.

The release notes are generated from the commit log by
[git-cliff](https://git-cliff.org/), from `cliff.toml`. Each commit subject
is read as `type(scope): summary`: the type picks the section it lands in
(`feat` under Features, `fix` under Bug fixes, and so on), the scope becomes
the bold lead-in, and the rest is the entry. A subject with no such prefix,
and one whose type `cliff.toml` lists as housekeeping, is left out. Subjects
alone make up the notes, so a change earns its line there through the way it
is summarized.

With git-cliff installed (a binary from its
[releases](https://github.com/orhun/git-cliff/releases), a distribution
package, or `cargo install git-cliff`), this previews what the next tag
would say:

```bash
git cliff --unreleased
```

`jacklet version` prints the version, commit and build time of a binary.
Release builds stamp the tag in with `-ldflags "-X main.version=v1.2.3"`;
without that, the version falls back to the module version and the VCS
revision the Go toolchain records, so a build from source still reports
truthfully, and marks a dirty working tree. The same details are logged
at startup.
