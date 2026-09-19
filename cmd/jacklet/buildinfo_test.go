// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuildDetails_String(t *testing.T) {
	tests := []struct {
		absent  []string
		details buildDetails
		name    string
		want    []string
	}{
		{
			absent:  []string{"dirty", "5d151bc6feeeabcdef"},
			details: buildDetails{Revision: "5d151bc6feeeabcdef", Time: "2026-09-16T20:12:46Z", Version: "v1.2.3"},
			name:    "a release build names its tag and commit",
			want:    []string{"v1.2.3", "5d151bc6feee", "2026-09-16T20:12:46Z"},
		},
		{
			details: buildDetails{IsDirty: true, Revision: "5d151bc6feee", Version: "v1.2.3"},
			name:    "a dirty tree is marked, since the revision alone does not describe the source",
			want:    []string{"5d151bc6feee-dirty"},
		},
		{
			absent:  []string{"5d151bc6feee 5d151bc6feee"},
			details: buildDetails{Revision: "5d151bc6feee", Version: "v0.0.0-20260916201246-5d151bc6feee"},
			name:    "a pseudo-version does not repeat its own commit",
			want:    []string{"v0.0.0-20260916201246-5d151bc6feee"},
		},
		{
			details: buildDetails{Version: "dev"},
			name:    "an unknown revision and time are omitted",
			want:    []string{"dev"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.details.String()
			for _, want := range tt.want {
				require.Contains(t, got, want)
			}
			for _, absent := range tt.absent {
				require.NotContains(t, got, absent)
			}
			// The toolchain and platform always identify the binary.
			require.Contains(t, got, runtime.Version())
			require.Contains(t, got, runtime.GOOS+"/"+runtime.GOARCH)
			require.False(t, strings.HasSuffix(got, " "), "no trailing separator")
		})
	}
}

func TestBuildDetails_LogAttrs(t *testing.T) {
	t.Run("pairs are balanced so slog does not log a dangling key", func(t *testing.T) {
		for _, details := range []buildDetails{
			{Version: "dev"},
			{IsDirty: true, Revision: "5d151bc6feee", Time: "2026-09-16T20:12:46Z", Version: "v1.2.3"},
		} {
			require.Zero(t, len(details.LogAttrs())%2, "odd number of slog arguments for %+v", details)
		}
	})

	t.Run("an unknown revision is omitted", func(t *testing.T) {
		attrs := buildDetails{Version: "dev"}.LogAttrs()
		require.NotContains(t, attrs, "revision")
		require.NotContains(t, attrs, "built")
	})
}

// TestReadBuildDetails covers the path the binary takes: the test binary
// is itself built by the toolchain, so build info is present.
func TestReadBuildDetails(t *testing.T) {
	details := readBuildDetails()
	require.NotEmpty(t, details.Version, "version always falls back to something")
}

func TestPrintVersion(t *testing.T) {
	var out bytes.Buffer
	printVersion(&out)

	line := out.String()
	require.True(t, strings.HasPrefix(line, "jacklet "), "got %q", line)
	require.True(t, strings.HasSuffix(line, "\n"), "output is a complete line")
}

// TestRunCommand_Version checks the subcommand is wired up, so `jacklet
// version` reaches printVersion.
func TestRunCommand_Version(t *testing.T) {
	require.NoError(t, runCommand([]string{"version"}))
	require.NoError(t, runCommand([]string{"--version"}))
	require.Error(t, runCommand([]string{"verzion"}))
}

// TestDefinitionsDir covers how the definitions directory is resolved:
// beside the executable, so an unpacked release works wherever it is
// launched from.
func TestDefinitionsDir(t *testing.T) {
	// The directory the test binary itself lives in stands in for an
	// unpacked release.
	exeDir := func(t *testing.T) string {
		t.Helper()
		exe, err := os.Executable()
		require.NoError(t, err)
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		return filepath.Dir(exe)
	}

	t.Run("a configured directory is used exactly as given", func(t *testing.T) {
		require.Equal(t, "/somewhere/else", definitionsDir("/somewhere/else"))
	})

	t.Run("an explicit directory is used even when it is missing", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "absent")
		require.Equal(t, missing, definitionsDir(missing))
	})

	t.Run("otherwise it sits beside the executable", func(t *testing.T) {
		require.Equal(t, filepath.Join(exeDir(t), "definitions"), definitionsDir(""))
	})

	t.Run("only the executable's directory is used", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.Mkdir(filepath.Join(dir, "definitions"), 0o750))
		t.Chdir(dir)

		got := definitionsDir("")
		require.NotEqual(t, filepath.Join(dir, "definitions"), got)
		require.NotEqual(t, "definitions", got)
		require.Equal(t, filepath.Join(exeDir(t), "definitions"), got)
	})

	t.Run("the result is absolute, so it does not move with the working directory", func(t *testing.T) {
		require.True(t, filepath.IsAbs(definitionsDir("")), "got %q", definitionsDir(""))
	})
}
