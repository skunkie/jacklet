// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"fmt"
	"strings"
	"time"
)

// Cardigann says a date filter's argument is a Go layout, but Jackett
// accepts two things there. DateTimeUtil.ParseDateTimeGoLang first tries
// the argument as a .NET custom format string -- gated on it containing a
// "y", "h" or "d" -- and only then treats it as a Go layout, which it
// translates into a .NET format. Real definitions carry both kinds, so a
// "yyyy-MM-dd" or "dd/MM/yyyy" has to parse as written rather than falling
// through to a guess: for "03/02/2024" a guess picks the wrong month.

// dotNetSpecifiers are the letters that mean something in a .NET custom
// date format. A letter outside this set cannot be one, which is what
// distinguishes "dd/MM/yyyy" from a Go layout like "Mon, 02 Jan 2006".
const dotNetSpecifiers = "dfFghHKmMstyz"

// dotNetTokens maps each .NET format token to its Go layout equivalent,
// longest first so that "yyyy" is never read as two "yy".
var dotNetTokens = []struct{ dotNet, golang string }{
	// A .NET fraction token is only the digits: the separator before it is
	// an ordinary literal, spelled out by the format as "ss.fff". Go wants
	// the separator inside its token, and gets it from that same literal
	// passing through, so emitting one here too would yield "ss..000" and
	// fail to parse a perfectly good timestamp.
	{dotNet: "fffffff", golang: "0000000"},
	{dotNet: "ffffff", golang: "000000"},
	{dotNet: "fffff", golang: "00000"},
	{dotNet: "ffff", golang: "0000"},
	{dotNet: "fff", golang: "000"},
	{dotNet: "ff", golang: "00"},
	{dotNet: "f", golang: "0"},
	// The capitals mean "drop trailing zeros", which Go spells with nines.
	{dotNet: "FFFFFFF", golang: "9999999"},
	{dotNet: "FFFFFF", golang: "999999"},
	{dotNet: "FFFFF", golang: "99999"},
	{dotNet: "FFFF", golang: "9999"},
	{dotNet: "FFF", golang: "999"},
	{dotNet: "FF", golang: "99"},
	{dotNet: "F", golang: "9"},
	{dotNet: "yyyy", golang: "2006"},
	{dotNet: "yyy", golang: "2006"},
	{dotNet: "yy", golang: "06"},
	// .NET's single "y" is a two-digit year written without a leading zero.
	// Go has no such token, so the value is widened to meet "06" instead;
	// see dotNetLayout below.
	{dotNet: "y", golang: "06"},
	{dotNet: "MMMM", golang: "January"},
	{dotNet: "MMM", golang: "Jan"},
	{dotNet: "MM", golang: "01"},
	{dotNet: "M", golang: "1"},
	{dotNet: "dddd", golang: "Monday"},
	{dotNet: "ddd", golang: "Mon"},
	{dotNet: "dd", golang: "02"},
	{dotNet: "d", golang: "2"},
	{dotNet: "HH", golang: "15"},
	// Go has no unpadded 24-hour token; "15" accepts both widths on input.
	{dotNet: "H", golang: "15"},
	{dotNet: "hh", golang: "03"},
	{dotNet: "h", golang: "3"},
	{dotNet: "mm", golang: "04"},
	{dotNet: "m", golang: "4"},
	{dotNet: "ss", golang: "05"},
	{dotNet: "s", golang: "5"},
	{dotNet: "tt", golang: "PM"},
	{dotNet: "t", golang: "PM"},
	{dotNet: "zzz", golang: "-07:00"},
	{dotNet: "zz", golang: "-07"},
	{dotNet: "z", golang: "-07"},
	{dotNet: "K", golang: "Z07:00"},
}

