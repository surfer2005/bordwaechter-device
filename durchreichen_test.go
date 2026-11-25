package main

// Durchreichen statt übersetzen (Peter 2026-11-25): mit KAI, das es ankündigt, gehen Sensor-Nachrichten unverändert an KAI;
// Lampe/Hupe und der Schlüsselschalter lesen ihre Werte aus den Quellen, die KAI mitschickt. Ohne Ankündigung (ältere KAI)
// übersetzt das Gerät wie bisher selbst — die alte Übersetzung bleibt der Rückfall und die Messlatte für KAI.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// ── Gemeinsame Fälle mit KAI (server/src/bordsensorik-roh.js) ───────────────────────────────────────────────────

type uebersetzungsFall struct {
	Name     string         `json:"name"`
	Art      string         `json:"art"`
	Praefix  string         `json:"praefix"`
	Thema    string         `json:"thema"`
	Inhalt   string         `json:"inhalt"`
	Erwartet map[string]any `json:"erwartet,omitempty"`
}

// uebersetzungsErgebnis: was werteAusNachricht liefert — Werte (nach Schlüssel sortiert, Go liest events/rpc in
// zufälliger Reihenfolge) und was die Nachricht am Sensorstand ändert (online, IP, Batterie; nur wenn gesetzt).
func uebersetzungsErgebnis(f uebersetzungsFall) map[string]any {
	const ohne = "\x00"
	a := &sensorStatus{Online: false, IP: ohne}
	b := &sensorStatus{Online: true, IP: ohne}
	w := werteAusNachricht(f.Art, f.Praefix, f.Thema, []byte(f.Inhalt), a)
	werteAusNachricht(f.Art, f.Praefix, f.Thema, []byte(f.Inhalt), b)
	liste := []map[string]any{}
	for _, x := range w {
		liste = append(liste, map[string]any{"schluessel": x.Schluessel, "wert": x.Wert})
	}
	sort.SliceStable(liste, func(i, j int) bool {
		if liste[i]["schluessel"] != liste[j]["schluessel"] {
			return liste[i]["schluessel"].(string) < liste[j]["schluessel"].(string)
		}
		return fmt.Sprint(liste[i]["wert"]) < fmt.Sprint(liste[j]["wert"])
	})
	out := map[string]any{"werte": liste}
	if a.Online == b.Online {
		out["online"] = a.Online
	}
	if a.IP != ohne {
		out["ip"] = a.IP
	}
	if a.Batterie != nil {
		out["batterie"] = *a.Batterie
	}
	// über JSON normalisieren (so liest es auch der JS-Test)
	roh, _ := json.Marshal(out)
	var norm map[string]any
	_ = json.Unmarshal(roh, &norm)
	return norm
}

