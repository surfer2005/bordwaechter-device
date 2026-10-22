package main

import (
	"testing"
	"time"
)

func TestGateHafenUndSpeed(t *testing.T) {
	cfg := defaultConfig()
	cfg.SIFA.HarborOff = true
	cfg.SIFA.CameraMinKmh = 5
	h := []Hafen{{Name: "Friedrichshafen", Lat: 47.65, Lng: 9.48, RadiusM: 300}}
	var g gateState
	im := Position{Valid: true, Lat: 47.65, Lng: 9.48, SpeedKmh: 2}
	r := g.update(cfg, h, im, true)
	if r.sifaRuht != "im Hafen Friedrichshafen" || r.kameraRuht != "unter 5 km/h" {
		t.Fatalf("im Hafen langsam: %+v", r)
	}
	// knapp außerhalb (≈ 310 m) → Hysterese: noch drin
	knapp := Position{Valid: true, Lat: 47.65 + 0.00279, Lng: 9.48, SpeedKmh: 20}
	if r = g.update(cfg, h, knapp, true); r.sifaRuht == "" || r.kameraRuht != "" {
		t.Fatalf("Hysterese/schnell: %+v", r)
	}
	draussen := Position{Valid: true, Lat: 47.66, Lng: 9.48, SpeedKmh: 20}
	if r = g.update(cfg, h, draussen, true); r.sifaRuht != "" || r.kameraRuht != "" {
		t.Fatalf("draußen: %+v", r)
	}
	// ohne frische Position: scharf (fail-safe)
	if r = g.update(cfg, h, im, false); r.sifaRuht != "" || r.kameraRuht != "" {
		t.Fatalf("ohne Position: %+v", r)
	}
}

func TestSifaRuhtKeineHupe(t *testing.T) {
	cfg := defaultConfig()
	cfg.SIFA.TriggerCombo = "manuell"
	cfg.SIFA.DeadmanSec = 1
	cfg.SIFA.HarborOff = true
	e := NewEngine(cfg, nil, noopGPIO{})
	e.bundle = &Bundle{Haefen: []Hafen{{Name: "Konstanz", Lat: 47.66, Lng: 9.18, RadiusM: 300}}}
	now := time.Now()
	e.pos = Position{Valid: true, Lat: 47.66, Lng: 9.18, TS: now}
	e.lastDeadmanAck = now.Add(-time.Hour)
	e.Tick(now)
	if st := e.Status(); st.Horn || st.Lamp || st.SifaRuht == "" {
		t.Fatalf("im Hafen darf nichts tuten: %+v", st)
	}
}
