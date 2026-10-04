// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

// Package configtest is a conformance suite for scraper.ConfigSource. The
// scraper reads a tracker's setting overrides on every scrape and lays them
// over the definition's defaults, so an implementation written for an
// embedding program runs Run against itself instead of finding out through a
// tracker that received the wrong password.
package configtest

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/torrplay/jacklet/pkg/scraper"
)

// Fixture is a source under test together with the way the suite seeds it,
// since the interface itself only reads.
type Fixture struct {
	// Set makes overrides what Source returns for trackerID, replacing
	// anything set before.
	Set func(trackerID string, overrides map[string]any) error
	// Source is the implementation under test.
	Source scraper.ConfigSource
}

// Run checks a ConfigSource against the contract its documentation states.
// newFixture is called once per subtest and must return an empty source each
// time, so no case sees another's settings.
func Run(t *testing.T, newFixture func(t *testing.T) Fixture) {
	t.Helper()

	t.Run("a tracker with no overrides has none", func(t *testing.T) {
		source := newFixture(t).Source
		got, err := source.Overrides("unknown")
		require.NoError(t, err, "no overrides is the ordinary case, not an error")
		require.NotNil(t, got, "the map is the caller's to change, so it cannot be nil")
		require.Empty(t, got)
		require.NotPanics(t, func() { got["sort"] = "seeders" }, "a caller could not write to the empty map")

		for _, tracker := range []string{"unknown", "another-unknown"} {
			again, err := source.Overrides(tracker)
			require.NoError(t, err)
			require.Empty(t, again, "a caller's write to an empty map reached what %q returns next", tracker)
		}
	})

	t.Run("overrides come back as they were set", func(t *testing.T) {
		fixture := newFixture(t)
		want := map[string]any{
			"username":                  "someone",
			"password":                  "correct horse: battery # staple",
			"sort":                      "seeders",
			scraper.FlareSolverrSetting: true,
			"freeleech":                 false,
		}
		require.NoError(t, fixture.Set("alpha", want))

		got, err := fixture.Source.Overrides("alpha")
		require.NoError(t, err)
		require.Equal(t, want, got, "a value changed on its way through the source")
	})

	t.Run("a string that looks like another type stays a string", func(t *testing.T) {
		fixture := newFixture(t)
		want := map[string]any{"password": "123456", "pin": "0042", "flag": "true", "empty": "null", "list": "[a, b]"}
		require.NoError(t, fixture.Set("alpha", want))

		got, err := fixture.Source.Overrides("alpha")
		require.NoError(t, err)
		require.Equal(t, want, got, "a credential that reads like a number, a boolean or a null was turned into one")
	})

	t.Run("a value spanning lines keeps its lines", func(t *testing.T) {
		fixture := newFixture(t)
		want := map[string]any{"cookie": "first=1;\nsecond=2", "note": "  padded  "}
		require.NoError(t, fixture.Set("alpha", want))

		got, err := fixture.Source.Overrides("alpha")
		require.NoError(t, err)
		require.Equal(t, want, got)
	})

	t.Run("trackers are independent", func(t *testing.T) {
		fixture := newFixture(t)
		require.NoError(t, fixture.Set("alpha", map[string]any{"sort": "seeders"}))
		require.NoError(t, fixture.Set("beta", map[string]any{"sort": "added"}))

		alpha, err := fixture.Source.Overrides("alpha")
		require.NoError(t, err)
		beta, err := fixture.Source.Overrides("beta")
		require.NoError(t, err)
		require.Equal(t, "seeders", alpha["sort"])
		require.Equal(t, "added", beta["sort"], "one tracker's settings reached another")

		other, err := fixture.Source.Overrides("gamma")
		require.NoError(t, err)
		require.Empty(t, other)
	})

	t.Run("changing a returned map changes nothing stored", func(t *testing.T) {
		fixture := newFixture(t)
		require.NoError(t, fixture.Set("alpha", map[string]any{"sort": "seeders"}))

		first, err := fixture.Source.Overrides("alpha")
		require.NoError(t, err)
		first["sort"] = "tampered"
		first["extra"] = "added"

		second, err := fixture.Source.Overrides("alpha")
		require.NoError(t, err)
		require.Equal(t, map[string]any{"sort": "seeders"}, second,
			"a caller's edit to a returned map leaked back into the source")
	})

	t.Run("safe for concurrent use", func(t *testing.T) {
		fixture := newFixture(t)
		require.NoError(t, fixture.Set("alpha", map[string]any{"sort": "seeders"}))
		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() {
				got, err := fixture.Source.Overrides("alpha")
				assert.NoError(t, err)
				assert.Equal(t, "seeders", got["sort"])
			})
		}
		wg.Wait()
	})
}
