// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package main

import (
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
	"strings"
)

// version is the release this binary was built from, set at link time:
//
//	go build -ldflags "-X main.version=v1.2.3" ./cmd/jacklet
//
// At its default, readBuildDetails takes the version the toolchain
// recorded for the module, alongside the VCS revision and build time
// stamped in beside it.
var version = "dev"

// buildDetails is the version, revision and build time of this binary.
// IsDirty reports that the working tree held uncommitted changes.
type buildDetails struct {
	IsDirty  bool
	Revision string
	Time     string
	Version  string
}

// readBuildDetails assembles the build details, preferring the ldflags
// value for Version and falling back to what the toolchain recorded.
func readBuildDetails() buildDetails {
	details := buildDetails{Version: version}

	info, ok := debug.ReadBuildInfo()
	if !ok {
		return details
	}

	// A tagged `go install example.com/cmd@v1.2.3` records that tag here,
	// a build from a module in VCS records a pseudo-version, and a build
	// outside one records "(devel)".
	if details.Version == "dev" && info.Main.Version != "" && info.Main.Version != "(devel)" {
		details.Version = info.Main.Version
	}

	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			details.Revision = setting.Value
		case "vcs.time":
			details.Time = setting.Value
		case "vcs.modified":
			details.IsDirty = setting.Value == "true"
		}
	}
	return details
}

// shortRevision abbreviates the commit hash to the usual 12 characters,
// the same form a Go pseudo-version embeds.
func (b buildDetails) shortRevision() string {
	if len(b.Revision) > 12 {
		return b.Revision[:12]
	}
	return b.Revision
}

// String renders the details on one line, omitting whatever is unknown.
// A dirty working tree is marked, since its revision alone does not
// describe the source the binary was built from.
func (b buildDetails) String() string {
	parts := []string{b.Version}
	if revision := b.shortRevision(); revision != "" {
		// A pseudo-version ends in the commit, which Version already
		// carries.
		if !strings.Contains(b.Version, revision) {
			if b.IsDirty {
				revision += "-dirty"
			}
			parts = append(parts, revision)
		}
	}
	if b.Time != "" {
		parts = append(parts, b.Time)
	}
	parts = append(parts, runtime.Version(), runtime.GOOS+"/"+runtime.GOARCH)
	return strings.Join(parts, " ")
}

// LogAttrs renders the details as alternating key/value arguments for
// slog, keeping each one queryable as its own field.
func (b buildDetails) LogAttrs() []any {
	attrs := []any{"version", b.Version}
	if b.Revision != "" {
		attrs = append(attrs, "revision", b.Revision, "dirty", b.IsDirty)
	}
	if b.Time != "" {
		attrs = append(attrs, "built", b.Time)
	}
	return append(attrs, "go", runtime.Version(), "platform", runtime.GOOS+"/"+runtime.GOARCH)
}

// printVersion writes the one-line build summary, for `jacklet version`.
func printVersion(out io.Writer) {
	fmt.Fprintf(out, "jacklet %s\n", readBuildDetails())
}