// dotNetLayout is a converted .NET format: the Go layout it became, and
// the fix-ups the value needs because three .NET tokens are narrower than
// anything Go can express.
//
// Go's tokens are fixed-width, so the narrow forms cannot be represented
// directly. Refusing them would leave those layouts parsing nothing at
// all, so the value is widened to meet the token instead -- a change that
// only ever adds the character the wider token is looking for.
//
// The counts matter, not just the presence: a format may carry the same
// narrow token more than once, and every one of them needs its own
// position in the value widened.
type dotNetLayout struct {
	// designators counts .NET's single "t", which consumes one character
	// ("A" or "P") where Go's "PM" consumes two.
	designators int
	layout      string
	// shortOffsets counts .NET's single "z", which accepts a one-digit
	// offset ("+5") where Go's "-07" demands two.
	shortOffsets int
	// shortYears counts .NET's single "y", which accepts a year written
	// without its leading zero ("9") where Go's "06" demands two digits.
	shortYears int
}

// maxWidenedValues caps how many variants of a value are tried, so that a
// long value full of candidate positions cannot turn one parse into
// hundreds.
const maxWidenedValues = 32

// widenedValues returns the value rewritten so Go's fixed-width tokens can
// read it -- one variant per place each narrow token might sit.
//
// Which position is the right one is decided by *parsing*, not by
// guessing: every other part of the layout still has to match, so a
// variant that widened the wrong character simply fails. That is what a
// context rule cannot do. Given the format "'P' t hh:mm" and the value
// "P A 07:30", both letters look equally like a designator, and only
// trying them tells you that widening the literal "P" leaves the rest of
// the value unable to match.
//
// Whether the value as it came is itself a candidate follows .NET's own
// leniency. A single numeric specifier there accepts one digit or two, so
// "z" reads "+05" as happily as "+5" and "y" reads "09" as happily as "9".
// A single "t" is not numeric: it is the designator's *first character*,
// so "7 PM" is a genuine mismatch for "h t" and must stay one rather than
// being quietly accepted through Go's two-character token.
func (l dotNetLayout) widenedValues(value string) []string {
	values := []string{value}
	for range l.designators {
		values = widenAt(values, false, isLoneDesignator, completeDesignator)
	}
	for range l.shortOffsets {
		values = widenAt(values, true, isShortOffset, padOffsetDigit)
	}
	for range l.shortYears {
		values = widenAt(values, true, isLoneDigit, padDigit)
	}
	return values
}

// widenAt returns a variant of every value for each position that matches,
// rewritten there, keeping the value itself as well when keepOriginal.
//
// Calling it once per occurrence of a token is what covers a format
// carrying several: each pass widens one more position, and a position
// already widened no longer matches, so the passes compound into every
// combination rather than repeating themselves.
func widenAt(values []string, keepOriginal bool, matches func(string, int) bool, rewrite func(string, int) string) []string {
	widened := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	// add reports whether there is room for another variant.
	add := func(value string) bool {
		if !seen[value] {
			seen[value] = true
			widened = append(widened, value)
		}
		return len(widened) < maxWidenedValues
	}

	for _, value := range values {
		if keepOriginal && !add(value) {
			return widened
		}
		for i := range len(value) {
			if matches(value, i) && !add(rewrite(value, i)) {
				return widened
			}
		}
	}
	return widened
}

// isLoneDesignator reports whether the byte at i could be an AM/PM
// designator written as a single letter. A designator in that form always
// has a non-letter on either side, so this cannot miss one -- it only has
// to be a superset of the real positions, since the parse rejects the rest.
func isLoneDesignator(value string, i int) bool {
	switch value[i] {
	case 'a', 'A', 'p', 'P':
	default:
		return false
	}
	if i > 0 && isASCIILetter(value[i-1]) {
		return false
	}
	return i+1 >= len(value) || !isASCIILetter(value[i+1])
}

// completeDesignator adds the "m" that Go's two-character token expects,
// matching the designator's own case so parseEitherCase still covers it.
func completeDesignator(value string, i int) string {
	completed := "m"
	if value[i] >= 'A' && value[i] <= 'Z' {
		completed = "M"
	}
	return value[:i+1] + completed + value[i+1:]
}

