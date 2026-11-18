package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Smartmeter-Puffer ohne Netz (Peter 2026-11-18): Werte bleiben an Bord und werden nachgeliefert.

func werte(n int, t time.Time) []smReading {
	out := []smReading{}
	for i := 0; i < n; i++ {
		out = append(out, smReading{EntityID: "sensor.ms_test_land_bb_wirkenergie_remote", Value: float64(1000 + i), RecordedAt: smZeit(t)})
	}
	return out
}

func TestSmAufnehmenOnlineImmerOfflineAusgeduennt(t *testing.T) {
	t0 := time.Date(2026, 11, 18, 8, 0, 0, 0, time.UTC)
	w, ok := smAufnehmen(nil, werte(3, t0), false, time.Time{}, t0, 5*time.Minute, 100)
	if !ok || len(w) != 3 {
		t.Fatalf("online: erwartet 3 aufgenommen, bekam %d (%v)", len(w), ok)
	}
	// offline 30 s später: zu früh → nicht aufnehmen
	w, ok = smAufnehmen(w, werte(3, t0.Add(30*time.Second)), true, t0, t0.Add(30*time.Second), 5*time.Minute, 100)
	if ok || len(w) != 3 {
		t.Fatalf("offline nach 30 s darf nichts dazukommen, bekam %d", len(w))
	}
	// offline 5 min später: aufnehmen
	w, ok = smAufnehmen(w, werte(3, t0.Add(5*time.Minute)), true, t0, t0.Add(5*time.Minute), 5*time.Minute, 100)
	if !ok || len(w) != 6 {
		t.Fatalf("offline nach 5 min erwartet 6, bekam %d", len(w))
	}
}

func TestSmAufnehmenObergrenzeWirftAeltesteRaus(t *testing.T) {
	t0 := time.Date(2026, 11, 18, 8, 0, 0, 0, time.UTC)
	var w []smReading
	for i := 0; i < 5; i++ {
		w, _ = smAufnehmen(w, []smReading{{EntityID: "x", Value: float64(i), RecordedAt: smZeit(t0)}}, false, time.Time{}, t0, 0, 3)
	}
	if len(w) != 3 || w[0].Value != 2 || w[2].Value != 4 {
		t.Fatalf("erwartet die jüngsten drei (2,3,4), bekam %+v", w)
	}
}

func TestSmZeitFormatUndFalscheUhr(t *testing.T) {
	if z := smZeit(time.Date(2026, 11, 18, 7, 5, 9, 123e6, time.FixedZone("CET", 3600))); z != "2026-11-18T06:05:09.123Z" {
		t.Fatalf("Format wie KAI (UTC, ms, Z) erwartet, bekam %q", z)
	}
	if z := smZeit(time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)); z != "" {
		t.Fatalf("Uhr vor 2024 → leer (KAI nimmt die Ankunft), bekam %q", z)
	}
}

// Ohne Netz bleibt alles im Puffer (auch auf der Platte); ist KAI wieder da, kommt alles in der richtigen Reihenfolge an.
func TestSmPufferLiefertNachUndUebersteht(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BORDWAECHTER_CONFIG", filepath.Join(dir, "config.json"))
	erreichbar := false
	var angekommen []smReading
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !erreichbar {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		var body struct {
			Readings []smReading `json:"readings"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		angekommen = append(angekommen, body.Readings...)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	k := newKaiClient(srv.URL, "tok")

	p := &smPuffer{}
	t0 := time.Date(2026, 11, 18, 8, 0, 0, 0, time.UTC)
	p.aufnehmen(werte(2, t0), 0)
	p.senden(k) // KAI nicht erreichbar
	if p.anzahl() != 2 || !p.offline {
		t.Fatalf("ohne Netz: 2 Werte im Puffer und offline erwartet, bekam %d/%v", p.anzahl(), p.offline)
	}
	if _, err := os.Stat(filepath.Join(smOrdner(), "puffer.json")); err != nil {
		t.Fatalf("Puffer muss auf der Platte liegen: %v", err)
	}
	// „Neustart“: neuer Puffer lädt den Stand von der Platte
	p2 := &smPuffer{}
	p2.laden()
	if p2.anzahl() != 2 {
		t.Fatalf("nach Neustart erwartet 2 Werte, bekam %d", p2.anzahl())
	}
	p2.werte = append(p2.werte, werte(1, t0.Add(10*time.Minute))...)
	erreichbar = true
	p2.senden(k)
	if p2.anzahl() != 0 || p2.offline {
		t.Fatalf("nach Rückkehr: Puffer leer und online erwartet, bekam %d/%v", p2.anzahl(), p2.offline)
	}
	if len(angekommen) != 3 || angekommen[0].RecordedAt != smZeit(t0) || angekommen[2].RecordedAt != smZeit(t0.Add(10*time.Minute)) {
		t.Fatalf("älteste zuerst erwartet, bekam %+v", angekommen)
	}
	if _, err := os.Stat(filepath.Join(smOrdner(), "puffer.json")); !os.IsNotExist(err) {
		t.Fatalf("leerer Puffer: Datei muss weg sein")
	}
}
