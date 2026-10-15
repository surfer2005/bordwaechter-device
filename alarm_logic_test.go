package main

import (
	"testing"
	"time"
)

// Ereignis-Alarm: Lampe+Hupe zusammen; Quittieren schaltet BEIDES aus; besteht die Ursache weiter,
// kommt nach ReAlarmSec erneut beides.
func TestEreignisAlarmQuittierenBeidesAus(t *testing.T) {
	cfg := defaultConfig()
	cfg.GPIO.Backend = "none"
	cfg.SIFA.TriggerCombo = "camera"
	cfg.SIFA.ReAlarmSec = 15
	e := NewEngine(cfg, nil, noopGPIO{})
	base := time.Now()
	e.camHazardUntil = base.Add(100 * time.Second) // Ursache besteht dauerhaft

	e.Tick(base.Add(1 * time.Second))
	if s := e.Status(); !s.Horn || !s.Lamp {
		t.Fatalf("Alarm: Hupe+Lampe erwartet, got horn=%v lamp=%v", s.Horn, s.Lamp)
	}
	e.Ack()
	e.Tick(base.Add(2 * time.Second))
	if s := e.Status(); s.Horn || s.Lamp {
		t.Fatalf("nach Quittieren muss BEIDES aus sein, got horn=%v lamp=%v", s.Horn, s.Lamp)
	}
	e.Tick(base.Add(20 * time.Second)) // > ReAlarmSec, Ursache besteht noch
	if s := e.Status(); !s.Horn || !s.Lamp {
		t.Fatalf("Wiederholung: Hupe+Lampe erneut erwartet, got horn=%v lamp=%v", s.Horn, s.Lamp)
	}
}

// Auslöser "aus": alles stillgelegt — weder Ereignis-Alarm noch Totmann, auch wenn beide
// konfiguriert/aktiv wären (für Präsentationen).
func TestAuslöserAusKeinAlarm(t *testing.T) {
	cfg := defaultConfig()
	cfg.GPIO.Backend = "none"
	cfg.SIFA.TriggerCombo = "aus"
	cfg.SIFA.DeadmanOn = true // würde sonst laufen
	cfg.SIFA.DeadmanSec = 30
	e := NewEngine(cfg, nil, noopGPIO{})
	base := time.Now()
	e.lastDeadmanAck = base
	e.camHazardUntil = base.Add(100 * time.Second) // Ursache da, darf aber nichts auslösen
	e.Tick(base.Add(120 * time.Second))            // lange nach Totmann-Intervall
	if s := e.Status(); s.Horn || s.Lamp || s.Alarm {
		t.Fatalf("Auslöser aus: nichts darf kommen, got horn=%v lamp=%v alarm=%v", s.Horn, s.Lamp, s.Alarm)
	}
}

// Totmann: nach dem Intervall zuerst die Lampe, „Vorlauf" Sekunden später die Hupe; Quittieren
// schaltet beides aus und startet das Intervall neu.
func TestTotmannZyklus(t *testing.T) {
	cfg := defaultConfig()
	cfg.GPIO.Backend = "none"
	cfg.SIFA.TriggerCombo = "gps" // kein Ereignis (kein Bundle) → nur Totmann
	cfg.SIFA.DeadmanOn = true
	cfg.SIFA.DeadmanSec = 30
	cfg.SIFA.DeadmanPrewarnSec = 3
	e := NewEngine(cfg, nil, noopGPIO{})
	base := time.Now()
	e.lastDeadmanAck = base

	e.Tick(base.Add(29 * time.Second))
	if s := e.Status(); s.Lamp || s.Horn {
		t.Fatalf("vor Intervallende alles aus erwartet, got lamp=%v horn=%v", s.Lamp, s.Horn)
	}
	e.Tick(base.Add(30 * time.Second))
	if s := e.Status(); !s.Lamp || s.Horn {
		t.Fatalf("bei Intervallende nur Lampe erwartet, got lamp=%v horn=%v", s.Lamp, s.Horn)
	}
	e.Tick(base.Add(33 * time.Second)) // + Vorlauf
	if s := e.Status(); !s.Lamp || !s.Horn {
		t.Fatalf("nach Vorlauf Lampe+Hupe erwartet, got lamp=%v horn=%v", s.Lamp, s.Horn)
	}
	// Quittieren setzt das Intervall zurück.
	e.lastDeadmanAck = base.Add(33 * time.Second)
	e.Tick(base.Add(34 * time.Second))
	if s := e.Status(); s.Lamp || s.Horn {
		t.Fatalf("nach Quittieren alles aus erwartet, got lamp=%v horn=%v", s.Lamp, s.Horn)
	}
	e.Tick(base.Add(64 * time.Second)) // 33 + 31 → neues Intervall erreicht
	if s := e.Status(); !s.Lamp {
		t.Fatalf("neuer Zyklus: Lampe erwartet, got lamp=%v", s.Lamp)
	}
}