// isShortOffset reports whether a sign at i is followed by exactly one
// digit, which is the only form Go's two-digit offset token cannot read.
func isShortOffset(value string, i int) bool {
	if value[i] != '+' && value[i] != '-' {
		return false
	}
	if i+1 >= len(value) || !isASCIIDigit(value[i+1]) {
		return false
	}
	return i+2 >= len(value) || !isASCIIDigit(value[i+2])
}

// isLoneDigit reports whether the byte at i is a digit standing on its own,
// which is the form Go's two-digit year token cannot read.
func isLoneDigit(value string, i int) bool {
	if !isASCIIDigit(value[i]) {
		return false
	}
	if i > 0 && isASCIIDigit(value[i-1]) {
		return false
	}
	return i+1 >= len(value) || !isASCIIDigit(value[i+1])
}

// padOffsetDigit gives a one-digit offset its leading zero.
func padOffsetDigit(value string, i int) string { return padDigit(value, i+1) }

// padDigit inserts the leading zero a fixed-width numeric token expects.
func padDigit(value string, i int) string {
	return value[:i] + "0" + value[i:]
}

func isASCIILetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isASCIIDigit(c byte) bool { return c >= '0' && c <= '9' }

// looksLikeDotNetLayout is Jackett's gate: a layout worth trying as a .NET
// format mentions a year, an hour or a day.
func looksLikeDotNetLayout(layout string) bool {
	lowered := strings.ToLower(layout)
	return strings.ContainsAny(lowered, "yhd")
}

// goLayoutFromDotNet converts a .NET custom date format into the Go layout
// that means the same thing. ok is false for a malformed format -- an
// unterminated quote or a trailing escape -- and for one whose literal
// text Go would read as a layout token of its own: "yyyy 'January' dd"
// requires that literal word, while the Go layout it would become accepts
// any month name. Refusing is right there, unlike for the narrow tokens
// above: those can be parsed correctly with a small fix-up, whereas this
// one cannot be represented at all, and a wrong parse is worse than none.
//
// A Go layout put through this comes out as nonsense rather than an error,
// which is fine: parseDeclaredLayout only tries the result, and falls back
// to reading the layout as Go's when it does not parse.
func goLayoutFromDotNet(format string) (converted dotNetLayout, ok bool) {
	var b strings.Builder

	// Literal text is accumulated rather than written straight out, so
	// that each run can be checked before it reaches the Go layout: Go has
	// no way to escape a literal, so a .NET format quoting a word that is
	// also a reference-time token cannot be represented at all.
	var literal strings.Builder
	flushLiteral := func() bool {
		text := literal.String()
		literal.Reset()
		if !literalIsInert(text) {
			return false
		}
		b.WriteString(text)
		return true
	}

	for i := 0; i < len(format); {
		switch c := format[i]; c {
		case '\\':
			// An escaped character is a literal.
			if i+1 >= len(format) {
				return dotNetLayout{}, false
			}
			literal.WriteByte(format[i+1])
			i += 2
			continue
		case '\'', '"':
			// A quoted run is a literal.
			end := strings.IndexByte(format[i+1:], c)
			if end < 0 {
				return dotNetLayout{}, false
			}
			literal.WriteString(format[i+1 : i+1+end])
			i += end + 2
			continue
		}

		if token, consumed, ok := matchDotNetToken(format[i:]); ok {
			if !flushLiteral() {
				return dotNetLayout{}, false
			}
			b.WriteString(token.golang)
			switch token.dotNet {
			case "t":
				converted.designators++
			case "z":
				converted.shortOffsets++
			case "y":
				converted.shortYears++
			}
			i += consumed
			continue
		}

		// .NET copies any character it has no specifier for straight
		// through, so "yyyy-MM-ddTHH:mm:ss" is a valid format whose "T" is
		// a literal. Rejecting unknown letters would refuse that whole
		// shape, and the Go-layout fallback cannot read its tokens either.
		//
		// Being permissive is safe because this conversion is only ever
		// attempted, never trusted: a layout that was really a Go one
		// converts to nonsense, fails to parse, and falls through to being
		// read as the Go layout it is.
		literal.WriteByte(format[i])
		i++
	}
	if !flushLiteral() {
		return dotNetLayout{}, false
	}

	converted.layout = b.String()
	return converted, true
}

