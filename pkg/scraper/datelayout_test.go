// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The .NET tokens a real definition writes have to come out as the Go
// layout that means the same thing.
func TestGoLayoutFromDotNet(t *testing.T) {
	for _, tc := range []struct{ name, dotNet, want string }{
		{name: "date", dotNet: "yyyy-MM-dd", want: "2006-01-02"},
		{name: "day first", dotNet: "dd/MM/yyyy", want: "02/01/2006"},
		{name: "month first", dotNet: "MM/dd/yyyy", want: "01/02/2006"},
		{name: "with time", dotNet: "dd.MM.yyyy HH:mm", want: "02.01.2006 15:04"},
		{name: "month name", dotNet: "d MMM yyyy", want: "2 Jan 2006"},
		{name: "long names", dotNet: "dddd, dd MMMM yyyy", want: "Monday, 02 January 2006"},
		{name: "12 hour", dotNet: "hh:mm tt", want: "03:04 PM"},
		{name: "offset", dotNet: "yyyy-MM-ddTHH:mm:sszzz", want: "2006-01-02T15:04:05-07:00"},
		{name: "round trip", dotNet: "yyyy-MM-ddTHH:mm:ssK", want: "2006-01-02T15:04:05Z07:00"},
		// An unrecognized character is a literal in .NET, which is what
		// makes the "T" of an ISO-8601 format legal.
		{name: "literal T", dotNet: "yyyy-MM-ddTHH:mm:ss", want: "2006-01-02T15:04:05"},
		{name: "escaped literal", dotNet: `yyyy\Tdd`, want: "2006T02"},
		{name: "quoted literal", dotNet: `yyyy'at'dd`, want: "2006at02"},
		// A fraction token is only the digits: the separator in front of it
		// is a literal the format already supplies. Emitting one here too
		// would produce "05..000", which parses nothing.
		{name: "fraction", dotNet: "ss.fff", want: "05.000"},
		{name: "wide fraction", dotNet: "ss.ffffff", want: "05.000000"},
		{name: "optional fraction", dotNet: "ss.FFF", want: "05.999"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := goLayoutFromDotNet(tc.dotNet)
			require.True(t, ok)
			require.Equal(t, tc.want, got.layout)
		})
	}
}

// Only a malformed format is refused; everything else is literals.
func TestGoLayoutFromDotNet_Malformed(t *testing.T) {
	for _, format := range []string{`yyyy\`, `yyyy'unterminated`} {
		_, ok := goLayoutFromDotNet(format)
		require.False(t, ok, "%q should be refused", format)
	}
}

// The gate is what keeps a Go layout away from the .NET path; a layout
// that slips past it still has to survive, because the conversion is only
// ever tried and never trusted.
func TestLooksLikeDotNetLayout(t *testing.T) {
	for _, layout := range []string{"yyyy-MM-dd", "dd/MM/yyyy", "HH:mm"} {
		require.True(t, looksLikeDotNetLayout(layout), "%q", layout)
	}
	for _, layout := range []string{"2006-01-02", "02/01/2006", "Mon, 02 Jan 2006", "15:04:05"} {
		require.False(t, looksLikeDotNetLayout(layout), "%q", layout)
	}
}

// Formats carrying a fractional second have to actually parse, which is
// what the duplicated separator prevented.
func TestParseDeclaredLayout_Fractions(t *testing.T) {
	for _, tc := range []struct{ layout, value string }{
		{layout: "yyyy-MM-dd HH:mm:ss.fff", value: "2024-03-15 18:30:00.123"},
		{layout: "yyyy-MM-ddTHH:mm:ss", value: "2024-03-15T18:30:00"},
		{layout: "yyyy-MM-ddTHH:mm:sszzz", value: "2024-03-15T18:30:00+03:00"},
	} {
		t.Run(tc.layout, func(t *testing.T) {
			parsed, err := parseDeclaredLayout(tc.value, tc.layout)
			require.NoError(t, err)
			require.Equal(t, 2024, parsed.Year())
			require.Equal(t, 15, parsed.Day())
			require.Equal(t, 18, parsed.Hour())
		})
	}
}

