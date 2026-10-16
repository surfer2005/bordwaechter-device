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
	AMKey        string `json:"am_key"`       // Asset-Manager-/HA-Schlüssel für den Netz-Scan (X-API-Key); leer = Scan aus
	ShipID       int    `json:"ship_id"`      // optional; 0 = aus dem Bundle übernehmen
	SendPosition bool   `json:"send_position"` // GPS direkt an KAI melden (aus, falls HA das liefert)
	Listen       string `json:"listen"`        // lokale UI/API, z. B. ":8090"
	UIPassword   string `json:"ui_password"`   // Schutz der lokalen Oberfläche (leer = offen im Bordnetz)
	HAPassword   string `json:"ha_password"`   // eigenes Passwort für Home Assistant (leer = HA nutzt UIPassword)
	Hostname     string `json:"hostname"`      // Gerätename (hostnamectl + /etc/hosts); Gerät danach als <hostname>.local erreichbar
	SSHPassword  string `json:"ssh_password"`  // Passwort des SSH-Benutzers "pi" (leer = unverändert / Image-Standard "kai-terminal")
	VNCPassword  string `json:"vnc_password"`  // Passwort für den VNC-Fernzugriff (leer = unverändert)
	DisplayToken string `json:"display_token"`  // Token für die Kiosk-Anzeige (/anzeige?token=…), leer = Anzeige aus
	DisplayView  string `json:"display_view"`   // Startansicht des Geräte-Kiosks: "pilot" | "seekarte"
	DisplayRotate string `json:"display_rotate"` // Bildschirmdrehung des Monitors: "normal"|"left"|"right"|"inverted"
	// Standalone-/Offline-Navigation OHNE KAI: Ziel aus dem gespeicherten Streckennetz wählen.
	// Nur dann zeigt die Seekarte das Ziel-Auswahlfeld. Wer KAI nutzt, bekommt die Route automatisch
	// aus dem Bundle und braucht das nicht. Vorgabe: aus.
	Standalone bool `json:"standalone"`

	GPS struct {
		Source     string `json:"source"` // "teltonika" | "nmea" (USB-GPS-Maus) | "manual"
		Device     string `json:"device"` // nur "nmea": serielles Gerät, z. B. /dev/ttyACM0 oder /dev/ttyUSB0
		Baud       int    `json:"baud"`   // nur "nmea": Baudrate (Vorgabe 9600)
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
		DeadmanPrewarnSec int    `json:"deadman_prewarn_sec"` // Sek. NACH der grünen Lampe bis zur Hupe (Frist zum Quittieren)
	} `json:"sifa"`

	GPIO struct {
		Backend    string `json:"backend"` // "sysfs" | "none"
		HornPin    int    `json:"horn_pin"`
		LampPin    int    `json:"lamp_pin"`
		AckPin      int  `json:"ack_pin"`       // Quittier-Taster (Eingang), -1 = keiner
		AckActiveLow bool `json:"ack_active_low"` // Taster schaltet gegen GND → gedrückt = LOW (statt gegen 3,3V)
		SifaKeyPin  int  `json:"sifa_key_pin"`  // Schlüsselschalter "SIFA an" (Eingang), -1 = keiner
		ActiveLow   bool `json:"active_low"`    // Relais schaltet bei LOW
		// Eingangs-Vorspannung (Bias) für Taster/Schalter. Leer = alt (sysfs, Chip-Default = Pull-Down).
		// "pullup" = interner Pull-up übers character device (Taster gegen GND: Ruhe HIGH, gedrückt LOW).
		// "pulldown"/"disabled" analog. Pull-up ist der richtige Wert für einen Taster gegen GND.
		InputBias string `json:"input_bias"`
	} `json:"gpio"`

	Camera struct {
		Enabled       bool   `json:"enabled"`
		WebhookSecret string `json:"webhook_secret"` // MxMessage ruft /api/camera/event?secret=… auf
		SnapshotURL   string `json:"snapshot_url"`   // HTTP-Snapshot (bei Alarm geholt, an KAI gehängt)
		StreamURL     string `json:"stream_url"`     // MJPEG-Livestream (flüssiges Live-Bild, AXIS /axis-cgi/mjpg/video.cgi)
		HoldSec       int    `json:"hold_sec"`       // wie lange ein Kamera-Hinweis als aktiv gilt
	} `json:"camera"`

	Detect struct {
		Enabled    bool    `json:"enabled"`     // Objekterkennung (Frigate) ein/aus — ein Image, Feature zuschaltbar
		FrigateURL string  `json:"frigate_url"` // lokal laufendes Frigate, z. B. http://127.0.0.1:5000
		Labels     string  `json:"labels"`      // Komma-getrennt, z. B. "person,boat"
		MinScore   float64 `json:"min_score"`   // Mindest-Score 0..1, darunter wird nicht gemeldet
		PollSec    int     `json:"poll_sec"`    // Abfrageintervall der Frigate-API
	} `json:"detect"`

	// Wetter + Pegel in der Anzeige. Werte kommen aus denselben öffentlichen Quellen wie KAI
	// (Open-Meteo / PEGELONLINE) → identische Zahlen, ohne Eingriff ins Pilot-Bundle; läuft mit
	// UND ohne KAI-Verbindung. Vorgabe: beides an, Bodensee/Konstanz.
	Info struct {
		Weather    bool   `json:"weather"`     // Wetter anzeigen
		Pegel      bool   `json:"pegel"`       // Pegel anzeigen
		Lat        string `json:"lat"`         // Wetter-Koordinaten (Vorgabe Bodensee/Konstanz)
		Lng        string `json:"lng"`
		PegelUUID  string `json:"pegel_uuid"`  // PEGELONLINE-Stations-UUID (Vorgabe Konstanz)
		PegelLabel string `json:"pegel_label"` // Anzeigename der Station
		PollSec    int    `json:"poll_sec"`    // Abrufintervall
	} `json:"info"`

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
	c.GPS.Device = "/dev/ttyACM0"
	c.GPS.Baud = 9600
	c.GPS.Port = 502
	c.GPS.Unit = 1
	c.GPS.LatReg, c.GPS.LngReg = 0, 2
	c.GPS.SpeedReg, c.GPS.HeadingReg = -1, -1
	c.GPS.Format = "float32"
	c.GPS.PollSec = 5
	c.SIFA.TriggerCombo = "either"
	c.SIFA.ReAlarmSec = 15
	c.SIFA.DeadmanSec = 30
	c.SIFA.DeadmanPrewarnSec = 3
	c.GPIO.Backend = "none"
	c.GPIO.HornPin, c.GPIO.LampPin = 17, 27
	c.GPIO.AckPin, c.GPIO.SifaKeyPin = 22, -1
	c.Camera.HoldSec = 20
	c.DisplayView = "pilot"
	c.DisplayRotate = "normal"
	c.Hostname = "bordwaechter"
	c.Detect.FrigateURL = "http://127.0.0.1:5000"
	c.Detect.Labels = "person,boat"
	c.Detect.MinScore = 0.6
	c.Detect.PollSec = 3
	c.Info.Weather = true
	c.Info.Pegel = true
	c.Info.Lat = "47.66"
	c.Info.Lng = "9.18"
	c.Info.PegelUUID = "aa9179c1-17ef-4c61-a48a-74193fa7bfdf" // Konstanz
	c.Info.PegelLabel = "Konstanz"
	c.Info.PollSec = 600
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

