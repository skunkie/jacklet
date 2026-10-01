#!/bin/sh
# SPDX-FileCopyrightText: 2026 TorrPlay
#
# SPDX-License-Identifier: MIT

# dpkg and rpm describe the same two events differently: dpkg passes
# "configure" with the previously configured version as $2, empty on a
# first install, while rpm passes the number of copies of the package that
# will be installed afterwards -- 1 for an install, 2 for an upgrade.

set -e

is_upgrade=false
case "${1:-}" in
configure)
	if [ -n "${2:-}" ]; then
		is_upgrade=true
	fi
	;;
2)
	is_upgrade=true
	;;
1) ;;
*)
	# An abort-upgrade or abort-remove: dpkg is unwinding a failed
	# operation, and the unit should be left as it is.
	exit 0
	;;
esac

# systemd is absent under a chroot or in a container image built without an
# init, where there is no manager to tell about the unit.
if [ -d /run/systemd/system ] && command -v systemctl >/dev/null 2>&1; then
	systemctl daemon-reload >/dev/null 2>&1 || :

	if [ "${is_upgrade}" = true ]; then
		# try-restart restarts the service only if it was already
		# running: an upgrade should not start one the operator stopped.
		systemctl try-restart jacklet.service >/dev/null 2>&1 || :
	fi
fi

if [ "${is_upgrade}" = true ]; then
	exit 0
fi

# A first install deliberately neither enables nor starts the service.
# Jacklet with no JACKLET_API_KEY serves every indexer endpoint to anyone
# who can reach the port, and with no definitions it has nothing to serve,
# so starting it here would put an open endpoint on the network before the
# operator has configured either.
cat <<'MESSAGE'
Jacklet is installed but not started.

  1. Set JACKLET_API_KEY in /etc/jacklet/jacklet.env
     (openssl rand -hex 16), or the indexer endpoints stay open.
  2. Put Cardigann indexer definitions in /var/lib/jacklet/definitions.
  3. systemctl enable --now jacklet

Configuration is documented in
/usr/share/doc/jacklet/docs/configuration.md.
MESSAGE
