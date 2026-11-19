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
	Force   string `json:"force"` // Token (Zeitstempel) aus KAI: neuer Wert = OTA erzwingen, auch bei gleicher Version
}
type sensorDef struct {
	ID        int             `json:"id"`
	Art       string          `json:"art"`
	Name      string          `json:"name"`
	User      string          `json:"user"`
	Salt      string          `json:"salt"`
	Hash      string          `json:"hash"`
	Praefix   string          `json:"praefix"`
	Werte     []sensorWertDef `json:"werte"`
	Firmware  *sensorFirmware `json:"firmware"`
	Schaltbar bool            `json:"schaltbar"` // Shelly mit Relais: aus KAI schaltbar (Steckdosen)
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
	fwStand   map[int]string   // installierte Firmware-Datei je Sensor (Version)
	fwForce   map[int]string   // zuletzt ausgeführtes OTA-Force-Token je Sensor (KAI-Knopf)
	logs      map[int][]string // ESPHome-Log je Sensor (über MQTT <praefix>/debug), Ringpuffer → an KAI
	broker    *MQTTBroker
	gemeinsam *brokerZugang // MQTT-B: EIN geteilter Broker-Zugang je Bordwächter (HA-Modell), zusätzlich zu den Pro-Sensor-Zugängen
	lampeAn   bool
	gesendet  time.Time
	geladenAm time.Time
}

// brokerZugang: der geteilte MQTT-Zugang (HA-Modell). Das Gerät bekommt nur Benutzer + Salt + Hash.
type brokerZugang struct {
	User string `json:"user"`
	Salt string `json:"salt"`
	Hash string `json:"hash"`
}

