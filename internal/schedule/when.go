// Package schedule runs what the user booked through the assistant for
// later: an action at a set time, once or repeating; standing rules, such
// as keeping apps updated with a safeguard; and check-ins, where the model
// looks the NAS over and writes a briefing. Each was approved on a card
// when it was booked, with its scope fixed then; nothing here runs
// anything else.
package schedule

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// When is when a job runs: once, at a date and time, or repeating daily,
// weekly or monthly at a time of day. Times are the NAS's local time.
type When struct {
	Repeat string `json:"repeat"` // once, daily, weekly or monthly
	// Date is YYYY-MM-DD, for once.
	Date string `json:"date,omitempty"`
	// Time is HH:MM, 24-hour.
	Time string `json:"time"`
	// Weekday is monday to sunday, for weekly.
	Weekday string `json:"weekday,omitempty"`
	// Day is the day of the month, 1 to 28, for monthly: every month has
	// those.
	Day int `json:"day,omitempty"`
	// Zone is the IANA time zone the times are in: the NAS's when booked.
	Zone string `json:"zone"`
}

var weekdays = map[string]time.Weekday{
	"sunday": time.Sunday, "monday": time.Monday, "tuesday": time.Tuesday, "wednesday": time.Wednesday,
	"thursday": time.Thursday, "friday": time.Friday, "saturday": time.Saturday,
}

// Validate checks the fields the repeat needs, and that they read.
func (w When) Validate() error {
	if _, err := w.location(); err != nil {
		return err
	}
	if _, _, err := w.clock(); err != nil {
		return err
	}
	switch w.Repeat {
	case "once":
		if _, err := time.Parse("2006-01-02", w.Date); err != nil {
			return fmt.Errorf("date %q isn't YYYY-MM-DD", w.Date)
		}
	case "daily":
	case "weekly":
		if _, ok := weekdays[strings.ToLower(w.Weekday)]; !ok {
			return fmt.Errorf("weekday %q isn't a day of the week", w.Weekday)
		}
	case "monthly":
		if w.Day < 1 || w.Day > 28 {
			return errors.New("day is 1 to 28, so every month has it")
		}
	default:
		return fmt.Errorf("repeat is once, daily, weekly or monthly, not %q", w.Repeat)
	}
	return nil
}

func (w When) location() (*time.Location, error) {
	if w.Zone == "" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(w.Zone)
	if err != nil {
		return nil, fmt.Errorf("time zone %q is unknown", w.Zone)
	}
	return loc, nil
}

func (w When) clock() (hour, minute int, err error) {
	parts := strings.Split(w.Time, ":")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("time %q isn't HH:MM", w.Time)
	}
	hour, err1 := strconv.Atoi(parts[0])
	minute, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, 0, fmt.Errorf("time %q isn't HH:MM", w.Time)
	}
	return hour, minute, nil
}

// Next is the first run after `after`, and false for a one-off whose time
// has passed. Validate first.
func (w When) Next(after time.Time) (time.Time, bool) {
	loc, err := w.location()
	if err != nil {
		return time.Time{}, false
	}
	hour, minute, err := w.clock()
	if err != nil {
		return time.Time{}, false
	}
	local := after.In(loc)
	at := func(y int, m time.Month, d int) time.Time {
		return time.Date(y, m, d, hour, minute, 0, 0, loc)
	}
	switch w.Repeat {
	case "once":
		day, err := time.ParseInLocation("2006-01-02", w.Date, loc)
		if err != nil {
			return time.Time{}, false
		}
		t := at(day.Year(), day.Month(), day.Day())
		return t, t.After(after)
	case "daily":
		t := at(local.Year(), local.Month(), local.Day())
		if !t.After(after) {
			t = at(local.Year(), local.Month(), local.Day()+1)
		}
		return t, true
	case "weekly":
		want := weekdays[strings.ToLower(w.Weekday)]
		ahead := (int(want) - int(local.Weekday()) + 7) % 7
		t := at(local.Year(), local.Month(), local.Day()+ahead)
		if !t.After(after) {
			t = at(local.Year(), local.Month(), local.Day()+ahead+7)
		}
		return t, true
	case "monthly":
		t := at(local.Year(), local.Month(), w.Day)
		if !t.After(after) {
			t = at(local.Year(), local.Month()+1, w.Day)
		}
		return t, true
	}
	return time.Time{}, false
}

// Phrase says when, for a card: "every Sunday at 2:00 am", "at 3:00 am on
// Thu 2 Oct".
func (w When) Phrase() string {
	hour, minute, _ := w.clock()
	clock := clockText(hour, minute)
	switch w.Repeat {
	case "once":
		day, err := time.Parse("2006-01-02", w.Date)
		if err != nil {
			return "at " + clock
		}
		return fmt.Sprintf("at %s on %s", clock, day.Format("Mon 2 Jan"))
	case "daily":
		return "every day at " + clock
	case "weekly":
		name := strings.ToLower(w.Weekday)
		if name != "" {
			name = strings.ToUpper(name[:1]) + name[1:]
		}
		return fmt.Sprintf("every %s at %s", name, clock)
	case "monthly":
		return fmt.Sprintf("on the %s of every month at %s", ordinal(w.Day), clock)
	}
	return clock
}

func clockText(hour, minute int) string {
	suffix := "am"
	if hour >= 12 {
		suffix = "pm"
	}
	h := hour % 12
	if h == 0 {
		h = 12
	}
	return fmt.Sprintf("%d:%02d %s", h, minute, suffix)
}

func ordinal(n int) string {
	switch {
	case n%100 >= 11 && n%100 <= 13:
		return fmt.Sprintf("%dth", n)
	case n%10 == 1:
		return fmt.Sprintf("%dst", n)
	case n%10 == 2:
		return fmt.Sprintf("%dnd", n)
	case n%10 == 3:
		return fmt.Sprintf("%drd", n)
	}
	return fmt.Sprintf("%dth", n)
}
