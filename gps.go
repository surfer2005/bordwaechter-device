package main

import (
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"time"
)

type Position struct {
	Lat, Lng float64
	SpeedKmh float64
	Heading  float64
	Valid    bool
	TS       time.Time
	// Ø-Geschwindigkeit der letzten Minute (setzt die Engine vor der Rechnung; für das Fahrprofil, ab 0.1.151)
	MittelKmh float64
	MittelOK  bool
}

type GPSSource interface {
	Read() (Position, error)
}

// ManualGPS: Position wird über die lokale UI gesetzt — für Test/Inbetriebnahme ohne Hardware.
type ManualGPS struct {
	mu sync.Mutex
	p  Position
}

func (m *ManualGPS) Set(p Position) {
	m.mu.Lock()
	p.Valid = true
	p.TS = time.Now()
	m.p = p
	m.mu.Unlock()
}

func (m *ManualGPS) Read() (Position, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.p.Valid {
		return Position{}, fmt.Errorf("keine manuelle Position gesetzt")
	}
	return m.p, nil
}

// TeltonikaGPS: liest Lat/Lng (je 2 Register) per Modbus-TCP. Speed wird im Engine aus
// aufeinanderfolgenden Positionen abgeleitet (robuster als ein unsicher skaliertes Speed-Register).
type TeltonikaGPS struct{ cfg *Config }

func regsToFloat(regs []uint16, format string) float64 {
	if len(regs) < 2 {
		return 0
	}
	raw := uint32(regs[0])<<16 | uint32(regs[1])
	switch format {
	case "int32_1e7":
		return float64(int32(raw)) / 1e7
	case "int32_1e6":
		return float64(int32(raw)) / 1e6
	default: // float32 (IEEE754, High-Word zuerst)
		return float64(math.Float32frombits(raw))
	}
}

func (t *TeltonikaGPS) Read() (Position, error) {
	g := t.cfg.GPS
	to := 5 * time.Second
	latR, err := modbusReadHolding(g.Host, g.Port, g.Unit, g.LatReg, 2, to)
	if err != nil {
		return Position{}, err
	}
	lngR, err := modbusReadHolding(g.Host, g.Port, g.Unit, g.LngReg, 2, to)
	if err != nil {
		return Position{}, err
	}
	if alter, fixText, ok := teltonikaFixAlter(g.Host, g.Port, g.Unit, to); ok && alter > gpsFixMaxAlter {
		gpsFixMelden(true, fixText)
		return Position{}, fmt.Errorf("GPS ohne aktuellen Fix seit %s UTC", fixText)
	}
	gpsFixMelden(false, "")
	p := Position{Lat: regsToFloat(latR, g.Format), Lng: regsToFloat(lngR, g.Format), Valid: true, TS: time.Now()}
	// Heading/Speed wie Lat/Lng als float32 (2 Register) im selben Format lesen, wenn ein Register
	// gesetzt ist. ⚠️ Einheit/Skalierung an der echten Teltonika prüfen (Speed km/h vs. Knoten).
	// Ist kein Speed-Register gesetzt (-1), bleibt Speed 0 und wird im Engine aus der Bewegung abgeleitet.
	if g.HeadingReg >= 0 {
		if hr, err := modbusReadHolding(g.Host, g.Port, g.Unit, g.HeadingReg, 2, to); err == nil && len(hr) >= 2 {
			p.Heading = regsToFloat(hr, g.Format)
		}
	}
	if g.SpeedReg >= 0 {
		if sr, err := modbusReadHolding(g.Host, g.Port, g.Unit, g.SpeedReg, 2, to); err == nil && len(sr) >= 2 {
			p.SpeedKmh = regsToFloat(sr, g.Format)
		}
	}
	return p, nil
}

// Fix-Alter (ab 0.1.142): Der Teltonika hält die letzte Koordinate in seinen Registern, auch wenn er seit Tagen
// keinen Fix mehr hat (Prüfung 18.11.2026: KS-CONSTANZE seit 13.11., MS-SCHWABEN seit 12.11. — die Schwaben fuhr am
// 14.11. laut Logmap 39 km, in KAI stand sie den ganzen Tag still). Deshalb die Fix-Zeit (Register 147,
// „JJJJ-MM-TT hh:mm:ss“, UTC) gegen die Uhr des Routers (Register 378, „JJJJ-MM-TTThh:mm:ss+0000“) prüfen — beide
// kommen vom Router, die Uhr des Pi spielt keine Rolle. Älter als gpsFixMaxAlter → keine Position (wie ein Lesefehler:
// SIFA und Anzeige sehen die Position veralten). Register nicht lesbar oder Text unbekannt → keine Aussage, die
// Position gilt wie bisher (andere Router, ältere Firmware). Dieselbe Grenze prüft KAI beim Eingang (gps-teltonika.js).
const (
	tRegFixZeit    = 147
	tRegRouterUhr  = 378
	gpsFixMaxAlter = 300 * time.Second
)

// teltonikaZeit: Zeittext des Routers → UTC. Ohne Zonenangabe gilt UTC (so liefert der Router die Fix-Zeit).
func teltonikaZeit(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	for _, f := range []string{"2006-01-02 15:04:05", "2006-01-02T15:04:05-0700", time.RFC3339, "2006-01-02T15:04:05"} {
		if t, err := time.Parse(f, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// fixAlter (rein, testbar): Alter des Fixes aus Fix-Zeit und Router-Uhr; ok=false = keine Aussage.
func fixAlter(fixText, uhrText string) (time.Duration, bool) {
	fix, ok1 := teltonikaZeit(fixText)
	uhr, ok2 := teltonikaZeit(uhrText)
	if !ok1 || !ok2 {
		return 0, false
	}
	return uhr.Sub(fix), true
}

func teltonikaFixAlter(host string, port, unit int, to time.Duration) (time.Duration, string, bool) {
	fr, err := modbusReadHolding(host, port, unit, tRegFixZeit, 16, to)
	if err != nil {
		return 0, "", false
	}
	ur, err := modbusReadHolding(host, port, unit, tRegRouterUhr, 12, to)
	if err != nil {
		return 0, "", false
	}
	fixText := regsToString(fr)
	alter, ok := fixAlter(fixText, regsToString(ur))
	return alter, fixText, ok
}

// gpsOhneFix merkt sich den letzten Stand — nur für eine Log-Zeile beim Wechsel, nicht bei jeder Abfrage.
var (
	gpsOhneFixMu sync.Mutex
	gpsOhneFix   bool
)

func gpsFixMelden(ohne bool, fixText string) {
	gpsOhneFixMu.Lock()
	defer gpsOhneFixMu.Unlock()
	if ohne && !gpsOhneFix {
		log.Printf("gps: Router ohne aktuellen GPS-Fix (letzter Fix %s UTC) — Position wird nicht übernommen, bis der Fix wieder aktuell ist", fixText)
	} else if !ohne && gpsOhneFix {
		log.Printf("gps: GPS-Fix wieder aktuell")
	}
	gpsOhneFix = ohne
}

func buildGPS(cfg *Config, manual *ManualGPS) GPSSource {
	switch cfg.GPS.Source {
	case "teltonika":
		return &TeltonikaGPS{cfg: cfg}
	case "nmea":
		return newNmeaGPS(cfg)
	}
	return manual
}
