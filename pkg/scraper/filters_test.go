// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"bytes"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestApplyFilter_URLEncodeDecode(t *testing.T) {
	logger := testLogger()

	encoded, err := applyFilter("a b/c", Filter{Name: "urlencode"}, templateData{}, logger)
	require.NoError(t, err)
	require.Equal(t, "a+b%2Fc", encoded)

	decoded, err := applyFilter(encoded, Filter{Name: "urldecode"}, templateData{}, logger)
	require.NoError(t, err)
	require.Equal(t, "a b/c", decoded)
}

// Jackett has no "andmatch": an unknown filter is a no-op there, so
// rewriting a query here would send a tracker terms Jackett never sends.
func TestApplyFilter_AndMatchIsNotRegistered(t *testing.T) {
	_, err := applyFilter("some show name", Filter{Name: "andmatch"}, templateData{}, testLogger())
	require.Error(t, err)
}

func TestApplyFilter_DateParse(t *testing.T) {
	got, err := applyFilter("2024-03-15", Filter{Args: "yyyy-MM-dd", Name: "dateparse"}, templateData{}, testLogger())
	require.NoError(t, err)

	parsed, err := time.Parse(time.RFC3339, got)
	require.NoError(t, err)
	require.Equal(t, 2024, parsed.Year())
	require.Equal(t, time.March, parsed.Month())
	require.Equal(t, 15, parsed.Day())
}

func TestApplyFilter_TimeAgo(t *testing.T) {
	before := time.Now().Add(-2 * time.Hour)

	got, err := applyFilter("posted 2 hours ago", Filter{Name: "timeago"}, templateData{}, testLogger())
	require.NoError(t, err)

	parsed, err := time.Parse(time.RFC3339, got)
	require.NoError(t, err)
	require.WithinDuration(t, before, parsed, 5*time.Second)
}

func TestApplyFilter_TimeAgo_NoMatch(t *testing.T) {
	_, err := applyFilter("not a relative time", Filter{Name: "timeago"}, templateData{}, testLogger())
	require.Error(t, err)
}

// Jackett's FromTimeAgo sums every number/unit pair it finds and accepts
// the abbreviations trackers actually print, so a definition rendering
// "1 day 2 hours ago" or "3h ago" resolves to the same instant here.
func TestApplyFilter_TimeAgo_JackettForms(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		ago   time.Duration
	}{
		{name: "single unit", value: "2 hours ago", ago: 2 * time.Hour},
		{name: "compound units sum", value: "1 day 2 hours ago", ago: 26 * time.Hour},
		{name: "filler is ignored", value: "1 day, and 2 hours ago", ago: 26 * time.Hour},
		{name: "abbreviated minutes", value: "5 min ago", ago: 5 * time.Minute},
		{name: "abbreviated hours", value: "3h ago", ago: 3 * time.Hour},
		{name: "abbreviated weeks", value: "2 wks ago", ago: 14 * 24 * time.Hour},
		{name: "fractional value", value: "1.5 hours ago", ago: 90 * time.Minute},
		{name: "now", value: "now", ago: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := applyFilter(tc.value, Filter{Name: "timeago"}, templateData{}, testLogger())
			require.NoError(t, err)

			parsed, err := time.Parse(time.RFC3339, got)
			require.NoError(t, err)
			require.WithinDuration(t, time.Now().Add(-tc.ago), parsed, 5*time.Second)
		})
	}
}

// An absolute date must not be mistaken for relative text, so that
// timeparse and fuzzytime fall through to absolute parsing instead.
func TestApplyFilter_TimeAgo_RejectsAbsoluteDates(t *testing.T) {
	for _, value := range []string{"2024-03-15 18:30:00", "15 Mar 2024", "2024-03-15T18:30:00Z"} {
		_, err := parseTimeAgo(value)
		require.Error(t, err, "parseTimeAgo(%q) should not claim an absolute date", value)
	}
}

