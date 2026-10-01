package schedule

import (
	"testing"
	"time"
)

func TestNextRuns(t *testing.T) {
	sydney, _ := time.LoadLocation("Australia/Sydney")
	// Thursday 1 Oct 2026, 2:30 pm in Sydney.
	now := time.Date(2026, 10, 1, 14, 30, 0, 0, sydney)
	cases := []struct {
		when When
		want time.Time
		ok   bool
	}{
		{When{Repeat: "once", Date: "2026-10-02", Time: "03:00", Zone: "Australia/Sydney"}, time.Date(2026, 10, 2, 3, 0, 0, 0, sydney), true},
		{When{Repeat: "once", Date: "2026-10-01", Time: "09:00", Zone: "Australia/Sydney"}, time.Date(2026, 10, 1, 9, 0, 0, 0, sydney), false},
		{When{Repeat: "daily", Time: "15:00", Zone: "Australia/Sydney"}, time.Date(2026, 10, 1, 15, 0, 0, 0, sydney), true},
		{When{Repeat: "daily", Time: "14:30", Zone: "Australia/Sydney"}, time.Date(2026, 10, 2, 14, 30, 0, 0, sydney), true},
		{When{Repeat: "weekly", Weekday: "Sunday", Time: "02:00", Zone: "Australia/Sydney"}, time.Date(2026, 10, 4, 2, 0, 0, 0, sydney), true},
		{When{Repeat: "weekly", Weekday: "wednesday", Time: "10:00", Zone: "Australia/Sydney"}, time.Date(2026, 10, 7, 10, 0, 0, 0, sydney), true},
		{When{Repeat: "monthly", Day: 1, Time: "04:00", Zone: "Australia/Sydney"}, time.Date(2026, 11, 1, 4, 0, 0, 0, sydney), true},
	}
	for _, c := range cases {
		if err := c.when.Validate(); err != nil {
			t.Fatalf("%+v: %v", c.when, err)
		}
		got, ok := c.when.Next(now)
		if ok != c.ok || (ok && !got.Equal(c.want)) {
			t.Errorf("%+v: Next = %v, %v; want %v, %v", c.when, got, ok, c.want, c.ok)
		}
	}
}

// Sydney's clocks went forward on 4 Oct 2026: 2:30 am didn't happen, and
// a daily 2:30 runs at 3:30 instead, still once that day.
func TestNextAcrossDaylightSaving(t *testing.T) {
	sydney, _ := time.LoadLocation("Australia/Sydney")
	w := When{Repeat: "daily", Time: "02:30", Zone: "Australia/Sydney"}
	got, _ := w.Next(time.Date(2026, 10, 3, 12, 0, 0, 0, sydney))
	if got.Day() != 4 || got.Hour() != 3 {
		t.Errorf("Next = %v", got)
	}
	after, _ := w.Next(got)
	if after.Day() != 5 || after.Hour() != 2 || after.Minute() != 30 {
		t.Errorf("then = %v", after)
	}
}

// In every zone, a time the clocks skip runs at the first time after,
// moved on by the gap: time.Date puts it before the change in some zones.
func TestASkippedTimeRunsAfterTheGap(t *testing.T) {
	for _, c := range []struct {
		zone, time      string
		from            string
		day, hour, mins int
	}{
		{"America/New_York", "02:30", "2026-03-07 12:00", 8, 3, 30},
		{"Europe/London", "01:30", "2026-03-28 12:00", 29, 2, 30},
		{"Australia/Sydney", "02:00", "2026-10-03 12:00", 4, 3, 0},
	} {
		loc, _ := time.LoadLocation(c.zone)
		from, _ := time.ParseInLocation("2006-01-02 15:04", c.from, loc)
		got, _ := When{Repeat: "daily", Time: c.time, Zone: c.zone}.Next(from)
		got = got.In(loc)
		if got.Day() != c.day || got.Hour() != c.hour || got.Minute() != c.mins {
			t.Errorf("%s %s: Next = %v", c.zone, c.time, got)
		}
	}
}

func TestValidateRefusesWhatDoesntRead(t *testing.T) {
	for _, w := range []When{
		{Repeat: "hourly", Time: "03:00"},
		{Repeat: "daily", Time: "25:00"},
		{Repeat: "weekly", Weekday: "funday", Time: "03:00"},
		{Repeat: "monthly", Day: 31, Time: "03:00"},
		{Repeat: "once", Date: "tomorrow", Time: "03:00"},
		{Repeat: "daily", Time: "03:00", Zone: "Mars/Olympus"},
	} {
		if err := w.Validate(); err == nil {
			t.Errorf("%+v passed", w)
		}
	}
}

func TestPhrases(t *testing.T) {
	cases := map[string]When{
		"at 3:00 am on Fri 2 Oct":                {Repeat: "once", Date: "2026-10-02", Time: "03:00"},
		"every day at 12:00 pm":                  {Repeat: "daily", Time: "12:00"},
		"every Sunday at 2:05 am":                {Repeat: "weekly", Weekday: "sunday", Time: "02:05"},
		"on the 22nd of every month at 11:30 pm": {Repeat: "monthly", Day: 22, Time: "23:30"},
	}
	for want, w := range cases {
		if got := w.Phrase(); got != want {
			t.Errorf("Phrase(%+v) = %q, want %q", w, got, want)
		}
	}
}
