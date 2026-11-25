package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// Gleichlauf mit KAI für „Ankunft abwarten“ (ab 0.1.152, Peter 2026-11-25): die Fälle sind in KAI erzeugt
// (server/test/ankunft-abwarten.test.js, ANKUNFT_FAELLE_SCHREIBEN=1).
//  1. regel: Eingaben aus der Nachrechnung an den echten Fahrten der MS-LINDAU + Randfälle → halten/Grund.
//  2. szenario: die Fahrt vom 24.11.2026 11:20 Friedrichshafen → Romanshorn (Soll-Ankunft 12:06, angelegt ~12:10) —
//     das Bundle, wie es das Gerät von KAI bekommt, die Positionen des Bordwächters und was KAI (gpsKioskData) zu jedem
//     Zeitpunkt zeigt. Das Gerät muss dieselbe Fahrt, dieselbe Phase und dasselbe „Ankunft abwarten“ zeigen.
type ankunftFaelle struct {
	Regel []struct {
		E struct {
			UeberSollMin  float64  `json:"ueberSollMin"`
			MaxMin        *float64 `json:"maxMin"`
			Frisch        bool     `json:"frisch"`
			RemainKm      *float64 `json:"remainKm"`
			IstKmh        *float64 `json:"istKmh"`
			RadiusKm      float64  `json:"radiusKm"`
			WarAmZiel     bool     `json:"warAmZiel"`
			Heading       *float64 `json:"heading"`
			LinienKurs    *float64 `json:"linienKurs"`
			ZielAbstandKm *float64 `json:"zielAbstandKm"`
			HafenKm       float64  `json:"hafenKm"`
		} `json:"e"`
		Erwartet struct {
			Halten bool   `json:"halten"`
			Grund  string `json:"grund"`
		} `json:"erwartet"`
	} `json:"regel"`
	Szenario struct {
		Bundle Bundle `json:"bundle"`
		Punkte []struct {
			T   time.Time `json:"t"`
			Lat float64   `json:"lat"`
			Lng float64   `json:"lng"`
			Kmh *float64  `json:"kmh"`
		} `json:"punkte"`
		Schritte []struct {
			T        time.Time `json:"t"`
			From     string    `json:"from"`
			To       string    `json:"to"`
			Phase    string    `json:"phase"`
			Nachlauf bool      `json:"nachlauf"`
			Verdict  string    `json:"verdict"`
			Prognose string    `json:"prognose"`
		} `json:"schritte"`
	} `json:"szenario"`
}

