package main

// Schalten als fertige Nachricht aus KAI (Peter 2026-11-25, gleiche Fassung wie das Durchreichen): das Gerät kündigt
// „nachricht“ an und veröffentlicht Thema + Inhalt nur noch. Ein älteres KAI schickt weiter Präfix + Kanal.

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
)

// Genau dieser Text kommt auch aus KAI (schaltNachricht in server/src/bordsensorik-roh.js, Test bordsensorik-durchreichen) —
// der Shelly bekommt Byte für Byte, was er bisher vom Bordwächter bekam.
const shellySchaltNachricht7 = `{"id":7,"method":"Switch.Set","params":{"id":2,"on":true},"src":"kai-bordwaechter"}`

func TestShellySchaltNachrichtWieKai(t *testing.T) {
	if got := string(shellySchaltNachricht(7, 2, true)); got != shellySchaltNachricht7 {
		t.Fatalf("bisherige Nachricht am Gerät: %s", got)
	}
}

// schaltUmgebung: echter MQTT-Empfänger mit einem verbundenen Sensor (Shelly-Leiste, Präfix „leiste“), der seine Befehle
// abonniert hat, und eine KAI-Attrappe, die nacheinander die vorgegebenen Antworten auf sensor-befehle liefert.
func schaltUmgebung(t *testing.T, antworten ...string) (*Sensorik, *KaiClient, *bufio.Reader, func() map[string]json.RawMessage) {
	t.Helper()
	s := neueSensorik()
	s.defs[5] = &sensorDef{ID: 5, Art: "shelly", Name: "Leiste Bar", User: "leiste", Praefix: "leiste", Schaltbar: true}
	s.defs[6] = &sensorDef{ID: 6, Art: "esphome", Name: "Pumpe", User: "pumpe", Praefix: "pumpe"}
	s.broker = neuerMQTTBroker(func(u, p string) (mqttZugang, bool) {
		if u == "leiste" && p == "geheim" {
			return mqttZugang{SensorID: 5, Praefix: "leiste"}, true
		}
		return mqttZugang{}, false
	}, s.eingang)
	if err := s.broker.Starten("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.broker.Stoppen)
	c, err := net.Dial("tcp", s.broker.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	r := bufio.NewReader(c)
	_, _ = c.Write(mqttConnect("leiste", "geheim"))
	if k, p := lesePaket(t, r); k != 0x20 || p[1] != 0 {
		t.Fatalf("Anmeldung: %x %v", k, p)
	}
	_, _ = c.Write(mqttPaket(0x82, append([]byte{0, 1}, append(mqttStr("leiste/#"), 0)...)))
	if k, _ := lesePaket(t, r); k != 0x90 {
		t.Fatal("SUBACK erwartet")
	}
	var mu sync.Mutex
	var letzte map[string]json.RawMessage
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		mu.Lock()
		letzte = nil
		_ = json.Unmarshal(b, &letzte)
		a := `{"befehle":[]}`
		if n < len(antworten) {
			a = antworten[n]
		}
		n++
		mu.Unlock()
		_, _ = w.Write([]byte(a))
	}))
	t.Cleanup(srv.Close)
	return s, &KaiClient{base: srv.URL, hc: srv.Client()}, r, func() map[string]json.RawMessage { mu.Lock(); defer mu.Unlock(); return letzte }
}

// lieferung: nächste Nachricht, die der Sensor vom Bordwächter bekommt (Thema, Inhalt).
func lieferung(t *testing.T, r *bufio.Reader) (string, string) {
	t.Helper()
	k, p := lesePaket(t, r)
	if k>>4 != 3 {
		t.Fatalf("PUBLISH erwartet, bekam %x", k)
	}
	thema, rest, _ := leseString(p)
	return thema, string(rest)
}

func TestSchaltenAlsFertigeNachricht(t *testing.T) {
	s, k, r, anfrage := schaltUmgebung(t,
		`{"befehle":[{"id":7,"sensor_id":5,"art":"nachricht","thema":"leiste/rpc","inhalt":`+strconv.Quote(shellySchaltNachricht7)+`,"text":"Steckdose 3 ein"},`+
			`{"id":8,"sensor_id":5,"art":"nachricht","thema":"pumpe/switch/pumpe/command","inhalt":"ON"},`+
			`{"id":9,"sensor_id":6,"art":"nachricht","thema":"pumpe/switch/pumpe/command","inhalt":"ON"}]}`)
	var q []sensorQuittung
	if !s.schaltBefehle(k, &q) {
		t.Fatal("Befehle erwartet")
	}
	if string(anfrage()["kann"]) != `["nachricht"]` {
		t.Fatalf("Ankündigung fehlt: %s", anfrage()["kann"])
	}
	thema, inhalt := lieferung(t, r)
	if thema != "leiste/rpc" || inhalt != shellySchaltNachricht7 {
		t.Fatalf("veröffentlicht: %s %s", thema, inhalt)
	}
	// Sicherungen: nur unter dem Thema des eigenen Sensors, nur wenn er verbunden ist
	if len(q) != 3 || !q[0].OK || q[1].OK || q[1].Fehler != "Thema gehört nicht zu diesem Sensor" || q[2].OK || q[2].Fehler != "Gerät ist nicht mit dem Bordwächter verbunden" {
		t.Fatalf("Quittungen: %+v", q)
	}
	// die Quittungen gehen mit der nächsten Abfrage an KAI
	s.schaltBefehle(k, &q)
	var erledigt []sensorQuittung
	_ = json.Unmarshal(anfrage()["erledigt"], &erledigt)
	if len(erledigt) != 3 || erledigt[0].ID != 7 || !erledigt[0].OK {
		t.Fatalf("Quittung an KAI: %+v", erledigt)
	}
}

// Älteres KAI kennt „nachricht“ nicht und schickt Präfix + Kanal — das Gerät baut Switch.Set wie bisher.
func TestSchaltenAltesKai(t *testing.T) {
	s, k, r, _ := schaltUmgebung(t, `{"befehle":[{"id":7,"sensor_id":5,"praefix":"leiste","kanal":2,"an":true}]}`)
	var q []sensorQuittung
	s.schaltBefehle(k, &q)
	if thema, inhalt := lieferung(t, r); thema != "leiste/rpc" || inhalt != shellySchaltNachricht7 {
		t.Fatalf("bisherige Form: %s %s", thema, inhalt)
	}
	if len(q) != 1 || !q[0].OK {
		t.Fatalf("Quittung: %+v", q)
	}
}
