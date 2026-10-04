// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"
)

// maxRequestGap caps a definition's requestDelay, so that converting an
// absurd value to a duration stays defined and alike on every platform.
// Every real definition asks for seconds; a gap this long already outlasts
// a search, so the cap changes nothing a working definition could use.
const maxRequestGap = time.Minute

// errRequestQueue marks a request that ended while it waited for its turn
// under the definition's requestDelay. The tracker was never asked
// anything, so this is not a failure of the tracker and finishScrape
// leaves its failure count and backoff alone.
var errRequestQueue = errors.New("gave up waiting for the definition's request delay")

// requestTurn is one tracker's place in line for its requestDelay: turn
// admits one request at a time, and answered, read and written only by
// the request holding the turn, is when the last one was answered.
type requestTurn struct {
	answered time.Time
	turn     chan struct{}
}

// requestGap is the definition's requestDelay as a duration, zero when it
// sets none.
func (t *Tracker) requestGap() time.Duration {
	if !(t.RequestDelay > 0) {
		return 0
	}
	if t.RequestDelay >= maxRequestGap.Seconds() {
		return maxRequestGap
	}
	return time.Duration(math.Round(t.RequestDelay * float64(time.Second)))
}

// paceRequest waits until def's requestDelay has passed since its previous
// request was answered, as Jackett waits before every request a definition
// makes, and returns the function to call once this one is answered.
// Concurrent requests also take turns, one in flight at a time, so a
// tracker allowing one request every two seconds is not sent several at
// once by searches running side by side, and the gap follows the answer
// even when that answer is slower than the gap.
func (s *Scraper) paceRequest(ctx context.Context, def *Tracker) (func(), error) {
	gap := def.requestGap()
	if gap == 0 {
		return func() {}, nil
	}
	// A caller that has already given up gets no turn, which also keeps a
	// failover to the next mirror from asking the tracker anything.
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: %w", errRequestQueue, err)
	}
	line := s.requestTurnFor(TrackerID(def))

	select {
	case line.turn <- struct{}{}:
	case <-ctx.Done():
		return nil, fmt.Errorf("%w: %w", errRequestQueue, ctx.Err())
	}
	if wait := time.Until(line.answered.Add(gap)); wait > 0 {
		s.logger.Debug("waiting for the definition's request delay", "tracker", def.Name, "wait", wait)
		if err := sleepContext(ctx, wait); err != nil {
			<-line.turn
			return nil, fmt.Errorf("%w: %w", errRequestQueue, err)
		}
	}
	return func() {
		line.answered = time.Now()
		<-line.turn
	}, nil
}

// requestTurnFor returns the tracker's place in line, creating it on first
// use.
func (s *Scraper) requestTurnFor(trackerID string) *requestTurn {
	s.requestTurnsMu.Lock()
	defer s.requestTurnsMu.Unlock()
	line := s.requestTurns[trackerID]
	if line == nil {
		line = &requestTurn{turn: make(chan struct{}, 1)}
		s.requestTurns[trackerID] = line
	}
	return line
}

// sleepContext waits for d, or returns ctx's error when ctx ends first.
func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// isQueueWait reports whether err is a request that ended while it waited
// for a turn of Jacklet's own, so the tracker was never asked anything.
func isQueueWait(err error) bool {
	return errors.Is(err, errFlareSolverrQueue) || errors.Is(err, errRequestQueue)
}

// trackerError is the error a scrape keeps after err, given the one it
// kept before: a queue wait does not replace an earlier failure, so a
// scrape counts as a queue wait, which spares the tracker, only when every
// failure it saw was one.
func trackerError(kept, err error) error {
	if kept != nil && isQueueWait(err) {
		return kept
	}
	return err
}
