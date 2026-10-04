// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

// Package configtest is a conformance suite for admin.Config. It holds an
// implementation to the scraper.ConfigSource contract, which the scraper
// reads through, and to what the admin panel does with it on top: saving,
// replacing and clearing a tracker's overrides, and reporting where they
// are kept.
package configtest

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torrplay/jacklet/pkg/admin"
	sourcetest "github.com/torrplay/jacklet/pkg/scraper/configtest"
)

// Run checks a Config against the contract its documentation states: every
// case the scraper's suite holds a ConfigSource to, seeded through Save, and
// the cases below. newConfig is called once per subtest and must return an
// enabled, empty Config each time. newDisabled builds one that is disabled,
// having nowhere to write; when it is nil those cases are skipped.
func Run(t *testing.T, newConfig, newDisabled func(t *testing.T) admin.Config) {
	t.Helper()

	sourcetest.Run(t, func(t *testing.T) sourcetest.Fixture {
		t.Helper()
		config := newConfig(t)
		return sourcetest.Fixture{Set: config.Save, Source: config}
	})

	t.Run("an enabled config says so", func(t *testing.T) {
		require.True(t, newConfig(t).Enabled())
	})

	t.Run("save replaces what was there", func(t *testing.T) {
		config := newConfig(t)
		require.NoError(t, config.Save("alpha", map[string]any{"username": "someone", "password": "secret"}))
		require.NoError(t, config.Save("alpha", map[string]any{"username": "another"}))

		got, err := config.Overrides("alpha")
		require.NoError(t, err)
		require.Equal(t, map[string]any{"username": "another"}, got, "a setting left out of a save survived it")
	})

	t.Run("saving nothing returns a tracker to its defaults", func(t *testing.T) {
		config := newConfig(t)
		require.NoError(t, config.Save("alpha", map[string]any{"username": "someone"}))
		require.NoError(t, config.Save("alpha", map[string]any{}))

		got, err := config.Overrides("alpha")
		require.NoError(t, err)
		require.Empty(t, got, "an empty save left overrides behind")

		require.NoError(t, config.Save("never-saved", nil), "clearing a tracker with nothing stored is not an error")
	})

	t.Run("saving one tracker leaves another alone", func(t *testing.T) {
		config := newConfig(t)
		require.NoError(t, config.Save("alpha", map[string]any{"sort": "seeders"}))
		require.NoError(t, config.Save("beta", map[string]any{"sort": "added"}))
		require.NoError(t, config.Save("beta", nil))

		got, err := config.Overrides("alpha")
		require.NoError(t, err)
		require.Equal(t, map[string]any{"sort": "seeders"}, got, "clearing one tracker cleared another")
	})

	t.Run("a location names where each tracker's settings are kept", func(t *testing.T) {
		config := newConfig(t)
		alpha, beta := config.Location("alpha"), config.Location("beta")
		require.NotEmpty(t, alpha, "an enabled config has somewhere its settings are kept")
		require.NotEqual(t, alpha, beta, "two trackers were given the same location")
	})

	t.Run("a disabled config writes nowhere", func(t *testing.T) {
		if newDisabled == nil {
			t.Skip("no disabled config was supplied")
		}
		config := newDisabled(t)
		require.False(t, config.Enabled())
		require.Empty(t, config.Location("alpha"), "a disabled config named a place it would write")
		require.Error(t, config.Save("alpha", map[string]any{"sort": "seeders"}), "a disabled config accepted a save")

		got, err := config.Overrides("alpha")
		require.NoError(t, err, "a disabled config has no overrides, which is not an error")
		require.Empty(t, got)
	})
}
