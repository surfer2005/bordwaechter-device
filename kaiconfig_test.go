package main

import "testing"

// Einstellungen aus KAI: nur gelieferte Felder werden übernommen, Netzwerk/KAI-Adresse/Schlüssel nie.
func TestMischeKaiConfig(t *testing.T) {
	c := defaultConfig()
	c.KaiBaseURL = "https://kai.example"
	c.EdgeToken = "geheim"
	c.Network.Mode = "static"
	c.Network.Address = "192.168.62.159/24"
	c.Camera.SnapshotURL = "http://kamera/bild"
	kai := map[string]any{
		"gps":            map[string]any{"source": "teltonika", "host": "192.168.9.1", "lat_reg": 143.0},
		"display_rotate": "left",
		"network":        map[string]any{"mode": "dhcp"},
		"kai_base_url":   "https://falsch",
		"edge_token":     "falsch",
	}
	n, err := mischeKaiConfig(c, kai)
	if err != nil {
		t.Fatal(err)
	}
	if n.GPS.Source != "teltonika" || n.GPS.Host != "192.168.9.1" || n.GPS.LatReg != 143 {
		t.Fatalf("GPS nicht übernommen: %+v", n.GPS)
	}
	if n.GPS.Format != c.GPS.Format || n.GPS.PollSec != c.GPS.PollSec {
		t.Fatal("nicht gelieferte GPS-Felder dürfen nicht verändert werden")
	}
	if n.DisplayRotate != "left" {
		t.Fatal("Drehung nicht übernommen")
	}
	if n.Network.Mode != "static" || n.Network.Address != "192.168.62.159/24" {
		t.Fatal("Netzwerk darf nie aus KAI kommen")
	}
	if n.KaiBaseURL != "https://kai.example" || n.EdgeToken != "geheim" {
		t.Fatal("KAI-Adresse/Schlüssel dürfen nie aus KAI überschrieben werden")
	}
	if n.Camera.SnapshotURL != "http://kamera/bild" {
		t.Fatal("Kamera-Adresse muss bleiben, wenn KAI sie nicht liefert")
	}
}

func TestKonfigSeiteGesperrtWennAusKai(t *testing.T) {
	cur := defaultConfig()
	cur.EdgeToken, cur.KaiConfigRev, cur.KaiBaseURL = "tok", 3, "https://kai-bsb.de/test"
	cur.GPS.Host, cur.SIFA.DeadmanSec = "192.168.2.1", 30
	neu := *cur
	neu.GPS.Host, neu.SIFA.DeadmanSec, neu.KaiBaseURL, neu.DisplayToken = "10.0.0.9", 5, "https://kai-bsb.de", "anz"
	neu.Network.Mode = "static"
	c, gesperrt := sperreAusKai(cur, neu)
	if !gesperrt || c.GPS.Host != "192.168.2.1" || c.SIFA.DeadmanSec != 30 {
		t.Fatalf("Einstellungen aus KAI dürfen am Gerät nicht änderbar sein: %v %s %d", gesperrt, c.GPS.Host, c.SIFA.DeadmanSec)
	}
	if c.KaiBaseURL != "https://kai-bsb.de" || c.Network.Mode != "static" {
		t.Fatalf("KAI-Adresse und Netzwerk bleiben am Gerät änderbar: %+v", c)
	}
	if c.DisplayToken == "anz" {
		t.Fatal("der Anzeige-Schlüssel kommt aus KAI")
	}
	// nicht gekoppelt (eigenständig): alles änderbar
	frei := defaultConfig()
	c, gesperrt = sperreAusKai(frei, neu)
	if gesperrt || c.GPS.Host != "10.0.0.9" {
		t.Fatal("ohne KAI bleibt alles am Gerät änderbar")
	}
}

func TestMedienRolleBleibtBeimSpeichern(t *testing.T) {
	cur := defaultConfig()
	cur.EdgeToken, cur.Rolle, cur.MedienPfad = "tok", "audio", "kiosk/medien/6?token=x"
	neu := *defaultConfig()
	neu.KaiBaseURL = "https://kai-bsb.de/test"
	c, gesperrt := sperreAusKai(cur, neu)
	if !gesperrt || c.Rolle != "audio" || c.MedienPfad == "" || c.EdgeToken != "tok" {
		t.Fatalf("Rolle Bordmusik darf beim Speichern der Konfig-Seite nicht verloren gehen: %v %+v", gesperrt, c)
	}
}