var sensorik = &Sensorik{defs: map[int]*sensorDef{}, aktuell: map[string]any{}, verletzt: map[string]time.Time{}, status: map[int]*sensorStatus{}, fwStand: map[int]string{}, fwForce: map[int]string{}, logs: map[int][]string{}}

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
	// MQTT-B: geteilter Zugang (HA-Modell) — darf jedes Sensor-Thema. Zusätzlich zu den Pro-Sensor-Zugängen oben.
	if g := s.gemeinsam; g != nil && g.User == user && g.Hash != "" {
		h := sha256.Sum256([]byte(g.Salt + pass))
		if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(h[:])), []byte(g.Hash)) == 1 {
			return mqttZugang{Alle: true}, true
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
		// Power Strip/Plug: Zählerstand (Wh), Strom, Spannung je Kanal
		if e, ok := obj["aenergy"].(map[string]any); ok {
			if f, ok := e["total"].(float64); ok {
				out = append(out, abgeleitet{mitIndex("energie", n), f})
			}
		}
		if f, ok := zahl("current"); ok {
			out = append(out, abgeleitet{mitIndex("strom", n), f})
		}
		if f, ok := zahl("voltage"); ok {
			out = append(out, abgeleitet{mitIndex("spannung", n), f})
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
	if d == nil && z.Alle {
		d = s.defFuerThema(topic) // gemeinsamer Zugang: Sensor über sein Thema finden
	}
	if d == nil {
		if z.Alle { // angemeldet, aber das Thema gehört zu keinem Sensor in KAI — sonst verschwänden die Werte spurlos
			kopf := strings.SplitN(topic, "/", 2)[0]
			gedrosseltMelden("ohne|"+kopf, 6*time.Hour, "warn", "Nachricht ohne passenden Sensor in KAI: Thema "+topic+" (Präfix „"+kopf+"“ ist bei keinem Sensor eingetragen)")
		}
		return
	}
	// ESPHome-Log über MQTT (<praefix>/debug) sammeln — für die Live-Anzeige in KAI, kein Messwert.
	if strings.HasSuffix(topic, "/debug") {
		zeile := strings.TrimRight(string(payload), "\r\n")
		if zeile != "" {
			s.logs[d.ID] = append(s.logs[d.ID], zeile)
			if len(s.logs[d.ID]) > 100 {
				s.logs[d.ID] = s.logs[d.ID][len(s.logs[d.ID])-100:]
			}
		}
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

// defFuerThema: Sensor, unter dessen Thema (Präfix) die Nachricht liegt; bei mehreren der längste Präfix.
// Aufrufer hält s.mu.
func (s *Sensorik) defFuerThema(topic string) *sensorDef {
	var best *sensorDef
	for _, d := range s.defs {
		if darfThema(d.Praefix, topic) && (best == nil || len(d.Praefix) > len(best.Praefix)) {
			best = d
		}
	}
	return best
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

func (k *KaiClient) GetSensorenConfig() ([]*sensorDef, *brokerZugang, error) {
	data, code, err := k.do("GET", "/api/edge/sensoren-config", nil)
	if err != nil {
		return nil, nil, err
	}
	if code != 200 {
		return nil, nil, fmt.Errorf("sensoren-config http %d", code)
	}
	var r struct {
		Sensoren []*sensorDef  `json:"sensoren"`
		Broker   *brokerZugang `json:"broker"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, nil, err
	}
	return r.Sensoren, r.Broker, nil
}

// MQTT-B: geteilten Zugang übernehmen + für den Offline-Betrieb auf die Platte sichern.
func (s *Sensorik) gemeinsamUebernehmen(b *brokerZugang) {
	s.mu.Lock()
	s.gemeinsam = b
	s.mu.Unlock()
	daten, _ := json.Marshal(b)
	_ = os.MkdirAll(sensorOrdner(), 0o700)
	_ = os.WriteFile(filepath.Join(sensorOrdner(), "broker.json"), daten, 0o600)
}
func (s *Sensorik) gemeinsamLaden() {
	b, err := os.ReadFile(filepath.Join(sensorOrdner(), "broker.json"))
	if err != nil {
		return
	}
	var g brokerZugang
	if json.Unmarshal(b, &g) == nil && g.User != "" {
		s.mu.Lock()
		s.gemeinsam = &g
		s.mu.Unlock()
	}
}

type sensorLog struct {
	SensorID int      `json:"sensor_id"`
	Zeilen   []string `json:"zeilen"`
}

func (k *KaiClient) PostSensorWerte(werte []sensorMessung, status []sensorStatus, logs []sensorLog) error {
	_, code, err := k.do("POST", "/api/edge/sensoren-werte", map[string]any{"werte": werte, "status": status, "logs": logs})
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

// sendenMu: senden läuft aus dem 10-s-Takt UND sofort nach einem Schaltbefehl — nie gleichzeitig,
// sonst würde derselbe Pufferanfang zweimal geschickt und zweimal entfernt.
var sendenMu sync.Mutex

func (s *Sensorik) senden(k *KaiClient) {
	sendenMu.Lock()
	defer sendenMu.Unlock()
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
		// Ehrlicher Verbindungsstand: online = derzeit am Broker verbunden. So erkennt KAI einen Abbruch sofort
		// (z. B. nach MQTT-Kennwortwechsel), statt „online" nach der ersten Nachricht dauerhaft true zu lassen.
		c.Online = verbunden[id]
		status = append(status, c)
	}
	logs := []sensorLog{}
	for id, zl := range s.logs {
		if len(zl) > 0 {
			logs = append(logs, sensorLog{SensorID: id, Zeilen: append([]string(nil), zl...)})
		}
	}
	s.mu.Unlock()
	if len(stueck) == 0 && len(status) == 0 && len(logs) == 0 {
		return
	}
	if err := k.PostSensorWerte(stueck, status, logs); err != nil {
		log.Printf("sensoren: senden: %v", err)
		s.pufferSichern()
		return
	}
	s.mu.Lock()
	for _, l := range logs { // nur die gesendeten Zeilen vorne entfernen (zwischenzeitlich eingegangene bleiben)
		if c := len(l.Zeilen); len(s.logs[l.SensorID]) >= c {
			s.logs[l.SensorID] = s.logs[l.SensorID][c:]
		} else {
			s.logs[l.SensorID] = nil
		}
	}
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
	forced := map[int]bool{}
	for _, d := range s.defs {
		if d.Firmware == nil || d.Firmware.Version == "" {
			continue
		}
		erzwingen := d.Firmware.Force != "" && s.fwForce[d.ID] != d.Firmware.Force
		if s.fwStand[d.ID] != d.Firmware.Version || erzwingen {
			ziele = append(ziele, d)
			forced[d.ID] = erzwingen
		}
	}
	s.mu.Unlock()
	for _, d := range ziele {
		ordner := filepath.Join(sensorOrdner(), "fw", d.User)
		// Bei erzwungenem OTA (KAI-Knopf) NICHT über den Datei-Stand abkürzen — erneut aufspielen, auch bei gleicher Version.
		if !forced[d.ID] {
			if b, err := os.ReadFile(filepath.Join(ordner, "version")); err == nil && strings.TrimSpace(string(b)) == d.Firmware.Version {
				s.mu.Lock()
				s.fwStand[d.ID] = d.Firmware.Version
				s.fwForce[d.ID] = d.Firmware.Force
				s.mu.Unlock()
				continue
			}
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
		s.fwForce[d.ID] = d.Firmware.Force
		b := s.broker
		s.mu.Unlock()
		if forced[d.ID] {
			logEvent("info", "OTA erzwungen für Sensor "+d.Name+" ("+d.Firmware.Version+")")
		} else {
			logEvent("info", "Neue Firmware für Sensor "+d.Name+": "+d.Firmware.Version)
		}
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
	s.gemeinsamLaden()
	s.pufferLaden()
	s.broker = neuerMQTTBroker(s.pruefeZugang, s.eingang)
	s.broker.zuordnen = func(topic string) int {
		s.mu.Lock()
		defer s.mu.Unlock()
		if d := s.defFuerThema(topic); d != nil {
			return d.ID
		}
		return 0
	}
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
				if liste, broker, err := k.GetSensorenConfig(); err == nil {
					s.configUebernehmen(liste)
					s.gemeinsamUebernehmen(broker)
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

// ── Steckdosen schalten (wie beim DALI-Licht) ──────────────────────────────────────────────────────

type sensorBefehl struct {
	ID       int    `json:"id"`
	SensorID int    `json:"sensor_id"`
	Art      string `json:"art"` // "" / schalten = Switch.Set über MQTT · mqtt_steuerung = am Shelly per HTTP einschalten (0.1.138)
	Praefix  string `json:"praefix"`
	Kanal    int    `json:"kanal"`
	An       bool   `json:"an"`
	IP       string `json:"ip"`
	Neustart bool   `json:"neustart"`
}
type sensorQuittung struct {
	ID      int    `json:"id"`
	OK      bool   `json:"ok"`
	Fehler  string `json:"fehler,omitempty"`
	Antwort string `json:"antwort,omitempty"`
}

// hatShelly: irgendein Shelly eingetragen — dann fragt der Bordwächter (seltener) auch nach Gerätebefehlen wie
// „MQTT-Steuerung einschalten“, selbst wenn noch keine Steckdose als schaltbar gilt.
func (s *Sensorik) hatShelly() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.defs {
		if d.Art == "shelly" {
			return true
		}
	}
	return false
}

func (s *Sensorik) hatSchaltbare() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.defs {
		if d.Schaltbar {
			return true
		}
	}
	return false
}

// shellySchaltNachricht: RPC Switch.Set an "<praefix>/rpc" (Shelly Gen2+ abonniert das Thema, wenn
// „MQTT-Steuerung" eingeschaltet ist). Die Antwort geht an "kai-bordwaechter/rpc" und wird verworfen —
// den neuen Zustand meldet der Shelly ohnehin über status/… bzw. events/rpc.
func shellySchaltNachricht(id, kanal int, an bool) []byte {
	b, _ := json.Marshal(map[string]any{"id": id, "src": "kai-bordwaechter", "method": "Switch.Set", "params": map[string]any{"id": kanal, "on": an}})
	return b
}

// schaltBefehle: einmal Befehle abholen (mit Quittungen) und ausführen. Rückgabe: ob geschaltet wurde.
func (s *Sensorik) schaltBefehle(k *KaiClient, quittung *[]sensorQuittung) bool {
	data, code, err := k.do("POST", "/api/edge/sensor-befehle", map[string]any{"erledigt": *quittung})
	if err != nil || code != 200 {
		return false
	}
	*quittung = nil
	var r struct {
		Befehle []sensorBefehl `json:"befehle"`
	}
	if json.Unmarshal(data, &r) != nil || len(r.Befehle) == 0 {
		return false
	}
	verbunden := map[int]bool{}
	if s.broker != nil {
		verbunden = s.broker.Verbunden()
	}
	for _, b := range r.Befehle {
		if b.Art == "mqtt_steuerung" {
			*quittung = append(*quittung, mqttSteuerungBefehl(b))
			continue
		}
		if b.Art != "" && b.Art != "schalten" {
			*quittung = append(*quittung, sensorQuittung{ID: b.ID, Fehler: "Befehl „" + b.Art + "“ kennt dieses Bordgerät nicht"})
			continue
		}
		q := sensorQuittung{ID: b.ID, OK: true}
		switch {
		case s.broker == nil || b.Praefix == "":
			q.OK, q.Fehler = false, "MQTT-Empfänger nicht bereit"
		case !verbunden[b.SensorID]:
			q.OK, q.Fehler = false, "Gerät ist nicht mit dem Bordwächter verbunden"
		default:
			s.broker.Senden(b.Praefix+"/rpc", shellySchaltNachricht(b.ID, b.Kanal, b.An), false)
			zustand := "aus"
			if b.An {
				zustand = "ein"
			}
			logEvent("info", fmt.Sprintf("Steckdose geschaltet: %s Kanal %d %s", b.Praefix, b.Kanal, zustand))
		}
		*quittung = append(*quittung, q)
	}
	return true
}

// sensorBefehlLoop: fragt KAI im Halbsekundentakt nach Schaltbefehlen — nur, wenn es schaltbare Sensoren
// gibt (sonst kein Leerlauf-Verkehr). Nach dem Schalten Quittung + frischen Stand sofort an KAI, statt
// erst im 10-s-Takt (Peter 2026-11-16: „wie beim DALI, ganz schnell").
func (a *App) sensorBefehlLoop() {
	time.Sleep(30 * time.Second)
	s := sensorik
	var quittung []sensorQuittung
	var letzteShellyFrage time.Time
	for {
		if s.hatSchaltbare() || len(quittung) > 0 || (s.hatShelly() && time.Since(letzteShellyFrage) > 5*time.Second) {
			letzteShellyFrage = time.Now()
			if k := a.getKai(); k != nil && a.getCfg().EdgeToken != "" {
				if s.schaltBefehle(k, &quittung) {
					time.Sleep(700 * time.Millisecond) // Shelly schaltet und meldet seinen Stand
					s.schaltBefehle(k, &quittung)      // Quittung sofort abliefern
					s.senden(k)
				}
			}
			time.Sleep(500 * time.Millisecond)
		} else {
			time.Sleep(5 * time.Second)
		}
	}
}
