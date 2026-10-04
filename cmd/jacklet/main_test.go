// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
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
	store, err := database.Open(t.Context(), ":memory:")
	require.NoError(t, err, "failed to initialize in-memory database")
	t.Cleanup(func() { store.Close() })
	return store
}

func TestHealthCheck_Healthy(t *testing.T) {
	store := newTestStore(t)

	req := httptest.NewRequest(http.MethodGet, "/healthz", http.NoBody)
	rec := httptest.NewRecorder()
	healthCheck(store)(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
}

func TestHealthCheck_Unhealthy(t *testing.T) {
	store := newTestStore(t)
	store.Close() // force the ping to fail

	req := httptest.NewRequest(http.MethodGet, "/healthz", http.NoBody)
	rec := httptest.NewRecorder()
	healthCheck(store)(rec, req)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code, "want 503 for an unreachable database")
}

func TestPruneExpired_RemovesAgedTorrentsUntilCanceled(t *testing.T) {
	store := newTestStore(t)

	insert := func(name string, age time.Duration) {
		t.Helper()
		require.NoError(t, store.Upsert(t.Context(), scraper.Torrent{
			Name:      name,
			Published: time.Now().UTC().Add(-age).Format(time.RFC3339),
			Tracker:   "tracker",
		}), "failed to insert %s", name)
	}
	const retention = 30 * 24 * time.Hour
	insert("expired", retention+24*time.Hour)
	insert("fresh", time.Hour)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		pruneExpired(ctx, store, retention, time.Millisecond, slog.New(slog.DiscardHandler))
	}()

	deadline := time.After(5 * time.Second)
	for {
		stats, err := store.Stats(t.Context())
		require.NoError(t, err, "failed to count torrents")
		remaining := stats["tracker"].Torrents
		if remaining == 1 {
			break
		}
		select {
		case <-deadline:
			require.FailNowf(t, "the expired torrent was not pruned", "%d rows remain", remaining)
		case <-time.After(5 * time.Millisecond):
		}
	}

	kept, _, err := store.Search(t.Context(), scraper.Query{Limit: 10, Trackers: []string{"tracker"}})
	require.NoError(t, err, "failed to read the remaining torrent")
	require.Len(t, kept, 1)
	require.Equal(t, "fresh", kept[0].Name)

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "pruneExpired did not return after its context was canceled")
	}
}

