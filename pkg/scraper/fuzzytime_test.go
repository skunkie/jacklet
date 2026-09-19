// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package scraper

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A bare time of day is today's. 1337x lists what it published in the last
// few hours that way, under a plain "fuzzytime" filter.
//
// This must be resolved before the general parser rather than after it:
// that one reads "7:05pm" as the 1st of July in year zero and reports no
// error, which stores as a release two thousand years old.
func TestParseUnknownTime_TimeOfDayIsToday(t *testing.T) {
	for _, tc := range []struct {
		value        string
		hour, minute int
	}{
		{value: "12:25am", hour: 0, minute: 25},
		{value: "12:25 am", hour: 0, minute: 25},
		{value: "7:05pm", hour: 19, minute: 5},
		{value: "7:05 PM", hour: 19, minute: 5},
		{value: "14:22", hour: 14, minute: 22},
		{value: "09:10:30", hour: 9, minute: 10},
	} {
		t.Run(tc.value, func(t *testing.T) {
			parsed, err := parseUnknownTimeValue(tc.value)
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

// Jackett resolves the relative-day words a forum-style tracker prints.
func TestParseUnknownTime_RelativeDays(t *testing.T) {
	for _, tc := range []struct {
		name, value  string
		dayOffset    int
		hour, minute int
	}{
		{name: "today at", value: "Today at 14:22", dayOffset: 0, hour: 14, minute: 22},
		{name: "today comma", value: "Today, 14:22", dayOffset: 0, hour: 14, minute: 22},
		{name: "today alone", value: "Today", dayOffset: 0},
		{name: "yesterday", value: "Yesterday 09:10", dayOffset: -1, hour: 9, minute: 10},
		{name: "yesterday at", value: "Yesterday at 09:10", dayOffset: -1, hour: 9, minute: 10},
		{name: "tomorrow", value: "Tomorrow at 08:00", dayOffset: 1, hour: 8},
		{name: "case insensitive", value: "YESTERDAY AT 09:10", dayOffset: -1, hour: 9, minute: 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := parseUnknownTimeValue(tc.value)
			require.NoError(t, err)

			want := startOfDay(time.Now()).AddDate(0, 0, tc.dayOffset)
			require.Equal(t, want.Year(), parsed.Year())
			require.Equal(t, want.Month(), parsed.Month())
			require.Equal(t, want.Day(), parsed.Day())
			require.Equal(t, tc.hour, parsed.Hour())
			require.Equal(t, tc.minute, parsed.Minute())
		})
	}
}

// A named weekday means the one just gone, today included.
func TestParseUnknownTime_Weekday(t *testing.T) {
	now := time.Now()
	for day := range 7 {
		want := startOfDay(now).AddDate(0, 0, -day)

		parsed, err := parseUnknownTimeValue(want.Weekday().String() + " at 14:22")
		require.NoError(t, err)
		require.Equal(t, want.Year(), parsed.Year())
		require.Equal(t, want.Month(), parsed.Month())
		require.Equal(t, want.Day(), parsed.Day(), "%s should resolve %d day(s) back", want.Weekday(), day)
		require.Equal(t, 14, parsed.Hour())
	}
}

// A date whose year the tracker left out takes this one, stepping back when
// that would put it in the future.
func TestParseUnknownTime_MissingYear(t *testing.T) {
	now := time.Now()

	t.Run("day and month", func(t *testing.T) {
		past := now.AddDate(0, -1, 0)
		parsed, err := parseUnknownTimeValue(past.Format("1-2"))
		require.NoError(t, err)
		require.Equal(t, past.Year(), parsed.Year())
		require.Equal(t, past.Month(), parsed.Month())
	})

	t.Run("a future month means last year", func(t *testing.T) {
		future := now.AddDate(0, 2, 0)
		parsed, err := parseUnknownTimeValue(future.Format("1-2"))
		require.NoError(t, err)
		require.False(t, parsed.After(now), "a yearless date must not be in the future")
		require.Equal(t, future.Month(), parsed.Month())
	})

	t.Run("day month and time", func(t *testing.T) {
		past := now.AddDate(0, -1, 0)
		parsed, err := parseUnknownTimeValue(past.Format("2 Jan") + " 10:30")
		require.NoError(t, err)
		require.Equal(t, past.Year(), parsed.Year())
		require.Equal(t, past.Month(), parsed.Month())
		require.Equal(t, 10, parsed.Hour())
	})
}

// Whatever the parser cannot place is an error, not a date in year zero:
// stored, that would be a release older than the calendar.
func TestParseUnknownTime_NeverReturnsYearZero(t *testing.T) {
	for _, value := range []string{"total nonsense", "", "--"} {
		parsed, err := parseUnknownTimeValue(value)
		if err == nil {
			require.NotZero(t, parsed.Year(), "%q yielded year zero", value)
		}
	}
}

// The shapes handled before the new ones must still be.
func TestParseUnknownTime_ExistingShapesStillWork(t *testing.T) {
	t.Run("relative", func(t *testing.T) {
		parsed, err := parseUnknownTimeValue("2 hours ago")
		require.NoError(t, err)
		require.WithinDuration(t, time.Now().Add(-2*time.Hour), parsed, 5*time.Second)
	})

	t.Run("now", func(t *testing.T) {
		parsed, err := parseUnknownTimeValue("now")
		require.NoError(t, err)
		require.WithinDuration(t, time.Now(), parsed, 5*time.Second)
	})

	t.Run("unix timestamp", func(t *testing.T) {
		parsed, err := parseUnknownTimeValue("1710526200")
		require.NoError(t, err)
		require.Equal(t, int64(1710526200), parsed.Unix())
	})

	t.Run("absolute", func(t *testing.T) {
		parsed, err := parseUnknownTimeValue("2024-03-15 18:30:00")
		require.NoError(t, err)
		require.Equal(t, 2024, parsed.Year())
		require.Equal(t, 18, parsed.Hour())
	})
}

// A clock reading is built from its components, not added to midnight as
// elapsed time. On the day the clocks go forward the hour between 02:00
// and 03:00 does not exist, so adding 3h30m to midnight lands on 04:30 and
// reports a release half an hour later than the tracker said.
func TestRelativeTimes_SurviveDSTTransitions(t *testing.T) {
	newYork, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)

	// 2024-03-10: clocks go forward at 02:00 local.
	springForward := time.Date(2024, time.March, 10, 12, 0, 0, 0, newYork)
	clock := clockTime{hour: 3, minute: 30}

	t.Run("the wall clock is preserved", func(t *testing.T) {
		built := clock.at(startOfDay(springForward))
		require.Equal(t, 3, built.Hour(), "03:30 must stay 03:30")
		require.Equal(t, 30, built.Minute())

		// The old behaviour, for contrast: elapsed time lands an hour on.
		added := startOfDay(springForward).Add(3*time.Hour + 30*time.Minute)
		require.Equal(t, 4, added.Hour(), "adding a duration drifts, which is why this is not how it is done")
	})

	t.Run("yesterday across the transition", func(t *testing.T) {
		dayAfter := time.Date(2024, time.March, 11, 12, 0, 0, 0, newYork)
		resolved, ok := relativeDay("Yesterday at 03:30", yesterdayRe, -1, dayAfter)
		require.True(t, ok)
		require.Equal(t, 10, resolved.Day())
		require.Equal(t, 3, resolved.Hour())
		require.Equal(t, 30, resolved.Minute())
	})

	t.Run("a weekday walk across the transition", func(t *testing.T) {
		// From Saturday 2024-03-16 back to Sunday 2024-03-10, the
		// transition day itself.
		saturday := time.Date(2024, time.March, 16, 12, 0, 0, 0, newYork)
		resolved, ok := weekdayBefore("Sunday at 03:30", saturday)
		require.True(t, ok)
		require.Equal(t, time.Sunday, resolved.Weekday())
		require.Equal(t, 10, resolved.Day())
		require.Equal(t, 3, resolved.Hour())
		require.Equal(t, 30, resolved.Minute())
	})

	// And the autumn transition, where an hour happens twice.
	t.Run("autumn transition", func(t *testing.T) {
		fallBack := time.Date(2024, time.November, 3, 12, 0, 0, 0, newYork)
		built := clockTime{hour: 1, minute: 30}.at(startOfDay(fallBack))
		require.Equal(t, 1, built.Hour())
		require.Equal(t, 30, built.Minute())
	})
}
