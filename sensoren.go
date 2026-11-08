package main

// Sensoren am Bordwächter (Peter 2026-11-07): Shelly, ESPHome und Web-IO melden per MQTT an den eingebauten
// Empfänger (mqttbroker.go, Port 1883). Die Zugänge kommen aus KAI (GET /api/edge/sensoren-config, nur Hash),
// die Werte gehen gesammelt an KAI (POST /api/edge/sensoren-werte) und werden ohne Netz gepuffert.
// Grenzwerte mit „Lampe/Hupe an Bord“ prüft der Bordwächter selbst — die Lampe geht auch ohne Netz an.
// ESPHome-Firmware: der Bordwächter lädt sie von KAI und bietet sie im Bord-Netz an (/fw/<sensor>/…);
// der Sensor holt sie sich über seine Update-Funktion.

import (
	"crypto/md5"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type sensorWertDef struct {
	Schluessel string   `json:"schluessel"`
	Min        *float64 `json:"min"`
	Max        *float64 `json:"max"`
	AlarmBei   *string  `json:"alarm_bei"`
	DauerMin   int      `json:"dauer_min"`
	Lampe      bool     `json:"lampe"`
}
type sensorFirmware struct {
	Version string `json:"version"`
	Pfad    string `json:"pfad"`
	Chip    string `json:"chip"`
}
type sensorDef struct {
	ID       int             `json:"id"`
	Art      string          `json:"art"`
	Name     string          `json:"name"`
	User     string          `json:"user"`
	Salt     string          `json:"salt"`
	Hash     string          `json:"hash"`
	Praefix  string          `json:"praefix"`
	Werte    []sensorWertDef `json:"werte"`
	Firmware *sensorFirmware `json:"firmware"`
}

type sensorMessung struct {
	SensorID   int    `json:"sensor_id"`
	Schluessel string `json:"schluessel"`
	Wert       any    `json:"wert"`
	At         string `json:"at"`
}
type sensorStatus struct {
	SensorID  int      `json:"sensor_id"`
	Online    bool     `json:"online"`
	IP        string   `json:"ip,omitempty"`
	FW        string   `json:"fw,omitempty"`
	Batterie  *float64 `json:"batterie,omitempty"`
	ZuletztAt string   `json:"zuletzt_at,omitempty"`
}

type Sensorik struct {
	mu        sync.Mutex
	defs      map[int]*sensorDef
	puffer    []sensorMessung
	aktuell   map[string]any       // "sensorID|schluessel" → letzter Wert
	verletzt  map[string]time.Time // seit wann eine Grenze mit Lampe verletzt ist
	status    map[int]*sensorStatus
	fwStand   map[int]string // installierte Firmware-Datei je Sensor (Version)
	broker    *MQTTBroker
	lampeAn   bool
	gesendet  time.Time
	geladenAm time.Time
}

var sensorik = &Sensorik{defs: map[int]*sensorDef{}, aktuell: map[string]any{}, verletzt: map[string]time.Time{}, status: map[int]*sensorStatus{}, fwStand: map[int]string{}}

func sensorOrdner() string { return filepath.Join(filepath.Dir(configPath()), "sensoren") }

// ── Zugang ──────────────────────────────────────────────────────────────────────────────────────────

func (s *Sensorik) pruefeZugang(user, pass string) (mqttZugang, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.defs {
		if d.User != user || d.Hash == "" {
			continue
		}
		h := sha256.Sum256([]byte(d.Salt + pass))
		if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(h[:])), []byte(d.Hash)) == 1 {
			return mqttZugang{SensorID: d.ID, Praefix: d.Praefix}, true
		}
	}
	return mqttZugang{}, false
}

// ── Werte aus MQTT-Nachrichten ableiten ──────────────────────────────────────────────────────────────

type abgeleitet struct {
	Schluessel string
	Wert       any
}

var shellyKomp = regexp.MustCompile(`^([a-z]+):(\d+)$`)

func zahlOderZustand(s string) any {
	t := strings.TrimSpace(s)
	if f, err := strconv.ParseFloat(t, 64); err == nil {
		return f
	}
	switch strings.ToUpper(t) {
	case "ON", "TRUE", "OPEN":
		return true
	case "OFF", "FALSE", "CLOSED":
		return false
	}
	return t
}

