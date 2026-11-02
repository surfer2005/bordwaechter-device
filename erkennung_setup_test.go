package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRtspAusKamera(t *testing.T) {
	got := rtspAusKamera("http://root:Ge%40heim@192.168.73.20/axis-cgi/mjpg/video.cgi", "")
	want := "rtsp://root:Ge%40heim@192.168.73.20/axis-media/media.amp?resolution=1280x720&fps=5"
	if got != want {
		t.Fatalf("got %s", got)
	}
	if g := rtspAusKamera("", "http://u:p@10.0.0.5:8080/axis-cgi/jpg/image.cgi"); !strings.HasPrefix(g, "rtsp://u:p@10.0.0.5/") {
		t.Fatalf("Rückfall auf Standbild-Adresse ohne HTTP-Port erwartet, got %s", g)
	}
	if rtspAusKamera("", "") != "" {
		t.Fatal("ohne Adresse leer erwartet")
	}
}

func TestFrigateConfig(t *testing.T) {
	c := frigateConfig(`rtsp://u:p"x@h/axis-media/media.amp`, "person, boat ,")
	if !strings.Contains(c, `path: "rtsp://u:p\"x@h/axis-media/media.amp"`) {
		t.Fatalf("Pfad nicht sicher gequotet:\n%s", c)
	}
	if !strings.Contains(c, "track: [person, boat]") || !strings.Contains(c, "type: hailo8l") {
		t.Fatalf("Klassen/Detektor fehlen:\n%s", c)
	}
	if !strings.Contains(frigateConfig("rtsp://h/x", ""), "track: [person, boat]") {
		t.Fatal("Vorgabe-Klassen fehlen")
	}
}

func TestFrigateImage(t *testing.T) {
	if frigateImage("") != frigateBasis || frigateImage(frigateHailoRT) != frigateBasis {
		t.Fatal("bei passendem/unbekanntem Treiber das Original-Image erwartet")
	}
	if frigateImage("4.23.0") != "kai-frigate:hailort-4.23.0" {
		t.Fatal(frigateImage("4.23.0"))
	}
}

func TestFrigateSicherheit(t *testing.T) {
	var e frigateEvent
	if err := json.Unmarshal([]byte(`{"id":"1","label":"person","top_score":null,"score":null,"data":{"top_score":0.83,"score":0.71}}`), &e); err != nil {
		t.Fatal(err)
	}
	if e.sicherheit() != 0.83 {
		t.Fatalf("neue Frigate-Form: %v", e.sicherheit())
	}
	var alt frigateEvent
	_ = json.Unmarshal([]byte(`{"id":"2","label":"boat","top_score":0.77}`), &alt)
	if alt.sicherheit() != 0.77 {
		t.Fatalf("alte Form: %v", alt.sicherheit())
	}
}
