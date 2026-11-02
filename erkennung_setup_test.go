package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRtspAusKamera(t *testing.T) {
	got := rtspAusKamera("http://root:Ge%40heim@192.168.73.20/axis-cgi/mjpg/video.cgi", "", "")
	want := "rtsp://root:Ge%40heim@192.168.73.20/axis-media/media.amp?resolution=1280x720&fps=5"
	if got != want {
		t.Fatalf("got %s", got)
	}
	if g := rtspAusKamera("", "http://u:p@10.0.0.5:8080/axis-cgi/jpg/image.cgi", "1280x720"); !strings.HasPrefix(g, "rtsp://u:p@10.0.0.5/") {
		t.Fatalf("Rückfall auf Standbild-Adresse ohne HTTP-Port erwartet, got %s", g)
	}
	if rtspAusKamera("", "", "") != "" {
		t.Fatal("ohne Adresse leer erwartet")
	}
}

func TestFrigateConfig(t *testing.T) {
	c := frigateConfig(`rtsp://u:p"x@h/axis-media/media.amp`, "person, boat ,", "3840x2160", 0, 0)
	if !strings.Contains(c, "width: 3840") || !strings.Contains(c, "height: 2160") {
		t.Fatalf("Auflösung fehlt:\n%s", c)
	}
	if !strings.Contains(c, `path: "rtsp://u:p\"x@h/axis-media/media.amp"`) {
		t.Fatalf("Pfad nicht sicher gequotet:\n%s", c)
	}
	if !strings.Contains(c, "track: [person, boat]") || !strings.Contains(c, "type: hailo8l") {
		t.Fatalf("Klassen/Detektor fehlen:\n%s", c)
	}
	if !strings.Contains(frigateConfig("rtsp://h/x", "", "", 0, 0), "track: [person, boat]") {
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

func TestAlarmAusErkennung(t *testing.T) {
	ende := 1.0
	ev := []frigateEvent{{Label: "person", EndTime: &ende}, {Label: "boat"}}
	ev[1].Data.TopScore = 0.8
	if alarmAusErkennung(ev, "", 0.6, false) || alarmAusErkennung(ev, "aus", 0.6, false) {
		t.Fatal("ohne Alarm-Arten nie Alarm")
	}
	if alarmAusErkennung(ev, "person", 0.6, false) {
		t.Fatal("beendete Person darf keinen Alarm auslösen")
	}
	if !alarmAusErkennung(ev, "person,boat", 0.6, false) {
		t.Fatal("laufendes Boot muss auslösen")
	}
	if alarmAusErkennung(ev, "boat", 0.9, false) {
		t.Fatal("unter dem Mindest-Score kein Alarm")
	}
	if alarmAusErkennung(ev, "boat", 0.6, true) {
		t.Fatal("mit Wasserfläche: Boot außerhalb der Zone darf nicht auslösen")
	}
	ev[1].Zones = []string{"wasser"}
	if !alarmAusErkennung(ev, "boat", 0.6, true) {
		t.Fatal("Boot auf dem Wasser muss auslösen")
	}
}

func TestWasserZone(t *testing.T) {
	if _, _, ok := wasserZone(0, 0, 720); ok {
		t.Fatal("0/0 = Vollbild, keine Zone")
	}
	o, u, ok := wasserZone(40, 10, 720)
	if !ok || o != 288 || u != 648 {
		t.Fatalf("40/10 bei 720: %d..%d", o, u)
	}
	if o, u, _ := wasserZone(90, 50, 720); u-o <= 0 {
		t.Fatalf("zu große Werte müssen eine Fläche lassen: %d..%d", o, u)
	}
	c := frigateConfig("rtsp://h/x", "", "1280x720", 40, 10)
	if !strings.Contains(c, "coordinates: 0,648,1280,648,1280,288,0,288") {
		t.Fatalf("Zone fehlt:\n%s", c)
	}
	if strings.Contains(frigateConfig("rtsp://h/x", "", "", 0, 0), "zones:") {
		t.Fatal("Vollbild darf keine Zone schreiben")
	}
}