func ladeAnkunftFaelle(t *testing.T) ankunftFaelle {
	raw, err := os.ReadFile("testdata/ankunft-faelle.json")
	if err != nil {
		t.Fatal(err)
	}
	var d ankunftFaelle
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func wertOK(p *float64) (float64, bool) {
	if p == nil {
		return 0, false
	}
	return *p, true
}

func TestAnkunftAbwartenRegelGleichlaufMitKAI(t *testing.T) {
	d := ladeAnkunftFaelle(t)
	if len(d.Regel) < 60 {
		t.Fatalf("zu wenige Fälle: %d", len(d.Regel))
	}
	gruende := map[string]int{}
	for i, f := range d.Regel {
		e := nachlaufEingabe{UeberSollMin: f.E.UeberSollMin, MaxMin: -1, Frisch: f.E.Frisch, RadiusKm: f.E.RadiusKm, WarAmZiel: f.E.WarAmZiel, HafenKm: f.E.HafenKm}
		if f.E.MaxMin != nil {
			e.MaxMin = *f.E.MaxMin
		}
		e.RemainKm, e.RemainOK = wertOK(f.E.RemainKm)
		e.IstKmh, e.IstOK = wertOK(f.E.IstKmh)
		e.Heading, e.HeadingOK = wertOK(f.E.Heading)
		e.LinienKurs, e.LinienKursOK = wertOK(f.E.LinienKurs)
		e.ZielAbstandKm, e.ZielAbstandOK = wertOK(f.E.ZielAbstandKm)
		halten, grund := ankunftAbwarten(e)
		if halten != f.Erwartet.Halten || grund != f.Erwartet.Grund {
			t.Errorf("Fall %d: %v/%s statt %v/%s (%+v)", i, halten, grund, f.Erwartet.Halten, f.Erwartet.Grund, f.E)
		}
		gruende[grund]++
	}
	for _, g := range []string{"soll_offen", "hoechstdauer", "ohne_gps", "rest_unbekannt", "angekommen", "war_am_ziel", "faehrt_weg", "unterwegs"} {
		if gruende[g] == 0 {
			t.Errorf("kein Fall mit Grund %q", g)
		}
	}
}

// Die Fahrt vom 24.11. im Gerät nachspielen: Positionen wie vom Bordwächter (SetPosition leitet Kurs und Tempo ab und
// führt die Spur), zu jedem Zeitpunkt computeAdvisory wie in Tick — und mit dem vergleichen, was KAI gezeigt hat.
func TestAnkunftAbwartenFahrt24November(t *testing.T) {
	d := ladeAnkunftFaelle(t)
	sz := d.Szenario
	if len(sz.Schritte) < 200 || len(sz.Bundle.Legs) < 10 {
		t.Fatalf("Szenario unvollständig: %d Schritte, %d Abschnitte", len(sz.Schritte), len(sz.Bundle.Legs))
	}
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		berlin = time.FixedZone("MEZ", 3600) // November
	}
	e := &Engine{cfg: defaultConfig()}
	k := 0
	nachlauf, abweichendeUrteile := 0, []string{}
	for i, s := range sz.Schritte {
		for ; k < len(sz.Punkte) && !sz.Punkte[k].T.After(s.T); k++ {
			p := sz.Punkte[k]
			kmh, _ := wertOK(p.Kmh)
			e.SetPosition(Position{Lat: p.Lat, Lng: p.Lng, SpeedKmh: kmh, Valid: true, TS: p.T})
		}
		now := s.T.In(berlin)
		pos := e.pos
		pos.MittelKmh, pos.MittelOK = e.mittelLocked(now)
		pos.Spur = e.spur
		st := computeAdvisory(&sz.Bundle, pos, now, 1.5)
		uhr := now.Format("15:04:05")
		if st.From != s.From || st.To != s.To || st.Phase != s.Phase || st.Nachlauf != s.Nachlauf {
			t.Errorf("%s (Schritt %d): Gerät %s → %s %s Ankunft abwarten %v — KAI %s → %s %s %v", uhr, i, st.From, st.To, st.Phase, st.Nachlauf, s.From, s.To, s.Phase, s.Nachlauf)
		}
		if st.Nachlauf {
			nachlauf++
			// solange die Fahrt bleibt: Hafeneinfahrt bzw. gleich am Ziel und die Verspätung — nie „Zu spät, auch bei Vollgas“
			if st.RestMin != 0 {
				t.Errorf("%s: Restzeit %d statt 0 (Soll-Ankunft vorbei)", uhr, st.RestMin)
			}
			if st.Verdict != "einfahrt" && st.Verdict != "ankunft" {
				t.Errorf("%s: Urteil %q während „Ankunft abwarten“", uhr, st.Verdict)
			}
			if !strings.HasSuffix(st.PrognoseText, "min zu spät") && st.PrognoseText != "gleich am Ziel" {
				t.Errorf("%s: Prognose %q während „Ankunft abwarten“", uhr, st.PrognoseText)
			}
		}
		// Urteil und Prognose rechnen Gerät und KAI je mit eigener Reststrecke (Haversine bzw. eben) und eigenem Tempo
		// (Bordwächter bzw. ship_positions, Ø der letzten Minute) — an einer Zonengrenze darf das Urteil die Nachbarzone
		// sein und die Prognose um eine Minute abweichen, mehr nicht.
		if st.Verdict != s.Verdict || st.PrognoseText != s.Prognose {
			abweichendeUrteile = append(abweichendeUrteile, uhr+" Gerät "+st.Verdict+"/"+st.PrognoseText+" KAI "+s.Verdict+"/"+s.Prognose)
			if !nachbarUrteil(st.Verdict, s.Verdict) || !prognoseNah(st.PrognoseText, s.Prognose) {
				t.Errorf("%s: Gerät %s/%q, KAI %s/%q", uhr, st.Verdict, st.PrognoseText, s.Verdict, s.Prognose)
			}
		}
	}
	if nachlauf < 20 {
		t.Errorf("nur %d Zeitpunkte mit „Ankunft abwarten“ (Soll-Ankunft 12:06, angelegt ~12:10)", nachlauf)
	}
	if len(abweichendeUrteile) > len(sz.Schritte)/20 {
		t.Errorf("%d von %d Zeitpunkten mit anderem Urteil/Prognose als KAI: %s", len(abweichendeUrteile), len(sz.Schritte), strings.Join(abweichendeUrteile, "; "))
	}
}

