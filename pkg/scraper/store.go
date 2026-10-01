// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// TrackerSource supplies tracker definitions to a consumer. DefinitionStore
// is the implementation Jacklet ships — definitions as YAML files in a
// directory — but a program embedding this package can supply its own:
// definitions held in a database, fetched from a remote service, generated
// in memory for a test, or wrapped to add its own filtering.
//
// An implementation must be safe for concurrent use: consumers call it from
// multiple request goroutines.
type TrackerSource interface {
	// Find returns the definition whose identifier (see TrackerID) matches
	// id, or an error wrapping ErrTrackerNotFound when none does. The
	// returned Tracker must not alias state the source mutates later.
	Find(id string) (*Tracker, error)

	// Trackers returns every available definition, along with any that
	// could not be loaded. A per-definition failure belongs in errs; err is
	// for a failure to enumerate them at all.
	Trackers() ([]Tracker, []DefinitionError, error)
}

// DefinitionStore loads tracker definitions from a directory and caches
// the parsed results, re-reading a file only when it changes on disk.
//
// Definitions stay hot-reloadable — a file added, edited, or removed takes
// effect on the next lookup without restarting Jacklet — but a lookup no
// longer re-parses every definition in the directory. That matters at the
// scale Jacklet is meant for: parsing a Jackett-sized directory on every
// request costs tens of milliseconds and tens of megabytes of garbage,
// where checking whether anything changed costs a directory listing.
//
// A DefinitionStore is safe for concurrent use.
type DefinitionStore struct {
	dir    string
	files  map[string]*cachedDefinition
	logger *slog.Logger
	mu     sync.Mutex
}

// cachedDefinition is one definition file's parsed form, along with the
// file identity it was parsed from.
type cachedDefinition struct {
	err     error
	modTime time.Time
	size    int64
	tracker *Tracker
}

var _ TrackerSource = (*DefinitionStore)(nil)

// NewDefinitionStore creates a store reading definitions from dir.
func NewDefinitionStore(dir string, logger *slog.Logger) *DefinitionStore {
	return &DefinitionStore{
		dir:    dir,
		files:  make(map[string]*cachedDefinition),
		logger: logger,
	}
}

// Trackers returns every definition currently in the directory, together
// with the files that failed to parse. An individual file's failure is
// reported in errs so one broken definition doesn't hide every working
// one.
//
// A directory that does not exist yields no definitions rather than an
// error: definitions are supplied by the operator, so an empty or absent
// directory is the ordinary state of a fresh install, not a fault. It
// leaves the indexer list empty instead of failing every request.
//
// A file that parsed successfully before and fails now keeps serving its
// last good version, and reports the failure alongside it: an editing
// mistake shouldn't take a working indexer off the air until it's noticed.
func (s *DefinitionStore) Trackers() ([]Tracker, []DefinitionError, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	names, errs, err := s.refresh()
	if err != nil {
		return nil, nil, err
	}

	duplicates := s.duplicateIDs(names)
	trackers := make([]Tracker, 0, len(names))
	for _, name := range names {
		cached := s.files[name]
		if cached.tracker == nil {
			continue
		}
		id := TrackerID(cached.tracker)
		if len(duplicates[id]) > 1 {
			errs = append(errs, DefinitionError{
				Err:  fmt.Errorf("duplicate tracker id %q", id),
				Path: filepath.Join(s.dir, name),
			})
			continue
		}
		trackers = append(trackers, cloneTracker(cached.tracker))
	}
	return trackers, errs, nil
}

// duplicateIDs maps each loaded tracker id to the definition files that use
// it. An id is a global key for persistence, configuration, sessions and
// routing, so every duplicate is unusable rather than one file winning by
// directory order.
func (s *DefinitionStore) duplicateIDs(names []string) map[string][]string {
	files := make(map[string][]string)
	for _, name := range names {
		if cached := s.files[name]; cached.tracker != nil {
			id := TrackerID(cached.tracker)
			files[id] = append(files[id], name)
		}
	}
	return files
}

