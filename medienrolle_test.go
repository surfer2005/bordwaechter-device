package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestMedienStartZiel(t *testing.T) {
	if medienStartZiel(&Config{KaiBaseURL: "https://kai-bsb.de/test"}) != "" {
		t.Error("Bordwächter öffnet keine Abspielseite")
	}
	got := medienStartZiel(&Config{KaiBaseURL: "https://kai-bsb.de/test", Rolle: "bildschirm", MedienPfad: "kiosk/medien/3?token=a"})
	if got != "https://kai-bsb.de/test/kiosk/medien/3?token=a" {
		t.Errorf("Abspielseite falsch: %s", got)
	}
	if kaiHerkunft("https://kai-bsb.de/test/") != "https://kai-bsb.de" {
		t.Error("Herkunft für die Chromium-Freigabe")
	}
	if rolleFuerKai("") != "bordwaechter" || rolleFuerKai("audio") != "audio" {
		t.Error("Art im Herzschlag")
	}
}

func TestMedienPositionVomBordwaechter(t *testing.T) {
	bw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/bord/position" || r.URL.Query().Get("token") != "geheim" {
			http.Error(w, "forbidden", 403)
			return
		}
		_, _ = w.Write([]byte(`{"valid":true,"lat":47.65,"lng":9.48}`))
	}))
	defer bw.Close()
	a := &App{cfg: &Config{Rolle: "bildschirm", Bordwaechter: bw.URL, BordwaechterToken: "geheim"}}
	hol := func(remote string) (*httptest.ResponseRecorder, map[string]any) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/api/medien/position", nil)
		req.RemoteAddr = remote
		a.handleMedienPosition(rec, req)
		var p map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &p)
		return rec, p
	}
	rec, p := hol("127.0.0.1:5555")
	if p["valid"] != true || p["lat"] != 47.65 {
		t.Fatalf("Position erwartet: %s", rec.Body.String())
	}
	if rec.Header().Get("Access-Control-Allow-Private-Network") != "true" {
		t.Error("Freigabe-Kopf fürs lokale Netz fehlt")
	}
	if rec, _ = hol("192.168.10.50:5555"); rec.Code != 403 {
		t.Fatalf("aus dem Netz muss gesperrt sein, war %d", rec.Code)
	}
	a.cfg = &Config{Rolle: "bildschirm"}
	if _, p = hol("127.0.0.1:5555"); p["valid"] != false {
		t.Fatal("ohne Bordwächter keine Position")
	}
}

func TestHerzschlagUebernimmtAbspielseiteUndBordwaechter(t *testing.T) {
	os.Setenv("BORDWAECHTER_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	defer os.Unsetenv("BORDWAECHTER_CONFIG")
	a := &App{cfg: &Config{Rolle: "audio", MedienPfad: "kiosk/medien/4?token=x"}}
	hr := &HelloResp{Rolle: "audio"}
	hr.Medien = &MedienInfo{Pfad: "kiosk/medien/4?token=y"}
	hr.Bordwaechter = &struct {
		URL   string `json:"url"`
		Token string `json:"token"`
	}{URL: "http://192.168.10.20:8090", Token: "t1"}
	a.medienAusHerzschlag(hr)
	c := a.getCfg()
	if c.MedienPfad != "kiosk/medien/4?token=y" || c.Bordwaechter != "http://192.168.10.20:8090" || c.BordwaechterToken != "t1" {
		t.Fatalf("nicht übernommen: %+v", c)
	}
	a.medienAusHerzschlag(&HelloResp{Rolle: "audio"}) // KAI kennt gerade keinen Bordwächter → letzten behalten
	if a.getCfg().Bordwaechter == "" {
		t.Fatal("letzter bekannter Bordwächter darf nicht verloren gehen")
	}
	bwg := &App{cfg: &Config{}}
	bwg.medienAusHerzschlag(&HelloResp{Rolle: "bordwaechter"})
	if bwg.getCfg().Rolle != "" {
		t.Fatal("ein Bordwächter bleibt Bordwächter")
	}
}

func TestBordmusikArgs(t *testing.T) {
	a := bordmusikArgs("https://kai-bsb.de/test/kiosk/medien/6?token=x", "/home/pi/.config/bordwaechter-bordmusik")
	hat := func(s string) bool {
		for _, x := range a {
			if x == s {
				return true
			}
		}
		return false
	}
	if !hat("--headless=new") || !hat("--autoplay-policy=no-user-gesture-required") || a[len(a)-1] != "https://kai-bsb.de/test/kiosk/medien/6?token=x" {
		t.Fatalf("Bordmusik ohne Monitor braucht unsichtbaren Browser mit Autoplay und die Abspielseite zuletzt: %v", a)
	}
}

func TestSenkenUndAuswahl(t *testing.T) {
	st := `PipeWire 'pipewire-0' [1.4.2, pi@kai, cookie:123]
 └─ Clients:
        33. WirePlumber

Audio
 ├─ Devices:
 │      49. Built-in Audio                      [alsa]
 │      50. Built-in Audio                      [alsa]
 │
 ├─ Sinks:
 │  *   56. Built-in Audio Digital Stereo (HDMI) [vol: 0.40]
 │      57. Built-in Audio Stereo               [vol: 1.00 MUTED]
 │
 ├─ Sources:
 │      60. Mikrofon                            [vol: 1.00]
 │
 └─ Streams:

Video
 ├─ Devices:
`
	l := senken(st)
	if len(l) != 2 || l[0].ID != "56" || !l[0].Standard || l[1].Name != "Built-in Audio Stereo" {
		t.Fatalf("Tonausgänge falsch gelesen: %+v", l)
	}
	if passendeSenke(l, "analog") != "57" || passendeSenke(l, "hdmi") != "56" {
		t.Fatal("Ausgang laut KAI falsch gewählt")
	}
}