// nachbarUrteil: gleich oder an einer Zonengrenze die Nachbarzone (Ein-/Ausfahrt ↔ freie Strecke, Einfahrt ↔ zu spät).
func nachbarUrteil(a, b string) bool {
	if a == b {
		return true
	}
	frei := map[string]bool{"ok": true, "faster": true, "slower": true, "late": true}
	switch {
	case a == "einfahrt" && frei[b], b == "einfahrt" && frei[a]:
		return true
	case a == "ausfahrt" && frei[b], b == "ausfahrt" && frei[a]:
		return true
	case frei[a] && frei[b]:
		return true
	}
	return false
}

// prognoseNah: gleicher Text oder höchstens eine Minute Unterschied („3 min zu spät“ ↔ „2 min zu spät“, „pünktlich“ ↔ „1 min zu früh“).
func prognoseNah(a, b string) bool {
	if a == b {
		return true
	}
	ma, oka := prognoseMin(a)
	mb, okb := prognoseMin(b)
	return oka && okb && ma-mb <= 1 && mb-ma <= 1
}

func prognoseMin(s string) (int, bool) {
	if s == "pünktlich" {
		return 0, true
	}
	var n int
	if _, err := fmt.Sscanf(s, "%d min zu", &n); err != nil {
		return 0, false
	}
	switch {
	case strings.HasSuffix(s, "zu spät"):
		return -n, true
	case strings.HasSuffix(s, "zu früh"):
		return n, true
	}
	return 0, false
}

// Spur im Speicher: höchstens alle 5 s ein Punkt, beim Wechsel fahren ↔ stehen sofort; Punkte älter als 4 h fallen heraus.
func TestAnkunftSpurImSpeicher(t *testing.T) {
	e := &Engine{cfg: defaultConfig()}
	t0 := time.Date(2026, 11, 24, 12, 0, 0, 0, time.Local)
	for i := 0; i < 20; i++ { // 1-s-Takt in Fahrt → 4 Punkte (0, 5, 10, 15 s)
		e.SetPosition(Position{Lat: 47.6, Lng: 9.4 + float64(i)*0.0001, SpeedKmh: 15, Valid: true, TS: t0.Add(time.Duration(i) * time.Second)})
	}
	if len(e.spur) != 4 {
		t.Fatalf("Spur in Fahrt: %d Punkte statt 4", len(e.spur))
	}
	e.SetPosition(Position{Lat: 47.6, Lng: 9.402, SpeedKmh: 0.5, Valid: true, TS: t0.Add(20500 * time.Millisecond)}) // steht → sofort
	if len(e.spur) != 5 || e.spur[4].Kmh != 0.5 {
		t.Fatalf("Wechsel ins Stehen nicht sofort gemerkt: %d Punkte", len(e.spur))
	}
	e.SetPosition(Position{Lat: 47.6, Lng: 9.402, SpeedKmh: 0.4, Valid: true, TS: t0.Add(5 * time.Hour)})
	if len(e.spur) != 1 {
		t.Fatalf("alte Punkte nicht herausgefallen: %d", len(e.spur))
	}
}

