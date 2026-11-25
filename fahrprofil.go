package main

// Fahrprofil je Schiff und Strecke (ab 0.1.151, Peter 2026-11-25, Beobachtung an Bord der MS-LINDAU im Ersatzverkehr
// Friedrichshafen ↔ Romanshorn): eine Fahrt hat drei Teile — Hafenausfahrt (vom Steg, bis das Schiff Reisetempo
// erreicht) · freie Strecke · Hafeneinfahrt (ab dem Punkt, an dem es vor dem Ziel das Tempo zurücknimmt). Bisher wurde
// mit voller Fahrt bis zum Stillstand gerechnet; in der ersten Minute nach dem Ablegen lag die Prognose dadurch im
// Median 91 min daneben (Gerät), mit Profil +2 min.
//
// KAI lernt das Profil aus den Positionen und schickt es im Bundle je Abschnitt mit (Leg.Profil, schon „wirksam“:
// Übersteuerung von Hand vor gelerntem Wert). Fehlt es (älterer KAI-Stand, z. B. die Produktion), rechnet das Gerät wie
// bisher — nur mit der Sicherung am Steg (keine Hochrechnung aus dem Stillstand, „Abfahrt überfällig seit N min“).
//
// Dieselbe Rechnung steht in KAI (server/src/fahrprofil.js tempoBewertung/profilRechnung/prognoseAnzeige). Die Fälle in
// testdata/fahrprofil-faelle.json sind dort erzeugt — fahrprofil_test.go prüft, dass beide Seiten gleich rechnen.

import (
	"fmt"
	"math"
	"time"
)

// LegProfil: Fahrprofil eines Abschnitts aus dem Bundle (server/src/kiosk-gps.js edgeBundle()).
type LegProfil struct {
	AusfahrtM   float64 `json:"ausfahrt_m"`   // Ausfahrt endet so viele Meter nach dem Ablegen
	AusfahrtKmh float64 `json:"ausfahrt_kmh"` // Ø-Tempo der Ausfahrt
	EinfahrtM   float64 `json:"einfahrt_m"`   // Einfahrt beginnt so viele Meter vor dem Ziel
	EinfahrtKmh float64 `json:"einfahrt_kmh"` // Ø-Tempo der Einfahrt
	ReiseKmh    float64 `json:"reise_kmh"`    // Reisetempo auf der freien Strecke
	HoechstKmh  float64 `json:"hoechst_kmh"`  // Höchsttempo des Schiffs (für „Zu spät“)
	RestAbKm    float64 `json:"rest_ab_km"`   // Reststrecke auf der Fahrrinne am üblichen Liegeplatz
	Fahrten     int     `json:"fahrten"`
	Quelle      string  `json:"quelle"` // schiff | alle
}

// restAb: Reststrecke am üblichen Liegeplatz, sonst die Länge der Fahrrinne. ok=false → Profil nicht verwenden.
func (p *LegProfil) grenzen(lengthKm float64) (remExit, remEntry float64, ok bool) {
	if p == nil || !(p.AusfahrtKmh > 0) || !(p.EinfahrtKmh > 0) || !(p.ReiseKmh > 0) || !(p.HoechstKmh > 0) || p.AusfahrtM < 0 || p.EinfahrtM < 0 {
		return 0, 0, false
	}
	restAb := p.RestAbKm
	if !(restAb > 0) {
		restAb = lengthKm
	}
	if !(restAb > 0) {
		return 0, 0, false
	}
	remExit = restAb - p.AusfahrtM/1000
	remEntry = p.EinfahrtM / 1000
	if remExit <= remEntry+0.1 {
		return 0, 0, false // Hafenzonen überlappen → nicht rechnen
	}
	return remExit, remEntry, true
}

type tempoEingabe struct {
	RemainKm        float64
	RestMin         float64 // ≥ 0,5 (Soll-Ankunft − jetzt)
	RestMinRoh      float64 // ungeklammert, für die Prognose
	RestMinRohOK    bool
	IstKmh          float64
	IstOK           bool
	MittelKmh       float64 // Ø der letzten Minute
	MittelOK        bool
	Wartet          bool // steht/manövriert am Abfahrtssteg, Soll-Abfahrt vorbei
	Profil          *LegProfil
	LengthKm        float64
	Tol             float64
	MaxKmh          float64 // Rückfall „realistisches Maxtempo“ aus KAI
	RampeM          float64
	AnkunftRadiusKm float64
}

type tempoErgebnis struct {
	Verdict     string // ok | faster | slower | late | ausfahrt | einfahrt | ankunft | angekommen | none
	Diff        float64
	DiffOK      bool // Diff = Soll − Ist (wie in KAI)
	SollKmh     float64
	SollOK      bool
	DelayMin    int
	DelayOK     bool
	BufferMin   float64
	BufferOK    bool
	Zone        string // ausfahrt | frei | einfahrt (nur mit Profil)
	Profil      bool
	RestzeitMin float64
	RestzeitOK  bool
	VMax        float64
}

