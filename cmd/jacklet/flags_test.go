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
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torrplay/jacklet/pkg/admin"
	"github.com/torrplay/jacklet/pkg/scraper"
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

	t.Run("a flag wins over the environment for the magnet trackers", func(t *testing.T) {
		t.Setenv("JACKLET_MAGNET_TRACKERS", "udp://env.example.test:1/announce")

		cfg, err := parseSettings(nil, io.Discard)
		require.NoError(t, err)
		require.Equal(t, "udp://env.example.test:1/announce", cfg.MagnetTrackers)

		cfg, err = parseSettings([]string{"-magnet-trackers", "udp://flag.example.test:1/announce"}, io.Discard)
		require.NoError(t, err)
		require.Equal(t, "udp://flag.example.test:1/announce", cfg.MagnetTrackers)
	})

	t.Run("the admin prefix defaults to the panel's own, then follows the environment and the flag", func(t *testing.T) {
		t.Setenv("JACKLET_ADMIN_PREFIX", "")
		cfg, err := parseSettings(nil, io.Discard)
		require.NoError(t, err)
		require.Equal(t, admin.DefaultPrefix, cfg.AdminPrefix)

		t.Setenv("JACKLET_ADMIN_PREFIX", "/ops")
		cfg, err = parseSettings(nil, io.Discard)
		require.NoError(t, err)
		require.Equal(t, "/ops", cfg.AdminPrefix)

		cfg, err = parseSettings([]string{"-admin-prefix", "/console"}, io.Discard)
		require.NoError(t, err)
		require.Equal(t, "/console", cfg.AdminPrefix)
	})

	t.Run("the FlareSolverr session limit defaults to the scraper's, then follows the environment and the flag", func(t *testing.T) {
		t.Setenv("JACKLET_FLARESOLVERR_SESSIONS", "")
		cfg, err := parseSettings(nil, io.Discard)
		require.NoError(t, err)
		require.Equal(t, strconv.Itoa(scraper.DefaultMaxFlareSolverrSessions), cfg.FlareSolverrSessions)

		t.Setenv("JACKLET_FLARESOLVERR_SESSIONS", "20")
		cfg, err = parseSettings(nil, io.Discard)
		require.NoError(t, err)
		require.Equal(t, "20", cfg.FlareSolverrSessions)

		cfg, err = parseSettings([]string{"-flaresolverr-sessions", "30"}, io.Discard)
		require.NoError(t, err)
		require.Equal(t, "30", cfg.FlareSolverrSessions)
	})

	t.Run("the log level defaults to info, then follows the environment and the flag", func(t *testing.T) {
		t.Setenv("JACKLET_LOG_LEVEL", "")
		cfg, err := parseSettings(nil, io.Discard)
		require.NoError(t, err)
		require.Equal(t, "info", cfg.LogLevel)

		t.Setenv("JACKLET_LOG_LEVEL", "warn")
		cfg, err = parseSettings(nil, io.Discard)
		require.NoError(t, err)
		require.Equal(t, "warn", cfg.LogLevel)

		cfg, err = parseSettings([]string{"-log-level", "debug"}, io.Discard)
		require.NoError(t, err)
		require.Equal(t, "debug", cfg.LogLevel)
	})

	t.Run("a flag wins over the environment for the user agent", func(t *testing.T) {
		t.Setenv("JACKLET_USER_AGENT", "EnvBrowser/1.0")

		cfg, err := parseSettings(nil, io.Discard)
		require.NoError(t, err)
		require.Equal(t, "EnvBrowser/1.0", cfg.UserAgent)

		cfg, err = parseSettings([]string{"-user-agent", "FlagBrowser/2.0"}, io.Discard)
		require.NoError(t, err)
		require.Equal(t, "FlagBrowser/2.0", cfg.UserAgent)
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
		"-admin-prefix", "-base-url", "-port", "-definitions-dir", "-config-dir", // flags
		"-db-path", "-flaresolverr-sessions", "-flaresolverr-url", "-log-level", "-magnet-trackers", "-retention-days", "-trusted-proxies",
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
	for _, arg := range []string{"version", "help", "-port", "--port", "--verbose", "hash-password", ""} {
		require.False(t, isInfoFlag(arg), arg)
	}
}
