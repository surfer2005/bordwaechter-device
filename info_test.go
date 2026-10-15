package main

import (
	"testing"
	"time"
)

func TestParseOpenMeteo(t *testing.T) {
	js := []byte(`{"current":{"time":"2026-10-15T06:45","temperature_2m":13.2,"wind_speed_10m":8.0,"wind_direction_10m":315,"weather_code":1,"precipitation":0.0}}`)
	now, _ := time.Parse("2006-01-02T15:04", "2026-10-15T07:00")
	w, err := parseOpenMeteo(js, now)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if w.TempC == nil || *w.TempC != 13.2 {
		t.Fatalf("temp falsch: %v", w.TempC)
	}
	if w.WindKmh == nil || *w.WindKmh != 8.0 {
		t.Fatalf("wind falsch: %v", w.WindKmh)
	}
	if w.Code == nil || *w.Code != 1 {
		t.Fatalf("code falsch: %v", w.Code)
	}
	if w.AgeSec != 900 { // 15 min
		t.Fatalf("age falsch: %d", w.AgeSec)
	}
}

func TestPegelRateUndTrend(t *testing.T) {
	// 2 Stunden, +4 cm → 2 cm/h, Trend steigend.
	js := []byte(`[{"timestamp":"2026-10-15T05:00:00+02:00","value":267.0},
	               {"timestamp":"2026-10-15T06:00:00+02:00","value":269.0},
	               {"timestamp":"2026-10-15T07:00:00+02:00","value":271.0}]`)
	pts, err := parsePegelSeries(js)
	if err != nil || len(pts) != 3 {
		t.Fatalf("parse: %v len=%d", err, len(pts))
	}
	rate, ok := pegelRateCmPerH(pts)
	if !ok || rate != 2.0 {
		t.Fatalf("rate falsch: %v ok=%v", rate, ok)
	}
	if trendFromRate(rate, 0.3) != 1 {
		t.Fatalf("trend sollte steigend sein")
	}
	if trendFromRate(-2.0, 0.3) != -1 {
		t.Fatalf("trend sollte fallend sein")
	}
	if trendFromRate(0.1, 0.3) != 0 {
		t.Fatalf("kleine Änderung sollte stabil sein")
	}
}

func TestPegelRateZuWenig(t *testing.T) {
	if _, ok := pegelRateCmPerH([]pegelPoint{{Timestamp: "2026-10-15T07:00:00+02:00", Value: 271}}); ok {
		t.Fatalf("eine Messung darf keine Rate liefern")
	}
}

func TestDownsample(t *testing.T) {
	pts := make([]pegelPoint, 100)
	for i := range pts {
		pts[i].Value = float64(i)
	}
	s := downsample(pts, 16)
	if len(s) != 16 {
		t.Fatalf("downsample len=%d", len(s))
	}
	if s[0] != 0 || s[len(s)-1] != 99 {
		t.Fatalf("downsample Ränder falsch: %v..%v", s[0], s[len(s)-1])
	}
	// weniger Punkte als n → alle zurück
	s2 := downsample(pts[:5], 16)
	if len(s2) != 5 {
		t.Fatalf("downsample klein len=%d", len(s2))
	}
}