func TestUebersetzungGemeinsameFaelle(t *testing.T) {
	pfad := filepath.Join("testdata", "sensor-uebersetzung.json")
	daten, err := os.ReadFile(pfad)
	if err != nil {
		t.Fatal(err)
	}
	var datei struct {
		Hinweis string              `json:"hinweis"`
		Faelle  []uebersetzungsFall `json:"faelle"`
	}
	if err := json.Unmarshal(daten, &datei); err != nil {
		t.Fatal(err)
	}
	if len(datei.Faelle) < 50 {
		t.Fatalf("zu wenige Fälle: %d", len(datei.Faelle))
	}
	if os.Getenv("FIXTURE_SCHREIBEN") == "1" {
		for i := range datei.Faelle {
			datei.Faelle[i].Erwartet = uebersetzungsErgebnis(datei.Faelle[i])
		}
		neu, _ := json.MarshalIndent(datei, "", " ")
		if err := os.WriteFile(pfad, append(neu, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	for _, f := range datei.Faelle {
		ist, _ := json.Marshal(uebersetzungsErgebnis(f))
		soll, _ := json.Marshal(f.Erwartet)
		if string(ist) != string(soll) {
			t.Errorf("%s: Gerät liefert %s, festgehalten ist %s", f.Name, ist, soll)
		}
	}
}

// ── Quellen ────────────────────────────────────────────────────────────────────────────────────────────────────

func TestWertAusQuelle(t *testing.T) {
	notify := []byte(`{"method":"NotifyStatus","params":{"ts":1.7e9,"switch:2":{"id":2,"output":true,"apower":12.3}}}`)
	if v, ok := wertAusQuelle(wertQuelle{Thema: "leiste/events/rpc", Pfad: []string{"params", "switch:2", "output"}}, "leiste/events/rpc", notify); !ok || v != true {
		t.Fatalf("Relais aus events/rpc: %v %v", v, ok)
	}
	if v, ok := wertAusQuelle(wertQuelle{Thema: "leiste/events/rpc", Pfad: []string{"params", "switch:2", "apower"}}, "leiste/events/rpc", notify); !ok || v != 12.3 {
		t.Fatalf("Leistung: %v %v", v, ok)
	}
	if _, ok := wertAusQuelle(wertQuelle{Thema: "leiste/events/rpc", Pfad: []string{"params", "switch:0", "output"}}, "leiste/events/rpc", notify); ok {
		t.Fatal("fehlendes Feld darf kein Wert sein")
	}
	if _, ok := wertAusQuelle(wertQuelle{Thema: "leiste/status/switch:2", Pfad: []string{"output"}}, "leiste/events/rpc", notify); ok {
		t.Fatal("anderes Thema darf nicht passen")
	}
	// ganzer Inhalt: ESPHome / Web-IO / allgemein
	if v, ok := wertAusQuelle(wertQuelle{Thema: "kuehl/sensor/temperatur/state"}, "kuehl/sensor/temperatur/state", []byte(" 8.6 ")); !ok || v != 8.6 {
		t.Fatalf("ESPHome: %v %v", v, ok)
	}
	if v, ok := wertAusQuelle(wertQuelle{Thema: "BSBWEBIO000001/get/input/1"}, "BSBWEBIO000001/get/input/1", []byte("OFF")); !ok || v != false {
		t.Fatalf("Web-IO: %v %v", v, ok)
	}
	if _, ok := wertAusQuelle(wertQuelle{Thema: "kuehl/sensor/temperatur/state"}, "kuehl/sensor/temperatur/state", []byte("nan")); ok {
		t.Fatal("nan ist kein Wert")
	}
	// Text im JSON wie zahlOderZustand, Platzhalter im Thema wie beim Abonnieren
	if v, ok := wertAusQuelle(wertQuelle{Thema: "g/+/stand", Pfad: []string{"a", "b"}}, "g/x/stand", []byte(`{"a":{"b":"ON"}}`)); !ok || v != true {
		t.Fatalf("Pfad mit Text: %v %v", v, ok)
	}
	if _, ok := wertAusQuelle(wertQuelle{Thema: "g/x", Pfad: []string{"a"}}, "g/x", []byte(`kein json`)); ok {
		t.Fatal("kein JSON → kein Wert")
	}
}

// ── Durchreichen am Eingang ────────────────────────────────────────────────────────────────────────────────────

func defsMitQuellen() map[int]*sensorDef {
	max := 7.0
	aus := "aus"
	return map[int]*sensorDef{
		3: {ID: 3, Art: "esphome", Praefix: "kuehl", Werte: []sensorWertDef{{Schluessel: "temperatur", Max: &max, DauerMin: 15, Lampe: true,
			Quellen: []wertQuelle{{Thema: "kuehl/sensor/temperatur/state"}, {Thema: "kuehl/binary_sensor/temperatur/state"}}}}},
		5: {ID: 5, Art: "webio", Praefix: "BSBWEBIO000001", Werte: []sensorWertDef{{Schluessel: "input_1", AlarmBei: &aus,
			Quellen: []wertQuelle{{Thema: "BSBWEBIO000001/get/input/1"}}}}},
		8: {ID: 8, Art: "shelly", Praefix: "bw-temperatur", Werte: []sensorWertDef{{Schluessel: "temperatur",
			Quellen: []wertQuelle{{Thema: "bw-temperatur/status/temperature:0", Pfad: []string{"tC"}}, {Thema: "bw-temperatur/events/rpc", Pfad: []string{"params", "temperature:0", "tC"}}}}}},
	}
}

func TestDurchreichenPuffertUnveraendertUndPrueftLampe(t *testing.T) {
	s := neueSensorik()
	s.defs = defsMitQuellen()
	s.durchreichen = true
	shelly := []byte(`{"method":"NotifyFullStatus","params":{"temperature:0":{"id":0,"tC":7.5},"humidity:0":{"id":0,"rh":55}}}`)
	s.eingang(mqttZugang{Alle: true, IP: "192.168.75.52"}, "bw-temperatur/events/rpc", shelly)
	s.eingang(mqttZugang{SensorID: 3, Praefix: "kuehl", IP: "192.168.75.60"}, "kuehl/sensor/temperatur/state", []byte("8.6"))
	s.eingang(mqttZugang{SensorID: 3, Praefix: "kuehl"}, "kuehl/debug", []byte("[D][dallas]: 8.6°C"))
	s.eingang(mqttZugang{Alle: true}, "unbekannt/events/rpc", shelly)
	if len(s.puffer) != 0 {
		t.Fatalf("beim Durchreichen übersetzt das Gerät nichts: %v", s.puffer)
	}
	if len(s.roh) != 2 {
		t.Fatalf("zwei Nachrichten erwartet (Log und unbekanntes Thema nicht), bekam %d: %+v", len(s.roh), s.roh)
	}
	if s.roh[0].Thema != "bw-temperatur/events/rpc" || s.roh[0].Inhalt != string(shelly) || s.roh[0].SensorID != 0 {
		t.Fatalf("gemeinsamer Zugang: unverändert, ohne Sensor (KAI ordnet zu): %+v", s.roh[0])
	}
	if s.roh[1].SensorID != 3 || s.roh[1].Inhalt != "8.6" || s.roh[1].At == "" {
		t.Fatalf("eigener Zugang: mit Sensor: %+v", s.roh[1])
	}
	if s.status[3].VerbindungIP != "192.168.75.60" || s.status[3].IP != "" || s.status[8].VerbindungIP != "192.168.75.52" {
		t.Fatalf("Adresse aus der Verbindung für KAI: %+v %+v", s.status[3], s.status[8])
	}
	if len(s.logs[3]) != 1 {
		t.Fatal("ESPHome-Log bleibt am Gerät")
	}
	// Werte aus den Quellen für Lampe/Hupe
	if s.aktuell["8|temperatur"] != 7.5 || s.aktuell["3|temperatur"] != 8.6 {
		t.Fatalf("Quellen gelesen: %v", s.aktuell)
	}
	t0 := time.Now()
	if s.lampeBerechnen(t0) || !s.lampeBerechnen(t0.Add(16*time.Minute)) {
		t.Fatal("Lampe nach 15 Minuten über der Grenze — auch beim Durchreichen")
	}
	s.eingang(mqttZugang{SensorID: 3, Praefix: "kuehl"}, "kuehl/sensor/temperatur/state", []byte("4.0"))
	if s.lampeBerechnen(t0.Add(17 * time.Minute)) {
		t.Fatal("Lampe aus, wenn der Wert zurück ist")
	}
}

// Der Schlüsselschalter des Lichts (Web-IO) kommt beim Durchreichen über seine Quelle — DALI bleibt unberührt.
func TestDurchreichenSchluesselschalter(t *testing.T) {
	alt := sensorik
	defer func() { sensorik = alt }()
	sensorik = neueSensorik()
	sensorik.defs = defsMitQuellen()
	sensorik.durchreichen = true
	sensorik.eingang(mqttZugang{Alle: true}, "BSBWEBIO000001/get/input/1", []byte("ON"))
	if v, ok := sensorik.WertVon("BSBWEBIO000001", "input_1"); !ok || v != true {
		t.Fatalf("Schlüsselschalter: %v %v", v, ok)
	}
	sensorik.eingang(mqttZugang{Alle: true}, "BSBWEBIO000001/get/input/1", []byte("OFF"))
	if v, _ := sensorik.WertVon("BSBWEBIO000001", "input_1"); v != false {
		t.Fatalf("Schlüsselschalter aus: %v", v)
	}
}

func TestDurchreichenBinaerInhaltUndObergrenzen(t *testing.T) {
	s := neueSensorik()
	s.defs = defsMitQuellen()
	s.durchreichen = true
	s.eingang(mqttZugang{Alle: true}, "kuehl/roh", []byte{0xff, 0x00, 0x41})
	if s.roh[0].Inhalt != "" || s.roh[0].InhaltB64 != "/wBB" {
		t.Fatalf("kein UTF-8 → base64: %+v", s.roh[0])
	}
	s.eingang(mqttZugang{Alle: true}, "kuehl/gross", []byte(strings.Repeat("x", rohMaxNachricht+1)))
	if len(s.roh) != 1 {
		t.Fatal("übergroße Nachricht wird nicht gepuffert")
	}
	for i := 0; i < rohMaxAnzahl+10; i++ {
		s.eingang(mqttZugang{Alle: true}, "kuehl/sensor/temperatur/state", []byte(fmt.Sprint(i)))
	}
	if len(s.roh) != rohMaxAnzahl || s.roh[len(s.roh)-1].Inhalt != fmt.Sprint(rohMaxAnzahl+9) {
		t.Fatalf("älteste fallen raus: %d, letzte %q", len(s.roh), s.roh[len(s.roh)-1].Inhalt)
	}
	summe := 0
	for _, r := range s.roh {
		summe += r.groesse()
	}
	if summe != s.rohBytes {
		t.Fatalf("Größe mitgezählt: %d gegen %d", summe, s.rohBytes)
	}
}

// ── An KAI senden ──────────────────────────────────────────────────────────────────────────────────────────────

type kaiAttrappe struct {
	mu       sync.Mutex
	anfragen []map[string]json.RawMessage
	code     int
	srv      *httptest.Server
}

func neueKaiAttrappe() *kaiAttrappe {
	a := &kaiAttrappe{code: 200}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]json.RawMessage
		_ = json.Unmarshal(b, &m)
		a.mu.Lock()
		a.anfragen = append(a.anfragen, m)
		code := a.code
		a.mu.Unlock()
		w.WriteHeader(code)
	}))
	return a
}