func mitIndex(basis string, n string) string {
	if n == "" || n == "0" {
		return basis
	}
	return basis + "_" + n
}

// shellyKomponente: eine Shelly-Komponente (z. B. "temperature:0") mit ihrem Status-Objekt.
func shellyKomponente(name string, obj map[string]any, st *sensorStatus) []abgeleitet {
	m := shellyKomp.FindStringSubmatch(name)
	typ, n := name, ""
	if m != nil {
		typ, n = m[1], m[2]
	}
	out := []abgeleitet{}
	zahl := func(k string) (float64, bool) { f, ok := obj[k].(float64); return f, ok }
	switch typ {
	case "temperature":
		if f, ok := zahl("tC"); ok {
			out = append(out, abgeleitet{mitIndex("temperatur", n), f})
		}
	case "humidity":
		if f, ok := zahl("rh"); ok {
			out = append(out, abgeleitet{mitIndex("feuchte", n), f})
		}
	case "devicepower":
		if b, ok := obj["battery"].(map[string]any); ok {
			if f, ok := b["percent"].(float64); ok {
				out = append(out, abgeleitet{"batterie", f})
				if st != nil {
					st.Batterie = &f
				}
			}
		}
	case "switch":
		if v, ok := obj["output"].(bool); ok {
			out = append(out, abgeleitet{mitIndex("relais", n), v})
		}
		if f, ok := zahl("apower"); ok {
			out = append(out, abgeleitet{mitIndex("leistung", n), f})
		}
	case "input":
		if v, ok := obj["state"].(bool); ok {
			out = append(out, abgeleitet{mitIndex("eingang", n), v})
		}
	case "illuminance":
		if f, ok := zahl("lux"); ok {
			out = append(out, abgeleitet{mitIndex("helligkeit", n), f})
		}
	case "voltmeter":
		if f, ok := zahl("voltage"); ok {
			out = append(out, abgeleitet{mitIndex("spannung", n), f})
		}
	case "wifi":
		if st != nil {
			if ip, ok := obj["sta_ip"].(string); ok {
				st.IP = ip
			}
		}
	}
	return out
}

// werteAusNachricht: aus Thema + Inhalt die Messwerte eines Sensors ableiten (je Geräteart).
func werteAusNachricht(art, praefix, topic string, payload []byte, st *sensorStatus) []abgeleitet {
	rest := strings.TrimPrefix(strings.TrimPrefix(topic, praefix), "/")
	text := strings.TrimSpace(string(payload))
	switch art {
	case "shelly":
		switch {
		case rest == "online":
			st.Online = text == "true"
			return nil
		case strings.HasPrefix(rest, "status/"):
			var obj map[string]any
			if json.Unmarshal(payload, &obj) != nil {
				return nil
			}
			return shellyKomponente(strings.TrimPrefix(rest, "status/"), obj, st)
		case rest == "events/rpc":
			var ev struct {
				Method string                    `json:"method"`
				Params map[string]map[string]any `json:"params"`
			}
			// params enthält neben Komponenten auch "ts" (Zahl) — einzeln entpacken
			var roh struct {
				Method string                     `json:"method"`
				Params map[string]json.RawMessage `json:"params"`
			}
			if json.Unmarshal(payload, &roh) != nil || (roh.Method != "NotifyStatus" && roh.Method != "NotifyFullStatus") {
				return nil
			}
			ev.Params = map[string]map[string]any{}
			for k, v := range roh.Params {
				var o map[string]any
				if json.Unmarshal(v, &o) == nil {
					ev.Params[k] = o
				}
			}
			out := []abgeleitet{}
			for k, o := range ev.Params {
				out = append(out, shellyKomponente(k, o, st)...)
			}
			return out
		}
	case "esphome":
		if rest == "status" {
			st.Online = text == "online"
			return nil
		}
		teile := strings.Split(rest, "/")
		if len(teile) == 3 && teile[2] == "state" {
			switch teile[0] {
			case "sensor":
				if f, err := strconv.ParseFloat(text, 64); err == nil {
					return []abgeleitet{{teile[1], f}}
				}
			case "binary_sensor", "switch":
				return []abgeleitet{{teile[1], strings.EqualFold(text, "ON")}}
			case "text_sensor":
				return []abgeleitet{{teile[1], text}}
			}
		}
		return nil
	case "webio":
		if rest == "" {
			st.Online = strings.EqualFold(text, "online")
			return nil
		}
		teile := strings.Split(rest, "/")
		if len(teile) == 3 && teile[0] == "get" {
			return []abgeleitet{{teile[1] + "_" + teile[2], zahlOderZustand(text)}}
		}
		return nil
	}
	// allgemein: Rest des Themas als Schlüssel
	if rest == "" || strings.HasSuffix(rest, "/set") || strings.Contains(rest, "/command") {
		return nil
	}
	return []abgeleitet{{strings.ReplaceAll(rest, "/", "_"), zahlOderZustand(text)}}
}