// A layout carrying no date at all is a time of day, which a tracker
// prints for today's releases. .NET dates it to today; Go would date it to
// the 1st of January in year zero, which is not a release date at all.
func TestParseDeclaredLayout_TimeOnly(t *testing.T) {
	for _, tc := range []struct {
		name, layout, value string
		hour, minute        int
	}{
		{name: "dotnet 24 hour", layout: "HH:mm", value: "18:30", hour: 18, minute: 30},
		{name: "dotnet with seconds", layout: "HH:mm:ss", value: "06:05:04", hour: 6, minute: 5},
		{name: "dotnet 12 hour", layout: "hh:mm tt", value: "06:30 PM", hour: 18, minute: 30},
		{name: "go layout", layout: "15:04", value: "18:30", hour: 18, minute: 30},
		{name: "go layout with seconds", layout: "15:04:05", value: "06:05:04", hour: 6, minute: 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := parseDeclaredLayout(tc.value, tc.layout)
			require.NoError(t, err)

			today := time.Now()
			require.Equal(t, today.Year(), parsed.Year())
			require.Equal(t, today.Month(), parsed.Month())
			require.Equal(t, today.Day(), parsed.Day())
			require.Equal(t, tc.hour, parsed.Hour())
			require.Equal(t, tc.minute, parsed.Minute())
		})
	}
}

// The year step-back must not reach a time of day: a release stamped a few
// minutes ahead of us, which a clock or timezone skew alone produces,
// happened a moment ago rather than last year.
func TestParseDeclaredLayout_TimeOnlyIsNotRolledBack(t *testing.T) {
	// A minute from now, so the value is unambiguously in the future.
	future := time.Now().Add(time.Minute)

	parsed, err := parseDeclaredLayout(future.Format("15:04"), "15:04")
	require.NoError(t, err)
	require.Equal(t, future.Year(), parsed.Year())
	require.Equal(t, future.Day(), parsed.Day())
}

// layoutHasDate is what separates the two cases, so it has to answer for
// every shape of layout the converter can produce.
func TestLayoutHasDate(t *testing.T) {
	for _, layout := range []string{
		"2006-01-02", "02/01/2006", "2 Jan 2006", "Monday, 02 January 2006",
		"02 Jan", "01/02", "2006", "Mon 15:04",
	} {
		require.True(t, layoutHasDate(layout), "%q carries a date", layout)
	}
	for _, layout := range []string{"15:04", "15:04:05", "03:04 PM", "15:04:05.000"} {
		require.False(t, layoutHasDate(layout), "%q is a time of day", layout)
	}
}

// A tracker printing no offset means its own wall clock, which is what
// Jackett's date utilities read it as. Parsing in UTC instead puts the
// stored instant hours out on any host that is not itself UTC.
func TestParseDeclaredLayout_ZonelessIsLocal(t *testing.T) {
	for _, tc := range []struct{ name, layout, value string }{
		{name: "dotnet", layout: "yyyy-MM-dd HH:mm", value: "2024-03-15 18:30"},
		{name: "go layout", layout: "2006-01-02 15:04", value: "2024-03-15 18:30"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := parseDeclaredLayout(tc.value, tc.layout)
			require.NoError(t, err)
			require.Equal(t, time.Date(2024, time.March, 15, 18, 30, 0, 0, time.Local), parsed) //nolint:gosmopolitan // asserting the local-time behaviour under test, on whatever host runs it
		})
	}

	// An offset in the value still wins over the location.
	t.Run("an explicit offset overrides", func(t *testing.T) {
		parsed, err := parseDeclaredLayout("2024-03-15T18:30:00+05:00", "yyyy-MM-ddTHH:mm:sszzz")
		require.NoError(t, err)
		require.Equal(t, time.Date(2024, time.March, 15, 13, 30, 0, 0, time.UTC), parsed.UTC())
	})
}

// The heuristic path resolves zones the same way, since it is what the
// date field goes through when a definition declares no date filter.
func TestParseUnknownTime_ZonelessIsLocal(t *testing.T) {
	parsed, err := parseUnknownTimeValue("2024-03-15 18:30:00")
	require.NoError(t, err)
	require.Equal(t, time.Date(2024, time.March, 15, 18, 30, 0, 0, time.Local).UTC(), parsed.UTC()) //nolint:gosmopolitan // asserting the local-time behaviour under test, on whatever host runs it

	withOffset, err := parseUnknownTimeValue("2024-03-15T18:30:00+05:00")
	require.NoError(t, err)
	require.Equal(t, time.Date(2024, time.March, 15, 13, 30, 0, 0, time.UTC), withOffset.UTC())
}