func TestDurchreichenSendetRohUndAltenPuffer(t *testing.T) {
	t.Setenv("BORDWAECHTER_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	kai := neueKaiAttrappe()
	defer kai.srv.Close()
	k := &KaiClient{base: kai.srv.URL, hc: kai.srv.Client()}
	s := neueSensorik()
	s.defs = defsMitQuellen()
	// noch im alten Format gepuffert (vor dem Update) — geht über „werte“ an KAI, nichts geht verloren
	s.puffer = []sensorMessung{{SensorID: 3, Schluessel: "temperatur", Wert: 5.1, At: "2026-11-25T08:00:00Z"}}
	s.durchreichen = true
	s.eingang(mqttZugang{SensorID: 3, Praefix: "kuehl"}, "kuehl/sensor/temperatur/state", []byte("8.6"))
	kai.code = 500 // ohne Netz: beide Puffer bleiben und landen auf der Platte
	s.senden(k)
	if len(s.puffer) != 1 || len(s.roh) != 1 {
		t.Fatalf("nach Fehlschlag bleibt alles: %d / %d", len(s.puffer), len(s.roh))
	}
	for _, f := range []string{"puffer.json", "puffer-roh.json"} {
		if _, err := os.Stat(filepath.Join(sensorOrdner(), f)); err != nil {
			t.Fatalf("%s fehlt: %v", f, err)
		}
	}
	kai.code = 200
	s.senden(k)
	if len(s.puffer) != 0 || len(s.roh) != 0 || s.rohBytes != 0 {
		t.Fatalf("nach Erfolg leer: %d / %d / %d", len(s.puffer), len(s.roh), s.rohBytes)
	}
	letzte := kai.anfragen[len(kai.anfragen)-1]
	var roh []sensorRoh
	var werte []sensorMessung
	_ = json.Unmarshal(letzte["roh"], &roh)
	_ = json.Unmarshal(letzte["werte"], &werte)
	if len(roh) != 1 || roh[0].Inhalt != "8.6" || roh[0].Thema != "kuehl/sensor/temperatur/state" || len(werte) != 1 || werte[0].Wert != 5.1 {
		t.Fatalf("Sendung: roh=%+v werte=%+v", roh, werte)
	}
	for _, f := range []string{"puffer.json", "puffer-roh.json"} {
		if _, err := os.Stat(filepath.Join(sensorOrdner(), f)); err == nil {
			t.Fatalf("%s muss nach dem Senden weg sein", f)
		}
	}
	// Beim Durchreichen geht „roh“ immer mit (auch leer) — daran erkennt KAI den Meldeweg
	s.status[3] = &sensorStatus{SensorID: 3}
	s.senden(k)
	if r, ok := kai.anfragen[len(kai.anfragen)-1]["roh"]; !ok || string(r) != "[]" {
		t.Fatalf("leeres roh erwartet, bekam %q", r)
	}
}

// Älteres KAI (z. B. die Produktion) kündigt nichts an: das Gerät übersetzt selbst, „roh“ geht nicht mit.
func TestAltesKaiOhneDurchreichen(t *testing.T) {
	t.Setenv("BORDWAECHTER_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	kai := neueKaiAttrappe()
	defer kai.srv.Close()
	k := &KaiClient{base: kai.srv.URL, hc: kai.srv.Client()}
	var c sensorenConfig
	if err := json.Unmarshal([]byte(`{"sensoren":[{"id":3,"art":"esphome","praefix":"kuehl","werte":[{"schluessel":"temperatur","max":7,"lampe":true}]}],"broker":null}`), &c); err != nil {
		t.Fatal(err)
	}
	s := neueSensorik()
	s.configUebernehmen(c.Sensoren, c.Durchreichen)
	s.eingang(mqttZugang{SensorID: 3, Praefix: "kuehl"}, "kuehl/sensor/temperatur/state", []byte("8.6"))
	if len(s.roh) != 0 || len(s.puffer) != 1 || s.puffer[0].Schluessel != "temperatur" || s.puffer[0].Wert != 8.6 {
		t.Fatalf("wie bisher übersetzt: roh=%d puffer=%+v", len(s.roh), s.puffer)
	}
	s.senden(k)
	if _, ok := kai.anfragen[0]["roh"]; ok {
		t.Fatal("älteres KAI bekommt kein „roh“")
	}
}

// Rückfall: KAI nimmt das Durchreichen zurück (Einstellung oder älteres KAI) — gepufferte Nachrichten werden hier
// übersetzt wie bisher, mit Messzeit, und gehen über „werte“ an KAI. Auch nach einem Neustart ohne Netz.
func TestRueckfallUebersetztGepufferteNachrichten(t *testing.T) {
	t.Setenv("BORDWAECHTER_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	s := neueSensorik()
	defs := defsMitQuellen()
	liste := []*sensorDef{defs[3], defs[5], defs[8]}
	s.configUebernehmen(liste, true)
	s.eingang(mqttZugang{Alle: true}, "bw-temperatur/events/rpc", []byte(`{"method":"NotifyStatus","params":{"temperature:0":{"tC":7.5},"devicepower:0":{"battery":{"percent":80}}}}`))
	s.eingang(mqttZugang{Alle: true}, "BSBWEBIO000001/get/input/1", []byte("ON"))
	at := s.roh[0].At
	// Neustart ohne Netz: Meldeweg und Puffer von der Platte
	s.pufferSichern()
	n := neueSensorik()
	n.defsLaden()
	n.meldewegLaden()
	n.pufferLaden()
	if !n.durchreichen || len(n.roh) != 2 {
		t.Fatalf("nach Neustart: durchreichen=%v roh=%d", n.durchreichen, len(n.roh))
	}
	n.configUebernehmen(liste, false)
	if len(n.roh) != 0 || n.rohBytes != 0 {
		t.Fatal("durchgereichte Nachrichten sind übersetzt")
	}
	got := map[string]any{}
	for _, m := range n.puffer {
		got[fmt.Sprintf("%d|%s", m.SensorID, m.Schluessel)] = m.Wert
		if m.SensorID == 8 && m.At != at {
			t.Fatalf("Messzeit bleibt: %s gegen %s", m.At, at)
		}
	}
	if got["8|temperatur"] != 7.5 || got["8|batterie"] != 80.0 || got["5|input_1"] != true || len(n.puffer) != 3 {
		t.Fatalf("übersetzt: %v", got)
	}
	if n.status[8].Batterie == nil || *n.status[8].Batterie != 80 {
		t.Fatal("Batterie wie bisher im Stand")
	}
	n2 := neueSensorik()
	n2.meldewegLaden()
	if n2.durchreichen {
		t.Fatal("Rückfall ist gemerkt")
	}
}