// literalIsInert reports whether text passes through Go's layout scanner
// untouched, meaning it holds no reference-time token.
//
// It is answered by formatting rather than by searching for tokens: the
// reference values overlap each other and ordinary words ("1" inside a
// year, "Jan" inside a sentence), so a search gets it wrong in both
// directions. Two instants are needed because one of them will render some
// tokens as the literal itself -- formatting "January" with the January
// reference returns "January" unchanged.
func literalIsInert(text string) bool {
	for _, reference := range []time.Time{referenceMonday, referenceSunday, referenceElsewhere} {
		if reference.Format(text) != text {
			return false
		}
	}
	return true
}

// maxDotNetRun is the longest run the table defines for each specifier,
// which is how many of a repeated letter still mean something distinct.
var maxDotNetRun = func() map[byte]int {
	longest := make(map[byte]int, len(dotNetTokens))
	for _, token := range dotNetTokens {
		if c := token.dotNet[0]; len(token.dotNet) > longest[c] {
			longest[c] = len(token.dotNet)
		}
	}
	return longest
}()

// matchDotNetToken reads the token the format starts with, and reports how
// many characters of the format it took.
//
// .NET reads a *run* of one letter as a single token, not as the longest
// table entry followed by whatever is left: "ttt" is one designator, the
// way "tt" is, and not "tt" plus a stray "t" demanding a second one.
// (Its own parser does this with ParseRepeatPattern.) A run longer than
// the table defines is clamped rather than refused, which is also what
// .NET does -- "hhh" parses the same two digits "hh" does, and "ddddd"
// the same weekday name "dddd" does.
//
// Clamping is exact for every specifier but "y", where .NET pads to the
// run's own width: a five-digit year has no Go token, so "yyyyy" is read
// as the four-digit one. No real definition writes it.
//
// "K" is excluded because .NET does not repeat it: each one stands alone.
func matchDotNetToken(format string) (token struct{ dotNet, golang string }, consumed int, ok bool) {
	c := format[0]
	if !strings.ContainsRune(dotNetSpecifiers, rune(c)) {
		return token, 0, false
	}

	run := 1
	if c != 'K' {
		for run < len(format) && format[run] == c {
			run++
		}
	}

	key := strings.Repeat(string(c), min(run, maxDotNetRun[c]))
	for _, candidate := range dotNetTokens {
		if candidate.dotNet == key {
			return candidate, run, true
		}
	}
	return token, 0, false
}

// parseDeclaredLayout parses value against the layout a definition declared,
// trying it as a .NET format first and as a Go layout second, which is the
// order Jackett tries them in.
//
// Whatever the layout leaves out, Go supplies as zero and .NET supplies
// from the current date; resolveMissingDate reconciles the two.
//
// Parsing is in the local zone rather than UTC. A tracker that prints no
// offset means its own wall clock, which .NET's ParseExact reads as local
// time and time.Parse would read as UTC -- several hours of error on any
// host that is not itself UTC. A value carrying an explicit offset still
// overrides the location.
func parseDeclaredLayout(value, layout string) (time.Time, error) {
	if looksLikeDotNetLayout(layout) {
		if converted, ok := goLayoutFromDotNet(layout); ok {
			for _, candidate := range converted.widenedValues(value) {
				if parsed, err := parseEitherCase(candidate, converted.layout); err == nil {
					return resolveMissingDate(parsed, converted.layout, false), nil
				}
			}
		}
	}

	parsed, err := parseEitherCase(value, layout)
	if err != nil {
		return time.Time{}, fmt.Errorf("dateparse: %q does not match layout %q: %w", value, layout, err)
	}
	return resolveMissingDate(parsed, layout, true), nil
}

