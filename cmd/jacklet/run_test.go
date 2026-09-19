// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// freePort returns a port nothing is listening on, by binding one and
// letting it go. It binds the wildcard address, the one the server binds,
// so the port it hands back is free on every interface rather than only on
// loopback.
func freePort(t *testing.T) string {
	t.Helper()
	//nolint:gosec // G102: the server binds the wildcard address, so picking a free port has to as well.
	listener, err := net.Listen("tcp", ":0")
	require.NoError(t, err)
	defer listener.Close()
	_, port, err := net.SplitHostPort(listener.Addr().String())
	require.NoError(t, err)
	return port
}

// testSettings is a configuration that starts a server without touching
// anything outside the test's own directory.
func testSettings(t *testing.T, port string) *settings {
	t.Helper()
	dir := t.TempDir()
	return &settings{
		ConfigDir:      filepath.Join(dir, "config"),
		DBPath:         filepath.Join(dir, "jacklet.db"),
		DefinitionsDir: filepath.Join(dir, "definitions"),
		Port:           port,
		RetentionDays:  "30",
	}
}

func TestRun_ServesUntilTheContextIsCancelled(t *testing.T) {
	port := freePort(t)
	logger := slog.New(slog.DiscardHandler)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, logger, testSettings(t, port), func() { close(ready) })
	}()

	select {
	case <-ready:
	case err := <-done:
		require.FailNowf(t, "run returned before reporting readiness", "%v", err)
	case <-time.After(30 * time.Second):
		require.FailNow(t, "run never reported readiness")
	}

	// Readiness means the listener is already accepting, so this needs no
	// retry loop: if it did, ready would be firing too early.
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%s/healthz", port))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err, "a cancelled run shuts down cleanly")
	case <-time.After(30 * time.Second):
		require.FailNow(t, "run did not return after its context was cancelled")
	}
}

func TestRun_ReportsABindFailureInsteadOfReadiness(t *testing.T) {
	// Hold the port for the duration, so run cannot bind it. run listens on
	// the wildcard address, and this listener has to hold the same one:
	// Windows lets a wildcard bind coexist with a loopback-only one, so
	// holding 127.0.0.1 alone would leave the port bindable there.
	//nolint:gosec // G102: run binds the wildcard address, so holding the port has to as well.
	listener, err := net.Listen("tcp", ":0")
	require.NoError(t, err)
	defer listener.Close()
	_, port, err := net.SplitHostPort(listener.Addr().String())
	require.NoError(t, err)

	// A run that does bind reports readiness and then serves until its
	// context ends. Reporting that failure unwinds this goroutine without
	// ever returning to run, so it is the test's own context ending that
	// releases the one run is still holding.
	logger := slog.New(slog.DiscardHandler)
	err = run(t.Context(), logger, testSettings(t, port), func() {
		require.Fail(t, "readiness reported despite the port being in use")
	})
	require.ErrorContains(t, err, "failed to listen")
}

// A malformed contact address is a typo in a deployment, and must be
// visible at startup rather than at the first caps request. This also
// covers the wiring: a setting that never reaches run would start fine.
func TestRun_RejectsAMalformedContactEmail(t *testing.T) {
	cfg := testSettings(t, freePort(t))
	cfg.ContactEmail = "Ops <ops@example.org>"

	err := run(context.Background(), slog.New(slog.DiscardHandler), cfg, func() {
		require.Fail(t, "the server became ready with a malformed contact address")
	})
	require.ErrorContains(t, err, "invalid contact email")
}
