#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 TorrPlay
#
# SPDX-License-Identifier: MIT

# Holds the project's own rules in .vacuum.yml to
# cmd/jacklet/testdata/openapi-violations.yaml, which breaks each of them
# once, since a rule that silently stops matching passes the spec as
# readily as one that holds, and holds the minimum score CI fails the spec
# below to a document with a single finding of each kind it must catch.
# The arguments are the command that runs vacuum, such as `vacuum` or a
# `docker run` of its image, and it runs from the repository root.

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

# CI fails the spec through a minimum score of 100, which holds only while
# every warning and info finding costs a point, so a vacuum release that
# scores differently would let findings through without this. Each
# document is first held to its one finding, of the severity it is named
# for (1 a warning, 2 an info finding), so a failing score can only be that
# finding's.
for check in "openapi-one-warning.yaml jacklet-summary-no-period 1" "openapi-one-info.yaml description-duplication 2"; do
  read -r document rule severity <<<"${check}"
  findings=$("$@" spectral-report -r .vacuum.yml -o -n -q "cmd/jacklet/testdata/${document}")
  if [[ $(grep -o '"code":' <<<"${findings}" | wc -l) -ne 1 ||
    "${findings}" != *"\"code\":\"${rule}\""* ||
    "${findings}" != *"\"severity\":${severity},"* ]]; then
    echo "${document} must hold exactly one ${rule} finding of severity ${severity}:" >&2
    echo "${findings}" >&2
    failed=1
    continue
  fi
  status=0
  output=$("$@" lint -r .vacuum.yml --min-score 100 -b -q "cmd/jacklet/testdata/${document}" 2>&1) || status=$?
  if [[ "${output}" != *"Quality Score"* ]]; then
    echo "vacuum produced no report for ${document}:" >&2
    echo "${output}" >&2
    failed=1
  elif ((status == 0)); then
    echo "a minimum score of 100 let the one ${rule} finding in ${document} through" >&2
    failed=1
  fi
done
exit "${failed}"
