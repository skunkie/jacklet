// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"time"
)

// IndexerStatus reports what the Scraper knows about one tracker's recent
// behavior: whether it is currently backed off after failures, and whether
// a login session is established. It is a snapshot, safe to hold and read
// after the call returns.
type IndexerStatus struct {
	// Authenticated reports whether a login session is currently held. It
	// is false for a tracker that needs no login.
	Authenticated bool
	// Failures is the number of consecutive failed scrapes. Zero means the
	// last scrape succeeded, or that none has run yet.
	Failures int
	// NextAllowed is when the tracker may be scraped again. A request
	// before it is served from the store instead.
	NextAllowed time.Time
	// Scraped reports whether this tracker has been scraped at all since
	// the process started, which distinguishes "idle" from "healthy".
	Scraped bool
	// TrackerID identifies the tracker the status describes.
	TrackerID string
}

// BackedOff reports whether the tracker is currently waiting out a failure
// backoff, as opposed to merely being inside the ordinary rate-limit
// window.
func (s IndexerStatus) BackedOff() bool {
	return s.Failures > 0 && time.Now().Before(s.NextAllowed)
}

// Status returns the current scrape and login state for one tracker.
func (s *Scraper) Status(trackerID string) IndexerStatus {
	status := IndexerStatus{TrackerID: trackerID}

	s.scrapeStateMu.Lock()
	if st, ok := s.scrapeState[trackerID]; ok {
		status.Failures = st.failures
		status.NextAllowed = st.nextAllowed
		status.Scraped = true
	}
	s.scrapeStateMu.Unlock()

	s.loginStateMu.Lock()
	if st, ok := s.loginState[trackerID]; ok {
		status.Authenticated = time.Now().Before(st.validUntil)
	}
	s.loginStateMu.Unlock()

	return status
}

// InvalidateLogin drops any cached login session for a tracker, so the
// next scrape authenticates again. Call it after changing a tracker's
// stored credentials (see ConfigStore.Save), since a session established
// with the old ones must not be trusted.
func (s *Scraper) InvalidateLogin(trackerID string) {
	s.invalidateLogin(trackerID)
}
