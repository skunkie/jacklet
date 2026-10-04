// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"context"
	"errors"
	"strings"
	"sync"
)

// fakeStore is an in-memory Sink that keeps what it is given and reads it
// back, with the same identity rule as a real store: a torrent seen again
// replaces the row it matches instead of adding another.
type fakeStore struct {
	mu   sync.Mutex
	rows []Torrent
}

// UpsertAllForSearch stores each torrent under its tracker and identity.
func (f *fakeStore) UpsertAllForSearch(_ context.Context, torrents []Torrent, _ string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	for i := range torrents {
		stored := torrents[i]
		if at := f.indexOf(stored); at >= 0 {
			stored.ID = f.rows[at].ID
			f.rows[at] = stored
			continue
		}
		stored.ID = int64(len(f.rows) + 1)
		f.rows = append(f.rows, stored)
	}
	return len(torrents), nil
}

// Recent returns up to limit of a tracker's torrents, the most recently
// stored first.
func (f *fakeStore) Recent(_ context.Context, tracker string, limit int) ([]Torrent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var recent []Torrent
	for i := len(f.rows) - 1; i >= 0 && len(recent) < limit; i-- {
		if f.rows[i].Tracker == tracker {
			recent = append(recent, f.rows[i])
		}
	}
	return recent, nil
}

// all returns every stored torrent, in the order they were first stored.
func (f *fakeStore) all() []Torrent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Torrent(nil), f.rows...)
}

func (f *fakeStore) indexOf(t Torrent) int {
	for i := range f.rows {
		if f.rows[i].Tracker == t.Tracker && identityOf(f.rows[i]) == identityOf(t) {
			return i
		}
	}
	return -1
}

// identityOf is the value that makes two listings of one tracker the same
// torrent: the info hash, else the details page, else the title.
func identityOf(t Torrent) string {
	switch {
	case t.InfoHash != "":
		return "btih:" + strings.ToLower(t.InfoHash)
	case t.DetailsURL != "":
		return "url:" + t.DetailsURL
	default:
		return "name:" + t.Name
	}
}

// scrapeIndexer runs a scrape for its side effect on the Sink, reading a
// search that was skipped as the no-op it is.
func (s *Scraper) scrapeIndexer(ctx context.Context, def *Tracker, params SearchParams) error {
	_, err := s.Scrape(ctx, def, params)
	if errors.Is(err, ErrThrottled) {
		return nil
	}
	return err
}