// The filter registry declares each filter's arity in one place. Filters
// that never validated their arguments must keep ignoring surplus ones,
// and those that did must keep rejecting the wrong count.
func TestApplyFilter_Arity(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	run := func(name string, args any, value string) (string, error) {
		return applyFilter(value, Filter{Args: args, Name: name}, templateData{}, logger)
	}

	t.Run("surplus args are ignored where they always were", func(t *testing.T) {
		for _, name := range []string{"tolowercase", "touppercase", "trim", "urlencode", "reverse", "htmldecode"} {
			_, err := run(name, []any{"extra", "args"}, "Value")
			require.NoError(t, err, "%s rejected surplus args", name)
		}
	})

	t.Run("trim still takes an optional cutset", func(t *testing.T) {
		got, err := run("trim", nil, "  padded  ")
		require.NoError(t, err)
		require.Equal(t, "padded", got, "trim()")

		// Jackett trims cutset[0] only, so the trailing "y" survives.
		got, err = run("trim", "xy", "xyvaluexy")
		require.NoError(t, err)
		require.Equal(t, "yvaluexy", got, "trim(cutset)")
	})

	t.Run("a wrong count is still rejected", func(t *testing.T) {
		for _, tc := range []struct {
			args any
			name string
		}{
			{args: []any{"only-one"}, name: "re_replace"},
			{args: []any{"-"}, name: "split"},
			{args: nil, name: "append"},
			{args: nil, name: "prepend"},
			{args: nil, name: "querystring"},
			{args: []any{"a"}, name: "replace"},
		} {
			_, err := run(tc.name, tc.args, "value")
			require.Error(t, err, "%s accepted the wrong argument count", tc.name)
		}
	})

	t.Run("an unknown filter is reported", func(t *testing.T) {
		_, err := run("no-such-filter", nil, "value")
		require.Error(t, err, "an unknown filter was accepted")
	})
}

