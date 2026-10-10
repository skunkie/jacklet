#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 TorrPlay
#
# SPDX-License-Identifier: MIT

# Holds the project's own rules in .vacuum.yml to
# cmd/jacklet/testdata/openapi-violations.yaml, which breaks each of them
# once, since a rule that silently stops matching passes the spec as
# readily as one that holds. The arguments are the command that runs
# vacuum, such as `vacuum` or a `docker run` of its image, and it runs from
# the repository root.

set -euo pipefail

# The planted violations must not fail vacuum, so that a failure here is
# vacuum or its image failing to run, which stops the script on its own
# error rather than reading as missing findings.
report=$("$@" lint -r .vacuum.yml --fail-severity none -d -a -b -q --no-clip \
  cmd/jacklet/testdata/openapi-violations.yaml)
if [[ "${report}" != *"Quality Score"* ]]; then
  echo "vacuum produced no report:" >&2
  echo "${report}" >&2
  exit 1
fi

failed=0
expect() {
  grep -qF -- "$1" <<<"${report}" || { echo "expected a finding: $1" >&2; failed=1; }
}
refuse() {
  ! grep -qF -- "$1" <<<"${report}" || { echo "unexpected finding: $1" >&2; failed=1; }
}

for where in Info Tag Operation Parameter Response "Inline schema" Schema Property Item; do
  expect "${where} description without a period"
done
expect "properties['Empty'].description"
expect "rule: jacklet-operation-id-camel"
expect "rule: jacklet-summary-no-period"
expect "rule: jacklet-tag-title-case"
refuse "Example data without a period"
refuse '`string` does not match'
exit "${failed}"
