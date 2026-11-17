package main

import (
	"testing"
	"time"
)

func merkeAirplay(t *testing.T) {
	alt, altBis, altRef, altLA, altLR := apSoll, apBis, apRef, apLokalAus, apLokalRef
	t.Cleanup(func() { apSoll, apBis, apRef, apLokalAus, apLokalRef = alt, altBis, altRef, altLA, altLR })
	apSoll, apBis, apRef, apLokalAus, apLokalRef = nil, time.Time{}, "", false, ""
}

func zeiger(b bool) *bool { return &b }

// AirPlay-Empfänger nur, solange in KAI als Quelle gewählt (Dummy Medienplayer-Bedienung v4, ab 0.1.135).
func TestAirplayGewaehlt(t *testing.T) {
	merkeAirplay(t)
	jetzt := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

	if !airplayGewaehlt(jetzt) {
		t.Fatal("ältere KAI ohne Soll-Angabe: Empfänger läuft wie bisher, solange erlaubt")
	}
	apSoll = zeiger(false)
	if airplayGewaehlt(jetzt) {
		t.Fatal("nicht als Quelle gewählt: Empfänger aus")
	}
	apSoll, apBis = zeiger(true), time.Time{}
	if !airplayGewaehlt(jetzt) {
		t.Fatal("gewählt ohne Ende (bis „Zurück zum Zeitplan“): an")
	}
	apBis = jetzt.Add(30 * time.Minute)
	if !airplayGewaehlt(jetzt) {
		t.Fatal("gewählte Dauer noch nicht um: an — auch ohne Verbindung kein Rückfall")
	}
	if airplayGewaehlt(jetzt.Add(31 * time.Minute)) {
		t.Fatal("gewählte Dauer abgelaufen: aus")
	}
}

// Prüfrunde 1, Punkt 8: fehlt die Angabe in einer späteren Antwort, gilt der gespeicherte Stand nicht ewig weiter.
func TestAirplaySollFehltVerwirft(t *testing.T) {
	merkeAirplay(t)
	jetzt := time.Now()
	if !airplaySollUebernehmen(zeiger(false), "", "") {
		t.Fatal("erster Stand aus KAI muss gespeichert werden")
	}
	if airplayGewaehlt(jetzt) {
		t.Fatal("KAI: nicht gewählt → aus")
	}
	if airplaySollUebernehmen(zeiger(false), "", "") {
		t.Fatal("unveränderter Stand: nichts zu speichern")
	}
	if !airplaySollUebernehmen(nil, "", "") {
		t.Fatal("Antwort ohne Angabe: gespeicherten Stand verwerfen (und speichern)")
	}
	if apSoll != nil {
		t.Fatal("gespeicherter Soll-Stand muss weg sein")
	}
	if !airplayGewaehlt(jetzt) {
		t.Fatal("ohne Angabe wie ältere KAI: läuft, solange erlaubt")
	}
	if airplaySollUebernehmen(nil, "", "") {
		t.Fatal("wiederholt ohne Angabe: nichts mehr zu speichern")
	}
}

// Prüfrunde 1, Punkt 3: „Zurück zum Zeitplan“/„AirPlay beenden“ an Bord ohne Verbindung zu KAI.
func TestAirplayLokalAus(t *testing.T) {
	merkeAirplay(t)
	jetzt := time.Now()
	if airplayLokalAusMerken() {
		t.Fatal("ohne gewähltes AirPlay gibt es nichts zu beenden")
	}
	airplaySollUebernehmen(zeiger(true), "", "41")
	if !airplayGewaehlt(jetzt) {
		t.Fatal("in KAI gewählt: an")
	}
	if !airplayLokalAusMerken() {
		t.Fatal("an Bord beendet: muss gemerkt werden")
	}
	if airplayGewaehlt(jetzt) {
		t.Fatal("an Bord beendet: Empfänger aus, obwohl KAI (noch) gewählt meldet")
	}
	// KAI meldet weiter dieselbe Steuerung (z. B. nach Wiederverbindung) → bleibt aus
	airplaySollUebernehmen(zeiger(true), "", "41")
	if airplayGewaehlt(jetzt) {
		t.Fatal("gleiche Steuerung aus KAI: bleibt aus, bis KAI neu steuert")
	}
	// KAI steuert neu (andere Steuerung) → gilt wieder
	if !airplaySollUebernehmen(zeiger(true), "", "42") {
		t.Fatal("neue Steuerung: speichern")
	}
	if !airplayGewaehlt(jetzt) {
		t.Fatal("neu in KAI gewählt: an")
	}
	// erneut an Bord beendet, danach hebt KAI auf → Merker weg, aus bleibt aus
	airplayLokalAusMerken()
	airplaySollUebernehmen(zeiger(false), "", "")
	if apLokalAus {
		t.Fatal("KAI hat aufgehoben: der Merker „an Bord beendet“ ist erledigt")
	}
	if airplayGewaehlt(jetzt) {
		t.Fatal("aufgehoben: aus")
	}
}

func TestBeendetAirplay(t *testing.T) {
	for _, f := range []struct {
		b    medienBefehl
		soll bool
	}{
		{medienBefehl{Art: "zeitplan"}, true},
		{medienBefehl{Art: "steuerung", Paused: true}, true},
		{medienBefehl{Art: "steuerung", Playlist: 3}, true},
		{medienBefehl{Art: "befehl", Cmd: "lautstaerke"}, false},
		{medienBefehl{Art: "befehl", Cmd: "vor"}, false},
		{medienBefehl{Art: "ereignis", Ereignis: 2}, false},
	} {
		if beendetAirplay(f.b) != f.soll {
			t.Fatalf("%+v: erwartet %v", f.b, f.soll)
		}
	}
}
