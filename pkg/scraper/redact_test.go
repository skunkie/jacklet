// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRedactAddresses(t *testing.T) {
	cause := errors.New("connection reset")
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "an error naming no URL is unchanged", err: cause, want: "connection reset"},
		{
			name: "a query is dropped, and said to be",
			err:  &url.Error{Err: cause, Op: "Get", URL: "https://tracker.test/dl.php?passkey=secret"},
			want: `Get "https://tracker.test/dl.php (query redacted)": connection reset`,
		},
		{
			name: "a URL with no query is kept whole",
			err:  &url.Error{Err: cause, Op: "Get", URL: "https://tracker.test/dl.php"},
			want: `Get "https://tracker.test/dl.php": connection reset`,
		},
		{
			name: "a wrapped error keeps its own message",
			err: fmt.Errorf("tracker X: all links unreachable: %w",
				&url.Error{Err: cause, Op: "Get", URL: "https://tracker.test/dl.php?passkey=secret"}),
			want: `tracker X: all links unreachable: Get "https://tracker.test/dl.php (query redacted)": connection reset`,
		},
		{
			name: "every URL in a joined error is redacted",
			err: errors.Join(
				&url.Error{Err: cause, Op: "Get", URL: "https://one.test/?passkey=first"},
				&url.Error{Err: cause, Op: "Get", URL: "https://two.test/?passkey=second"}),
			want: "Get \"https://one.test/ (query redacted)\": connection reset\n" +
				`Get "https://two.test/ (query redacted)": connection reset`,
		},
		{
			// Replacing the shorter address first would rewrite the start
			// of the longer one, which would then keep its query.
			name: "an address that begins another does not shield it",
			err: errors.Join(
				&url.Error{Err: cause, Op: "Get", URL: "https://tracker.test/dl.php?id=1"},
				&url.Error{Err: cause, Op: "Get", URL: "https://tracker.test/dl.php?id=1&passkey=secret"}),
			want: "Get \"https://tracker.test/dl.php (query redacted)\": connection reset\n" +
				`Get "https://tracker.test/dl.php (query redacted)": connection reset`,
		},
		{
			name: "a passkey in a path segment is redacted",
			err:  &url.Error{Err: cause, Op: "Get", URL: "https://tracker.test/download/12345/0123456789abcdef0123abcd/file.torrent"},
			want: `Get "https://tracker.test/download/12345/…/file.torrent": connection reset`,
		},
		{
			name: "a fragment is dropped, and said to be",
			err:  &url.Error{Err: cause, Op: "Get", URL: "https://tracker.test/dl.php#passkey=secret"},
			want: `Get "https://tracker.test/dl.php (fragment redacted)": connection reset`,
		},
		{
			name: "a URL that does not parse is not repeated",
			err:  &url.Error{Err: cause, Op: "Get", URL: "https://tracker.test/%zz?passkey=secret"},
			want: `Get "(address redacted)": connection reset`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := redactAddresses(tc.err)
			require.EqualError(t, got, tc.want)
			require.ErrorIs(t, got, cause, "the redacted error lost its cause")
			if tc.want != tc.err.Error() {
				var urlErr *url.Error
				require.NotErrorAs(t, got, &urlErr, "the unredacted URL is reachable through the redacted error")
			}
		})
	}
}

