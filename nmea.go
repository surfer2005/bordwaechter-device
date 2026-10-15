package main

// GPS-Quelle "nmea": eine per USB angeschlossene GPS-Maus (u-blox-G-Mouse o. ä.). Diese Empfänger
// erscheinen unter Linux als serielles Gerät (/dev/ttyACM0 oder /dev/ttyUSB0) und senden laufend
// NMEA-0183-Sätze (RMC/GGA). Wir lesen den Strom in einer Hintergrund-Goroutine und halten den
// letzten gültigen Fix; Read() liefert ihn. Kein externes Paket: die Baudrate wird per `stty`
// gesetzt (Linux-Gerät), das Gerät dann als Datei gelesen.
//
// Die reine Satz-Zerlegung (parseNMEA / nmeaToDecimal / nmeaChecksumOK) ist ohne Hardware testbar
// (nmea_test.go).

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

type NmeaGPS struct {
	cfg  *Config
	mu   sync.Mutex
	last Position
}

func newNmeaGPS(cfg *Config) *NmeaGPS {
	g := &NmeaGPS{cfg: cfg}
	go g.reader()
	return g
}

func (g *NmeaGPS) device() (string, int) {
	dev := g.cfg.GPS.Device
	if dev == "" {
		dev = "/dev/ttyACM0"
	}
	baud := g.cfg.GPS.Baud
	if baud <= 0 {
		baud = 9600
	}
	return dev, baud
}

// reader: Gerät öffnen, NMEA-Zeilen lesen, letzten gültigen Fix merken; bei Fehler neu versuchen.
func (g *NmeaGPS) reader() {
	for {
		dev, baud := g.device()
		// Baudrate/Rohmodus setzen (best effort; schlägt es fehl, versuchen wir trotzdem zu lesen).
		_ = exec.Command("stty", "-F", dev, strconv.Itoa(baud), "raw", "-echo", "-echoe", "-echok", "clocal", "cs8", "-cstopb", "-parenb").Run()
		f, err := os.OpenFile(dev, os.O_RDONLY, 0)
		if err != nil {
			time.Sleep(5 * time.Second)
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimRight(sc.Text(), "\r")
			if p, ok := parseNMEA(line); ok {
				p.TS = time.Now()
				g.mu.Lock()
				g.last = p
				g.mu.Unlock()
			}
		}
		f.Close()
		time.Sleep(2 * time.Second) // Gerät weg/EOF → kurz warten, neu öffnen
	}
}

func (g *NmeaGPS) Read() (Position, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.last.Valid {
		return Position{}, fmt.Errorf("noch keine NMEA-Position (GPS-Maus ohne Fix?)")
	}
	return g.last, nil
}

// --- reine, testbare NMEA-Zerlegung ------------------------------------------

// nmeaChecksumOK prüft die Prüfsumme (XOR zwischen '$' und '*'). Ohne '*' gilt der Satz als ok
// (manche Geräte/Logs lassen sie weg).
func nmeaChecksumOK(line string) bool {
	if len(line) == 0 || line[0] != '$' {
		return false
	}
	star := strings.IndexByte(line, '*')
	if star < 0 {
		return true
	}
	var cs byte
	for i := 1; i < star; i++ {
		cs ^= line[i]
	}
	want, err := strconv.ParseUint(strings.TrimSpace(line[star+1:]), 16, 8)
	if err != nil {
		return false
	}
	return byte(want) == cs
}

// nmeaToDecimal wandelt "ddmm.mmmm"/"dddmm.mmmm" + Hemisphäre in Dezimalgrad.
func nmeaToDecimal(v, hemi string) (float64, bool) {
	if v == "" {
		return 0, false
	}
	dot := strings.IndexByte(v, '.')
	if dot < 3 {
		return 0, false
	}
	degEnd := dot - 2 // die letzten 2 Vorkommastellen sind Minuten
	deg, err1 := strconv.ParseFloat(v[:degEnd], 64)
	min, err2 := strconv.ParseFloat(v[degEnd:], 64)
	if err1 != nil || err2 != nil {
		return 0, false
	}
	d := deg + min/60.0
	if hemi == "S" || hemi == "W" {
		d = -d
	}
	return d, true
}

// parseNMEA zerlegt einen RMC- oder GGA-Satz zu einer Position. RMC liefert zusätzlich
// Geschwindigkeit (Knoten→km/h) und Kurs; GGA nur die Position.
func parseNMEA(line string) (Position, bool) {
	if !nmeaChecksumOK(line) {
		return Position{}, false
	}
	body := line
	if star := strings.IndexByte(body, '*'); star >= 0 {
		body = body[:star]
	}
	f := strings.Split(body, ",")
	if len(f) == 0 {
		return Position{}, false
	}
	typ := f[0]
	if len(typ) >= 6 && typ[3:6] == "RMC" { // $GPRMC / $GNRMC …
		if len(f) < 9 || f[2] != "A" {      // A = gültig
			return Position{}, false
		}
		lat, ok1 := nmeaToDecimal(f[3], f[4])
		lng, ok2 := nmeaToDecimal(f[5], f[6])
		if !ok1 || !ok2 {
			return Position{}, false
		}
		p := Position{Lat: lat, Lng: lng, Valid: true}
		if kn, err := strconv.ParseFloat(f[7], 64); err == nil {
			p.SpeedKmh = kn * 1.852
		}
		if hd, err := strconv.ParseFloat(f[8], 64); err == nil {
			p.Heading = hd
		}
		return p, true
	}
	if len(typ) >= 6 && typ[3:6] == "GGA" {
		if len(f) < 7 || f[6] == "0" || f[6] == "" { // Fix-Qualität 0 = kein Fix
			return Position{}, false
		}
		lat, ok1 := nmeaToDecimal(f[2], f[3])
		lng, ok2 := nmeaToDecimal(f[4], f[5])
		if !ok1 || !ok2 {
			return Position{}, false
		}
		return Position{Lat: lat, Lng: lng, Valid: true}, true
	}
	return Position{}, false
}
