package main

import (
	"testing"
	"time"
)

func testEngine() *Engine {
	cfg := defaultConfig()
	cfg.GPS.HeadingReg = -1
	return NewEngine(cfg, nil, nil)
}

func TestTodayKmAkkumuliert(t *testing.T) {
	e := testEngine()
	base := time.Now()
	// Konstanz ~ 47.6600,9.1800 → 47.6650,9.1800: ~0.556 km Nord.
	e.SetPosition(Position{Lat: 47.6600, Lng: 9.1800, Valid: true, TS: base})
	e.SetPosition(Position{Lat: 47.6650, Lng: 9.1800, Valid: true, TS: base.Add(60 * time.Second)})
	_, km := e.TodayKm()
	d := haversineKm(47.6600, 9.1800, 47.6650, 9.1800)
	if km < d*0.99 || km > d*1.01 {
		t.Fatalf("Tageskm %v erwartet ~%v", km, d)
	}
}

func TestTodayKmIgnoriertJitterUndSprung(t *testing.T) {
	e := testEngine()
	base := time.Now()
	e.SetPosition(Position{Lat: 47.6600, Lng: 9.1800, Valid: true, TS: base})
	// Jitter im Stand (~2 m) → nicht zählen.
	e.SetPosition(Position{Lat: 47.66002, Lng: 9.1800, Valid: true, TS: base.Add(5 * time.Second)})
	// Teleport (viele km in 1 s) → abgeleitete Geschwindigkeit > 120 km/h → nicht zählen.
	e.SetPosition(Position{Lat: 47.80, Lng: 9.40, Valid: true, TS: base.Add(6 * time.Second)})
	_, km := e.TodayKm()
	if km != 0 {
		t.Fatalf("Jitter/Sprung dürfen nicht zählen, km=%v", km)
	}
}

func TestTodayKmRollover(t *testing.T) {
	e := testEngine()
	e.mu.Lock()
	e.todayDate = "2000-01-01"
	e.todayKm = 42
	e.mu.Unlock()
	// Ein Fix heute → Rollover auf den aktuellen Tag, Zähler zurück.
	now := time.Now()
	e.SetPosition(Position{Lat: 47.66, Lng: 9.18, Valid: true, TS: now})
	e.SetPosition(Position{Lat: 47.665, Lng: 9.18, Valid: true, TS: now.Add(60 * time.Second)})
	date, km := e.TodayKm()
	if date != now.Local().Format("2006-01-02") {
		t.Fatalf("Datum nicht gerollt: %v", date)
	}
	if km > 1.0 { // nur die ~0,56 km von heute, nicht die 42 von gestern
		t.Fatalf("alter Stand nicht zurückgesetzt, km=%v", km)
	}
}

func TestSeedTodayKm(t *testing.T) {
	e := testEngine()
	today := time.Now().Local().Format("2006-01-02")
	e.SeedTodayKm(today, 12.3)
	if _, km := e.TodayKm(); km != 12.3 {
		t.Fatalf("Seed heute sollte greifen, km=%v", km)
	}
	e2 := testEngine()
	e2.SeedTodayKm("2000-01-01", 99) // alter Tag → ignorieren
	if _, km := e2.TodayKm(); km != 0 {
		t.Fatalf("Seed von gestern darf nicht greifen, km=%v", km)
	}
}