// jsRound rundet wie Math.round in JavaScript (x,5 immer nach oben) — sonst wichen Gerät und KAI bei negativen
// halben Minuten voneinander ab.
func jsRound(x float64) float64 { return math.Floor(x + 0.5) }

// restBezug: Bezug für die Verspätung „Zu spät +N min“ — die ungeklammerte Restzeit, sobald sie unter der geklammerten
// liegt (kurz vor bzw. nach der Soll-Ankunft); sonst die geklammerte (≥ 0,5 min). Wie KAI (fahrprofil.js restBezug).
func restBezug(e tempoEingabe) float64 {
	if e.RestMinRohOK && e.RestMinRoh < e.RestMin {
		return e.RestMinRoh
	}
	return e.RestMin
}

// tempoBewertung: EIN Rechenweg wie in KAI. Ohne Profil die bisherige Rechnung (volle Fahrt bis zum Steg), mit Profil
// die Hafenzonen mit ihrem gelernten Tempo. Am Steg wartend: keine Empfehlung und keine Hochrechnung.
func tempoBewertung(e tempoEingabe) tempoErgebnis {
	out := tempoErgebnis{Verdict: "none"}
	if e.Wartet {
		return out
	}
	tol := e.Tol
	if !(tol > 0) {
		tol = 1.5
	}
	maxKmh := e.MaxKmh
	if !(maxKmh > 0) {
		maxKmh = 40
	}
	out.VMax = maxKmh
	ar := e.AnkunftRadiusKm
	if !(ar > 0) {
		ar = 0.15
	}
	if e.RemainKm <= ar {
		if e.IstOK && e.IstKmh <= 2 {
			out.Verdict = "angekommen"
		} else {
			out.Verdict = "ankunft"
		}
		return out
	}
	if remExit, remEntry, ok := e.Profil.grenzen(e.LengthKm); ok {
		return profilRechnung(e, remExit, remEntry, tol, maxKmh)
	}
	// bisherige Rechnung
	if e.RestMin > 0 {
		tgt := e.RemainKm / (e.RestMin / 60)
		// Soll-Ankunft vorbei (Ankunft abwarten, ab 0.1.152): kein Soll-Tempo mehr — aus der geklammerten Restzeit
		// (0,5 min) käme kurz vor dem Ziel sonst „Schneller, 36 km/h“. Wie KAI.
		vorbei := e.RestMinRohOK && e.RestMinRoh <= 0
		if tgt > maxKmh || vorbei {
			out.Verdict = "late"
			d := jsRound(e.RemainKm/maxKmh*60 - restBezug(e))
			if d < 0 {
				d = 0
			}
			out.DelayMin, out.DelayOK = int(d), true
		} else {
			out.SollKmh, out.SollOK = tgt, true
			if e.IstOK {
				out.Diff, out.DiffOK = tgt-e.IstKmh, true
				switch {
				case math.Abs(out.Diff) <= tol:
					out.Verdict = "ok"
				case out.Diff > 0:
					out.Verdict = "faster"
				default:
					out.Verdict = "slower"
				}
			}
		}
	}
	if e.IstOK && e.IstKmh > 1 && e.RestMinRohOK {
		out.RestzeitMin, out.RestzeitOK = e.RemainKm/e.IstKmh*60, true
		out.BufferMin, out.BufferOK = e.RestMinRoh-out.RestzeitMin, true
	}
	return out
}