// parseEitherCase parses value in the local zone, retrying with a
// lowercase AM/PM designator when the layout carries one.
//
// .NET compares the designator case-insensitively and Go does not: its
// "PM" token matches only "PM"/"AM" and its "pm" only the lowercase pair.
// Trackers print both -- 1337x lists "7am Sep. 14" against a "htt MMM. d"
// layout -- so a single spelling would reject half of them.
func parseEitherCase(value, layout string) (time.Time, error) {
	parsed, err := time.ParseInLocation(layout, value, time.Local) //nolint:gosmopolitan // a tracker's zone-less timestamp is its own wall clock; see parseDeclaredLayout
	if err == nil || !strings.Contains(layout, "PM") {
		return parsed, err
	}
	return time.ParseInLocation(strings.ReplaceAll(layout, "PM", "pm"), value, time.Local) //nolint:gosmopolitan // as above
}

// referenceMonday and referenceSunday differ in year, month, day and
// weekday while deliberately sharing a time of day, so that formatting
// both with a layout reveals whether it renders any part of a date. That
// shared clock is what layoutHasDate depends on and is why the third
// reference below is a third rather than a change to these two.
var (
	referenceMonday = time.Date(2006, time.January, 2, 15, 4, 5, 0, time.UTC)
	referenceSunday = time.Date(2017, time.July, 23, 15, 4, 5, 0, time.UTC)
)

// referenceElsewhere differs from both of those in every component a Go
// token can render: year, month, day, weekday, both clocks, AM/PM,
// minute, second, fractional second, zone offset and zone name.
//
// literalIsInert needs it because the other two share a clock, so a
// literal "15", "04", "05" or "PM" renders identically for both and reads
// as ordinary text -- which let "yyyy '15' dd" through as a layout that
// accepts any hour where .NET requires the literal characters.
var referenceElsewhere = time.Date(
	2011, time.November, 11, 9, 8, 7, 123456789, time.FixedZone("EST", -5*60*60),
)

// layoutHasDate reports whether a Go layout renders any part of a date.
//
// It asks the layout rather than inspecting it: reading Go's date tokens
// out of a layout string means reimplementing the scanner in the standard
// library, and the tokens overlap in ways that defeat a plain search --
// "2" is a day but also sits inside "2006". Formatting two instants that
// differ only in their date answers the same question exactly.
func layoutHasDate(layout string) bool {
	return referenceMonday.Format(layout) != referenceSunday.Format(layout)
}

// resolveMissingDate fills in what a layout does not carry, the way .NET's
// ParseExact does and Go does not.
//
// A layout with no date at all is a time of day, which a tracker prints
// for today's releases; .NET dates it to today, while Go would date it to
// the 1st of January in year zero. A layout with a date but no year takes
// the current year, and on the Go-layout path steps back one when that
// lands in the future -- a tracker printing "31 Dec" in January means last
// December. (Jackett applies that step-back only on that path, so neither
// does this.)
//
// The step-back is deliberately not applied to a time of day: a release
// timestamped a few minutes ahead of us, which a clock or timezone skew is
// enough to produce, means a moment ago and not a year ago.
func resolveMissingDate(parsed time.Time, layout string, rollBackFuture bool) time.Time {
	if parsed.Year() != 0 {
		return parsed
	}
	now := time.Now()

	if !layoutHasDate(layout) {
		return time.Date(
			now.Year(), now.Month(), now.Day(),
			parsed.Hour(), parsed.Minute(), parsed.Second(), parsed.Nanosecond(),
			parsed.Location(),
		)
	}

	dated := parsed.AddDate(now.Year(), 0, 0)
	if rollBackFuture && dated.After(now) {
		dated = dated.AddDate(-1, 0, 0)
	}
	return dated
}
