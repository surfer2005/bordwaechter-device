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
		if tgt > maxKmh {
			out.Verdict = "late"
			d := jsRound(e.RemainKm/maxKmh*60 - e.RestMin)
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
	if tMin > e.RestMin {
		out.Verdict = "late"
		d := jsRound(tMin - e.RestMin)
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
