#!/bin/sh
# SPDX-FileCopyrightText: 2026 TorrPlay
#
# SPDX-License-Identifier: MIT

# dpkg passes "remove" when the package is going away and "upgrade" when a
# new version is about to replace it; rpm passes the number of packages
# that will remain, 0 on a removal and 1 on an upgrade. In both cases the
# unit is stopped only when it is not coming back.

set -e

case "${1:-}" in
remove | 0) ;;
*)
	exit 0
	;;
esac

if [ ! -d /run/systemd/system ] || ! command -v systemctl >/dev/null 2>&1; then
	exit 0
fi

systemctl --no-reload disable --now jacklet.service >/dev/null 2>&1 || :
