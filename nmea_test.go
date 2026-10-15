package main

import (
	"math"
	"testing"
)

func approx(a, b, eps float64) bool { return math.Abs(a-b) < eps }

func TestNmeaToDecimal(t *testing.T) {
	// 4807.038 N → 48 + 7.038/60 = 48.1173
	d, ok := nmeaToDecimal("4807.038", "N")
	if !ok || !approx(d, 48.1173, 1e-4) {
		t.Fatalf("lat falsch: %v ok=%v", d, ok)
	}
	// 01131.000 E → 11 + 31/60 = 11.51667
	d, ok = nmeaToDecimal("01131.000", "E")
	if !ok || !approx(d, 11.51667, 1e-4) {
		t.Fatalf("lng falsch: %v ok=%v", d, ok)
	}
	// Süd/West negativ
	d, _ = nmeaToDecimal("4807.038", "S")
	if d >= 0 {
		t.Fatalf("Süd sollte negativ sein: %v", d)
	}
}

func TestNmeaChecksum(t *testing.T) {
	ok := "$GPRMC,123519,A,4807.038,N,01131.000,E,022.4,084.4,230394,003.1,W*6A"
	if !nmeaChecksumOK(ok) {
		t.Fatalf("gültige Prüfsumme als ungültig erkannt")
	}
	bad := "$GPRMC,123519,A,4807.038,N,01131.000,E,022.4,084.4,230394,003.1,W*00"
	if nmeaChecksumOK(bad) {
		t.Fatalf("falsche Prüfsumme als gültig erkannt")
	}
	// ohne '*' gilt als ok
	if !nmeaChecksumOK("$GPRMC,123519,A,4807.038,N,01131.000,E,022.4,084.4") {
		t.Fatalf("Satz ohne Prüfsumme sollte toleriert werden")
	}
}

func TestParseRMC(t *testing.T) {
	p, ok := parseNMEA("$GPRMC,123519,A,4807.038,N,01131.000,E,022.4,084.4,230394,003.1,W*6A")
	if !ok || !p.Valid {
		t.Fatalf("RMC nicht geparst")
	}
	if !approx(p.Lat, 48.1173, 1e-4) || !approx(p.Lng, 11.51667, 1e-4) {
		t.Fatalf("Position falsch: %v/%v", p.Lat, p.Lng)
	}
	if !approx(p.SpeedKmh, 22.4*1.852, 1e-3) {
		t.Fatalf("Speed falsch: %v", p.SpeedKmh)
	}
	if !approx(p.Heading, 84.4, 1e-3) {
		t.Fatalf("Heading falsch: %v", p.Heading)
	}
}

func TestParseRMCVoid(t *testing.T) {
	// Status V = ungültig → kein Fix
	if _, ok := parseNMEA("$GPRMC,123519,V,4807.038,N,01131.000,E,0.0,0.0,230394,,*17"); ok {
		t.Fatalf("ungültiger RMC (V) darf keinen Fix liefern")
	}
}

func TestParseGGA(t *testing.T) {
	p, ok := parseNMEA("$GPGGA,123519,4807.038,N,01131.000,E,1,08,0.9,545.4,M,46.9,M,,*47")
	if !ok || !p.Valid || !approx(p.Lat, 48.1173, 1e-4) {
		t.Fatalf("GGA nicht geparst: ok=%v p=%+v", ok, p)
	}
	// Fix-Qualität 0 → kein Fix
	if _, ok := parseNMEA("$GPGGA,123519,4807.038,N,01131.000,E,0,00,,,M,,M,,*66"); ok {
		t.Fatalf("GGA ohne Fix (0) darf keinen Fix liefern")
	}
}