// Ohne Feld nachlaufMaxMin im Bundle (älterer KAI, z. B. die Produktion vor 0.1.152): Vorgabe 30 min; 0 = aus.
func TestAnkunftAbwartenEinstellung(t *testing.T) {
	b := testBundle(true)
	b.Legs = append(b.Legs, Leg{From: "Romanshorn", To: "Friedrichshafen", Dep: "11:00", Arr: "11:46", Geometry: umgekehrt(b.Legs[0].Geometry), LengthKm: b.Legs[0].LengthKm})
	now := time.Date(2026, 11, 25, 10, 49, 0, 0, time.Local) // 3 min nach der Soll-Ankunft, noch 400 m vor dem Ziel
	pos := posBei(11.6, 10, now)
	st := computeAdvisory(b, pos, now, 1.5)
	if !st.Nachlauf || st.From != "Friedrichshafen" || st.Phase != "unterwegs" || st.Verdict != "einfahrt" || st.PrognoseText != "5 min zu spät" {
		t.Fatalf("ohne Feld (Vorgabe 30): %s → %s %s %v %q %q", st.From, st.To, st.Phase, st.Nachlauf, st.Verdict, st.PrognoseText)
	}
	aus := 0.0
	b.Thresholds.NachlaufMaxMin = &aus
	st = computeAdvisory(b, pos, now, 1.5)
	if st.Nachlauf || st.From != "Romanshorn" || st.Phase != "vor_abfahrt" {
		t.Fatalf("0 = aus (nach der Uhr): %s → %s %s %v", st.From, st.To, st.Phase, st.Nachlauf)
	}
	zwei := 2.0
	b.Thresholds.NachlaufMaxMin = &zwei
	if st = computeAdvisory(b, pos, now, 1.5); st.Nachlauf {
		t.Fatalf("Höchstdauer 2 min überschritten, trotzdem gehalten")
	}
	// ohne frische Position: nach der Uhr
	b.Thresholds.NachlaufMaxMin = nil
	alt := posBei(11.6, 10, now.Add(-10*time.Minute))
	if st = computeAdvisory(b, alt, now, 1.5); st.Nachlauf || st.From != "Romanshorn" {
		t.Fatalf("veraltete Position: %s %v", st.From, st.Nachlauf)
	}
	// Gerät an der Produktion (KAI ohne Fahrprofil und ohne nachlaufMaxMin): hält die Fahrt trotzdem, ohne Zonen
	// „Zu spät“ mit der Verspätung ab der Soll-Ankunft (0,4 km mit 40 km/h = 0,6 min + 3 min), nie ein Soll-Tempo
	o := testBundle(false)
	o.Legs = append(o.Legs, Leg{From: "Romanshorn", To: "Friedrichshafen", Dep: "11:00", Arr: "11:46", Geometry: umgekehrt(o.Legs[0].Geometry), LengthKm: o.Legs[0].LengthKm})
	st = computeAdvisory(o, pos, now, 1.5)
	if !st.Nachlauf || st.From != "Friedrichshafen" || st.Verdict != "late" || st.DelayMin != 4 || st.SollKmh != 0 || st.RestMin != 0 || st.PrognoseText != "5 min zu spät" {
		t.Fatalf("ohne Profil: %s %v %q +%d Soll %.1f Rest %d %q", st.From, st.Nachlauf, st.Verdict, st.DelayMin, st.SollKmh, st.RestMin, st.PrognoseText)
	}
}

func umgekehrt(g [][2]float64) [][2]float64 {
	out := make([][2]float64, len(g))
	for i := range g {
		out[i] = g[len(g)-1-i]
	}
	return out
}
