package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// Config: alle Einstellungen des Bordgeräts. Wird als JSON gespeichert und über die lokale
// Web-Oberfläche gepflegt. Bewusst flach + mit Defaults, damit "installieren → eintragen → läuft".
type Config struct {
	KaiBaseURL   string `json:"kai_base_url"` // z. B. https://kai-bsb.de  (Gerät ruft <base>/api/edge/… )
	EdgeToken    string `json:"edge_token"`   // je Gerät, aus KAI-Verwaltung
	ShipID       int    `json:"ship_id"`      // optional; 0 = aus dem Bundle übernehmen
	SendPosition bool   `json:"send_position"` // GPS direkt an KAI melden (aus, falls HA das liefert)
	Listen       string `json:"listen"`        // lokale UI/API, z. B. ":8090"
	UIPassword   string `json:"ui_password"`   // Schutz der lokalen Oberfläche (leer = offen im Bordnetz)
	HAPassword   string `json:"ha_password"`   // eigenes Passwort für Home Assistant (leer = HA nutzt UIPassword)

	GPS struct {
		Source     string `json:"source"` // "teltonika" | "manual"
		Host       string `json:"host"`
		Port       int    `json:"port"`
		Unit       int    `json:"unit"`        // Modbus-Unit-ID
		LatReg     int    `json:"lat_reg"`     // Holding-Register (konfigurierbar, an echter Teltonika verifizieren)
		LngReg     int    `json:"lng_reg"`
		SpeedReg   int    `json:"speed_reg"`   // -1 = nicht vorhanden
		HeadingReg int    `json:"heading_reg"` // -1 = nicht vorhanden
		Format     string `json:"format"`      // "float32" | "int32_1e7" | "int32_1e6"
		PollSec    int    `json:"poll_sec"`
	} `json:"gps"`

	SIFA struct {
		TriggerCombo      string `json:"trigger_combo"` // "gps" | "camera" | "both" | "either"
		ReAlarmSec        int    `json:"realarm_sec"`   // Ereignis-SIFA: Wiederholung nach Quittieren, solange Ursache besteht
		DeadmanOn         bool   `json:"deadman_on"`    // allgemeine SIFA (Totmann)
		DeadmanSec        int    `json:"deadman_sec"`
		DeadmanPrewarnSec int    `json:"deadman_prewarn_sec"`
	} `json:"sifa"`

	GPIO struct {
		Backend    string `json:"backend"` // "sysfs" | "none"
		HornPin    int    `json:"horn_pin"`
		LampPin    int    `json:"lamp_pin"`
		AckPin     int    `json:"ack_pin"`      // Quittier-Taster (Eingang), -1 = keiner
		SifaKeyPin int    `json:"sifa_key_pin"` // Schlüsselschalter "SIFA an" (Eingang), -1 = keiner
		ActiveLow  bool   `json:"active_low"`   // Relais schaltet bei LOW
	} `json:"gpio"`

	Camera struct {
		Enabled       bool   `json:"enabled"`
		WebhookSecret string `json:"webhook_secret"` // MxMessage ruft /api/camera/event?secret=… auf
		SnapshotURL   string `json:"snapshot_url"`   // HTTP-Snapshot (bei Alarm geholt, an KAI gehängt)
		HoldSec       int    `json:"hold_sec"`       // wie lange ein Kamera-Hinweis als aktiv gilt
	} `json:"camera"`

	// Netzwerk: DHCP (Vorgabe) oder feste IP. Wird als NetworkManager-Keyfile geschrieben und
	// wirkt nach einem Neustart des Geräts — ohne Shell/nmcli (reines Datei-Schreiben).
	Network struct {
		Mode      string `json:"mode"`      // "dhcp" | "static"
		Interface string `json:"interface"` // z. B. "eth0"
		Address   string `json:"address"`   // CIDR, z. B. "192.168.1.50/24"
		Gateway   string `json:"gateway"`   // z. B. "192.168.1.1"
		DNS       string `json:"dns"`       // Komma-getrennt, z. B. "192.168.1.1,1.1.1.1"
	} `json:"network"`

	BundleRefreshSec int `json:"bundle_refresh_sec"`
}

func defaultConfig() *Config {
	c := &Config{
		KaiBaseURL:       "https://kai-bsb.de",
		Listen:           ":8090",
		SendPosition:     true,
		BundleRefreshSec: 300,
	}
	c.GPS.Source = "manual"
	c.GPS.Port = 502
	c.GPS.Unit = 1
	c.GPS.LatReg, c.GPS.LngReg = 0, 2
	c.GPS.SpeedReg, c.GPS.HeadingReg = -1, -1
	c.GPS.Format = "float32"
	c.GPS.PollSec = 5
	c.SIFA.TriggerCombo = "either"
	c.SIFA.ReAlarmSec = 15
	c.SIFA.DeadmanSec = 30
	c.SIFA.DeadmanPrewarnSec = 5
	c.GPIO.Backend = "none"
	c.GPIO.HornPin, c.GPIO.LampPin = 17, 27
	c.GPIO.AckPin, c.GPIO.SifaKeyPin = 22, -1
	c.Camera.HoldSec = 20
	c.Network.Mode = "dhcp"
	c.Network.Interface = "eth0"
	return c
}

func configPath() string {
	if p := os.Getenv("BORDWAECHTER_CONFIG"); p != "" {
		return p
	}
	if _, err := os.Stat("/etc/bordwaechter"); err == nil {
		return "/etc/bordwaechter/config.json"
	}
	return "config.json"
}

var cfgMu sync.Mutex

func loadConfig() *Config {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	c := defaultConfig()
	b, err := os.ReadFile(configPath())
	if err != nil {
		return c
	}
	_ = json.Unmarshal(b, c) // unbekannte/fehlende Felder behalten die Defaults
	return c
}

func saveConfig(c *Config) error {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	p := configPath()
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}