// isDefinitionFile reports whether a directory entry names a Cardigann
// definition. The extension is matched case-insensitively: a definition
// saved as "Tracker.YML", or carried through a case-preserving
// filesystem, is the same file.
func isDefinitionFile(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".yml", ".yaml":
		return true
	default:
		return false
	}
}

// refresh brings the cache in line with the directory and returns the
// definition filenames in a stable order, along with the files that failed.
// The caller holds s.mu.
func (s *DefinitionStore) refresh() ([]string, []DefinitionError, error) {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, fs.ErrNotExist) {
		clear(s.files)
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}

	// os.ReadDir sorts by filename, so results are in a stable order.
	names := make([]string, 0, len(entries))
	var errs []DefinitionError
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !isDefinitionFile(name) {
			continue
		}
		names = append(names, name)

		if cached := s.definition(name, entry); cached.err != nil {
			errs = append(errs, DefinitionError{Err: cached.err, Path: filepath.Join(s.dir, name)})
		}
	}

	if len(s.files) != len(names) {
		present := make(map[string]bool, len(names))
		for _, name := range names {
			present[name] = true
		}
		for name := range s.files {
			if !present[name] {
				delete(s.files, name)
			}
		}
	}

	return names, errs, nil
}

// definition returns the cached parse of one file, re-reading it only when
// its size or modification time no longer matches what was cached. The
// caller holds s.mu.
func (s *DefinitionStore) definition(name string, entry os.DirEntry) *cachedDefinition {
	path := filepath.Join(s.dir, name)

	info, err := entry.Info()
	if err != nil {
		return &cachedDefinition{err: err}
	}

	cached, ok := s.files[name]
	if ok && cached.size == info.Size() && cached.modTime.Equal(info.ModTime()) {
		return cached
	}

	// Record the identity of the file just examined either way, so a file
	// that is failing to parse is not re-read on every single lookup.
	fresh := &cachedDefinition{modTime: info.ModTime(), size: info.Size()}
	s.files[name] = fresh

	data, err := os.ReadFile(path)
	if err != nil {
		s.logger.Warn("failed to read definition file", "path", path, "error", err)
		fresh.err = err
		fresh.tracker = lastGood(cached)
		return fresh
	}

	var tracker Tracker
	if err := unmarshalYAML(data, &tracker); err != nil {
		s.logger.Warn("failed to parse definition file", "path", path, "error", err)
		fresh.err = err
		if fresh.tracker = lastGood(cached); fresh.tracker != nil {
			s.logger.Warn("serving the last version of this definition that parsed", "path", path)
		}
		return fresh
	}

	fresh.tracker = &tracker
	return fresh
}

// lastGood returns the previously parsed definition to keep serving after
// a failed re-read, if there is one.
func lastGood(cached *cachedDefinition) *Tracker {
	if cached == nil {
		return nil
	}
	return cached.tracker
}

// Find returns the definition whose ID matches id. The returned Tracker is
// a copy of the cached one, so a caller cannot alter what other requests
// see. Only the match is copied: this is the per-search hot path, and
// copying every definition in the directory to scan for one would give
// back much of what the cache saves.
func (s *DefinitionStore) Find(id string) (*Tracker, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	names, _, err := s.refresh()
	if err != nil {
		return nil, err
	}

	duplicates := s.duplicateIDs(names)
	if files := duplicates[id]; len(files) > 1 {
		return nil, fmt.Errorf("duplicate tracker id %q in %s", id, strings.Join(files, ", "))
	}

	for _, name := range names {
		cached := s.files[name]
		if cached.tracker != nil && TrackerID(cached.tracker) == id {
			found := cloneTracker(cached.tracker)
			return &found, nil
		}
	}
	return nil, fmt.Errorf("%w: %q", ErrTrackerNotFound, id)
}
