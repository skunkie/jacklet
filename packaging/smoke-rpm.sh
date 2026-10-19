#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 TorrPlay
#
# SPDX-License-Identifier: MIT

# Installs, upgrades and removes the .rpm in /out inside a Fedora container,
# which is where the release workflow runs it, as root. MAN7_RENDERED is the
# number of section 7 pages the release rendered, which the package is
# expected to install every one of.

set -euo pipefail
shopt -s failglob

# The base image carries neither systemd-analyze nor useradd, and
# the scriptlets need the latter to create the service account.
dnf install -y --setopt=install_weak_deps=False systemd shadow-utils >/dev/null

rpm_files=(/out/*.rpm)
test "${#rpm_files[@]}" -eq 1
rpm_file=${rpm_files[0]}

# nfpm expands an unset variable to nothing rather than failing,
# so an empty URL means the packaging step lost REPO_URL. rpm
# reports a tag it does not carry as "(none)".
url=$(rpm --query --package --queryformat '%{URL}' "${rpm_file}")
test -n "${url}"
test "${url}" != "(none)"

rpm --install "${rpm_file}" 2>&1 | tee /tmp/install.log
getent passwd jacklet
test -x /usr/bin/jacklet
test -f /etc/jacklet/jacklet.env
systemd-analyze verify /usr/lib/systemd/system/jacklet.service

# nfpm reports a missing file, but packages an empty man7
# directory without complaint, so what is checked is that every
# page the release rendered is a page the package installed.
test -f /usr/share/man/man1/jacklet.1.gz
installed=(/usr/share/man/man7/jacklet-*.7.gz)
test "${#installed[@]}" -eq "${MAN7_RENDERED}"

# A first install leaves the service stopped and not enabled, so
# that an endpoint with no API key set does not reach the network.
# There is no manager running in here for systemctl is-enabled to
# ask, and it would report the unit as disabled for that reason
# alone, so what is checked is the symlink enabling would create.
test ! -e /etc/systemd/system/multi-user.target.wants/jacklet.service

# The first-install branch of postinstall is the one that prints
# the setup instructions, so its message is what distinguishes the
# two branches from outside.
grep -q 'Jacklet is installed but not started' /tmp/install.log

# --force reinstalls the same version as an upgrade transaction,
# which is how the upgrade branch of the scriptlets -- the one
# that keeps a running service running and stays quiet -- is
# reached without building a second version.
rpm --upgrade --force "${rpm_file}" 2>&1 | tee /tmp/upgrade.log
if grep -q 'Jacklet is installed but not started' /tmp/upgrade.log; then
  echo "the upgrade printed the first-install instructions" >&2
  exit 1
fi

# The unit must still not be enabled after an upgrade either.
test ! -e /etc/systemd/system/multi-user.target.wants/jacklet.service

rpm --erase jacklet
test ! -e /usr/bin/jacklet
