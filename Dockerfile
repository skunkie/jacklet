# syntax=docker/dockerfile:1
# A parser directive is honoured only as the very first line; any comment
# above it, a licence header included, makes it an ordinary comment.
#
# SPDX-FileCopyrightText: 2026 TorrPlay
#
# SPDX-License-Identifier: MIT

# Jacklet's SQLite driver (modernc.org/sqlite) is pure Go, so the binary
# links statically with cgo off and the final image needs no libc. That is
# what makes a distroless base viable here.

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build

# TARGETOS/TARGETARCH are supplied by buildx for each platform being built.
# Compiling for them from the build platform's native toolchain is far
# faster than emulating the target under QEMU.
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev

WORKDIR /src

# Dependencies resolve in their own layer, so editing source does not
# re-download the module cache.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/jacklet ./cmd/jacklet

# The runtime image is distroless and has no shell, so RUN cannot create
# directories there. Stage the layout here and copy it as one tree: `COPY
# <dir> <dest>` copies a directory's contents, so an empty source can leave
# the destination uncreated, while a populated tree carries every directory
# within it, empty leaves included.
#
# /config and /data must exist and belong to the runtime user before VOLUME
# names them: a mountpoint Docker creates for itself belongs to root, which
# leaves Jacklet unable to write the database. definitions/ is empty
# because the operator supplies definitions, and exists so DEFINITIONS_DIR
# resolves and there is somewhere to mount over.
RUN mkdir -p /rootfs/app/definitions /rootfs/config /rootfs/data

FROM gcr.io/distroless/static-debian12:nonroot

# The API reference and admin templates are embedded in the binary; only
# definitions are read from disk, and they are supplied by the operator.
COPY --from=build /out/jacklet /usr/local/bin/jacklet

# distroless's nonroot user is uid 65532. /app/definitions, /config and
# /data all arrive here, already owned by it.
COPY --from=build --chown=65532:65532 /rootfs/ /

# Dedicated paths, so the database and per-indexer credentials land on the
# mounted volumes below.
ENV JACKLET_DEFINITIONS_DIR=/app/definitions \
    JACKLET_CONFIG_DIR=/config \
    JACKLET_DB_PATH=/data/jacklet.db \
    JACKLET_PORT=9117

# `docker run` without -v then gets anonymous volumes, keeping state out of
# the container's own layer, which an upgrade discards.
VOLUME ["/config", "/data"]

EXPOSE 9117
USER nonroot:nonroot

ENTRYPOINT ["/usr/local/bin/jacklet"]
