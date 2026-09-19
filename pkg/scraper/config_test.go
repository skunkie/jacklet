// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConfigStore_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	config := NewConfigStore(dir)

	t.Run("a tracker with no overrides reads as empty", func(t *testing.T) {
		overrides, err := config.Overrides("absent")
		require.NoError(t, err)
		require.Empty(t, overrides)
	})

	t.Run("saved overrides read back", func(t *testing.T) {
		require.NoError(t, config.Save("demo", map[string]any{"sort": "seeders", "password": "hunter2"}))

		overrides, err := config.Overrides("demo")
		require.NoError(t, err)
		require.Equal(t, map[string]any{"sort": "seeders", "password": "hunter2"}, overrides)
	})

	t.Run("the file holding credentials is owner-only", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("Windows files carry no Unix permission bits")
		}

		info, err := os.Stat(filepath.Join(dir, "demo.yml"))
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	})

	t.Run("saving nothing removes the file", func(t *testing.T) {
		require.NoError(t, config.Save("demo", map[string]any{}))

		_, err := os.Stat(filepath.Join(dir, "demo.yml"))
		require.ErrorIs(t, err, os.ErrNotExist)

		overrides, err := config.Overrides("demo")
		require.NoError(t, err)
		require.Empty(t, overrides)
	})
}

// A tracker id becomes a filename here, so one that would escape the
// config directory is refused.
func TestConfigStore_RejectsATraversingID(t *testing.T) {
	dir := t.TempDir()
	config := NewConfigStore(dir)

	for _, id := range []string{"", ".", "..", "../escape", "sub/dir"} {
		require.Error(t, config.Save(id, map[string]any{"a": "b"}), "id %q should be refused", id)
	}

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries, "nothing should have been written")
}

// Without a directory the store is disabled: it reads as empty rather than
// failing, and refuses to write.
func TestConfigStore_DisabledWithoutADirectory(t *testing.T) {
	config := NewConfigStore("")

	require.False(t, config.Enabled())
	require.Empty(t, config.Dir())

	overrides, err := config.Overrides("demo")
	require.NoError(t, err)
	require.Empty(t, overrides)

	require.Error(t, config.Save("demo", map[string]any{"a": "b"}))
}

// Resolve layers stored overrides over the definition's own defaults, and
// keeps a checkbox a Go bool so "{{ if .Config.x }}" behaves.
func TestConfigStore_ResolveLayersOverDefaults(t *testing.T) {
	dir := t.TempDir()
	config := NewConfigStore(dir)
	require.NoError(t, config.Save("demo", map[string]any{"sort": "seeders", "freeleech": true}))

	def := &Tracker{
		ID: "demo",
		Settings: []Setting{
			{Default: "added", Name: "sort", Type: "select"},
			{Default: "desc", Name: "order", Type: "select"},
			{Default: false, Name: "freeleech", Type: "checkbox"},
		},
	}

	resolved, err := config.Resolve(def)
	require.NoError(t, err)
	require.Equal(t, map[string]any{
		"sort":  "seeders", // overridden
		"order": "desc",    // the definition's own default
		// A checkbox resolves to Cardigann's own "True"/"" rather than a Go
		// bool, so a definition can compare it against ".False".
		"freeleech": cardigannTrue,
	}, resolved)
}

// Changing a tracker's stored credentials must drop any session
// established with the old ones, which is why ConfigStore.Save's caller
// invalidates the login rather than the store doing it silently.
func TestScraper_InvalidateLoginDropsASession(t *testing.T) {
	scrpr := New(nil, NewConfigStore(""), "", testLogger())

	scrpr.markLoggedIn("demo")
	require.True(t, scrpr.Status("demo").Authenticated)

	scrpr.InvalidateLogin("demo")
	require.False(t, scrpr.Status("demo").Authenticated)

	// A session that simply aged out is equally untrusted.
	scrpr.markLoggedIn("demo")
	scrpr.expireLoginState("demo")
	require.False(t, scrpr.Status("demo").Authenticated)
}

// TestConfigStore_RepairsTheSlashEscape covers an override written in the
// same style as a definition. Both go through unmarshalYAML, so a regex in
// a double-quoted scalar is not refused in one file and accepted in the
// other.
func TestConfigStore_RepairsTheSlashEscape(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "demo.yml"),
		[]byte(`pattern: "a\/b"`+"\n"), 0o600))

	overrides, err := NewConfigStore(dir).Overrides("demo")
	require.NoError(t, err)
	require.Equal(t, "a/b", overrides["pattern"])
}