func (s *Sensorik) eingang(z mqttZugang, topic string, payload []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.defs[z.SensorID]
	if d == nil {
		return
	}
	st := s.status[d.ID]
	if st == nil {
		st = &sensorStatus{SensorID: d.ID}
		s.status[d.ID] = st
	}
	jetzt := time.Now().UTC()
	werte := werteAusNachricht(d.Art, d.Praefix, topic, payload, st)
	if len(werte) > 0 {
		st.Online = true
	}
	st.ZuletztAt = jetzt.Format(time.RFC3339)
	for _, w := range werte {
		s.aktuell[fmt.Sprintf("%d|%s", d.ID, w.Schluessel)] = w.Wert
		s.puffer = append(s.puffer, sensorMessung{SensorID: d.ID, Schluessel: w.Schluessel, Wert: w.Wert, At: jetzt.Format(time.RFC3339Nano)})
	}
	if len(s.puffer) > 50000 {
		s.puffer = s.puffer[len(s.puffer)-50000:]
	}
}

// WertVon: letzter Wert eines Sensors anhand seines Themas (für das Licht: Schlüsselschalter).
func (s *Sensorik) WertVon(praefix, schluessel string) (any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.defs {
		if d.Praefix == praefix {
			v, ok := s.aktuell[fmt.Sprintf("%d|%s", d.ID, schluessel)]
			return v, ok
		}
	}
	return nil, false
}

// ── Grenzwerte mit Lampe (auch ohne Netz) ───────────────────────────────────────────────────────────

func grenzeVerletzt(def sensorWertDef, wert any) bool {
	if def.AlarmBei != nil && *def.AlarmBei != "" {
		b, ok := wert.(bool)
		if !ok {
			if f, ok2 := wert.(float64); ok2 {
				b, ok = f != 0, true
			}
		}
		return ok && ((*def.AlarmBei == "ein") == b)
	}
	f, ok := wert.(float64)
	if !ok {
		return false
	}
	return (def.Max != nil && f > *def.Max) || (def.Min != nil && f < *def.Min)
}

// lampeBerechnen: true, wenn ein Wert mit „Lampe“ seine Grenze länger als die Dauer verletzt.
func (s *Sensorik) lampeBerechnen(jetzt time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	an := false
	for _, d := range s.defs {
		for _, w := range d.Werte {
			if !w.Lampe {
				continue
			}
			k := fmt.Sprintf("%d|%s", d.ID, w.Schluessel)
			v, ok := s.aktuell[k]
			if !ok || !grenzeVerletzt(w, v) {
				delete(s.verletzt, k)
				continue
			}
			seit, ok := s.verletzt[k]
			if !ok {
				seit = jetzt
				s.verletzt[k] = seit
			}
			if jetzt.Sub(seit) >= time.Duration(w.DauerMin)*time.Minute {
				an = true
			}
		}
	}
	return an
}

// ── Puffer auf Platte (ohne Netz, über einen Neustart hinweg) ───────────────────────────────────────

