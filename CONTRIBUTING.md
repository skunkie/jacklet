<!--
SPDX-FileCopyrightText: 2026 TorrPlay

SPDX-License-Identifier: MIT
-->

# Contributing

Thanks for helping with Jacklet. This page covers what a change needs before it
can be merged. Taking part is governed by the [code of
conduct](CODE_OF_CONDUCT.md).

Jacklet is a lightweight, single-binary alternative to Jackett. A small
footprint and few dependencies matter more than feature parity with Jackett's
whole plugin ecosystem, and Jackett's own indexer definitions are expected to
work here unmodified. A change that adds a heavy dependency, or that needs a
definition to be edited before it loads, is unlikely to be accepted.

## Before you start

For anything larger than a small fix, open an issue first so the approach can
be agreed before you write it. The issue forms ask for what a report needs:
the version, the definition involved, the request that was made, and what
came back.

Report a security problem privately, as the [security policy](SECURITY.md)
describes, rather than in a public issue.

## Setting up

You need a current Go toolchain, [golangci-lint](https://golangci-lint.run/)
and [reuse](https://reuse.software/). The checks a change has to pass are:

```bash
go build ./...
go test ./...
golangci-lint run
reuse lint
```

Code that only builds on Windows, such as the service and installer support,
is checked by cross-building it. [Development](docs/development.md) lists
those commands, the separate modules under `packaging/`, and how packages and
release notes are built.

## Making a change

- **Keep it cohesive.** One change and the refactors and tests it needs make
  one commit, even across several packages. Split changes only when each part
  is meaningful alone and leaves the repository working.
- **Add tests.** Every bug fix, refactor and feature comes with tests that
  cover its edge cases and error paths. For a bug fix, include a test that
  fails without the fix.
- **Assert with `testify/require`.** Use `assert` only inside a loop meant to
  report every failure, or where the assertion runs off the test's own
  goroutine, such as an `httptest` handler.
- **Build definitions inline.** `definitions/` and `config/` are not
  committed, so a test writes whatever definition it needs itself and never
  reads one from those directories.
- **Use invented sample data.** Titles in tests, fixtures and documentation
  are made up, such as "Example Release" or "Тестовый Релиз", never the names
  of real films, series, albums or books. Scrub anything captured from a live
  site before committing it.
- **Use test terms precisely.** A `stub` returns fixed answers, a `fake` is a
  working in-memory implementation, and a `mock` verifies calls. Canned data
  is a `fixture` or has a `test` prefix.
- **Follow the surrounding code.** Standard Go naming applies, struct fields
  are sorted alphabetically (case-sensitively), and boolean names read as
  predicates unless the name is a wire format's own key or is read from a
  template.
- **Comment sparingly.** Add a comment only for a reason the code cannot show.
  A doc comment goes above the declaration it explains, and history of a bug
  belongs in the commit message, not the source.
- **Keep the docs true.** A change to behavior, a setting or an endpoint
  updates the documentation and, for an HTTP change, `openapi.yaml` in the
  same change. Documentation states current behavior without contrasting it
  with an earlier or absent design, and carries no figures that a later change
  would make stale. After a rename or removal, check that no page still names
  the old identifier.
- **Match Jackett where clients notice.** Endpoints live where Jackett puts
  them, and definition behavior follows Jackett's, quirks included, because
  definitions are written and tested against it. Where Jacklet deliberately
  differs, say so where the difference is declared.

## Commit messages

Use [Conventional Commits](https://www.conventionalcommits.org/):
`type(scope): summary`. Name the package or subsystem as the scope when there
is one, and omit it for a change that genuinely spans several. Keep the
subject imperative, lowercase after the colon, and without a trailing period.

A non-trivial commit adds a body after a blank line, written as `-` bullets
that are each a complete sentence ending in a period. Describe observable
behavior, important safety or implementation details, and the tests that
cover it, not a file-by-file list of edits.

```text
fix(scraper): handle missing release date on listing page

- Fall back to the torrent detail page when the listing omits a release date.
- Skip entries whose date cannot be parsed instead of aborting the scrape.
- Add coverage for missing and malformed date fields.
```

The release notes are generated from commit subjects, so a subject is worth
writing for a reader who has not seen the change. Every commit should build
and pass the checks above on its own.

## Pull requests

- Describe what the change does and why, and link the issue it closes.
- Run the checks above first; CI runs them again, with the tests on a Windows runner as well.
- Keep review comments and replies about the work, and push follow-up commits
  rather than rewriting history while a review is in progress. When history is
  rewritten before merging, keep each commit's author and committer dates
  identical.

## License

Jacklet is MIT licensed. By contributing you agree that your contribution is
provided under the same license. Every file carries an SPDX copyright and
license header, or is covered by `REUSE.toml`, and `reuse lint` fails when one
does not, so a new file needs its header.