// Bundle-Persistenz: das zuletzt von KAI geholte Bundle auf die Platte schreiben und beim Start laden.
// So gelten nach einem Offline-Neustart weiter die KAI-Werte (statt der Code-Defaults). Siehe main.go.
func bundlePath() string {
	if p := os.Getenv("BORDWAECHTER_BUNDLE"); p != "" {
		return p
	}
	if _, err := os.Stat("/etc/bordwaechter"); err == nil {
		return "/etc/bordwaechter/bundle.json"
	}
	return "bundle.json"
}

func saveBundle(b *Bundle) {
	data, err := json.Marshal(b)
	if err != nil {
		return
	}
	p := bundlePath()
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	tmp := p + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		_ = os.Rename(tmp, p)
	}
}

func loadBundle() *Bundle {
	data, err := os.ReadFile(bundlePath())
	if err != nil {
		return nil
	}
	var b Bundle
	if json.Unmarshal(data, &b) != nil {
		return nil
	}
	return &b
}

// Tageskilometer-Persistenz: Tag + Stand auf die Platte, damit ein Neustart mitten am Tag den
// Zähler nicht verliert. Gilt nur, wenn der gespeicherte Tag dem heutigen Ortstag entspricht.
type todayKmFile struct {
	Date string  `json:"date"`
	Km   float64 `json:"km"`
}

func kmPath() string {
	if p := os.Getenv("BORDWAECHTER_KM"); p != "" {
		return p
	}
	if _, err := os.Stat("/etc/bordwaechter"); err == nil {
		return "/etc/bordwaechter/today_km.json"
	}
	return "today_km.json"
}

func saveTodayKm(date string, km float64) {
	data, err := json.Marshal(todayKmFile{Date: date, Km: km})
	if err != nil {
		return
	}
	p := kmPath()
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	tmp := p + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		_ = os.Rename(tmp, p)
	}
}

func loadTodayKm() (string, float64) {
	data, err := os.ReadFile(kmPath())
	if err != nil {
		return "", 0
	}
	var f todayKmFile
	if json.Unmarshal(data, &f) != nil {
		return "", 0
	}
	return f.Date, f.Km
}
