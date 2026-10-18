#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 TorrPlay
#
# SPDX-License-Identifier: MIT

# Holds cliff.toml's commit parsers to the sections they promise: it renders
# the notes for a throwaway repository whose commits each name the section
# they belong in, or none, and checks that every one landed there. Rendering
# the real history only shows the template runs, since a commit filed under
# the wrong section, or dropped, renders as readily as one filed right.
# The arguments are the command that runs git-cliff, such as `git-cliff`,
# and it runs from the repository root.

set -euo pipefail

if (($# == 0)); then
  echo "usage: $0 git-cliff-command [argument ...]" >&2
  exit 2
fi

# Git exports these to its hooks, and git-cliff reads a GIT_CLIFF_ variable
# for most of its options and merges GIT_CLIFF__ ones over its config file.
# Left set, the first would point the fixture's commits at the caller's
# repository, and the others would check something other than cliff.toml
# rendering the fixture.
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_OBJECT_DIRECTORY \
  GIT_ALTERNATE_OBJECT_DIRECTORIES GIT_COMMON_DIR GIT_NAMESPACE
while read -r name; do
  unset "${name}"
done < <(compgen -e | grep '^GIT_CLIFF_' || true)

fixture=$(mktemp -d)
trap 'rm -rf "${fixture}"' EXIT

git -C "${fixture}" init -q
# The caller's own hooks and signing would judge the fixture's subjects,
# several of which no commit-message linter accepts.
commit() {
  git -C "${fixture}" -c user.name=Fixture -c user.email=fixture@example.com \
    -c commit.gpgsign=false -c core.hooksPath=/dev/null \
    commit -q --allow-empty --no-verify -m "$1"
}

# Each subject's summary is unique, so it names the commit in the notes.
commit "chore: set up the fixture"
commit "Feat(cli): add the sample flag"
commit "fix: correct the example output"
commit "perf(scraper): speed up the example parse"
commit "refactor: split the sample module"
commit "revert: restore the example default"
commit "docs: explain the sample option"
commit "test: cover the example case"
commit "build: pin the example toolchain"
commit "ci: cache the sample build"
commit "feat!: replace the sample format"
commit "CHORE(deps): bump the example module"
commit "chore!: drop the legacy settings file"
commit "style(api)!: rename the sample fields"
commit $'chore: move the example data\n\nBREAKING CHANGE: the data lives elsewhere.'
commit "fixture!: rename the sample data"
commit "testing!: replace the example harness"
commit "fixture: refresh the sample data"
commit "testing: widen the example harness"
commit "cicd: rename the example pipeline"
commit "chore(deps-dev): bump the example linter"
commit "style: tidy the example formatting"
commit "chore(release): prepare the example release"
commit "Merge branch 'example'"

# The template links each commit to its repository, which is read from
# here rather than from the fixture's absent remote.
notes=$(GITHUB_REPO=example/fixture GIT_CLIFF_OFFLINE=true \
  "$@" --config "${PWD}/cliff.toml" --repository "${fixture}" --unreleased)
if [[ "${notes}" != *"## Unreleased"* ]]; then
  echo "git-cliff rendered no notes:" >&2
  echo "${notes}" >&2
  exit 1
fi

# Every entry prefixed with the section it was rendered under.
entries=$(awk '/^### /{section=substr($0, 5); next} /^- /{print section ": " $0}' <<<"${notes}")

failed=0
expect() {
  grep -F -- "$2" <<<"${entries}" | grep -q "^$1: " ||
    { echo "expected under $1: $2" >&2; failed=1; }
}
refuse() {
  ! grep -qF -- "$1" <<<"${entries}" || { echo "expected left out: $1" >&2; failed=1; }
}

expect "Features" "add the sample flag"
expect "Features" "replace the sample format"
expect "Bug fixes" "correct the example output"
expect "Performance" "speed up the example parse"
expect "Refactoring" "split the sample module"
expect "Reverts" "restore the example default"
expect "Documentation" "explain the sample option"
expect "Tests" "cover the example case"
expect "Build & CI" "pin the example toolchain"
expect "Build & CI" "cache the sample build"
expect "Dependencies" "bump the example module"
expect "Other breaking changes" "drop the legacy settings file"
expect "Other breaking changes" "rename the sample fields"
expect "Other breaking changes" "move the example data"
expect "Other breaking changes" "rename the sample data"
expect "Other breaking changes" "replace the example harness"
refuse "set up the fixture"
refuse "refresh the sample data"
refuse "widen the example harness"
refuse "rename the example pipeline"
refuse "bump the example linter"
refuse "tidy the example formatting"
refuse "prepare the example release"
refuse "Merge branch"

if ((failed)); then
  echo "rendered notes:" >&2
  echo "${notes}" >&2
fi
exit "${failed}"