func TestRetentionPeriod(t *testing.T) {
	tests := []struct {
		name    string
		env     string
		want    time.Duration
		wantErr bool
	}{
		{name: "defaults when unset", env: "", want: defaultRetentionDays * 24 * time.Hour},
		{name: "reads a day count", env: "7", want: 7 * 24 * time.Hour},
		{name: "zero disables pruning", env: "0", want: 0},
		{name: "rejects a non-numeric value", env: "forever", wantErr: true},
		{name: "rejects a negative value", env: "-1", wantErr: true},
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

func TestListenPort(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "the default", in: "9117", want: "9117"},
		{name: "the lowest port", in: "1", want: "1"},
		{name: "the highest port", in: "65535", want: "65535"},
		{name: "leading zeros are dropped", in: "09117", want: "9117"},
		{name: "rejects zero", in: "0", wantErr: true},
		{name: "rejects a negative port", in: "-1", wantErr: true},
		{name: "rejects a port above the range", in: "65536", wantErr: true},
		{name: "rejects a name", in: "http", wantErr: true},
		{name: "rejects empty", in: "", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := listenPort(tc.in)
			if tc.wantErr {
				require.ErrorContains(t, err, "invalid port")
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
			got, isSecure, err := publicBaseURL(tc.value)
			if tc.wantErr {
				require.Error(t, err, "publicBaseURL(%q) succeeded", tc.value)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
			require.Equal(t, tc.wantSecure, isSecure, "the origin's security was misread")
		})
	}
}

func TestMagnetTrackerList(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    []string
		wantErr bool
	}{
		{name: "empty builds trackerless magnets", value: ""},
		{name: "whitespace only is empty", value: "  ,  "},
		{name: "one tracker", value: "udp://tracker.example.test:6969/announce", want: []string{"udp://tracker.example.test:6969/announce"}},
		{
			name:  "several, trimmed and in order",
			value: " udp://a.example.test:1/announce , https://b.example.test/announce,wss://c.example.test ",
			want:  []string{"udp://a.example.test:1/announce", "https://b.example.test/announce", "wss://c.example.test"},
		},
		{name: "a trailing comma is ignored", value: "udp://a.example.test:1/announce,", want: []string{"udp://a.example.test:1/announce"}},
		{name: "a repeat is kept once", value: "udp://a.example.test:1,udp://a.example.test:1", want: []string{"udp://a.example.test:1"}},
		{name: "rejects a bare host", value: "tracker.example.test:6969", wantErr: true},
		{name: "rejects an unsupported scheme", value: "ftp://tracker.example.test/announce", wantErr: true},
		{name: "rejects a missing host", value: "udp:///announce", wantErr: true},
		{name: "rejects a port with no host", value: "udp://:6969/announce", wantErr: true},
		{name: "rejects a single bad entry among good ones", value: "udp://a.example.test:1,nonsense", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := magnetTrackerList(tc.value)
			if tc.wantErr {
				require.Error(t, err, "magnetTrackerList(%q) succeeded", tc.value)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

// An invalid base URL or admin password hash fails stackOptions with an error
// naming the setting. Its other settings are rejected by their own parsers'
// tests and by the run tests.
func TestStackOptions_RejectsInvalidSettings(t *testing.T) {
	tests := []struct {
		name      string
		configure func(cfg *settings)
		want      string
	}{
		{name: "admin password hash", configure: func(cfg *settings) { cfg.AdminPasswordHash = "not-a-hash" }, want: "JACKLET_ADMIN_PASSWORD_HASH"},
		{name: "base URL", configure: func(cfg *settings) { cfg.BaseURL = "ftp://jacklet.example.test" }, want: "base URL"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testSettings(t, freePort(t))
			tc.configure(cfg)
			_, err := stackOptions(cfg)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestTrustedProxyList(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    []netip.Prefix
		wantErr bool
	}{
		{name: "empty trusts none", value: ""},
		{
			name:  "addresses and ranges, trimmed and in order",
			value: " 10.0.0.0/8 , ::1,192.0.2.1, 2001:db8::/32 ",
			want: []netip.Prefix{
				netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("::1/128"),
				netip.MustParsePrefix("192.0.2.1/32"), netip.MustParsePrefix("2001:db8::/32"),
			},
		},
		{name: "a trailing comma is ignored", value: "10.0.0.1,", want: []netip.Prefix{netip.MustParsePrefix("10.0.0.1/32")}},
		{name: "a range is masked", value: "10.1.2.3/8", want: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}},
		{name: "an IPv4-mapped address is read as IPv4", value: "::ffff:10.0.0.1", want: []netip.Prefix{netip.MustParsePrefix("10.0.0.1/32")}},
		{name: "an IPv4-mapped range is read as IPv4", value: "::ffff:10.0.0.0/104", want: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}},
		{name: "rejects a host name", value: "proxy.example.test", wantErr: true},
		{name: "rejects an address with a port", value: "10.0.0.1:80", wantErr: true},
		{name: "rejects an out-of-range length", value: "10.0.0.0/33", wantErr: true},
		{name: "rejects a zoned address", value: "fe80::1%eth0", wantErr: true},
		{name: "rejects a single bad entry among good ones", value: "10.0.0.1,nonsense", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := trustedProxyList(tc.value)
			if tc.wantErr {
				require.Error(t, err, "trustedProxyList(%q) succeeded", tc.value)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

// The trusted proxies reach the panel's options, which the startup rejection
// test alone would not show: a list that is parsed and then dropped starts fine.
func TestStackOptions_PassesTrustedProxiesToThePanel(t *testing.T) {
	cfg := testSettings(t, freePort(t))
	cfg.TrustedProxies = "10.0.0.0/8"
	options, err := stackOptions(cfg)
	require.NoError(t, err)
	require.Equal(t, []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}, options.Admin.TrustedProxies)
}

// The user agent reaches the scraper's options, which the startup rejection
// test alone would not show: a value that is parsed and then dropped starts
// fine and quietly sends the built-in one.
func TestStackOptions_PassesTheUserAgentToTheScraper(t *testing.T) {
	cfg := testSettings(t, freePort(t))
	cfg.UserAgent = "ExampleBrowser/9.0"
	options, err := stackOptions(cfg)
	require.NoError(t, err)
	require.Equal(t, "ExampleBrowser/9.0", options.Scraper.UserAgent)
}

func TestUserAgentValue(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    string
		wantErr bool
	}{
		{name: "empty keeps the built-in one", value: ""},
		{name: "a browser string", value: "Mozilla/5.0 (X11; Linux x86_64) ExampleBrowser/9.0", want: "Mozilla/5.0 (X11; Linux x86_64) ExampleBrowser/9.0"},
		{name: "surrounding whitespace", value: "  ExampleBrowser/9.0\n", want: "ExampleBrowser/9.0"},
		{name: "an embedded newline", value: "ExampleBrowser/9.0\r\nX-Injected: 1", wantErr: true},
		{name: "a control character", value: "Example\x00Browser", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := userAgentValue(tc.value)
			if tc.wantErr {
				require.ErrorContains(t, err, "invalid user agent")
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestFlareSolverrSessionLimit(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    int
		wantErr bool
	}{
		{name: "the default", value: "8", want: 8},
		{name: "more than the default", value: "40", want: 40},
		{name: "one", value: "1", want: 1},
		{name: "surrounding whitespace", value: " 12 ", want: 12},
		{name: "zero", value: "0", wantErr: true},
		{name: "negative", value: "-1", wantErr: true},
		{name: "not a number", value: "many", wantErr: true},
		{name: "empty", value: "", wantErr: true},
		{name: "a fraction", value: "2.5", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := flareSolverrSessionLimit(tc.value)
			if tc.wantErr {
				require.ErrorContains(t, err, "invalid FlareSolverr sessions")
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
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

func TestAbsPath(t *testing.T) {
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

func TestPromptPassword(t *testing.T) {
	tests := []struct {
		name    string
		answers []string
		want    string
		wantErr string
	}{
		{name: "matching answers", answers: []string{"hunter2", "hunter2"}, want: "hunter2"},
		{name: "a mistyped repeat", answers: []string{"hunter2", "hunter3"}, wantErr: "do not match"},
		{name: "a terminal that closes", answers: []string{"hunter2"}, wantErr: "terminal closed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			answers := tc.answers
			readHidden := func() ([]byte, error) {
				if len(answers) == 0 {
					return nil, errors.New("terminal closed")
				}
				answer := answers[0]
				answers = answers[1:]
				return []byte(answer), nil
			}
			var errOut strings.Builder
			got, err := promptPassword(readHidden, &errOut)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
			require.Equal(t, "Password: \nRepeat it: \n", errOut.String(), "each prompt ends its line once the hidden input is read")
		})
	}
}

func TestReadUnlessInterrupted(t *testing.T) {
	t.Run("an answer is returned as it is", func(t *testing.T) {
		isRestored := false
		got, err := readUnlessInterrupted(
			func() ([]byte, error) { return []byte("hunter2"), nil },
			make(chan os.Signal),
			func() error { isRestored = true; return nil },
		)
		require.NoError(t, err)
		require.Equal(t, []byte("hunter2"), got)
		require.False(t, isRestored, "the terminal was restored under a read that still owned it")
	})

	t.Run("an interrupt restores the terminal before giving up", func(t *testing.T) {
		blocked := make(chan struct{})
		defer close(blocked)
		interrupted := make(chan os.Signal, 1)
		interrupted <- os.Interrupt
		isRestored := false
		_, err := readUnlessInterrupted(
			func() ([]byte, error) { <-blocked; return nil, nil },
			interrupted,
			func() error { isRestored = true; return nil },
		)
		require.ErrorIs(t, err, errInterrupted)
		require.True(t, isRestored, "an interrupted prompt left the terminal without echo")
	})

	t.Run("a failed restore is reported with the interrupt", func(t *testing.T) {
		blocked := make(chan struct{})
		defer close(blocked)
		interrupted := make(chan os.Signal, 1)
		interrupted <- os.Interrupt
		_, err := readUnlessInterrupted(
			func() ([]byte, error) { <-blocked; return nil, nil },
			interrupted,
			func() error { return errors.New("not a terminal") },
		)
		require.ErrorIs(t, err, errInterrupted)
		require.ErrorContains(t, err, "not a terminal")
	})
}

func TestCommandExitStatus(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "an interrupt", err: errInterrupted, want: 130},
		{name: "an interrupt with a failed restore", err: fmt.Errorf("%w; restoring the terminal failed: %w", errInterrupted, errors.New("not a terminal")), want: 130},
		{name: "any other failure", err: errors.New("the passwords do not match"), want: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, commandExitStatus(tc.err))
		})
	}
}

func TestRunCommand_UnknownCommand(t *testing.T) {
	require.Error(t, runCommand([]string{"nonsense"}), "an unknown command was accepted")
}
