package main

import (
	"testing"
	"time"
)

// Der Teltonika hält die letzte Koordinate auch ohne GPS-Fix (KS-CONSTANZE stand am 18.11.2026 seit 5 Tagen auf dem
// Fix vom 13.11.). fixAlter vergleicht Fix-Zeit und Router-Uhr — beide Texte so, wie der Router sie liefert.
func TestFixAlter(t *testing.T) {
	a, ok := fixAlter("2026-11-18 19:55:56", "2026-11-18T19:55:57+0000")
	if !ok || a != time.Second {
		t.Fatalf("aktueller Fix: erwartet 1 s, bekommen %v %v", a, ok)
	}
	a, ok = fixAlter("2026-11-13 18:45:26", "2026-11-18T19:56:04+0000")
	if !ok || a <= gpsFixMaxAlter {
		t.Fatalf("alter Fix: erwartet > %v, bekommen %v %v", gpsFixMaxAlter, a, ok)
	}
	if a != 436238*time.Second {
		t.Fatalf("alter Fix: erwartet 436238 s, bekommen %v", a)
	}
	for _, f := range [][2]string{{"", "2026-11-18T19:55:57+0000"}, {"2026-11-18 19:55:56", ""}, {"kaputt", "2026-11-18T19:55:57+0000"}} {
		if _, ok := fixAlter(f[0], f[1]); ok {
			t.Fatalf("unlesbar %q/%q: erwartet keine Aussage", f[0], f[1])
		}
	}
	// Grenze: genau 300 s gilt noch
	a, _ = fixAlter("2026-11-18 19:50:57", "2026-11-18T19:55:57+0000")
	if a > gpsFixMaxAlter {
		t.Fatalf("genau an der Grenze darf nicht veraltet sein: %v", a)
	}
}
