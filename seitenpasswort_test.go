package main

import "testing"

// Passwort der Geräteseite aus dem Herzschlag (Peter 2026-11-27): nur ein gesetztes, abweichendes übernehmen.
func TestSeitenPasswortNeu(t *testing.T) {
	faelle := []struct {
		ist, kai, erwartet string
		aendern            bool
	}{
		{"", "", "", false},                       // KAI ohne Passwort: Seite bleibt, wie sie ist
		{"alt", "", "alt", false},                 // ältere KAI schickt nichts: nichts löschen
		{"", "ZentralFuerAlle9", "ZentralFuerAlle9", true},
		{"ZentralFuerAlle9", "ZentralFuerAlle9", "ZentralFuerAlle9", false},
		{"alt", "  neu12345678  ", "neu12345678", true},
	}
	for _, f := range faelle {
		pw, ok := seitenPasswortNeu(f.ist, f.kai)
		if pw != f.erwartet || ok != f.aendern {
			t.Errorf("seitenPasswortNeu(%q, %q) = %q, %v; erwartet %q, %v", f.ist, f.kai, pw, ok, f.erwartet, f.aendern)
		}
	}
}
