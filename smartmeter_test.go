package main

import (
	"encoding/json"
	"errors"
	"fmt"
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

// Stummer Zähler (Peter 2026-11-18): antwortet die erste Anfrage nicht, wird der Zähler für diesen Durchgang
// übersprungen statt je Register 5 s zu warten. Antwortende Zähler werden genau wie bisher gelesen.

type smAnfrage struct {
	host  string
	slave int
	reg   int
}

// smFalscheLeser liefert je Slave ein Verhalten: "ok", "stumm" (keine Antwort), "ausnahme" (Modbus-Ausnahme
// beim ersten Register), "spaeter" (erstes Register ok, danach keine Antwort mehr).
func smFalscheLeser(t *testing.T, verhalten map[int]string) *[]smAnfrage {
	t.Helper()
	alt := smLesen
	smStumm = map[string]bool{}
	t.Cleanup(func() { smLesen = alt; smStumm = map[string]bool{} })
	protokoll := &[]smAnfrage{}
	smLesen = func(host string, port, unit, startReg, count int, timeout time.Duration) ([]uint16, error) {
		*protokoll = append(*protokoll, smAnfrage{host, unit, startReg})
		switch verhalten[unit] {
		case "stumm":
			return nil, errors.New("read tcp 192.168.82.18:502: i/o timeout")
		case "ausnahme":
			if startReg == 1 {
				return nil, modbusAusnahme{fc: 0x83}
			}
		case "spaeter":
			if startReg > 1 {
				return nil, errors.New("i/o timeout")
			}
		}
		return []uint16{0x4120, 0x0000}, nil // 10.0 als float32
	}
	return protokoll
}

func smZaehler(name string, slave, n int) smMeter {
	m := smMeter{Name: name, Slave: slave}
	for i := 1; i <= n; i++ {
		m.Registers = append(m.Registers, smRegister{Address: i, EntityID: fmt.Sprintf("sensor.test_%s_%d", name, i)})
	}
	return m
}

func smAnfragenAn(p *[]smAnfrage, slave int) int {
	n := 0
	for _, a := range *p {
		if a.slave == slave {
			n++
		}
	}
	return n
}

func TestStummerZaehlerWirdNurEinmalGefragt(t *testing.T) {
	p := smFalscheLeser(t, map[int]string{1: "ok", 2: "stumm", 3: "ok"})
	sc := &smConfig{Enabled: true, Host: "192.168.82.18", Port: 502, Meters: []smMeter{
		smZaehler("land_fn", 1, 11), smZaehler("land_ro", 2, 11), smZaehler("gastro", 3, 13)}}
	out := readSmartmeter(sc)
	if got := smAnfragenAn(p, 2); got != 1 {
		t.Fatalf("stummer Zähler: %d Anfragen, erwartet genau 1", got)
	}
	if smAnfragenAn(p, 1) != 11 || smAnfragenAn(p, 3) != 13 {
		t.Fatalf("antwortende Zähler nicht vollständig gelesen: FN %d, Gastro %d", smAnfragenAn(p, 1), smAnfragenAn(p, 3))
	}
	if len(out) != 24 {
		t.Fatalf("%d Werte, erwartet 24 (11 + 13)", len(out))
	}
	// Nächster Durchgang: der stumme Zähler wird wieder gefragt (bleibt eingeschaltet).
	*p = nil
	readSmartmeter(sc)
	if smAnfragenAn(p, 2) != 1 {
		t.Fatal("stummer Zähler wird im nächsten Durchgang nicht mehr gefragt")
	}
}

func TestStummerZaehlerAntwortetWieder(t *testing.T) {
	verhalten := map[int]string{2: "stumm"}
	smFalscheLeser(t, verhalten)
	sc := &smConfig{Host: "h", Port: 502, Meters: []smMeter{smZaehler("land_ro", 2, 11)}}
	if out := readSmartmeter(sc); len(out) != 0 || !smStumm["land_ro|h:502|2"] {
		t.Fatal("stummer Zähler nicht vermerkt")
	}
	verhalten[2] = "ok"
	if out := readSmartmeter(sc); len(out) != 11 {
		t.Fatalf("wieder antwortender Zähler: %d Werte, erwartet 11", len(out))
	}
	if smStumm["land_ro|h:502|2"] {
		t.Fatal("Vermerk „stumm“ nach Antwort nicht entfernt")
	}
}

func TestAusnahmeIstEineAntwort(t *testing.T) {
	// Modbus-Ausnahme beim ersten Register: der Zähler ist da → übrige Register weiter lesen.
	p := smFalscheLeser(t, map[int]string{1: "ausnahme"})
	out := readSmartmeter(&smConfig{Host: "h", Port: 502, Meters: []smMeter{smZaehler("land", 1, 11)}})
	if smAnfragenAn(p, 1) != 11 || len(out) != 10 {
		t.Fatalf("Ausnahme beim ersten Register: %d Anfragen / %d Werte, erwartet 11 / 10", smAnfragenAn(p, 1), len(out))
	}
}

func TestSpaetererAusfallUeberspringtNurDasRegister(t *testing.T) {
	// Erstes Register beantwortet, danach Zeitüberschreitungen: wie bisher je Register weiter versuchen.
	p := smFalscheLeser(t, map[int]string{1: "spaeter"})
	out := readSmartmeter(&smConfig{Host: "h", Port: 502, Meters: []smMeter{smZaehler("land", 1, 11)}})
	if smAnfragenAn(p, 1) != 11 || len(out) != 1 {
		t.Fatalf("späterer Ausfall: %d Anfragen / %d Werte, erwartet 11 / 1", smAnfragenAn(p, 1), len(out))
	}
}

func TestKeineAntwortUnterscheidung(t *testing.T) {
	if smKeineAntwort(nil) {
		t.Fatal("kein Fehler gilt als keine Antwort")
	}
	if smKeineAntwort(modbusAusnahme{fc: 0x83}) || smKeineAntwort(fmt.Errorf("register 5: %w", modbusAusnahme{fc: 0x83})) {
		t.Fatal("Modbus-Ausnahme gilt fälschlich als keine Antwort")
	}
	if !smKeineAntwort(errors.New("dial tcp 192.168.82.18:502: connect: connection refused")) {
		t.Fatal("abgelehnte Verbindung gilt nicht als keine Antwort")
	}
	if got := (modbusAusnahme{fc: 0x83}).Error(); got != "modbus-fehler/ausnahme (fc=0x83)" {
		t.Fatalf("Fehlertext geändert: %q", got)
	}
}

// Nach einem Neustart ohne Zeitabgleich: keine Messzeit (KAI nimmt die Ankunft) — sonst trägt der Wert die
// zuletzt gespeicherte Uhrzeit und erscheint in KAI als Rücksprung (Radolfzell 18.11.2026).
func TestSmStempelNurMitAbgeglichenerUhr(t *testing.T) {
	alt := uhrIstSynchron
	defer func() { uhrIstSynchron = alt }()
	t0 := time.Date(2026, 11, 18, 9, 45, 30, 780e6, time.UTC)
	uhrIstSynchron = func() bool { return false }
	if z := smStempel(t0); z != "" {
		t.Fatalf("ohne Abgleich erwartet leer, bekommen %q", z)
	}
	uhrIstSynchron = func() bool { return true }
	if z := smStempel(t0); z != "2026-11-18T09:45:30.780Z" {
		t.Fatalf("mit Abgleich erwartet Messzeit, bekommen %q", z)
	}
}