// profilRechnung: Ausfahrt mit Ausfahrt-Tempo + freie Strecke mit Reisetempo + Einfahrt mit Einfahrt-Tempo. Soll-Tempo
// und „schneller/langsamer“ NUR auf der freien Strecke; in der Hafenzone keine Tempo-Empfehlung. „Zu spät“ mit dem
// Höchsttempo des Schiffs. Das Reisetempo ist zuerst das typische und gleitet nach dem Ende der Ausfahrt über RampeM
// Meter auf das aktuelle (Ø der letzten Minute) — so springt die Prognose am Übergang nicht.
func profilRechnung(e tempoEingabe, remExit, remEntry, tol, maxKmh float64) tempoErgebnis {
	p := e.Profil
	out := tempoErgebnis{Verdict: "none", Profil: true}
	R := e.RemainKm
	ausTeil := math.Max(0, R-remExit)
	freiTeil := math.Max(0, math.Min(R, remExit)-remEntry)
	einTeil := math.Min(R, remEntry)
	switch {
	case R > remExit:
		out.Zone = "ausfahrt"
	case R > remEntry:
		out.Zone = "frei"
	default:
		out.Zone = "einfahrt"
	}
	vMax := p.HoechstKmh
	if !(vMax > 0) {
		vMax = maxKmh
	}
	out.VMax = vMax
	rampeKm := 0.5
	if e.RampeM > 0 {
		rampeKm = e.RampeM / 1000
	}
	if rampeKm < 0.001 {
		rampeKm = 0.001
	}
	vFrei := p.ReiseKmh
	if out.Zone == "frei" && (e.MittelOK || e.IstOK) {
		akt := e.IstKmh
		if e.MittelOK {
			akt = e.MittelKmh
		}
		w := math.Max(0, math.Min(1, (remExit-R)/rampeKm))
		vFrei = p.ReiseKmh + w*(akt-p.ReiseKmh)
	}
	tHafen := (ausTeil/p.AusfahrtKmh + einTeil/p.EinfahrtKmh) * 60
	if freiTeil <= 0 {
		out.RestzeitMin, out.RestzeitOK = tHafen, true
	} else if vFrei > 1 {
		out.RestzeitMin, out.RestzeitOK = tHafen+freiTeil/vFrei*60, true
	}
	tMin := tHafen + freiTeil/vMax*60
	// In der Hafeneinfahrt gibt es nichts mehr aufzuholen — dort bleibt es „Hafeneinfahrt“, auch wenn das Schiff zu spät
	// ist; die Verspätung steht in der Prognose (ab 0.1.152, wie KAI). Die Verspätung bei Höchsttempo zählt ab der
	// Soll-Ankunft (ungeklammert).
	if tMin > e.RestMin && out.Zone != "einfahrt" {
		out.Verdict = "late"
		d := jsRound(tMin - restBezug(e))
		if d < 0 {
			d = 0
		}
		out.DelayMin, out.DelayOK = int(d), true
	} else {
		avail := e.RestMin - tHafen
		sollFrei, sollOK := 0.0, false
		if freiTeil > 0 && avail > 0 {
			sollFrei, sollOK = freiTeil/avail*60, true
		}
		switch out.Zone {
		case "ausfahrt":
			out.Verdict = "ausfahrt"
			out.SollKmh, out.SollOK = sollFrei, sollOK // „danach … km/h“
		case "einfahrt":
			out.Verdict = "einfahrt"
		default:
			out.SollKmh, out.SollOK = sollFrei, sollOK
			if sollOK && e.IstOK {
				out.Diff, out.DiffOK = sollFrei-e.IstKmh, true
				switch {
				case math.Abs(out.Diff) <= tol:
					out.Verdict = "ok"
				case out.Diff > 0:
					out.Verdict = "faster"
				default:
					out.Verdict = "slower"
				}
			}
		}
	}
	if out.RestzeitOK && e.RestMinRohOK {
		out.BufferMin, out.BufferOK = e.RestMinRoh-out.RestzeitMin, true
	}
	return out
}

// ueberfaelligText: Hinweis, solange das Schiff nach der Soll-Abfahrt noch am Steg wartet (Wortlaut wie KAI).
func ueberfaelligText(min float64) string {
	if min >= 1 {
		return fmt.Sprintf("Abfahrt überfällig seit %d min", int(math.Floor(min)))
	}
	return "Abfahrt fällig"
}

// prognoseAnzeige: Text + Ton der Kachel „Prognose“ — derselbe Wortlaut wie in KAI (fahrprofil.js prognoseAnzeige).
// Die Anzeigen am Gerät rechnen nichts mehr selbst, sie zeigen nur an.
func prognoseAnzeige(verdict string, buf float64, bufOK bool, ueberMin float64, ueberOK bool) (string, string) {
	switch verdict {
	case "angekommen":
		return "am Ziel", "ok"
	case "ankunft":
		return "gleich am Ziel", "ok"
	}
	if ueberOK {
		if ueberMin >= 1 {
			return fmt.Sprintf("überfällig seit %d min", int(math.Floor(ueberMin))), "late"
		}
		return "Abfahrt fällig", "late"
	}
	if !bufOK {
		return "—", "dim"
	}
	b := int(jsRound(buf))
	switch {
	case b == 0:
		return "pünktlich", "ok"
	case b > 0:
		return fmt.Sprintf("%d min zu früh", b), "early"
	default:
		return fmt.Sprintf("%d min zu spät", -b), "late"
	}
}

