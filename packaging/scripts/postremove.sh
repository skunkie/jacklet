#!/bin/sh
# SPDX-FileCopyrightText: 2026 TorrPlay
#
# SPDX-License-Identifier: MIT

# /var/lib/jacklet is left in place on every path, purge included, and so
# is the service account that owns it: it holds the operator's definitions,
# their per-indexer tracker credentials and the scraped database, none of
# which this package created or can recreate. Removing it is a deliberate
# `rm -rf /var/lib/jacklet` by the operator.

set -e

if [ ! -d /run/systemd/system ] || ! command -v systemctl >/dev/null 2>&1; then
	exit 0
fi

systemctl daemon-reload >/dev/null 2>&1 || :
