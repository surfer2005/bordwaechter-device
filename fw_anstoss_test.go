package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func neueSensorik() *Sensorik {
	return &Sensorik{defs: map[int]*sensorDef{}, aktuell: map[string]any{}, verletzt: map[string]time.Time{}, status: map[int]*sensorStatus{}, fwStand: map[int]string{}, fwForce: map[int]string{}, logs: map[int][]string{}}
}

// 19.11.2026: ein Anstoß aus KAI („Auf Sensor aufspielen“) wurde nach dem Neustart des Bordwächters ein zweites Mal
// ausgeführt — er stand nur im Arbeitsspeicher. Jetzt liegt er in fw/<sensor>/force.
func TestAnstossUeberlebtNeustart(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BORDWAECHTER_CONFIG", filepath.Join(dir, "config.json"))
	var abrufe int32
	kai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&abrufe, 1)
		_, _ = w.Write(bytes.Repeat([]byte{0xE9}, 4096))
	}))
	defer kai.Close()
	k := newKaiClient(kai.URL, "x")
	def := func(force string) *sensorDef {
		return &sensorDef{ID: 9, Art: "esphome", User: "bw-buero00001", Praefix: "bw-buero00001",
			Firmware: &sensorFirmware{Version: "2026.11.19.1540", Pfad: "/api/edge/firmware/9", Chip: "ESP8266", Force: force}}
	}

	s1 := neueSensorik()
	s1.defs[9] = def("2026-11-19T15:44:41.273Z")
	s1.firmwareHolen(k)
	if n := atomic.LoadInt32(&abrufe); n != 1 {
		t.Fatalf("erster Anstoß: %d Abrufe, erwartet 1", n)
	}
	b, _ := os.ReadFile(filepath.Join(sensorOrdner(), "fw", "bw-buero00001", "force"))
	if strings.TrimSpace(string(b)) != "2026-11-19T15:44:41.273Z" {
		t.Fatalf("Anstoß nicht gemerkt: %q", b)
	}

	// Neustart: frischer Arbeitsspeicher, KAI schickt denselben Anstoß noch → NICHT erneut
	s2 := neueSensorik()
	s2.defs[9] = def("2026-11-19T15:44:41.273Z")
	s2.firmwareHolen(k)
	if n := atomic.LoadInt32(&abrufe); n != 1 {
		t.Fatalf("nach Neustart erneut aufgespielt (%d Abrufe)", n)
	}
	// neuer Anstoß → wieder
	s2.defs[9] = def("2026-11-19T17:00:00.000Z")
	s2.firmwareHolen(k)
	if n := atomic.LoadInt32(&abrufe); n != 2 {
		t.Fatalf("neuer Anstoß nicht ausgeführt (%d Abrufe)", n)
	}
}

// ESPHome meldet seine IP über wifi_info nicht verlässlich (Büro00001) — der Bordwächter nimmt sie aus der Verbindung.
func TestEsphomeIPAusVerbindung(t *testing.T) {
	s := neueSensorik()
	s.defs[9] = &sensorDef{ID: 9, Art: "esphome", User: "bw-buero00001", Praefix: "bw-buero00001"}
	s.eingang(mqttZugang{SensorID: 9, Praefix: "bw-buero00001", IP: "192.168.62.117"}, "bw-buero00001/sensor/buero00001/state", []byte("23.5"))
	if st := s.status[9]; st == nil || st.IP != "192.168.62.117" {
		t.Fatalf("IP nicht übernommen: %+v", st)
	}
	// Shelly: nicht aus der Verbindung (meldet sie selbst)
	s.defs[10] = &sensorDef{ID: 10, Art: "shelly", User: "shelly-x", Praefix: "shelly-x"}
	s.eingang(mqttZugang{SensorID: 10, Praefix: "shelly-x", IP: "10.0.0.9"}, "shelly-x/status/temperature:0", []byte(`{"tC":21.2}`))
	if st := s.status[10]; st != nil && st.IP == "10.0.0.9" {
		t.Fatal("Shelly-IP darf nicht aus der Verbindung kommen")
	}
}
