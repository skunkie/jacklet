// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"regexp"
	"strings"
	"time"
)

// This file is the rest of Jackett's DateTimeUtil.FromUnknown: the shapes a
// tracker prints that are not a date at all until you supply what they
// leave out. A bare "12:25am" means today, "Yesterday 09:10" means
// yesterday, "Saturday at 14:22" means the Saturday just gone, and "03-15"
// means this year -- or last, if this year has not reached March yet.
//
// None of these survive a general-purpose date parser. Worse than failing,
// dateparse reads "7:05pm" as the 1st of July in year zero and reports no
// error, which then stores as a release from two thousand years ago. Every
// shape below is therefore resolved before that fallback is reached.

var (
	// agoRe gates the relative branch, as Jackett's does, so that a date
	// merely containing a number and a word is not read as "N units ago".
	agoRe = regexp.MustCompile(`(?i)\bago`)
	// The relative-day expressions each swallow a trailing "at", so that
	// what remains is only the time of day.
	todayRe     = regexp.MustCompile(`(?i)\btoday(?:[\s,]+(?:at)?\s*|[\s,]*|$)`)
	yesterdayRe = regexp.MustCompile(`(?i)\byesterday(?:[\s,]+(?:at)?\s*|[\s,]*|$)`)
	tomorrowRe  = regexp.MustCompile(`(?i)\btomorrow(?:[\s,]+(?:at)?\s*|[\s,]*|$)`)
	weekdayRe   = regexp.MustCompile(`(?i)\b(monday|tuesday|wednesday|thursday|friday|saturday|sunday)\s+at\s+`)
	// missingYearRe matches a bare "3-15", missingYearDayMonthRe a
	// "1 Jan 10:30". Both are dates whose year the tracker left out.
	missingYearRe         = regexp.MustCompile(`^(\d{1,2}-\d{1,2})(\s|$)`)
	missingYearDayMonthRe = regexp.MustCompile(`^(\d{1,2}\s+\w{3})\s+(\d{1,2}:\d{1,2}.*)$`)
)

// weekdays maps the name Jackett matches to the day it means.
var weekdays = map[string]time.Weekday{
	"monday": time.Monday, "tuesday": time.Tuesday, "wednesday": time.Wednesday,
	"thursday": time.Thursday, "friday": time.Friday, "saturday": time.Saturday,
	"sunday": time.Sunday,
}

// timeOfDayLayouts are the shapes a bare time is printed in. The
// designator is lowercased before matching, so only the lowercase spelling
// is listed.
var timeOfDayLayouts = []string{
	"15:04:05", "15:04",
	"3:04:05pm", "3:04:05 pm", "3:04pm", "3:04 pm",
	"3pm", "3 pm",
}

// clockTime is a time of day kept as the components the tracker printed,
// not as an elapsed duration.
//
// The difference matters on the two days a year a zone changes offset.
// Adding "03:30" to local midnight lands on 04:30 when the clocks go
// forward at 03:00, because the duration is elapsed time and an hour of it
// does not exist that day. Building the instant with time.Date instead
// keeps the wall clock the tracker actually wrote.
type clockTime struct {
	hour   int
	minute int
	second int
}

// parseTimeOfDay reads a bare time of day. An empty string is midnight, as
// it is in Jackett's ParseTimeSpan: the relative-day expressions leave
// nothing behind when the tracker printed only "Yesterday".
func parseTimeOfDay(value string) (clockTime, bool) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return clockTime{}, true
	}
	for _, layout := range timeOfDayLayouts {
		parsed, err := time.Parse(layout, value)
		if err != nil {
			continue
		}
		return clockTime{hour: parsed.Hour(), minute: parsed.Minute(), second: parsed.Second()}, true
	}
	return clockTime{}, false
}

// startOfDay is midnight on the day of t, in t's own zone.
func startOfDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

// at builds the instant for a clock reading on the calendar day of t.
func (c clockTime) at(day time.Time) time.Time {
	return time.Date(day.Year(), day.Month(), day.Day(), c.hour, c.minute, c.second, 0, day.Location())
}

// relativeDay resolves "today"/"yesterday"/"tomorrow" plus a time of day,
// dayOffset being how many days from today the word means.
func relativeDay(value string, expr *regexp.Regexp, dayOffset int, now time.Time) (time.Time, bool) {
	matched := expr.FindString(value)
	if matched == "" {
		return time.Time{}, false
	}
	clock, ok := parseTimeOfDay(strings.ReplaceAll(value, matched, ""))
	if !ok {
		return time.Time{}, false
	}
	return clock.at(startOfDay(now).AddDate(0, 0, dayOffset)), true
}

// weekdayBefore resolves "Saturday at 14:22" to the most recent such
// weekday, today included -- a tracker naming a weekday is pointing at the
// week just gone.
//
// The walk back is over whole days at midnight, with the clock reading
// applied once at the end, so a transition day in between cannot drag the
// time along with it.
func weekdayBefore(value string, now time.Time) (time.Time, bool) {
	matched := weekdayRe.FindStringSubmatch(value)
	if matched == nil {
		return time.Time{}, false
	}
	clock, ok := parseTimeOfDay(strings.ReplaceAll(value, matched[0], ""))
	if !ok {
		return time.Time{}, false
	}

	want := weekdays[strings.ToLower(matched[1])]
	day := startOfDay(now)
	for day.Weekday() != want {
		day = startOfDay(day.AddDate(0, 0, -1))
	}
	return clock.at(day), true
}

// notInTheFuture steps a date back a year when it lands ahead of now,
// which is Jackett's FromFuzzyPastTime. A tracker that omits the year is
// describing something already released.
func notInTheFuture(parsed, now time.Time) time.Time {
	if parsed.After(now) {
		return parsed.AddDate(-1, 0, 0)
	}
	return parsed
}