func TestRedactedPath(t *testing.T) {
	for _, tc := range []struct{ name, path, want string }{
		{name: "a path without a query is kept", path: "/browse.php", want: "/browse.php"},
		{name: "a value is redacted, its key kept", path: "/rss.php?passkey=SAMPLEKEY", want: "/rss.php?passkey=…"},
		{
			// Base64 padding puts an "=" inside the value, which must not
			// make the run before it read as a key.
			name: "a value holding an equals sign is redacted whole",
			path: "/rss.php?passkey=c2FtcGxla2V5==&t=1",
			want: "/rss.php?passkey=…==&t=…",
		},
		{name: "a bare query is redacted", path: "/rss.php?SAMPLEKEY", want: "/rss.php?…"},
		{name: "a fragment is redacted", path: "/rss.php#key=SAMPLEKEY", want: "/rss.php#…=…"},
		{
			name: "an action is kept",
			path: "/s.php?q={{ .Keywords }}&pk=SAMPLEKEY",
			want: "/s.php?q={{ .Keywords }}&pk=…",
		},
		{
			// A "?" inside an action is not the start of the query.
			name: "a question mark inside an action is not the query",
			path: `/search/{{ if .Query.Q }}?q={{ .Query.Q }}{{ end }}`,
			want: `/search/{{ if .Query.Q }}?q={{ .Query.Q }}{{ end }}`,
		},
		{
			name: "a passkey in a path segment is redacted",
			path: "/rss/0123456789abcdef0123/feed.xml",
			want: "/rss/…/feed.xml",
		},
		{
			name: "a passkey in an action's string is redacted",
			path: `/s.php?pk={{ "0123456789abcdef0123" }}`,
			want: `/s.php?pk={{ "…" }}`,
		},
		{
			name: "a passkey written as a key is redacted",
			path: "/rss.php?{{ .Config.uid }}&0123456789abcdef0123=1",
			want: "/rss.php?{{ .Config.uid }}&…=…",
		},
		{name: "a dated segment is kept", path: "/archive/2024-01-01-releases/{{ .Keywords }}", want: "/archive/2024-01-01-releases/{{ .Keywords }}"},
		{name: "an id list is kept", path: `/s.php?{{ if eq .Config.cat "1000_2000_3000_40" }}c=1{{ end }}`, want: `/s.php?{{ if eq .Config.cat "1000_2000_3000_40" }}c=…{{ end }}`},
		{name: "a long word without a digit is kept", path: "/browse/sampletrackercategories", want: "/browse/sampletrackercategories"},
		{name: "an unclosed action is dropped", path: "/s.php?pk=SAMPLEKEY{{ .Unclosed", want: "/s.php?pk=……"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, redactedPath(tc.path))
		})
	}
}

// A query FlareSolverr may echo is replaced wherever it appears. A bare word
// such as the keywords would match unrelated text, so it is replaced only
// when it looks like a passkey on its own.
func TestRedactError(t *testing.T) {
	cause := errors.New("Error: timeout after 10 s loading q=test&pk=SAMPLEKEY while testing 0123456789abcdef0123")
	for _, tc := range []struct {
		name    string
		queries []string
		want    string
	}{
		{
			name:    "a query is replaced",
			queries: []string{"q=test&pk=SAMPLEKEY"},
			want:    "Error: timeout after 10 s loading (query redacted) while testing 0123456789abcdef0123",
		},
		{name: "a bare word is left alone", queries: []string{"test", "1"}, want: cause.Error()},
		{name: "a long bare word without a digit is left alone", queries: []string{"timeout after 10"}, want: cause.Error()},
		{
			name:    "a long bare query is replaced",
			queries: []string{"0123456789abcdef0123"},
			want:    "Error: timeout after 10 s loading q=test&pk=SAMPLEKEY while testing (query redacted)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.EqualError(t, redactError(cause, nil, tc.queries), tc.want)
		})
	}
}

// A record holding no error reaches the wrapped handler as it was sent,
// and an error is redacted wherever it sits in the record.
func TestRedactingHandler_Handle(t *testing.T) {
	var logged strings.Builder
	logger := slog.New(redactingHandler{slog.NewJSONHandler(&logged, nil)})

	logger.Info("example", "page", "/browse.php", slog.Group("request", "status", 200))
	require.Contains(t, logged.String(), `"page":"/browse.php"`)
	require.Contains(t, logged.String(), `"request":{"status":200}`)

	logged.Reset()
	logger.Warn("example", slog.Group("request",
		"error", &url.Error{Err: errors.New("boom"), Op: "Get", URL: "https://tracker.test/s.php?passkey=SAMPLEKEY"}))
	require.NotContains(t, logged.String(), "SAMPLEKEY", "an error inside a group was not redacted")

	var resolved int
	logger.Warn("example", "dump", countingValuer{&resolved},
		"error", &url.Error{Err: errors.New("boom"), Op: "Get", URL: "https://tracker.test/s.php?passkey=SAMPLEKEY"})
	require.Equal(t, 1, resolved, "a lazily rendered value was rendered more than once")
}

// countingValuer is a slog.LogValuer that counts how often it is resolved.
type countingValuer struct{ count *int }

func (v countingValuer) LogValue() slog.Value {
	*v.count++
	return slog.StringValue("rendered")
}