func (s *Sensorik) pufferSichern() {
	s.mu.Lock()
	daten, _ := json.Marshal(s.puffer)
	s.mu.Unlock()
	_ = os.MkdirAll(sensorOrdner(), 0o700)
	_ = os.WriteFile(filepath.Join(sensorOrdner(), "puffer.json"), daten, 0o600)
}
func (s *Sensorik) pufferLaden() {
	b, err := os.ReadFile(filepath.Join(sensorOrdner(), "puffer.json"))
	if err != nil {
		return
	}
	var p []sensorMessung
	if json.Unmarshal(b, &p) == nil {
		s.mu.Lock()
		s.puffer = append(p, s.puffer...)
		s.mu.Unlock()
	}
}
func (s *Sensorik) defsSichern() {
	s.mu.Lock()
	liste := []*sensorDef{}
	for _, d := range s.defs {
		liste = append(liste, d)
	}
	s.mu.Unlock()
	daten, _ := json.Marshal(liste)
	_ = os.MkdirAll(sensorOrdner(), 0o700)
	_ = os.WriteFile(filepath.Join(sensorOrdner(), "zugaenge.json"), daten, 0o600)
}
func (s *Sensorik) defsLaden() {
	b, err := os.ReadFile(filepath.Join(sensorOrdner(), "zugaenge.json"))
	if err != nil {
		return
	}
	var liste []*sensorDef
	if json.Unmarshal(b, &liste) == nil {
		s.mu.Lock()
		for _, d := range liste {
			s.defs[d.ID] = d
		}
		s.mu.Unlock()
	}
}

// ── KAI ─────────────────────────────────────────────────────────────────────────────────────────────

