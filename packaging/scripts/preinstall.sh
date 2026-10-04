#!/bin/sh
# SPDX-FileCopyrightText: 2026 TorrPlay
#
# SPDX-License-Identifier: MIT

# One script serves both packagers, so it takes no argument into account:
# creating the service account is idempotent, and dpkg and rpm describe an
# install and an upgrade with entirely different arguments.

set -e

if ! getent group jacklet >/dev/null 2>&1; then
	groupadd --system jacklet
fi

if ! getent passwd jacklet >/dev/null 2>&1; then
	useradd --system --gid jacklet \
		--home-dir /var/lib/jacklet --no-create-home \
		--shell /usr/sbin/nologin \
		--comment "Jacklet Torznab indexer proxy" \
		jacklet
fi
