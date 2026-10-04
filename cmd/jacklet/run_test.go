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
	"sync"
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
		ConfigDir:            filepath.Join(dir, "config"),
		DBPath:               filepath.Join(dir, "jacklet.db"),
		DefinitionsDir:       filepath.Join(dir, "definitions"),
		FlareSolverrSessions: "8",
		Port:                 port,
		RetentionDays:        "30",
	}
}

// startServer runs run with cfg and returns once it reports readiness. The
// stop it returns cancels run and fails the test unless run returns nil
// promptly. It also runs as a cleanup, registered after cfg's temporary
// directory, so the server has let go of the database before that
// directory is removed however the test ends.
func startServer(t *testing.T, cfg *settings) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	ready := make(chan struct{})
	stopped := make(chan struct{})
	var runErr error
	go func() {
		defer close(stopped)
		runErr = run(ctx, slog.New(slog.DiscardHandler), cfg, func() { close(ready) })
	}()

	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case <-stopped:
				require.NoError(t, runErr, "a canceled run shuts down cleanly")
			case <-time.After(30 * time.Second):
				require.FailNow(t, "run did not return after its context was canceled")
			}
		})
	}
	t.Cleanup(stop)

	select {
	case <-ready:
	case <-stopped:
		// Reported here, with its cause, so the cleanup does not report
		// the same error a second time.
		once.Do(cancel)
		require.FailNowf(t, "run returned before reporting readiness", "%v", runErr)
	case <-time.After(30 * time.Second):
		require.FailNow(t, "run never reported readiness")
	}
	return stop
}

func TestRun_ServesUntilTheContextIsCanceled(t *testing.T) {
	port := freePort(t)
	stop := startServer(t, testSettings(t, port))

	// Readiness means the listener is already accepting, so this needs no
	// retry loop: if it did, ready would be firing too early.
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%s/healthz", port))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	stop()
}

// The panel is served under the configured prefix and nowhere else, which
// covers the wiring from the setting through run to the routes.
func TestRun_ServesTheAdminPanelUnderItsPrefix(t *testing.T) {
	port := freePort(t)
	cfg := testSettings(t, port)
	cfg.AdminPassword = "sample-admin-password"
	cfg.AdminPrefix = "/ops"
	startServer(t, cfg)

	status := func(path string) int {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%s%s", port, path))
		require.NoError(t, err)
		defer resp.Body.Close()
		return resp.StatusCode
	}
	require.Equal(t, http.StatusOK, status("/ops/login"))
	require.Equal(t, http.StatusNotFound, status("/admin/login"), "the panel answered at the default path")
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

// A bad announce URL must fail startup, and this covers the wiring: a
// setting that never reaches run would start fine.
func TestRun_RejectsAMalformedMagnetTracker(t *testing.T) {
	cfg := testSettings(t, freePort(t))
	cfg.MagnetTrackers = "not a url"

	err := run(t.Context(), slog.New(slog.DiscardHandler), cfg, func() {
		require.Fail(t, "the server became ready with a malformed magnet tracker")
	})
	require.ErrorContains(t, err, "invalid magnet tracker")
}

// A trusted proxy that is not an address is a typo that would leave every
// client behind the proxy sharing one sign-in slot, so it must fail startup,
// and this covers the wiring: a setting that never reaches run would start
// fine.
func TestRun_RejectsAMalformedTrustedProxy(t *testing.T) {
	cfg := testSettings(t, freePort(t))
	cfg.TrustedProxies = "10.0.0.0/8,proxy.example.test"

	err := run(t.Context(), slog.New(slog.DiscardHandler), cfg, func() {
		require.Fail(t, "the server became ready with a malformed trusted proxy")
	})
	require.ErrorContains(t, err, "invalid trusted proxy")
}

// A prefix the panel cannot be served under is a typo in a deployment, and
// must be visible at startup, and this covers the wiring: a setting that
// never reaches run would start fine.
func TestRun_RejectsAMalformedAdminPrefix(t *testing.T) {
	cfg := testSettings(t, freePort(t))
	cfg.AdminPrefix = "admin/"

	err := run(t.Context(), slog.New(slog.DiscardHandler), cfg, func() {
		require.Fail(t, "the server became ready with a malformed admin prefix")
	})
	require.ErrorContains(t, err, "admin prefix")
}

// A prefix that is well formed but names a path the server already serves must
// fail startup with a readable error, not panic when the panel's routes are
// registered.
func TestRun_RejectsAnAdminPrefixThatCollidesWithARoute(t *testing.T) {
	for _, prefix := range []string{"/docs", "/healthz", "/api/v2.0/indexers"} {
		cfg := testSettings(t, freePort(t))
		cfg.AdminPassword = "sample-admin-password"
		cfg.AdminPrefix = prefix

		err := run(t.Context(), slog.New(slog.DiscardHandler), cfg, func() {
			require.Fail(t, "the server became ready with a colliding admin prefix")
		})
		require.ErrorContains(t, err, "collides", "prefix %q", prefix)
	}
}

// A port outside the TCP range is a typo in a deployment, and startup
// reports it by the setting's name before trying to listen on it.
func TestRun_RejectsAPortOutOfRange(t *testing.T) {
	cfg := testSettings(t, "70000")

	err := run(t.Context(), slog.New(slog.DiscardHandler), cfg, func() {
		require.Fail(t, "the server became ready on a port outside the TCP range")
	})
	require.ErrorContains(t, err, "invalid port")
}

// A FlareSolverr address that is not an http or https address is a typo in a
// deployment, and must be visible at startup rather than at the first scrape.
func TestRun_RejectsAMalformedFlareSolverrURL(t *testing.T) {
	cfg := testSettings(t, freePort(t))
	cfg.FlareSolverrURL = "flaresolverr:8191"

	err := run(t.Context(), slog.New(slog.DiscardHandler), cfg, func() {
		require.Fail(t, "the server became ready with a malformed FlareSolverr address")
	})
	require.ErrorContains(t, err, "invalid FlareSolverr URL")
}

// A session limit that is not a whole number of at least one is a typo in a
// deployment, and must be visible at startup, and this covers the wiring: a
// setting that never reaches run would start fine.
func TestRun_RejectsAMalformedFlareSolverrSessionLimit(t *testing.T) {
	for _, value := range []string{"many", "0", "-3", ""} {
		cfg := testSettings(t, freePort(t))
		cfg.FlareSolverrSessions = value

		err := run(t.Context(), slog.New(slog.DiscardHandler), cfg, func() {
			require.Fail(t, "the server became ready with a bad FlareSolverr session limit")
		})
		require.ErrorContains(t, err, "invalid FlareSolverr sessions", "value %q", value)
	}
}

// A malformed contact address is a typo in a deployment, and must be
// visible at startup rather than at the first caps request. This also
// covers the wiring: a setting that never reaches run would start fine.
func TestRun_RejectsAMalformedContactEmail(t *testing.T) {
	cfg := testSettings(t, freePort(t))
	cfg.ContactEmail = "Ops <ops@example.org>"

	err := run(t.Context(), slog.New(slog.DiscardHandler), cfg, func() {
		require.Fail(t, "the server became ready with a malformed contact address")
	})
	require.ErrorContains(t, err, "invalid contact email")
}
