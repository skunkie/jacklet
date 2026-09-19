// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/torrplay/jacklet/pkg/admin"
	"github.com/torrplay/jacklet/pkg/database"
	"github.com/torrplay/jacklet/pkg/scraper"
)

func newTestStore(t *testing.T) *database.Store {
	t.Helper()
	store, err := database.Open(context.Background(), ":memory:")
	require.NoError(t, err, "failed to initialize in-memory database")
	t.Cleanup(func() { store.Close() })
	return store
}

func TestHealthCheck_Healthy(t *testing.T) {
	store := newTestStore(t)

	req := httptest.NewRequest(http.MethodGet, "/healthz", http.NoBody)
	rr := httptest.NewRecorder()
	healthCheck(store)(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
}

func TestHealthCheck_Unhealthy(t *testing.T) {
	store := newTestStore(t)
	store.Close() // force the ping to fail

	req := httptest.NewRequest(http.MethodGet, "/healthz", http.NoBody)
	rr := httptest.NewRecorder()
	healthCheck(store)(rr, req)

	require.Equal(t, http.StatusServiceUnavailable, rr.Code, "want 503 for an unreachable database")
}

func TestPruneExpired_RemovesAgedTorrentsUntilCancelled(t *testing.T) {
	store := newTestStore(t)

	insert := func(name string, age time.Duration) {
		t.Helper()
		require.NoError(t, store.Upsert(context.Background(), database.Torrent{
			Name:      name,
			Published: time.Now().UTC().Add(-age).Format(time.RFC3339),
			Tracker:   "tracker",
		}), "failed to insert %s", name)
	}
	const retention = 30 * 24 * time.Hour
	insert("expired", retention+24*time.Hour)
	insert("fresh", time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		pruneExpired(ctx, store, retention, time.Millisecond, slog.New(slog.DiscardHandler))
	}()

	deadline := time.After(5 * time.Second)
	for {
		remaining, err := store.Total(context.Background())
		require.NoError(t, err, "failed to count torrents")
		if remaining == 1 {
			break
		}
		select {
		case <-deadline:
			require.FailNowf(t, "the expired torrent was not pruned", "%d rows remain", remaining)
		case <-time.After(5 * time.Millisecond):
		}
	}

	kept, err := store.Recent(context.Background(), "tracker", 10)
	require.NoError(t, err, "failed to read the remaining torrent")
	require.Len(t, kept, 1)
	require.Equal(t, "fresh", kept[0].Name)

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "pruneExpired did not return after its context was cancelled")
	}
}

