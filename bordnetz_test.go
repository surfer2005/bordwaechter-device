package main

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBordPositionNurFrischerFix(t *testing.T) {
	jetzt := time.Now()
	ok := bordPositionAntwort(Position{Lat: 47.65, Lng: 9.48, SpeedKmh: 20, Heading: 270, Valid: true, TS: jetzt.Add(-2 * time.Second)}, jetzt)
	if ok["valid"] != true || ok["lat"] != 47.65 {
		t.Fatalf("frischer Fix muss gültig sein: %v", ok)
	}
	alt := bordPositionAntwort(Position{Lat: 47.65, Lng: 9.48, Valid: true, TS: jetzt.Add(-2 * time.Minute)}, jetzt)
	if alt["valid"] != false || alt["lat"] != nil {
		t.Fatalf("alter Fix darf keine Position liefern: %v", alt)
	}
}

func TestBordPositionBrauchtSchluessel(t *testing.T) {
	dir := t.TempDir()
	os.Setenv("BORDWAECHTER_CONFIG", filepath.Join(dir, "config.json"))
	defer os.Unsetenv("BORDWAECHTER_CONFIG")
	bordTok, bordTokGeladen = "", false
	a := &App{cfg: defaultConfig()}
	a.eng = NewEngine(a.cfg, nil, nil)
	rec := httptest.NewRecorder()
	a.handleBordPosition(rec, httptest.NewRequest("GET", "/api/bord/position?token=x", nil))
	if rec.Code != 403 {
		t.Fatalf("ohne Schlüssel aus KAI muss gesperrt sein, war %d", rec.Code)
	}
	setBordToken("abc123")
	bordTokGeladen = false // vom Datenträger neu lesen (Neustart ohne Netz)
	rec = httptest.NewRecorder()
	a.handleBordPosition(rec, httptest.NewRequest("GET", "/api/bord/position?token=abc123", nil))
	if rec.Code != 200 {
		t.Fatalf("mit Schlüssel erwartet 200, war %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	a.handleBordPosition(rec, httptest.NewRequest("GET", "/api/bord/position?token=falsch", nil))
	if rec.Code != 403 {
		t.Fatalf("falscher Schlüssel muss gesperrt sein, war %d", rec.Code)
	}
}
