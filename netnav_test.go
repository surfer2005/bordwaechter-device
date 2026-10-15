package main

import (
	"math"
	"testing"
)

// Kleines Testnetz: A — B — C als Kette, dazu eine direkte, aber lange Kante A — C.
// Koordinaten grob am Bodensee (lon,lat). Punkte sind [lon,lat].
func testNet() *NetworkData {
	return &NetworkData{Segments: []NetSegment{
		{From: "A", To: "B", Points: [][2]float64{{9.10, 47.60}, {9.15, 47.62}}},
		{From: "B", To: "C", Points: [][2]float64{{9.15, 47.62}, {9.20, 47.64}}},
		// direkte, absichtlich weit ausholende Kante (soll vom Dijkstra NICHT gewählt werden)
		{From: "A", To: "C", Km: 999, Points: [][2]float64{{9.10, 47.60}, {9.50, 47.90}, {9.20, 47.64}}},
	}}
}

func TestLandingsFromNet(t *testing.T) {
	ls := landingsFromNet(testNet())
	if len(ls) != 3 {
		t.Fatalf("erwartet 3 Anlegestellen, bekam %d", len(ls))
	}
	// sortiert nach Name
	if ls[0].Name != "A" || ls[2].Name != "C" {
		t.Fatalf("nicht sortiert: %+v", ls)
	}
	// A nimmt den ERSTEN Punkt seines Segments
	if math.Abs(ls[0].Lat-47.60) > 1e-9 || math.Abs(ls[0].Lng-9.10) > 1e-9 {
		t.Fatalf("A-Koordinate falsch: %+v", ls[0])
	}
}

func TestDijkstraNimmtKurzenWeg(t *testing.T) {
	g := buildGraph(testNet())
	route, km, ok := dijkstra(g, "A", "C")
	if !ok {
		t.Fatal("kein Weg gefunden")
	}
	// über B (zwei kurze Kanten) muss kürzer sein als die 999-km-Direktkante
	if km > 100 {
		t.Fatalf("Dijkstra nahm die lange Direktkante: %.1f km", km)
	}
	// verkettete Polylinie: A-Punkt, B-Punkt (nur einmal), C-Punkt
	if len(route) != 3 {
		t.Fatalf("erwartet 3 Routenpunkte (geteilter Knoten entdoppelt), bekam %d: %+v", len(route), route)
	}
}

func TestManualRouteNet(t *testing.T) {
	// aktuelle Position nahe A
	r := manualRoute(testNet(), 47.601, 9.101, "C", "net")
	if !r.Ok {
		t.Fatalf("Route nicht ok: %+v", r)
	}
	if r.Mode != "net" {
		t.Fatalf("Modus sollte net sein: %s", r.Mode)
	}
	if r.DistanceKm > 100 {
		t.Fatalf("Distanz unplausibel (lange Kante?): %.1f", r.DistanceKm)
	}
	// erster Punkt ist die aktuelle Position [lon,lat]
	if math.Abs(r.Points[0][0]-9.101) > 1e-9 {
		t.Fatalf("erster Punkt ist nicht die aktuelle Position: %+v", r.Points[0])
	}
	if math.Abs(r.ToLat-47.64) > 1e-9 {
		t.Fatalf("Ziel-Koordinate falsch: %+v", r)
	}
}

func TestManualRouteLuftlinie(t *testing.T) {
	r := manualRoute(testNet(), 47.601, 9.101, "C", "line")
	if !r.Ok || r.Mode != "line" {
		t.Fatalf("Luftlinie nicht ok: %+v", r)
	}
	if len(r.Points) != 2 {
		t.Fatalf("Luftlinie hat genau zwei Punkte, bekam %d", len(r.Points))
	}
}

func TestManualRouteUnbekanntesZiel(t *testing.T) {
	r := manualRoute(testNet(), 47.6, 9.1, "Z", "net")
	if r.Ok {
		t.Fatal("unbekanntes Ziel darf nicht ok sein")
	}
}