func TestApplyFilter_StringAndExtractionFilters(t *testing.T) {
	logger := testLogger()
	tests := []struct {
		args   any
		filter string
		name   string
		value  string
		want   string
	}{
		{args: " Release", filter: "append", name: "append", value: "Example", want: "Example Release"},
		{args: "Example ", filter: "prepend", name: "prepend", value: "Release", want: "Example Release"},
		{args: []any{"|", 1}, filter: "split", name: "split", value: "left|middle|right", want: "middle"},
		{args: nil, filter: "htmlencode", name: "html encode", value: `<tag attr="x">`, want: "&lt;tag attr=&quot;x&quot;&gt;"},
		{args: nil, filter: "htmlencode", name: "html encode latin-1", value: "Grüße", want: "Gr&#252;&#223;e"},
		{args: nil, filter: "htmlencode", name: "html encode leaves other scripts alone", value: "Привет", want: "Привет"},
		{args: nil, filter: "htmldecode", name: "html decode", value: "&lt;tag&gt;", want: "<tag>"},
		{args: []any{`Example-(\d+)`}, filter: "regexp", name: "regexp capture", value: "prefix-Example-42", want: "42"},
		{args: []any{"pending", "ready"}, filter: "validate", name: "validate", value: "ready", want: "ready"},
		{args: []any{"foo", "bar"}, filter: "validate", name: "validate keeps only accepted tokens", value: "bar baz", want: "bar"},
		{args: []any{"ready"}, filter: "validate", name: "validate empties an unaccepted value", value: "unexpected", want: ""},
		{args: []any{`zzz(\d+)`}, filter: "regexp", name: "regexp without a match is empty", value: "abc", want: ""},
		{args: []any{`\d+`}, filter: "regexp", name: "regexp without a group is empty", value: "abc123", want: ""},
		{args: []any{"::", 1}, filter: "split", name: "split uses the first separator char", value: "a::b::c", want: ""},
		{args: []any{"-", -1}, filter: "split", name: "split wraps a negative index", value: "a-b-c", want: "c"},
		{args: "id", filter: "querystring", name: "query string", value: "https://example.invalid/item?id=42", want: "42"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := applyFilter(tc.value, Filter{Args: tc.args, Name: tc.filter}, templateData{}, logger)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestApplyFilter_ErrorsForInvalidValues(t *testing.T) {
	for _, tc := range []struct {
		args  any
		name  string
		value string
	}{
		{args: []any{"|", 4}, name: "split", value: "one|two"},
		{args: []any{`(Example`}, name: "regexp", value: "no match"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := applyFilter(tc.value, Filter{Args: tc.args, Name: tc.name}, templateData{}, testLogger())
			require.Error(t, err)
		})
	}
}

// Jackett's re_replace takes a .NET replacement string, whose reference
// syntax differs from Go's as soon as a group is followed by text.
func TestApplyFilter_ReReplaceDotNetSyntax(t *testing.T) {
	for _, tc := range []struct {
		name, pattern, replacement, value, want string
	}{
		{name: "group followed by text", pattern: `(\d+)`, replacement: "$1x", value: "12", want: "12x"},
		{name: "whole match", pattern: `\d+`, replacement: "[$&]", value: "a12b", want: "a[12]b"},
		{name: "literal dollar", pattern: `(a)`, replacement: "$$$1", value: "a", want: "$a"},
		{name: "braced groups", pattern: `(\d)(\d)`, replacement: "${2}${1}", value: "12", want: "21"},
		{name: "plain group", pattern: `(\d+)`, replacement: "$1", value: "12", want: "12"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := applyFilter(tc.value, Filter{Args: []any{tc.pattern, tc.replacement}, Name: "re_replace"}, templateData{}, testLogger())
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

// The URL filters round-trip through the definition's character set, so a
// windows-1251 tracker receives cp1251 bytes rather than UTF-8 ones.
func TestApplyFilter_URLFiltersHonourEncoding(t *testing.T) {
	run := func(name, value, enc string) (string, error) {
		return applyFilter(value, Filter{Name: name}, templateData{encoding: enc}, testLogger())
	}

	t.Run("dotnet unreserved set", func(t *testing.T) {
		got, err := run("urlencode", "a b!*()~", "")
		require.NoError(t, err)
		require.Equal(t, "a+b!*()%7E", got)
	})

	t.Run("windows-1251", func(t *testing.T) {
		got, err := run("urlencode", "Привет", "windows-1251")
		require.NoError(t, err)
		require.Equal(t, "%CF%F0%E8%E2%E5%F2", got)

		back, err := run("urldecode", got, "windows-1251")
		require.NoError(t, err)
		require.Equal(t, "Привет", back)
	})

	t.Run("utf-8 by default", func(t *testing.T) {
		got, err := run("urlencode", "Привет", "")
		require.NoError(t, err)
		require.Equal(t, "%D0%9F%D1%80%D0%B8%D0%B2%D0%B5%D1%82", got)
	})
}

func TestApplyFilter_JSONJoinArray(t *testing.T) {
	run := func(path, sep, value string) (string, error) {
		return applyFilter(value, Filter{Args: []any{path, sep}, Name: "jsonjoinarray"}, templateData{}, testLogger())
	}

	t.Run("joins an array", func(t *testing.T) {
		got, err := run("$.tags", "|", `{"tags":["hd","x264"]}`)
		require.NoError(t, err)
		require.Equal(t, "hd|x264", got)
	})

	t.Run("numbers keep their scalar form", func(t *testing.T) {
		got, err := run("a", ",", `{"a":[1,2,3]}`)
		require.NoError(t, err)
		require.Equal(t, "1,2,3", got)
	})

	t.Run("reports a path that is not an array", func(t *testing.T) {
		_, err := run("a", ",", `{"a":"scalar"}`)
		require.Error(t, err)
	})
}

// Jackett registers "reltime" as an alias for "timeago", and leaves the
// value untouched for the two debug-only dump filters.
func TestApplyFilter_RegisteredAliasesAndNoops(t *testing.T) {
	t.Run("reltime resolves relative text", func(t *testing.T) {
		got, err := applyFilter("2 hours ago", Filter{Name: "reltime"}, templateData{}, testLogger())
		require.NoError(t, err)

		parsed, err := time.Parse(time.RFC3339, got)
		require.NoError(t, err)
		require.WithinDuration(t, time.Now().Add(-2*time.Hour), parsed, 5*time.Second)
	})

	for _, name := range []string{"hexdump", "strdump"} {
		t.Run(name+" leaves the value alone", func(t *testing.T) {
			got, err := applyFilter("untouched", Filter{Name: name}, templateData{}, testLogger())
			require.NoError(t, err)
			require.Equal(t, "untouched", got)
		})
	}
}

// "fuzzytime" is Jackett's FromUnknown: whatever shape the tracker prints.
func TestApplyFilter_FuzzyTimeShapes(t *testing.T) {
	t.Run("unix timestamp", func(t *testing.T) {
		got, err := applyFilter("1710526200", Filter{Name: "fuzzytime"}, templateData{}, testLogger())
		require.NoError(t, err)

		parsed, err := time.Parse(time.RFC3339, got)
		require.NoError(t, err)
		require.Equal(t, int64(1710526200), parsed.Unix())
	})

	t.Run("rfc1123z round-trips", func(t *testing.T) {
		got, err := applyFilter("Fri, 15 Mar 2024 18:10:00 +0000", Filter{Name: "fuzzytime"}, templateData{}, testLogger())
		require.NoError(t, err)

		parsed, err := time.Parse(time.RFC3339, got)
		require.NoError(t, err)
		require.Equal(t, int64(1710526200), parsed.Unix())
	})
}

// A Go layout is used directly, and both spellings of the filter take one.
func TestApplyFilter_DateParseUsesLayout(t *testing.T) {
	for _, name := range []string{"dateparse", "timeparse"} {
		t.Run(name, func(t *testing.T) {
			// Ambiguous without the layout: this is 3 February, not 2 March.
			got, err := applyFilter("03.02.2024", Filter{Args: "02.01.2006", Name: name}, templateData{}, testLogger())
			require.NoError(t, err)

			parsed, err := time.Parse(time.RFC3339, got)
			require.NoError(t, err)
			require.Equal(t, time.February, parsed.Month())
			require.Equal(t, 3, parsed.Day())
		})
	}
}

// The template-function form of re_replace takes the same .NET
// replacement string as the filter, so it needs the same translation.
func TestTemplateReReplaceDotNetSyntax(t *testing.T) {
	got, err := templateReReplace("S01E02", `S(\d+)E(\d+)`, "season $1 episode $2x")
	require.NoError(t, err)
	require.Equal(t, "season 01 episode 02x", got)

	whole, err := templateReReplace("a12b", `\d+`, "[$&]")
	require.NoError(t, err)
	require.Equal(t, "a[12]b", whole)
}

// The dump filters exist only for their output: a definition reaching for
// one to diagnose a selector or an encoding problem must not get silence.
func TestApplyFilter_DumpFiltersLog(t *testing.T) {
	run := func(t *testing.T, name string, args any, value string) string {
		t.Helper()
		var logged bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))

		got, err := applyFilter(value, Filter{Args: args, Name: name}, templateData{}, logger)
		require.NoError(t, err)
		require.Equal(t, value, got, "%s must pass the value through untouched", name)
		return logged.String()
	}

	t.Run("hexdump shows each character's code point", func(t *testing.T) {
		// A non-breaking space is exactly what hexdump is for: it looks
		// like a space and is why a trim quietly did nothing.
		logged := run(t, "hexdump", nil, "a b")
		require.Contains(t, logged, "a(61)")
		require.Contains(t, logged, "(A0)")
		require.Contains(t, logged, "b(62)")
	})

	t.Run("strdump spells out invisible whitespace", func(t *testing.T) {
		logged := run(t, "strdump", nil, "a b\r\n")
		require.Contains(t, logged, `a\xA0b\r\n`)
	})

	t.Run("strdump carries its tag", func(t *testing.T) {
		logged := run(t, "strdump", "before-trim", "value")
		require.Contains(t, logged, "before-trim")
		require.Contains(t, logged, "value")
	})
}
