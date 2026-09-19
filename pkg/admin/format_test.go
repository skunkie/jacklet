// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package admin

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestHumanizeBytes(t *testing.T) {
	for _, tc := range []struct {
		value int64
		want  string
	}{
		{value: 12, want: "12 B"},
		{value: 1536, want: "1.50 KB"},
		{value: 2 * 1024 * 1024, want: "2.00 MB"},
	} {
		require.Equal(t, tc.want, humanizeBytes(tc.value), "humanizeBytes(%d)", tc.value)
	}
}

func TestHumanizeDuration(t *testing.T) {
	for _, tc := range []struct {
		value time.Duration
		want  string
	}{
		{value: -10 * time.Second, want: "10s"},
		{value: 2 * time.Minute, want: "2m"},
		{value: 3 * time.Hour, want: "3h"},
		{value: 2 * 24 * time.Hour, want: "2d"},
		{value: 2 * 365 * 24 * time.Hour, want: "2.0y"},
	} {
		require.Equal(t, tc.want, humanizeDuration(tc.value), "humanizeDuration(%s)", tc.value)
	}
}

func TestIndexerState(t *testing.T) {
	for _, tc := range []struct {
		name string
		view indexerView
		want string
	}{
		{name: "backoff", view: indexerView{BackedOff: true, Failures: 1}, want: "backoff"},
		{name: "idle", view: indexerView{}, want: "idle"},
		{name: "failing", view: indexerView{Failures: 1, Scraped: true}, want: "failing"},
		{name: "ok", view: indexerView{Scraped: true}, want: "ok"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, tc.view.State())
		})
	}
}
