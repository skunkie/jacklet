// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// ConfigSource is where a Scraper reads an operator's per-tracker setting
// overrides from. *ConfigStore, which keeps them as files in a directory,
// satisfies it; a program can supply its own, keeping them in a database or
// fetching them from a service. An implementation must be safe for
// concurrent use, since trackers are scraped at the same time.
type ConfigSource interface {
	// Overrides returns the setting overrides stored for a tracker: a flat
	// map of setting name to value, layered over the defaults the definition
	// itself declares. A tracker with none yields an empty map and no error,
	// since having no overrides is the ordinary case; an error means they
	// exist but could not be read. A value comes back as it was saved: a
	// string stays a string, however much it reads like a number or a
	// boolean, and a boolean stays a boolean, which is how a checkbox is
	// told from text. The map belongs to the caller, so changing it must not
	// change what the source returns next. The configtest package's Run
	// checks an implementation against all of it.
	Overrides(trackerID string) (map[string]any, error)
}

// ConfigStore holds an operator's per-tracker setting overrides: one flat
// "<tracker-id>.yml" mapping of setting name to value per tracker, layered
// over the defaults the definition itself declares.
//
// It is separate from Scraper because it is a different job — editing
// stored settings has nothing to do with fetching pages — and because the
// admin panel reads and writes these files without scraping anything.
//
// A ConfigStore with no directory is disabled: it reads as empty and
// refuses to save, which is what a deployment that set no
// JACKLET_CONFIG_DIR gets.
type ConfigStore struct {
	dir string
}

// NewConfigStore reads and writes overrides under dir. An empty dir
// disables the store.
func NewConfigStore(dir string) *ConfigStore {
	return &ConfigStore{dir: dir}
}

// Dir returns the directory overrides are stored in, or "" when the store
// is disabled.
func (c *ConfigStore) Dir() string { return c.dir }

// Enabled reports whether a directory was configured.
func (c *ConfigStore) Enabled() bool { return c.dir != "" }

// Location returns where a tracker's overrides are written, the path of its
// file, or "" when the store is disabled and writes nowhere.
func (c *ConfigStore) Location(trackerID string) string {
	if !c.Enabled() {
		return ""
	}
	return filepath.Join(c.dir, trackerID+".yml")
}

// Overrides returns the setting overrides stored for a tracker. A tracker
// with no override file, or a disabled store, yields an empty map rather
// than an error: having no overrides is the ordinary case. An id that
// would name a file outside the directory is refused, as Save refuses it,
// since a definition's replaces is read through here too.
func (c *ConfigStore) Overrides(trackerID string) (map[string]any, error) {
	if !c.Enabled() {
		return map[string]any{}, nil
	}
	if err := validateTrackerID(trackerID); err != nil {
		return nil, err
	}

	data, err := os.ReadFile(filepath.Join(c.dir, trackerID+".yml"))
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}

	var overrides map[string]any
	// Through unmarshalYAML rather than yaml.Unmarshal, so an override
	// written in the same style as a definition — a regex in a
	// double-quoted scalar — is not refused where the definition itself
	// would be accepted.
	if err := unmarshalYAML(data, &overrides); err != nil {
		return nil, fmt.Errorf("config override for %q: %w", trackerID, err)
	}
	return overrides, nil
}

// Resolve returns the ".Config" template context for a definition: each
// setting's own default, overridden by any stored value.
func (c *ConfigStore) Resolve(def *Tracker) (map[string]any, error) {
	cfg, _, err := resolveConfig(c, def)
	return cfg, err
}

// resolveConfig is Resolve for any ConfigSource that also returns the raw
// overrides, for a caller that reads more from them than the definition's
// declared settings — the FlareSolverr opt-in is one — without reading them a
// second time.
func resolveConfig(source ConfigSource, def *Tracker) (cfg, overrides map[string]any, err error) {
	overrides, err = TrackerOverrides(source, def)
	if err != nil {
		return defaultConfig(def), nil, err
	}
	return mergedConfig(def, overrides), overrides, nil
}

// TrackerOverrides returns the setting overrides stored for def's
// tracker, or, while it has none, the first of those stored under an id
// its definition replaces: a renamed tracker keeps the settings an
// operator saved under its old id, as Jackett carries them over, until
// settings are saved under its own.
func TrackerOverrides(source ConfigSource, def *Tracker) (map[string]any, error) {
	overrides, err := source.Overrides(TrackerID(def))
	if err != nil || len(overrides) > 0 {
		return overrides, err
	}
	for _, oldID := range def.Replaces {
		// An id naming no file in the directory cannot hold saved
		// settings, only a mistake in the definition, so it is passed over
		// rather than costing the ids after it.
		if validateTrackerID(oldID) != nil {
			continue
		}
		previous, err := source.Overrides(oldID)
		if err != nil {
			return nil, err
		}
		if len(previous) > 0 {
			return previous, nil
		}
	}
	return overrides, nil
}

// Save writes a tracker's setting overrides, replacing any existing file.
// The file is written with owner-only permissions and swapped into place
// atomically, because it holds tracker credentials and a reader may be
// parsing it concurrently.
//
// Passing an empty map removes the file, returning the tracker to its
// definition's own defaults.
//
// Credentials may have changed, so a caller holding a Scraper must drop
// any session established with the old ones — see Scraper.InvalidateLogin.
func (c *ConfigStore) Save(trackerID string, overrides map[string]any) error {
	if !c.Enabled() {
		return errors.New("no config directory is configured")
	}
	if err := validateTrackerID(trackerID); err != nil {
		return err
	}

	path := filepath.Join(c.dir, trackerID+".yml")
	if len(overrides) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}

	data, err := yaml.Marshal(overrides)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(c.dir, "."+trackerID+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// validateTrackerID rejects an id that would escape the config directory
// when used as a filename. Ids come from definition files rather than from
// a request, but this is the one place an id becomes a filesystem path.
func validateTrackerID(trackerID string) error {
	if trackerID == "" {
		return errors.New("tracker id is empty")
	}
	if trackerID != filepath.Base(trackerID) || trackerID == "." || trackerID == ".." {
		return fmt.Errorf("invalid tracker id %q", trackerID)
	}
	return nil
}