// ---- Ankunft abwarten (ab 0.1.152, Peter 2026-11-25) --------------------------------------------------------------------
// Echte Fahrt der MS-LINDAU am 24.11. (11:20 Friedrichshafen → Romanshorn, Soll-Ankunft 12:06, angelegt 12:10): bisher
// entschied allein die Uhr, welche Fahrt gilt — ab der Soll-Ankunft zeigte der Pilot schon die nächste Abfahrt, obwohl
// das Schiff noch einfuhr. Jetzt bleibt eine Fahrt nach ihrer Soll-Ankunft die laufende, solange das Schiff laut frischer
// Position nicht angekommen ist (Reststrecke ≤ Ankunftsradius und ≤ 2 km/h, wie „Am Ziel“). Einmal angekommen bleibt
// angekommen — dafür die Spur im Speicher (Engine.spur). Ohne frische Position nach der Uhr; höchstens NachlaufMaxMin
// Minuten (KAI-Einstellung, Vorgabe 30, 0 = aus); fährt das Schiff erkennbar vom Ziel weg, gilt die Uhr.
// Dieselbe Regel wie KAI (server/src/fahrprofil.js ankunftAbwarten) — Fälle in testdata/fahrprofil-faelle.json.
const (
	nachlaufStandKmh      = 2.0   // „steht“ — wie das Urteil „Am Ziel“
	nachlaufWegKmh        = 3.0   // ab diesem Tempo zählt der Kurs
	nachlaufGegenGrad     = 110.0 // Kurs so weit gegen die Fahrrinne = fährt vom Ziel weg
	nachlaufMaxMinVorgabe = 30.0
)

func winkelAbstand(a, b float64) float64 {
	d := math.Mod(math.Abs(a-b), 360)
	if d > 180 {
		return 360 - d
	}
	return d
}

type nachlaufEingabe struct {
	UeberSollMin  float64 // jetzt − Soll-Ankunft
	MaxMin        float64 // < 0 = Vorgabe
	Frisch        bool
	RemainKm      float64
	RemainOK      bool
	IstKmh        float64
	IstOK         bool
	RadiusKm      float64
	WarAmZiel     bool
	Heading       float64
	HeadingOK     bool
	LinienKurs    float64
	LinienKursOK  bool
	ZielAbstandKm float64
	ZielAbstandOK bool
	HafenKm       float64
}

// ankunftAbwarten: bleibt der Abschnitt nach seiner Soll-Ankunft der laufende? Grund wie in KAI:
// soll_offen | hoechstdauer | ohne_gps | rest_unbekannt | angekommen | war_am_ziel | faehrt_weg | unterwegs (= halten)
func ankunftAbwarten(e nachlaufEingabe) (bool, string) {
	maxMin := e.MaxMin
	if maxMin < 0 {
		maxMin = nachlaufMaxMinVorgabe
	}
	if !(e.UeberSollMin > 0) {
		return false, "soll_offen"
	}
	if e.UeberSollMin > maxMin {
		return false, "hoechstdauer"
	}
	if !e.Frisch {
		return false, "ohne_gps"
	}
	if !e.RemainOK {
		return false, "rest_unbekannt"
	}
	r := e.RadiusKm
	if !(r > 0) {
		r = 0.15
	}
	if e.RemainKm <= r && e.IstOK && e.IstKmh <= nachlaufStandKmh {
		return false, "angekommen"
	}
	if e.WarAmZiel {
		return false, "war_am_ziel"
	}
	hafenKm := e.HafenKm
	if !(hafenKm > 0) {
		hafenKm = 0.5
	}
	if e.IstOK && e.IstKmh > nachlaufWegKmh && e.HeadingOK && e.LinienKursOK && e.ZielAbstandOK &&
		e.ZielAbstandKm > hafenKm && winkelAbstand(e.Heading, e.LinienKurs) > nachlaufGegenGrad {
		return false, "faehrt_weg"
	}
	return true, "unterwegs"
}

// spurPunkt: eine Position der Spur im Speicher (Engine.spur) — für „hat seit der Soll-Abfahrt am Ziel gestanden“.
type spurPunkt struct {
	TS       time.Time
	Lat, Lng float64
	Kmh      float64
}

// amZielGewesen: hat das Schiff seit ab am Ziel der Fahrrinne gestanden (Reststrecke ≤ Radius, ≤ 2 km/h)?
func amZielGewesen(spur []spurPunkt, linie [][2]float64, ab time.Time, radiusKm float64) bool {
	if len(linie) < 2 {
		return false
	}
	r := radiusKm
	if !(r > 0) {
		r = 0.15
	}
	for i := len(spur) - 1; i >= 0; i-- {
		p := spur[i]
		if p.TS.Before(ab) {
			break // Spur ist zeitlich sortiert
		}
		if p.Kmh > nachlaufStandKmh {
			continue
		}
		if pr := projectOntoPolyline(linie, p.Lat, p.Lng); pr.OK && pr.RemainKm <= r {
			return true
		}
	}
	return false
}
