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
