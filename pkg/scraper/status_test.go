// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestIndexerStatusTracksScrapeAndLoginState(t *testing.T) {
	scrpr := New(nil, NewConfigStore(""), "", testLogger())
	got := scrpr.Status("demo")
	require.Equal(t, "demo", got.TrackerID)
	require.False(t, got.Scraped, "an unscraped tracker reported as scraped")
	require.False(t, got.Authenticated, "an unauthenticated tracker reported as authenticated")

	scrpr.setScrapeState("demo", 2, time.Now().Add(time.Minute))
	scrpr.markLoggedIn("demo")
	got = scrpr.Status("demo")
	require.True(t, got.Scraped)
	require.Equal(t, 2, got.Failures)
	require.True(t, got.Authenticated)
	require.True(t, got.BackedOff())

	scrpr.InvalidateLogin("demo")
	got = scrpr.Status("demo")
	require.False(t, got.Authenticated, "InvalidateLogin left the tracker authenticated")

	got.NextAllowed = time.Now().Add(-time.Minute)
	require.False(t, got.BackedOff(), "expired backoff reported as active")
}