func (k *KaiClient) GetSensorenConfig() ([]*sensorDef, error) {
	data, code, err := k.do("GET", "/api/edge/sensoren-config", nil)
	if err != nil {
		return nil, err
	}
	if code != 200 {
		return nil, fmt.Errorf("sensoren-config http %d", code)
	}
	var r struct {
		Sensoren []*sensorDef `json:"sensoren"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	return r.Sensoren, nil
}

func (k *KaiClient) PostSensorWerte(werte []sensorMessung, status []sensorStatus) error {
	_, code, err := k.do("POST", "/api/edge/sensoren-werte", map[string]any{"werte": werte, "status": status})
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("sensoren-werte http %d", code)
	}
	return nil
}

func (s *Sensorik) configUebernehmen(liste []*sensorDef) {
	s.mu.Lock()
	neu := map[int]*sensorDef{}
	for _, d := range liste {
		neu[d.ID] = d
	}
	s.defs = neu
	s.geladenAm = time.Now()
	s.mu.Unlock()
	s.defsSichern()
}

func (s *Sensorik) senden(k *KaiClient) {
	s.mu.Lock()
	n := len(s.puffer)
	if n > 2000 {
		n = 2000
	}
	stueck := append([]sensorMessung(nil), s.puffer[:n]...)
	verbunden := map[int]bool{}
	if s.broker != nil {
		s.mu.Unlock()
		verbunden = s.broker.Verbunden()
		s.mu.Lock()
	}
	status := []sensorStatus{}
	for id, st := range s.status {
		c := *st
		if verbunden[id] {
			c.Online = true
		}
		status = append(status, c)
	}
	s.mu.Unlock()
	if len(stueck) == 0 && len(status) == 0 {
		return
	}
	if err := k.PostSensorWerte(stueck, status); err != nil {
		log.Printf("sensoren: senden: %v", err)
		s.pufferSichern()
		return
	}
	s.mu.Lock()
	s.puffer = s.puffer[len(stueck):]
	s.gesendet = time.Now()
	leer := len(s.puffer) == 0
	s.mu.Unlock()
	if leer {
		_ = os.Remove(filepath.Join(sensorOrdner(), "puffer.json"))
	}
}

// ── Firmware für ESPHome-Sensoren im Bord-Netz anbieten ─────────────────────────────────────────────

func (s *Sensorik) firmwareHolen(k *KaiClient) {
	s.mu.Lock()
	ziele := []*sensorDef{}
	for _, d := range s.defs {
		if d.Firmware != nil && d.Firmware.Version != "" && s.fwStand[d.ID] != d.Firmware.Version {
			ziele = append(ziele, d)
		}
	}
	s.mu.Unlock()
	for _, d := range ziele {
		ordner := filepath.Join(sensorOrdner(), "fw", d.User)
		if b, err := os.ReadFile(filepath.Join(ordner, "version")); err == nil && strings.TrimSpace(string(b)) == d.Firmware.Version {
			s.mu.Lock()
			s.fwStand[d.ID] = d.Firmware.Version
			s.mu.Unlock()
			continue
		}
		data, code, err := k.do("GET", d.Firmware.Pfad, nil)
		if err != nil || code != 200 || len(data) < 1024 {
			log.Printf("sensoren: Firmware %s nicht geladen (%v, http %d)", d.User, err, code)
			continue
		}
		_ = os.MkdirAll(ordner, 0o755)
		sum := md5.Sum(data)
		_ = os.WriteFile(filepath.Join(ordner, "firmware.ota.bin"), data, 0o644)
		manifest, _ := json.Marshal(map[string]any{
			"name": d.Name, "version": d.Firmware.Version, "home_assistant_domain": "esphome", "new_install_prompt_erase": false,
			"builds": []map[string]any{{"chipFamily": chipFamilie(d.Firmware.Chip),
				"ota": map[string]any{"path": "firmware.ota.bin", "md5": hex.EncodeToString(sum[:]), "summary": "KAI " + d.Firmware.Version}}},
		})
		_ = os.WriteFile(filepath.Join(ordner, "manifest.json"), manifest, 0o644)
		_ = os.WriteFile(filepath.Join(ordner, "version"), []byte(d.Firmware.Version), 0o644)
		s.mu.Lock()
		s.fwStand[d.ID] = d.Firmware.Version
		b := s.broker
		s.mu.Unlock()
		logEvent("info", "Neue Firmware für Sensor "+d.Name+": "+d.Firmware.Version)
		if b != nil {
			// ESPHome: Knopf „Firmware aktualisieren“ aus der KAI-Vorlage drücken
			b.Senden(d.Praefix+"/button/firmware_aktualisieren/command", []byte("PRESS"), false)
		}
	}
}

func chipFamilie(c string) string {
	if c == "ESP8266" {
		return "ESP8266"
	}
	return "ESP32"
}

var fwPfad = regexp.MustCompile(`^/fw/([a-z0-9-]+)/(manifest\.json|firmware\.ota\.bin)$`)

func (a *App) sensorRoutes(mux interface {
	HandleFunc(string, func(http.ResponseWriter, *http.Request))
}) {
	// Ohne Anmeldung: der Sensor holt die Firmware im Bord-Netz (enthält keine Zugangsdaten außer seinen eigenen)
	mux.HandleFunc("/fw/", func(w http.ResponseWriter, r *http.Request) {
		m := fwPfad.FindStringSubmatch(r.URL.Path)
		if m == nil {
			http.NotFound(w, r)
			return
		}
		f, err := os.Open(filepath.Join(sensorOrdner(), "fw", m[1], m[2]))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		if strings.HasSuffix(m[2], ".json") {
			w.Header().Set("Content-Type", "application/json")
		} else {
			w.Header().Set("Content-Type", "application/octet-stream")
		}
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.Copy(w, f)
	})
}

// sensorLoop: MQTT-Empfänger starten, Zugänge alle 60 s abgleichen, Werte alle 10 s senden, Lampe prüfen.
func (a *App) sensorLoop() {
	s := sensorik
	s.defsLaden()
	s.pufferLaden()
	s.broker = neuerMQTTBroker(s.pruefeZugang, s.eingang)
	port := os.Getenv("BORDWAECHTER_MQTT_PORT") // im Add-on 1885 (Mosquitto von Home Assistant belegt 1883 und 1884)
	if port == "" {
		port = "1883"
	}
	if err := s.broker.Starten(":" + port); err != nil {
		log.Printf("sensoren: MQTT-Empfänger startet nicht: %v", err)
		logEvent("error", "MQTT-Empfänger für Sensoren startet nicht: "+err.Error())
	}
	time.Sleep(15 * time.Second)
	letzteConfig := time.Time{}
	for {
		k := a.getKai()
		cfg := a.getCfg()
		if k != nil && cfg.EdgeToken != "" {
			if time.Since(letzteConfig) > 60*time.Second {
				if liste, err := k.GetSensorenConfig(); err == nil {
					s.configUebernehmen(liste)
					letzteConfig = time.Now()
					go s.firmwareHolen(k)
				}
			}
			s.senden(k)
		}
		an := s.lampeBerechnen(time.Now())
		s.mu.Lock()
		neu := an && !s.lampeAn
		s.lampeAn = an
		s.mu.Unlock()
		a.eng.SetSensorLampe(an)
		if neu {
			a.eng.TestPulse("horn", 3) // kurz hupen, wenn ein Sensoralarm beginnt
			logEvent("warn", "Sensor-Grenzwert verletzt – Lampe an")
		}
		time.Sleep(10 * time.Second)
	}
}
