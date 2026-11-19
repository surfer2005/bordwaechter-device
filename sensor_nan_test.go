package main

import (
	"math"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// Büro FN, 19.11.2026: ESPHome meldete „nan“ (DHT11 liest nicht), der Bordwächter nahm das als Zahl, JSON kennt kein
// NaN → die Sendung an KAI ging LEER raus (HTTP 400), blieb im Puffer stecken und hielt alle Sensoren auf.

func TestNanIstKeinMesswert(t *testing.T) {
	st := &sensorStatus{}
	for _, text := range []string{"nan", "NaN", "inf", "-inf"} {
		if w := werteAusNachricht("esphome", "bw-buero", "bw-buero/sensor/temperatur/state", []byte(text), st); len(w) != 0 {
			t.Fatalf("ESPHome %q darf kein Messwert sein: %v", text, w)
		}
		if w := werteAusNachricht("mqtt", "geraet", "geraet/temp", []byte(text), st); len(w) != 0 {
			t.Fatalf("allgemein %q darf kein Messwert sein: %v", text, w)
		}
	}
	w := werteAusNachricht("esphome", "bw-buero", "bw-buero/sensor/temperatur/state", []byte("21.5"), st)
	if len(w) != 1 || w[0].Wert != 21.5 {
		t.Fatalf("21.5 erwartet: %v", w)
	}
	if v := zahlOderZustand("ON"); v != true {
		t.Fatalf("ON → true erwartet, bekam %v", v)
	}
}

func TestKeineLeereAnfrageBeiNaN(t *testing.T) {
	var anfragen int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&anfragen, 1)
		w.WriteHeader(200)
	}))
	defer srv.Close()
	k := &KaiClient{base: srv.URL, hc: srv.Client()}
	if _, _, err := k.do("POST", "/x", map[string]any{"wert": math.NaN()}); err == nil {
		t.Fatal("Verpacken mit NaN muss als Fehler zurückkommen")
	}
	if atomic.LoadInt32(&anfragen) != 0 {
		t.Fatal("es darf keine (leere) Anfrage rausgehen")
	}
	// senden filtert nicht endliche Werte heraus und leert den Puffer trotzdem
	s := &Sensorik{status: map[int]*sensorStatus{}, logs: map[int][]string{}}
	s.puffer = []sensorMessung{{SensorID: 9, Schluessel: "t", Wert: math.NaN()}, {SensorID: 9, Schluessel: "t", Wert: 21.0}}
	s.senden(k)
	if len(s.puffer) != 0 || atomic.LoadInt32(&anfragen) != 1 {
		t.Fatalf("Puffer nicht geleert (%d) oder Anfragen %d", len(s.puffer), anfragen)
	}
}
