package capacity

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const tb = int64(1_000_000_000_000)

// days makes n daily samples ending on end, using used(i) bytes on day i.
func days(n int, end time.Time, size int64, used func(i int) int64) []Sample {
	var out []Sample
	for i := 0; i < n; i++ {
		day := end.AddDate(0, 0, i-n+1)
		out = append(out, Sample{Day: day.Format("2006-01-02"), Pool: "tank", Size: size, Allocated: used(i)})
	}
	return out
}

var today = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

func TestUnderTwoWeeksIsStillCollecting(t *testing.T) {
	f := ForecastFrom("tank", days(10, today, 10*tb, func(i int) int64 { return 5*tb + int64(i)*tb/100 }), today)
	if f.Status != "collecting" || f.DaysOfData != 10 || f.At100 != nil || len(f.History) != 10 {
		t.Errorf("forecast = %+v", f)
	}
}

func TestAGrowingPoolSaysWhenItFills(t *testing.T) {
	// 10 TB pool at 5 TB, growing 50 GB a day: 80% in 60 days, full in 100.
	f := ForecastFrom("tank", days(30, today, 10*tb, func(i int) int64 { return 5*tb - 29*tb/20 + int64(i)*tb/20 }), today)
	if f.Status != "growing" || f.Percent != 50 {
		t.Fatalf("forecast = %+v", f)
	}
	if want := 50e9; f.PerDay < want*0.999 || f.PerDay > want*1.001 {
		t.Errorf("per day = %v", f.PerDay)
	}
	day := func(d int) time.Time { return time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC).AddDate(0, 0, d) }
	if f.At80 == nil || !f.At80.Equal(day(60)) || f.At100 == nil || !f.At100.Equal(day(100)) {
		t.Errorf("at 80 %v, at 100 %v", f.At80, f.At100)
	}
}

func TestAPoolPast80PercentOnlyHasItsFullDate(t *testing.T) {
	f := ForecastFrom("tank", days(20, today, 10*tb, func(i int) int64 { return 8*tb + int64(i)*tb/100 }), today)
	if f.Status != "growing" || f.At80 != nil || f.At100 == nil {
		t.Errorf("forecast = %+v", f)
	}
}

func TestAFlatOrShrinkingPoolHasNoDate(t *testing.T) {
	flat := ForecastFrom("tank", days(30, today, 10*tb, func(i int) int64 { return 5*tb + int64(i%3)*1e9 }), today)
	if flat.Status != "steady" || flat.At100 != nil {
		t.Errorf("flat = %+v", flat)
	}
	shrinking := ForecastFrom("tank", days(30, today, 10*tb, func(i int) int64 { return 6*tb - int64(i)*tb/50 }), today)
	if shrinking.Status != "shrinking" || shrinking.At100 != nil {
		t.Errorf("shrinking = %+v", shrinking)
	}
}

func TestAGrowthTooSlowToMatterHasNoDate(t *testing.T) {
	// 0.02% a day from 10%: about 12 years to fill.
	f := ForecastFrom("tank", days(30, today, 10*tb, func(i int) int64 { return 1*tb + int64(i)*2e9 }), today)
	if f.Status != "growing" || f.At100 != nil {
		t.Errorf("forecast = %+v", f)
	}
}

func TestTheCapacityEndpointForecastsEachPool(t *testing.T) {
	store, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()
	for _, s := range days(20, today, 10*tb, func(i int) int64 { return 5*tb + int64(i)*tb/20 }) {
		if err := store.Record(context.Background(), s); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	mux := http.NewServeMux()
	(&Handler{Store: store, Now: func() time.Time { return today }}).RegisterMux(mux, func(h http.Handler) http.Handler { return h })
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/capacity", nil))
	var body struct {
		Pools   []Forecast `json:"pools"`
		MinDays int        `json:"min_days"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if len(body.Pools) != 1 || body.Pools[0].Pool != "tank" || body.Pools[0].Status != "growing" || body.MinDays != 14 {
		t.Errorf("body = %+v", body)
	}
}