// .NET compares an AM/PM designator case-insensitively; Go's "PM" token
// matches only the uppercase pair. 1337x lists "7am Sep. 14" against a
// "htt MMM. d" layout, so both spellings have to parse.
func TestParseDeclaredLayout_DesignatorCase(t *testing.T) {
	for _, value := range []string{"7am Sep. 14", "7AM Sep. 14", "7pm Sep. 14"} {
		t.Run(value, func(t *testing.T) {
			parsed, err := parseDeclaredLayout(value, "htt MMM. d")
			require.NoError(t, err)
			require.Equal(t, time.September, parsed.Month())
			require.Equal(t, 14, parsed.Day())
			require.Equal(t, time.Now().Year(), parsed.Year(), "the layout carries no year")
		})
	}

	// The other two layouts 1337x declares.
	t.Run("two digit year", func(t *testing.T) {
		parsed, err := parseDeclaredLayout("Apr. 18 11", "MMM. d yy")
		require.NoError(t, err)
		require.Equal(t, 2011, parsed.Year())
		require.Equal(t, time.April, parsed.Month())
		require.Equal(t, 18, parsed.Day())
	})
}

// Go's tokens are fixed-width, so .NET's single "t" and "z" cannot be
// represented directly. Refusing them would leave those layouts parsing
// nothing, so the value is widened to meet the token instead.
func TestParseDeclaredLayout_NarrowTokens(t *testing.T) {
	t.Run("single t takes a one-letter designator", func(t *testing.T) {
		for _, value := range []string{"7a Sep. 14", "7A Sep. 14", "7p Sep. 14"} {
			parsed, err := parseDeclaredLayout(value, "ht MMM. d")
			require.NoError(t, err, "%q", value)
			require.Equal(t, time.September, parsed.Month())
			require.Equal(t, 14, parsed.Day())
		}
	})

	// .NET's "t" is the designator's first character, not an abbreviation
	// of it, so the wide form is a mismatch rather than a courtesy. Go's
	// two-character token would accept it, which is exactly why the
	// unwidened value is not a candidate for this token.
	t.Run("single t rejects a full designator", func(t *testing.T) {
		_, err := parseDeclaredLayout("7 PM", "h t")
		require.Error(t, err)

		parsed, err := parseDeclaredLayout("7 P", "h t")
		require.NoError(t, err)
		require.Equal(t, 19, parsed.Hour())
	})

	// A single numeric specifier is lenient in .NET, so the padded form
	// stays acceptable where the designator's does not.
	t.Run("single y takes a year without its leading zero", func(t *testing.T) {
		parsed, err := parseDeclaredLayout("9-6-15", "y-M-d")
		require.NoError(t, err)
		require.Equal(t, 2009, parsed.Year())
		require.Equal(t, time.June, parsed.Month())
		require.Equal(t, 15, parsed.Day())

		padded, err := parseDeclaredLayout("09-6-15", "y-M-d")
		require.NoError(t, err)
		require.Equal(t, 2009, padded.Year())
	})

	t.Run("single z takes a one-digit offset", func(t *testing.T) {
		parsed, err := parseDeclaredLayout("2024-03-15 18:30 +5", "yyyy-MM-dd HH:mm z")
		require.NoError(t, err)
		require.Equal(t, "2024-03-15T13:30:00Z", parsed.UTC().Format(time.RFC3339))
	})

	t.Run("a two-digit offset still works under the narrow token", func(t *testing.T) {
		parsed, err := parseDeclaredLayout("2024-03-15 18:30 +05", "yyyy-MM-dd HH:mm z")
		require.NoError(t, err)
		require.Equal(t, "2024-03-15T13:30:00Z", parsed.UTC().Format(time.RFC3339))
	})

	// Widening must not touch an ordinary letter: the "p" of "Sep." is not
	// a designator, and only a letter following the hour is treated as one.
	t.Run("widening leaves ordinary letters alone", func(t *testing.T) {
		parsed, err := parseDeclaredLayout("Sep. 14 2024", "MMM. d yyyy")
		require.NoError(t, err)
		require.Equal(t, time.September, parsed.Month())
		require.Equal(t, 2024, parsed.Year())
	})

	// The wide forms are unaffected and need no fix-up.
	for _, tc := range []struct{ format, want string }{
		{format: "hh:mm tt", want: "03:04 PM"},
		{format: "HH:mm zz", want: "15:04 -07"},
		{format: "HH:mm zzz", want: "15:04 -07:00"},
	} {
		t.Run(tc.format, func(t *testing.T) {
			got, ok := goLayoutFromDotNet(tc.format)
			require.True(t, ok)
			require.Equal(t, tc.want, got.layout)
			require.Zero(t, got.designators)
			require.Zero(t, got.shortOffsets)
		})
	}
}

