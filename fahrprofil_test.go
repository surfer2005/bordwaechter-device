package main

import (
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"
	"time"
)

// Gleichlauf mit KAI: die Fälle stammen aus der Nachrechnung an den echten Fahrten der MS-LINDAU (02.–24.11.2026) und
// sind in KAI gerechnet (server/test/fahrprofil.test.js). Das Gerät muss für jeden Fall dasselbe Urteil, dieselbe Zone,
// dasselbe Soll-Tempo, dieselbe Verspätung, denselben Puffer und denselben Text der Kachel „Prognose“ liefern.
func TestFahrprofilGleichlaufMitKAI(t *testing.T) {
	raw, err := os.ReadFile("testdata/fahrprofil-faelle.json")
	if err != nil {
		t.Fatal(err)
	}
	type num = *float64
	var d struct {
		Profile map[string]LegProfil `json:"profile"`
		Faelle  []struct {
			Gruppe string  `json:"gruppe"`
			Profil *string `json:"profil"`
			E      struct {
				RemainKm, RestMin, RestMinRoh num
				IstKmh, MittelKmh             num
				Wartet                        bool
				Tol, MaxKmh, RampeM           float64
				AnkunftRadiusKm, LengthKm     float64
			} `json:"e"`
			UeberfaelligMin num `json:"ueberfaelligMin"`
			Erwartet        struct {
				Verdict   string  `json:"verdict"`
				Zone      *string `json:"zone"`
				SollKmh   num     `json:"sollKmh"`
				Diff      num     `json:"diff"`
				DelayMin  *int    `json:"delayMin"`
				BufferMin num     `json:"bufferMin"`
				Prognose  struct {
					Text string `json:"text"`
					Ton  string `json:"ton"`
				} `json:"prognose"`
			} `json:"erwartet"`
		} `json:"faelle"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	if len(d.Faelle) < 200 {
		t.Fatalf("zu wenige Fälle: %d", len(d.Faelle))
	}
	wert := func(p num) (float64, bool) {
		if p == nil {
			return 0, false
		}
		return *p, true
	}
	gleich := func(a float64, aok bool, b num) bool {
		if b == nil {
			return !aok
		}
		return aok && math.Abs(a-*b) < 0.002
	}
	urteile := map[string]int{}
	for i, f := range d.Faelle {
		e := tempoEingabe{Wartet: f.E.Wartet, Tol: f.E.Tol, MaxKmh: f.E.MaxKmh, RampeM: f.E.RampeM, AnkunftRadiusKm: f.E.AnkunftRadiusKm, LengthKm: f.E.LengthKm}
		e.RemainKm, _ = wert(f.E.RemainKm)
		e.RestMin, _ = wert(f.E.RestMin)
		e.RestMinRoh, e.RestMinRohOK = wert(f.E.RestMinRoh)
		e.IstKmh, e.IstOK = wert(f.E.IstKmh)
		e.MittelKmh, e.MittelOK = wert(f.E.MittelKmh)
		if f.Profil != nil {
			p := d.Profile[*f.Profil]
			e.Profil = &p
		}
		b := tempoBewertung(e)
		zone := ""
		if f.Erwartet.Zone != nil {
			zone = *f.Erwartet.Zone
		}
		u, uok := wert(f.UeberfaelligMin)
		text, ton := prognoseAnzeige(b.Verdict, b.BufferMin, b.BufferOK, u, uok)
		fehler := []string{}
		if b.Verdict != f.Erwartet.Verdict {
			fehler = append(fehler, "Urteil "+b.Verdict+" statt "+f.Erwartet.Verdict)
		}
		if b.Zone != zone {
			fehler = append(fehler, "Zone "+b.Zone+" statt "+zone)
		}
		if !gleich(b.SollKmh, b.SollOK, f.Erwartet.SollKmh) {
			fehler = append(fehler, "Soll")
		}
		if !gleich(b.Diff, b.DiffOK, f.Erwartet.Diff) {
			fehler = append(fehler, "Diff")
		}
		if (f.Erwartet.DelayMin == nil) == b.DelayOK || (b.DelayOK && b.DelayMin != *f.Erwartet.DelayMin) {
			fehler = append(fehler, "Verspätung")
		}
		if !gleich(b.BufferMin, b.BufferOK, f.Erwartet.BufferMin) {
			fehler = append(fehler, "Puffer")
		}
		if text != f.Erwartet.Prognose.Text || ton != f.Erwartet.Prognose.Ton {
			fehler = append(fehler, "Prognose „"+text+"“ statt „"+f.Erwartet.Prognose.Text+"“")
		}
		if len(fehler) > 0 {
			t.Errorf("Fall %d (%s, Profil %v): %s", i, f.Gruppe, f.Profil != nil, strings.Join(fehler, ", "))
		}
		urteile[b.Verdict]++
	}
	for _, v := range []string{"none", "ausfahrt", "ok", "faster", "slower", "late", "einfahrt", "ankunft", "angekommen"} {
		if urteile[v] == 0 {
			t.Errorf("kein Fall mit Urteil %q", v)
		}
	}
}

// Bundle mit einer Strecke FN → RO (gerade Linie, 12 km) für die Rechnung im Gerät.
func testBundle(mitProfil bool) *Bundle {
	b := &Bundle{HatBordbuch: true}
	b.Ship.Name = "MS-LINDAU"
	b.Thresholds.ToleranceKmh = 1.5
	b.Thresholds.MaxKmh = 40
	b.Thresholds.ProfilRampeM = 500
	geom := [][2]float64{}
	for i := 0; i <= 12; i++ { // Breite 47.6: 1 km ≈ 0.013326° Länge
		geom = append(geom, [2]float64{9.40 + float64(i)*0.013326, 47.60})
	}
	l := 12.0
	leg := Leg{From: "Friedrichshafen", To: "Romanshorn", Dep: "10:00", Arr: "10:46", Geometry: geom, LengthKm: &l}
	if mitProfil {
		leg.Profil = &LegProfil{AusfahrtM: 540, AusfahrtKmh: 12.3, EinfahrtM: 800, EinfahrtKmh: 10.4, ReiseKmh: 20.1, HoechstKmh: 23.7, RestAbKm: 12, Fahrten: 104, Quelle: "schiff"}
	}
	b.Legs = []Leg{leg}
	return b
}

func posBei(kmAbStart, kmh float64, ts time.Time) Position {
	return Position{Lat: 47.60, Lng: 9.40 + kmAbStart*0.013326, SpeedKmh: kmh, Valid: true, TS: ts, MittelKmh: kmh, MittelOK: true}
}

// Steg-Fix (Beobachtung MS-LINDAU 25.11.): 30 s nach der Soll-Abfahrt noch fest am Steg → keine Hochrechnung aus
// dem Stillstand, sondern „Abfahrt fällig“ bzw. „überfällig seit N min“ — mit UND ohne Fahrprofil (älteres KAI).
func TestStegKeineHochrechnung(t *testing.T) {
	for _, mitProfil := range []bool{false, true} {
		now := time.Date(2026, 11, 25, 10, 0, 30, 0, time.Local)
		st := computeAdvisory(testBundle(mitProfil), posBei(0.02, 0.3, now), now, 1.5)
		if st.Phase != "unterwegs" || st.Verdict != "none" || st.BufferMin != nil {
			t.Fatalf("Profil %v: am Steg Urteil %q, Puffer %v — erwartet keine Hochrechnung", mitProfil, st.Verdict, st.BufferMin)
		}
		if st.PrognoseText != "Abfahrt fällig" || st.UeberfaelligMin != 0 {
			t.Fatalf("Profil %v: Prognose %q (überfällig %d)", mitProfil, st.PrognoseText, st.UeberfaelligMin)
		}
		later := now.Add(4 * time.Minute)
		st = computeAdvisory(testBundle(mitProfil), posBei(0.02, 0.3, later), later, 1.5)
		if st.PrognoseText != "überfällig seit 4 min" || !strings.HasPrefix(st.Hint, "Abfahrt überfällig seit 4 min (Soll 10:00)") {
			t.Fatalf("Profil %v: %q / %q", mitProfil, st.PrognoseText, st.Hint)
		}
	}
}

// Ohne Fahrprofil im Bundle (Gerät an einem älteren KAI, z. B. der Produktion): bisherige Rechnung.
func TestOhneProfilBisherigeRechnung(t *testing.T) {
	now := time.Date(2026, 11, 25, 10, 16, 0, 0, time.Local) // Rest 30 min, 10 km zu fahren
	st := computeAdvisory(testBundle(false), posBei(2, 18, now), now, 1.5)
	if st.Zone != "" || st.ProfilQuelle != "" {
		t.Fatalf("ohne Profil Zone %q Quelle %q", st.Zone, st.ProfilQuelle)
	}
	if st.Verdict != "faster" || math.Abs(st.SollKmh-20) > 0.15 || st.MaxKmh != 40 {
		t.Fatalf("Urteil %q Soll %.1f Max %.0f", st.Verdict, st.SollKmh, st.MaxKmh)
	}
	if st.BufferMin == nil || *st.BufferMin != -3 || st.PrognoseText != "3 min zu spät" {
		t.Fatalf("Puffer %v Text %q", st.BufferMin, st.PrognoseText)
	}
}

// Mit Fahrprofil: Hafenausfahrt ohne Tempo-Empfehlung, freie Strecke mit Soll nur für die freie Strecke, Einfahrt.
func TestMitProfilZonen(t *testing.T) {
	b := testBundle(true)
	// 1 min nach Ablegen, 200 m draußen, 8 km/h → Ausfahrt; Prognose mit Ausfahrt-/Reise-/Einfahrt-Tempo statt 8 km/h
	now := time.Date(2026, 11, 25, 10, 2, 0, 0, time.Local)
	st := computeAdvisory(b, posBei(0.2, 8.5, now), now, 1.5)
	if st.Verdict != "ausfahrt" || st.Zone != "ausfahrt" || st.ProfilQuelle != "schiff" || st.MaxKmh != 23.7 {
		t.Fatalf("Ausfahrt: Urteil %q Zone %q Quelle %q Max %.1f", st.Verdict, st.Zone, st.ProfilQuelle, st.MaxKmh)
	}
	// Rest 44 min; Restzeit = 0,34/12,3·60 + 10,66/20,1·60 + 0,8/10,4·60 = 1,66 + 31,82 + 4,62 = 38,1 → 6 min zu früh
	// (bis 0.1.150: 11,8 km mit den gemessenen 8,5 km/h = 83 min → „39 min zu spät“)
	if st.BufferMin == nil || *st.BufferMin != 6 || st.PrognoseText != "6 min zu früh" {
		t.Fatalf("Ausfahrt: Puffer %v Text %q", st.BufferMin, st.PrognoseText)
	}
	if st.SollKmh <= 0 {
		t.Fatalf("Ausfahrt: Soll-Tempo für danach fehlt")
	}
	// freie Strecke
	now = time.Date(2026, 11, 25, 10, 20, 0, 0, time.Local)
	st = computeAdvisory(b, posBei(6, 20, now), now, 1.5)
	if st.Zone != "frei" || (st.Verdict != "ok" && st.Verdict != "faster" && st.Verdict != "slower") {
		t.Fatalf("frei: Urteil %q Zone %q", st.Verdict, st.Zone)
	}
	// Einfahrt
	now = time.Date(2026, 11, 25, 10, 43, 0, 0, time.Local)
	st = computeAdvisory(b, posBei(11.5, 10, now), now, 1.5)
	if st.Verdict != "einfahrt" || st.SollKmh != 0 {
		t.Fatalf("Einfahrt: Urteil %q Soll %.1f", st.Verdict, st.SollKmh)
	}
	// zu spät mit dem Höchsttempo des Schiffs (23,7), nicht mit 40
	now = time.Date(2026, 11, 25, 10, 35, 0, 0, time.Local)
	st = computeAdvisory(b, posBei(5, 20, now), now, 1.5)
	if st.Verdict != "late" || st.DelayMin <= 0 {
		t.Fatalf("zu spät: Urteil %q Verspätung %d", st.Verdict, st.DelayMin)
	}
}

// Ø der letzten Minute im Gerät
func TestMittelLetzteMinute(t *testing.T) {
	e := &Engine{}
	base := time.Date(2026, 11, 25, 10, 0, 0, 0, time.Local)
	for i := 0; i < 20; i++ {
		e.SetPosition(Position{Lat: 47.6, Lng: 9.4 + float64(i)*0.0005, SpeedKmh: float64(10 + i), Valid: true, TS: base.Add(time.Duration(i*5) * time.Second)})
	}
	m, ok := e.mittelLocked(base.Add(95 * time.Second))
	// Proben 35 s … 95 s (i = 7 … 19) → Ø der Werte 17 … 29 = 23
	if !ok || math.Abs(m-23) > 0.01 {
		t.Fatalf("Ø %v ok %v", m, ok)
	}
}