func TestRetentionPeriod(t *testing.T) {
	tests := []struct {
		env     string
		name    string
		want    time.Duration
		wantErr bool
	}{
		{env: "", name: "defaults when unset", want: defaultRetentionDays * 24 * time.Hour},
		{env: "7", name: "reads a day count", want: 7 * 24 * time.Hour},
		{env: "0", name: "zero disables pruning", want: 0},
		{env: "forever", name: "rejects a non-numeric value", wantErr: true},
		{env: "-1", name: "rejects a negative value", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("JACKLET_RETENTION_DAYS", tc.env)

			// Through parseSettings, so the env-to-flag-default chain is
			// covered rather than just the parsing.
			cfg, err := parseSettings(nil, io.Discard)
			require.NoError(t, err)

			got, err := retentionPeriod(cfg.RetentionDays)
			if tc.wantErr {
				require.Error(t, err, "RETENTION_DAYS=%q was accepted", tc.env)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestPublicBaseURL(t *testing.T) {
	tests := []struct {
		name       string
		value      string
		want       string
		wantErr    bool
		wantSecure bool
	}{
		{name: "empty uses the request", value: ""},
		{name: "http origin", value: "http://jacklet.example:9117/", want: "http://jacklet.example:9117"},
		{name: "https origin", value: "https://jacklet.example", want: "https://jacklet.example", wantSecure: true},
		{name: "scheme is case insensitive", value: "HTTPS://jacklet.example", want: "https://jacklet.example", wantSecure: true},
		{name: "rejects another scheme", value: "ftp://jacklet.example", wantErr: true},
		{name: "rejects a path", value: "https://jacklet.example/prefix", wantErr: true},
		//nolint:gosec // intentionally malformed public URL used to verify credentials are rejected
		{name: "rejects credentials", value: "https://user:secret@jacklet.example", wantErr: true},
		{name: "rejects a query", value: "https://jacklet.example?x=1", wantErr: true},
		{name: "rejects a missing host", value: "https:///", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, secure, err := publicBaseURL(tc.value)
			if tc.wantErr {
				require.Error(t, err, "publicBaseURL(%q) succeeded", tc.value)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
			require.Equal(t, tc.wantSecure, secure, "the origin's security was misread")
		})
	}
}

func TestContactAddress(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    string
		wantErr bool
	}{
		{name: "empty advertises none", value: ""},
		{name: "bare address", value: "ops@example.org", want: "ops@example.org"},
		{name: "subaddressed", value: "ops+jacklet@example.org", want: "ops+jacklet@example.org"},
		{name: "rejects a display name", value: "Ops <ops@example.org>", wantErr: true},
		{name: "trims surrounding whitespace", value: " ops@example.org ", want: "ops@example.org"},
		{name: "whitespace only advertises none", value: "   "},
		{name: "rejects a missing domain", value: "ops@", wantErr: true},
		{name: "rejects a missing local part", value: "@example.org", wantErr: true},
		{name: "rejects a bare word", value: "ops", wantErr: true},
		{name: "rejects two addresses", value: "ops@example.org, abuse@example.org", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := contactAddress(tc.value)
			if tc.wantErr {
				require.Error(t, err, "contactAddress(%q) succeeded", tc.value)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestInfoFlagAndAbsolutePath(t *testing.T) {
	for _, arg := range []string{"-v", "--version", "-h", "--help"} {
		require.True(t, isInfoFlag(arg), "isInfoFlag(%q)", arg)
	}
	for _, arg := range []string{"version", "--verbose", ""} {
		require.False(t, isInfoFlag(arg), "isInfoFlag(%q)", arg)
	}
	require.True(t, filepath.IsAbs(absPath(".")), "absPath(.) is not absolute")
}

func TestWarnIfNoDefinitions(t *testing.T) {
	loggerOutput := func(t *testing.T, dir string) string {
		t.Helper()
		var output bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&output, nil))
		warnIfNoDefinitions(scraper.NewDefinitionStore(dir, logger), dir, logger)
		return output.String()
	}

	empty := t.TempDir()
	require.Contains(t, loggerOutput(t, empty), "no indexer definitions found")

	loaded := t.TempDir()
	definition := "id: demo\nname: Demo\nsearch:\n  paths:\n    - path: /\n"
	require.NoError(t, os.WriteFile(filepath.Join(loaded, "demo.yml"), []byte(definition), 0o600))
	require.Contains(t, loggerOutput(t, loaded), "loaded indexer definitions")
}

func TestHashPassword_ReadsStdin(t *testing.T) {
	var out, errOut strings.Builder
	require.NoError(t, hashPassword(strings.NewReader("hunter2\n"), &out, &errOut))

	hash := strings.TrimSpace(out.String())
	require.True(t, strings.HasPrefix(hash, "pbkdf2-sha256$"), "unexpected hash %q", hash)
	require.NotContains(t, hash, "hunter2", "the printed hash leaks the password")

	// The hash must verify the password it was made from, and only that.
	cred, err := admin.NewPassword("", hash)
	require.NoError(t, err, "the printed hash was not accepted")
	require.True(t, cred.Verify("hunter2"), "the printed hash does not verify its own password")
	require.False(t, cred.Verify("hunter3"), "the printed hash verified the wrong password")
}

func TestHashPassword_RejectsEmptyInput(t *testing.T) {
	var out, errOut strings.Builder
	require.Error(t, hashPassword(strings.NewReader(""), &out, &errOut), "empty stdin was accepted")
}

func TestRunCommand_UnknownCommand(t *testing.T) {
	require.Error(t, runCommand([]string{"nonsense"}), "an unknown command was accepted")
}