// .NET allows its tokens anywhere in a format, so the fix-ups that widen a
// value to meet Go's fixed-width tokens cannot assume where they sit.
func TestParseDeclaredLayout_NarrowTokensAnywhere(t *testing.T) {
	t.Run("designator leading", func(t *testing.T) {
		morning, err := parseDeclaredLayout("a 07:30", "t hh:mm")
		require.NoError(t, err)
		require.Equal(t, 7, morning.Hour())

		evening, err := parseDeclaredLayout("P 07:30", "t hh:mm")
		require.NoError(t, err)
		require.Equal(t, 19, evening.Hour())
	})

	t.Run("offset leading", func(t *testing.T) {
		parsed, err := parseDeclaredLayout("+5 hours 2024-03-15", "z 'hours' yyyy-MM-dd")
		require.NoError(t, err)
		require.Equal(t, "2024-03-14T19:00:00Z", parsed.UTC().Format(time.RFC3339))
	})

	// A sign directly after a digit is a separator inside a date, not an
	// offset, so widening must leave it alone.
	t.Run("a single-digit date is not an offset", func(t *testing.T) {
		parsed, err := parseDeclaredLayout("2024-3-15 +5", "yyyy-M-d z")
		require.NoError(t, err)
		require.Equal(t, time.March, parsed.Month())
		require.Equal(t, 15, parsed.Day())
		require.Equal(t, "2024-03-14T19:00:00Z", parsed.UTC().Format(time.RFC3339))
	})
}

// A format may carry the same narrow token more than once, and widening
// one position is then not enough: every occurrence needs its own.
func TestParseDeclaredLayout_RepeatedNarrowTokens(t *testing.T) {
	t.Run("two designators", func(t *testing.T) {
		parsed, err := parseDeclaredLayout("A 7 A", "t h t")
		require.NoError(t, err)
		require.Equal(t, 7, parsed.Hour())
	})

	t.Run("designator and short year", func(t *testing.T) {
		parsed, err := parseDeclaredLayout("9-6-15 7 P", "y-M-d h t")
		require.NoError(t, err)
		require.Equal(t, 2009, parsed.Year())
		require.Equal(t, 19, parsed.Hour())
	})
}

// .NET reads a run of one specifier as a single token, so a longest-match
// table must not decompose "ttt" into "tt" plus a stray "t" -- that would
// build the designator twice and demand a one-letter one as well.
func TestGoLayoutFromDotNet_TokenRuns(t *testing.T) {
	for _, tc := range []struct {
		format, want string
		designators  int
	}{
		{format: "h tt", want: "3 PM"},
		{format: "h ttt", want: "3 PM"},
		{format: "h tttt", want: "3 PM"},
		{format: "h t", want: "3 PM", designators: 1},
		// A run longer than the table defines is clamped, as .NET clamps it.
		{format: "hhh:mmm", want: "03:04"},
		{format: "ddddd", want: "Monday"},
		{format: "MMMMM", want: "January"},
		{format: "zzzz", want: "-07:00"},
	} {
		t.Run(tc.format, func(t *testing.T) {
			got, ok := goLayoutFromDotNet(tc.format)
			require.True(t, ok)
			require.Equal(t, tc.want, got.layout)
			require.Equal(t, tc.designators, got.designators)
			require.Zero(t, got.shortOffsets)
		})
	}

	t.Run("a long designator run takes the full designator", func(t *testing.T) {
		parsed, err := parseDeclaredLayout("7 PM", "h ttt")
		require.NoError(t, err)
		require.Equal(t, 19, parsed.Hour())
	})

	// "K" is the one specifier .NET does not repeat, so a second one is a
	// token of its own rather than part of a run.
	t.Run("K does not run together", func(t *testing.T) {
		got, ok := goLayoutFromDotNet("KK")
		require.True(t, ok)
		require.Equal(t, "Z07:00Z07:00", got.layout)
	})
}

