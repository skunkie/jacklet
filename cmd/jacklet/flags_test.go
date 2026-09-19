// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseSettings(t *testing.T) {
	t.Run("built-in defaults apply when nothing is set", func(t *testing.T) {
		for _, key := range []string{"BASE_URL", "PORT", "CONFIG_DIR", "DB_PATH", "RETENTION_DAYS", "FLARESOLVERR_URL"} {
			t.Setenv(envPrefix+key, "")
		}

		cfg, err := parseSettings(nil, io.Discard)
		require.NoError(t, err)
		require.Equal(t, "9117", cfg.Port)
		require.Equal(t, "config", cfg.ConfigDir)
		require.Equal(t, defaultDBPath, cfg.DBPath)
		require.Equal(t, "30", cfg.RetentionDays)
		require.Empty(t, cfg.FlareSolverrURL)
		require.Empty(t, cfg.BaseURL)
	})

	t.Run("the environment supplies a value when no flag does", func(t *testing.T) {
		t.Setenv("JACKLET_PORT", "9001")
		t.Setenv("JACKLET_CONFIG_DIR", "/etc/jacklet")

		cfg, err := parseSettings(nil, io.Discard)
		require.NoError(t, err)
		require.Equal(t, "9001", cfg.Port)
		require.Equal(t, "/etc/jacklet", cfg.ConfigDir)
	})

	t.Run("a credential can come from a mounted file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "key")
		require.NoError(t, os.WriteFile(path, []byte("file-key\n"), 0o600))
		t.Setenv("JACKLET_API_KEY", "")
		t.Setenv("JACKLET_API_KEY_FILE", path)

		cfg, err := parseSettings(nil, io.Discard)
		require.NoError(t, err)
		require.Equal(t, "file-key", cfg.APIKey)
	})

	t.Run("a flag wins over the environment", func(t *testing.T) {
		t.Setenv("JACKLET_PORT", "9001")
		t.Setenv("JACKLET_DB_PATH", "/from/env.db")

		cfg, err := parseSettings([]string{"-port", "7000", "-db-path", "/from/flag.db"}, io.Discard)
		require.NoError(t, err)
		require.Equal(t, "7000", cfg.Port)
		require.Equal(t, "/from/flag.db", cfg.DBPath)
	})

	t.Run("double-dash spelling works too", func(t *testing.T) {
		cfg, err := parseSettings([]string{"--port", "7001"}, io.Discard)
		require.NoError(t, err)
		require.Equal(t, "7001", cfg.Port)
	})

	// Credentials have no flag: an argument is readable by every other
	// process for as long as the server runs.
	t.Run("credentials come from the environment and have no flag", func(t *testing.T) {
		t.Setenv("JACKLET_API_KEY", "key-from-env")
		t.Setenv("JACKLET_ADMIN_PASSWORD", "pw-from-env")
		t.Setenv("JACKLET_ADMIN_PASSWORD_HASH", "hash-from-env")

		cfg, err := parseSettings(nil, io.Discard)
		require.NoError(t, err)
		require.Equal(t, "key-from-env", cfg.APIKey)
		require.Equal(t, "pw-from-env", cfg.AdminPassword)
		require.Equal(t, "hash-from-env", cfg.AdminPasswordHash)

		for _, arg := range []string{"-api-key", "-admin-password", "-admin-password-hash"} {
			_, err := parseSettings([]string{arg, "secret"}, io.Discard)
			require.Error(t, err, "%s must not be accepted as a flag", arg)
		}
	})

	t.Run("an unknown flag is an error", func(t *testing.T) {
		_, err := parseSettings([]string{"-nonsense"}, io.Discard)
		require.Error(t, err)
	})

	t.Run("a stray positional argument is an error", func(t *testing.T) {
		_, err := parseSettings([]string{"-port", "7000", "leftover"}, io.Discard)
		require.Error(t, err)
		require.Contains(t, err.Error(), "leftover")
	})

	t.Run("asking for help is reported as such, not as a fault", func(t *testing.T) {
		_, err := parseSettings([]string{"-h"}, io.Discard)
		require.ErrorIs(t, err, flag.ErrHelp)
	})
}

func TestPrintUsage(t *testing.T) {
	var out bytes.Buffer
	fs, _, _ := newSettingsFlags(&out)
	fs.Usage()

	text := out.String()
	for _, want := range []string{
		"jacklet hash-password", "jacklet version", // subcommands
		"-base-url", "-port", "-definitions-dir", "-config-dir", // flags
		"-db-path", "-flaresolverr-url", "-retention-days",
		"API_KEY", "ADMIN_PASSWORD", "ADMIN_PASSWORD_HASH", // environment only
	} {
		require.Contains(t, text, want)
	}
	// Every setting is discoverable here, so nothing forces a reader to
	// the documentation to find out how to configure the server.
	require.NotContains(t, text, "`")
	require.True(t, strings.HasPrefix(text, "jacklet - "), "got %q", text[:40])
}

func TestIsInfoFlag(t *testing.T) {
	for _, arg := range []string{"-v", "-version", "--version", "-h", "-help", "--help"} {
		require.True(t, isInfoFlag(arg), arg)
	}
	for _, arg := range []string{"version", "help", "-port", "--port", "hash-password", ""} {
		require.False(t, isInfoFlag(arg), arg)
	}
}
