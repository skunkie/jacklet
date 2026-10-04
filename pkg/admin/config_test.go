// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package admin_test

import (
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torrplay/jacklet/pkg/admin"
	"github.com/torrplay/jacklet/pkg/admin/configtest"
	"github.com/torrplay/jacklet/pkg/scraper"
)

// fakeConfig is an in-memory admin.Config, which is also the scraper's
// ConfigSource, so the panel and the scraper share one place to keep
// settings with no file anywhere.
type fakeConfig struct {
	isEnabled bool
	mu        sync.Mutex
	saved     map[string]map[string]any
}

func (c *fakeConfig) Overrides(trackerID string) (map[string]any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	overrides := maps.Clone(c.saved[trackerID])
	if overrides == nil {
		overrides = map[string]any{}
	}
	return overrides, nil
}

func (c *fakeConfig) Enabled() bool { return c.isEnabled }

func (c *fakeConfig) Location(trackerID string) string {
	if !c.isEnabled {
		return ""
	}
	return "the settings table, row " + trackerID
}

func (c *fakeConfig) Save(trackerID string, overrides map[string]any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.isEnabled {
		return errors.New("no place to keep settings")
	}
	if c.saved == nil {
		c.saved = map[string]map[string]any{}
	}
	if len(overrides) == 0 {
		delete(c.saved, trackerID)
		return nil
	}
	c.saved[trackerID] = maps.Clone(overrides)
	return nil
}

// panelOverConfig builds a signed-in panel with one definition, over any
// admin.Config.
func panelOverConfig(t *testing.T, config *fakeConfig) (*panelFixture, *http.Cookie, string) {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "demo.yml"), []byte(`
id: demo
name: Demo Tracker
links:
  - http://tracker.example.test/
settings:
  - name: username
    type: text
    label: Username
    default: ""
search:
  paths:
    - path: "/"
  rows:
    selector: ".row"
  fields:
    title:
      selector: "a"
`), 0o600))

	logger := slog.New(slog.DiscardHandler)
	credential, err := admin.NewPassword(testPassword, "")
	require.NoError(t, err)
	panel, err := admin.New(&stubStore{}, scraper.New(config, "", logger), config, scraper.NewDefinitionStore(dir, logger), credential, logger)
	require.NoError(t, err)
	mux := http.NewServeMux()
	panel.Routes(mux)
	fixture := &panelFixture{mux: mux}
	cookie, csrf := fixture.signIn(t)
	return fixture, cookie, csrf
}

// The panel reads and writes settings through whatever Config it is given,
// and shows where that Config says they are kept.
func TestAdminSavesSettingsThroughAConfig(t *testing.T) {
	config := &fakeConfig{isEnabled: true}
	fixture, cookie, csrf := panelOverConfig(t, config)

	require.Contains(t, fixture.get(t, "/admin/indexers/demo", cookie), "the settings table, row demo",
		"the page did not show where the settings are kept")

	rec := fixture.post(t, "/admin/indexers/demo/settings", url.Values{"csrf_token": {csrf}, "username": {"someone"}}, cookie)
	require.Equal(t, http.StatusSeeOther, rec.Code, "save failed: %s", rec.Body.String())
	require.Equal(t, map[string]any{"username": "someone"}, config.saved["demo"], "the setting did not reach the Config")
}

// A Config with nowhere to write is reported, without naming a directory the
// panel knows nothing about, and saves nothing.
func TestAdminRefusesToSaveWhenTheConfigIsDisabled(t *testing.T) {
	config := &fakeConfig{}
	fixture, cookie, csrf := panelOverConfig(t, config)

	rec := fixture.post(t, "/admin/indexers/demo/settings", url.Values{"csrf_token": {csrf}, "username": {"someone"}}, cookie)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "cannot be saved")
	require.NotContains(t, rec.Body.String(), "CONFIG_DIR")
	require.Empty(t, config.saved, "a disabled Config was written to")
	require.NotContains(t, fixture.get(t, "/admin/indexers/demo", cookie), "Written to", "a disabled Config offered a location")
}

// The in-memory Config the panel tests use is held to the same contract as
// ConfigStore and as any Config an embedding program supplies.
func TestFakeConfigMeetsTheConfigContract(t *testing.T) {
	configtest.Run(t,
		func(t *testing.T) admin.Config {
			t.Helper()
			return &fakeConfig{isEnabled: true}
		},
		func(t *testing.T) admin.Config {
			t.Helper()
			return &fakeConfig{}
		})
}