// Go has no way to escape a literal, so a .NET format quoting a word that
// is also a reference-time token cannot be represented: the Go layout it
// would become accepts any month name where .NET requires that one word.
// Refusing is right here -- a wrong parse is worse than none.
func TestGoLayoutFromDotNet_LiteralsThatAreTokens(t *testing.T) {
	for _, format := range []string{
		"yyyy 'January' dd",
		"yyyy '2006' dd",
		"yyyy 'Mon' dd",
		`yyyy \J\a\n dd`,
		// An unquoted run is a literal in .NET too, and has the same problem.
		"yyyy Jan dd",
	} {
		_, ok := goLayoutFromDotNet(format)
		require.False(t, ok, "%q holds a literal Go would read as a token", format)
	}

	// Literals that mean nothing to Go still convert.
	for _, tc := range []struct{ format, want string }{
		{format: "yyyy 'at' dd", want: "2006 at 02"},
		{format: `yyyy\Tdd`, want: "2006T02"},
		{format: "yyyy-MM-ddTHH:mm:ss", want: "2006-01-02T15:04:05"},
	} {
		got, ok := goLayoutFromDotNet(tc.format)
		require.True(t, ok, "%q", tc.format)
		require.Equal(t, tc.want, got.layout)
	}
}

// The whole point: a value the .NET format would reject must not be
// accepted here.
func TestParseDeclaredLayout_RejectsWrongLiteral(t *testing.T) {
	_, err := parseDeclaredLayout("2024 February 15", "yyyy 'January' dd")
	require.Error(t, err, "the literal word is required, not any month name")
}

// Two references sharing a clock cannot tell a literal "15" from the hour
// token: both render it as "15". The third reference differs in every
// component a Go token can produce, which is what catches them.
func TestLiteralIsInert_ClockAndZoneTokens(t *testing.T) {
	for _, text := range []string{
		"15", "04", "05", "3", "4", "5", "PM", "pm",
		"MST", "-07:00", "-0700", "-07", "Z07:00", ".000", ".999",
		"2006", "06", "January", "Jan", "Monday", "Mon", "01", "02",
		// A literal need only contain one: "5" here is the seconds token.
		"GMT+5", "at 15:04",
	} {
		require.False(t, literalIsInert(text), "%q holds a reference-time token", text)
	}

	for _, text := range []string{"", "at", "UTC", "T", "hours", " - ", "de", "года"} {
		require.True(t, literalIsInert(text), "%q is ordinary text", text)
	}
}

// A format quoting any of those cannot be represented, so it is refused
// rather than turned into a layout that accepts more than .NET would.
func TestGoLayoutFromDotNet_RefusesClockLiterals(t *testing.T) {
	for _, format := range []string{
		"yyyy '15' dd", "yyyy '04' dd", "yyyy '05' dd", "yyyy 'PM' dd",
		"yyyy 'MST' dd", "yyyy '-07:00' dd", "'GMT+5' yyyy-MM-dd",
	} {
		_, ok := goLayoutFromDotNet(format)
		require.False(t, ok, "%q", format)
	}

	// The value that would otherwise have been wrongly accepted.
	_, err := parseDeclaredLayout("2024 09 15", "yyyy '15' dd")
	require.Error(t, err, "the literal characters are required, not any hour")
}

// Which position a narrow token occupies is decided by parsing every
// candidate, not by guessing from context: a literal in the format can
// look exactly like the token, and only the parse can tell them apart.
func TestParseDeclaredLayout_NarrowTokenAgainstALiteral(t *testing.T) {
	// The format's own "P" is a literal; the designator is the second
	// letter. Widening the literal instead leaves the rest unparseable.
	t.Run("literal P is not the designator", func(t *testing.T) {
		morning, err := parseDeclaredLayout("P A 07:30", "'P' t hh:mm")
		require.NoError(t, err)
		require.Equal(t, 7, morning.Hour())

		// Split so the two letters read as what they are: the format's
		// own literal, then the designator.
		evening, err := parseDeclaredLayout("P "+"P 07:30", "'P' t hh:mm")
		require.NoError(t, err)
		require.Equal(t, 19, evening.Hour())
	})

	// A value already in the wide form is tried as it came.
	t.Run("an already-wide value still parses", func(t *testing.T) {
		parsed, err := parseDeclaredLayout("2024-03-15 18:30 +05", "yyyy-MM-dd HH:mm z")
		require.NoError(t, err)
		require.Equal(t, "2024-03-15T13:30:00Z", parsed.UTC().Format(time.RFC3339))
	})
}
