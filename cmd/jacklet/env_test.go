// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEnvironmentGet(t *testing.T) {
	t.Run("reads the prefixed name", func(t *testing.T) {
		t.Setenv("JACKLET_DB_PATH", "/prefixed.db")

		env := &environment{}
		require.Equal(t, "/prefixed.db", env.get("DB_PATH"))
	})

	t.Run("only the prefixed name is read", func(t *testing.T) {
		t.Setenv("JACKLET_DB_PATH", "")
		t.Setenv("DB_PATH", "/bare.db")

		env := &environment{}
		require.Empty(t, env.get("DB_PATH"))
	})

	t.Run("orDefault falls back when the variable is unset", func(t *testing.T) {
		t.Setenv("JACKLET_CONFIG_DIR", "")

		env := &environment{}
		require.Equal(t, "fallback", env.orDefault("CONFIG_DIR", "fallback"))
	})
}

func TestEnvironmentSecret(t *testing.T) {
	clearAPIKeyEnv := func(t *testing.T) {
		t.Helper()
		for _, key := range []string{"JACKLET_API_KEY", "JACKLET_API_KEY_FILE"} {
			t.Setenv(key, "")
		}
	}

	t.Run("reads the value directly when no file is named", func(t *testing.T) {
		clearAPIKeyEnv(t)
		t.Setenv("JACKLET_API_KEY", "direct")

		env := &environment{}
		got, err := env.secret("API_KEY")
		require.NoError(t, err)
		require.Equal(t, "direct", got)
	})

	t.Run("reads a mounted secret file", func(t *testing.T) {
		clearAPIKeyEnv(t)
		path := filepath.Join(t.TempDir(), "api-key")
		require.NoError(t, os.WriteFile(path, []byte("from-file"), 0o600))
		t.Setenv("JACKLET_API_KEY_FILE", path)

		env := &environment{}
		got, err := env.secret("API_KEY")
		require.NoError(t, err)
		require.Equal(t, "from-file", got)
	})

	t.Run("a trailing newline is not part of the credential", func(t *testing.T) {
		clearAPIKeyEnv(t)
		path := filepath.Join(t.TempDir(), "api-key")
		require.NoError(t, os.WriteFile(path, []byte("from-file\n"), 0o600))
		t.Setenv("JACKLET_API_KEY_FILE", path)

		env := &environment{}
		got, err := env.secret("API_KEY")
		require.NoError(t, err)
		require.Equal(t, "from-file", got)
	})

	t.Run("inner whitespace survives, since it may be part of a password", func(t *testing.T) {
		clearAPIKeyEnv(t)
		path := filepath.Join(t.TempDir(), "pw")
		require.NoError(t, os.WriteFile(path, []byte("two words\r\n"), 0o600))
		t.Setenv("JACKLET_API_KEY_FILE", path)

		env := &environment{}
		got, err := env.secret("API_KEY")
		require.NoError(t, err)
		require.Equal(t, "two words", got)
	})

	t.Run("supplying both forms is an error", func(t *testing.T) {
		clearAPIKeyEnv(t)
		path := filepath.Join(t.TempDir(), "api-key")
		require.NoError(t, os.WriteFile(path, []byte("from-file"), 0o600))
		t.Setenv("JACKLET_API_KEY", "direct")
		t.Setenv("JACKLET_API_KEY_FILE", path)

		env := &environment{}
		_, err := env.secret("API_KEY")
		require.Error(t, err)
		require.Contains(t, err.Error(), "API_KEY_FILE")
	})

	t.Run("an unreadable file is an error, not an empty credential", func(t *testing.T) {
		clearAPIKeyEnv(t)
		t.Setenv("JACKLET_API_KEY_FILE", filepath.Join(t.TempDir(), "absent"))

		env := &environment{}
		_, err := env.secret("API_KEY")
		require.Error(t, err)
	})

	t.Run("unset yields an empty value without an error", func(t *testing.T) {
		clearAPIKeyEnv(t)

		env := &environment{}
		got, err := env.secret("API_KEY")
		require.NoError(t, err)
		require.Empty(t, got)
	})
}
