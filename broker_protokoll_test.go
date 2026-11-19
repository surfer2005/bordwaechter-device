package main

import (
	"bufio"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Der Sensor-Empfänger hinterlässt für JEDE nicht angenommene Verbindung eine Spur im Geräteprotokoll (Peter 2026-11-19:
// ein ESP war im WLAN, Port 1883 war erreichbar, im Protokoll stand nichts) — gedrosselt je Gerät und Grund.

func protokollZeilen() []string {
	logMu.Lock()
	defer logMu.Unlock()
	z := []string{}
	for _, e := range logBuf {
		z = append(z, e.Level+" "+e.Message)
	}
	return z
}

func enthaelt(zeilen []string, teil string) int {
	n := 0
	for _, z := range zeilen {
		if strings.Contains(z, teil) {
			n++
		}
	}
	return n
}

func TestBrokerProtokolliertAbgebrocheneVerbindungen(t *testing.T) {
	t.Setenv("BORDWAECHTER_DEVLOG", filepath.Join(t.TempDir(), "devlog.json"))
	logMu.Lock()
	logBuf = nil
	logMu.Unlock()
	meldungMu.Lock()
	meldungZuletzt = map[string]time.Time{}
	meldungMu.Unlock()

	b := neuerMQTTBroker(func(u, p string) (mqttZugang, bool) {
		return mqttZugang{Alle: true}, u == "mqttuser" && p == "geheim"
	}, func(mqttZugang, string, []byte) {})
	if err := b.Starten("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	defer b.Stoppen()
	addr := b.ln.Addr().String()
	warte := func(teil string) {
		t.Helper()
		for i := 0; i < 100; i++ {
			if enthaelt(protokollZeilen(), teil) > 0 {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("Protokollzeile %q fehlt: %v", teil, protokollZeilen())
	}

	// 1. Port-Prüfung (verbinden, nichts senden, schließen) → Info, zweimal hintereinander nur EINE Zeile
	for i := 0; i < 2; i++ {
		c, _ := net.Dial("tcp", addr)
		c.Close()
		time.Sleep(30 * time.Millisecond)
	}
	warte("ohne Daten wieder geschlossen")
	if n := enthaelt(protokollZeilen(), "ohne Daten wieder geschlossen"); n != 1 {
		t.Fatalf("Port-Prüfung %d× gemeldet, erwartet 1×", n)
	}

	// 2. falsche Protokollstufe (MQTT 5) → CONNACK 1 + Meldung mit Grund
	r := append(mqttStr("MQTT"), 5, 0xC2, 0, 60)
	r = append(r, mqttStr("esp")...)
	r = append(r, mqttStr("mqttuser")...)
	r = append(r, mqttStr("geheim")...)
	c, _ := net.Dial("tcp", addr)
	_, _ = c.Write(mqttPaket(0x10, r))
	if k, p := lesePaket(t, bufio.NewReader(c)); k != 0x20 || p[1] != 1 {
		t.Fatalf("erwartet CONNACK 1, bekam %x %v", k, p)
	}
	c.Close()
	warte("Stufe 5 wird nicht unterstützt")

	// 3. erstes Paket keine Anmeldung (PINGREQ)
	c, _ = net.Dial("tcp", addr)
	_, _ = c.Write([]byte{0xC0, 0})
	time.Sleep(30 * time.Millisecond)
	c.Close()
	warte("erstes Paket ist keine Anmeldung (Typ 12)")

	// 4. angenommene Anmeldung → Info „Sensor angemeldet“, keine Abbruch-Meldung
	c, _ = net.Dial("tcp", addr)
	rd := bufio.NewReader(c)
	_, _ = c.Write(mqttConnect("mqttuser", "geheim"))
	if k, p := lesePaket(t, rd); k != 0x20 || p[1] != 0 {
		t.Fatalf("Anmeldung abgelehnt: %x %v", k, p)
	}
	c.Close()
	warte("Sensor angemeldet: mqttuser (Client client-mqttuser")
	if enthaelt(protokollZeilen(), "Anmeldung beendet: Verbindung") > 0 {
		t.Fatalf("angemeldete Verbindung darf nicht als Abbruch erscheinen: %v", protokollZeilen())
	}
}
