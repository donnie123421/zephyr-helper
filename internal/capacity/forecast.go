package capacity

import (
	"math"
	"sort"
	"time"
)

// MinDays is how much history a forecast needs: under two weeks, a pool's
// day-to-day swings drown its trend.
const MinDays = 14

// forecastWindow is how far back the trend looks: long enough to smooth a
// burst of downloads, short enough to follow a change in how the pool is
// used.
const forecastWindow = 90

// Point is one day of a pool's history, for a chart.
type Point struct {
	Day       string `json:"day"`
	Allocated int64  `json:"allocated"`
	Size      int64  `json:"size"`
}

// Forecast says when a pool will fill, from the trend in its recent daily
// history. No model is involved.
type Forecast struct {
	Pool      string  `json:"pool"`
	Size      int64   `json:"size"`
	Allocated int64   `json:"allocated"`
	Percent   float64 `json:"percent"`
	// DaysOfData is how many days of history there are.
	DaysOfData int `json:"days_of_data"`
	// Status is collecting (under MinDays of history), growing, steady
	// (no meaningful change) or shrinking.
	Status string `json:"status"`
	// PerDay is the trend in bytes a day: negative when shrinking.
	PerDay float64 `json:"per_day"`
	// At80 and At100 are when the trend crosses 80% and 100%: nil when it
	// won't within five years, or (At80) when the pool is past 80% now.
	At80  *time.Time `json:"at_80,omitempty"`
	At100 *time.Time `json:"at_100,omitempty"`
	// History is the window the trend was drawn from, oldest first.
	History []Point `json:"history"`
}

// ForecastFrom reads a forecast from a pool's samples, oldest first, as of
// now.
func ForecastFrom(pool string, samples []Sample, now time.Time) Forecast {
	// The samples are kept by UTC day, so today is too.
	now = now.UTC()
	f := Forecast{Pool: pool, Status: "collecting", History: []Point{}}
	if len(samples) == 0 {
		return f
	}
	if len(samples) > forecastWindow {
		samples = samples[len(samples)-forecastWindow:]
	}
	latest := samples[len(samples)-1]
	f.Size, f.Allocated = latest.Size, latest.Allocated
	if f.Size > 0 {
		f.Percent = float64(f.Allocated) / float64(f.Size) * 100
	}
	f.DaysOfData = len(samples)
	for _, s := range samples {
		f.History = append(f.History, Point{Day: s.Day, Allocated: s.Allocated, Size: s.Size})
	}
	if len(samples) < MinDays || f.Size <= 0 {
		return f
	}

	// The trend is the median of the slopes between every pair of days
	// (Theil–Sen): unlike least squares, a one-off batch of new files or
	// a big delete barely moves it. x is in real days, so a missed day
	// doesn't matter.
	type reading struct{ day, used float64 }
	var readings []reading
	for _, s := range samples {
		day, err := time.Parse("2006-01-02", s.Day)
		if err != nil {
			continue
		}
		readings = append(readings, reading{float64(day.Unix()) / 86400, float64(s.Allocated)})
	}
	if len(readings) < MinDays {
		return f
	}
	slopes := make([]float64, 0, len(readings)*(len(readings)-1)/2)
	for i := range readings {
		for j := i + 1; j < len(readings); j++ {
			if dx := readings[j].day - readings[i].day; dx != 0 {
				slopes = append(slopes, (readings[j].used-readings[i].used)/dx)
			}
		}
	}
	if len(slopes) == 0 {
		return f
	}
	sort.Float64s(slopes)
	slope := slopes[len(slopes)/2]
	if len(slopes)%2 == 0 {
		slope = (slopes[len(slopes)/2-1] + slope) / 2
	}
	f.PerDay = slope

	// Under 0.01% of the pool a day is a year to fill 3.65%: steady.
	if math.Abs(slope) < float64(f.Size)*0.0001 {
		f.Status = "steady"
		return f
	}
	if slope < 0 {
		f.Status = "shrinking"
		return f
	}
	f.Status = "growing"
	// Projected from today's actual use, at the trend's rate.
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	when := func(fraction float64) *time.Time {
		remaining := fraction*float64(f.Size) - float64(f.Allocated)
		if remaining <= 0 {
			return nil
		}
		days := remaining / slope
		if days > 5*365 {
			return nil
		}
		t := today.Add(time.Duration(days*24) * time.Hour)
		return &t
	}
	f.At80 = when(0.8)
	f.At100 = when(1.0)
	return f
}
