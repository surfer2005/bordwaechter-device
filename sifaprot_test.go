package main

import (
	"testing"
	"time"
)

// Kurze Signale werden als steigende Flanke gezählt, mit Zeitpunkt; ein neuer Tag beginnt bei 0.
func TestSifaProtokoll(t *testing.T) {
	var p sifaProt
	t0 := time.Date(2026, 10, 24, 14, 0, 0, 0, time.Local)
	p.signale(t0, false, true, "Totmann nicht quittiert")                  // Lampe an
	p.signale(t0.Add(time.Second), false, true, "Totmann nicht quittiert") // bleibt an → keine neue Flanke
	p.signale(t0.Add(5*time.Second), true, true, "Totmann nicht quittiert")
	p.quittiert(t0.Add(6 * time.Second))
	p.signale(t0.Add(7*time.Second), false, false, "")
	if p.lampeHeute != 1 || p.hupeHeute != 1 || p.quittHeute != 1 {
		t.Fatalf("Zählung falsch: %+v", p)
	}
	if !p.hupeZuletzt.Equal(t0.Add(5*time.Second)) || p.alarmGrund != "Totmann nicht quittiert" {
		t.Fatalf("Hupe/Grund falsch: %+v", p)
	}
	p.signale(t0.Add(24*time.Hour), false, true, "")
	if p.lampeHeute != 1 || p.hupeHeute != 0 || p.quittHeute != 0 {
		t.Fatalf("neuer Tag nicht zurückgesetzt: %+v", p)
	}
}
