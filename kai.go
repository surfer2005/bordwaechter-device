package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Leg / Bundle spiegeln die Antwort von GET /api/edge/bundle (server/src/kiosk-gps.js edgeBundle()).
type Leg struct {
	From     string      `json:"from"`
	To       string      `json:"to"`
	Dep      string      `json:"dep"` // "HH:MM" oder ""
	Arr      string      `json:"arr"`
	Dist     *float64    `json:"dist"`
	Done     bool        `json:"done"`
	Geometry        [][2]float64 `json:"geometry"`         // [lon,lat]
	GeometryReverse [][2]float64 `json:"geometry_reverse"` // Gegenrichtung (blasse Linie)
	LengthKm        *float64     `json:"length_km"`
	SpreadM  *float64    `json:"spread_m"` // gelernte Korridor-Breite (für adaptive Kurs-Grenze)
}

type Bundle struct {
	Ship struct {
		ID                 int    `json:"id"`
		Name               string `json:"name"`
		RegistrationNumber string `json:"registration_number"`
	} `json:"ship"`
	Date       string `json:"date"`
	Thresholds struct {
		OffCourseM      float64 `json:"offCourseM"`      // abwärtskompatibel (alt), ab v0.1.19 ungenutzt
		OffCourseFactor float64 `json:"offCourseFactor"` // abwärtskompatibel (alt), ab v0.1.19 ungenutzt
		OffCoursePortM  float64 `json:"offCoursePortM"`
		ManeuverKmh     float64 `json:"maneuverKmh"`
		HafenRadiusM    float64 `json:"hafenRadiusM"`    // Zonen-Ampel: Radius Hafen-Zone
		HafenOrangeM    float64 `json:"hafenOrangeM"`    // Hafen: orange ab
		HafenRotM       float64 `json:"hafenRotM"`       // Hafen: rot ab
		SeeOrangeM      float64 `json:"seeOrangeM"`      // offene See: orange ab
		SeeRotM         float64 `json:"seeRotM"`         // offene See: rot ab
		PosStaleS       float64 `json:"posStaleS"`
		LateGraceMin    float64 `json:"lateGraceMin"`
		ToleranceKmh    float64 `json:"toleranceKmh"`
		MaxKmh          float64 `json:"maxKmh"`
		ArrivalRadiusM  float64 `json:"arrivalRadiusM"`
		NextLogbookHintMin float64 `json:"nextLogbookHintMin"`
		Unit            string  `json:"unit"`
	} `json:"thresholds"`
	HatBordbuch bool   `json:"hatBordbuch"`
	Haefen      []Hafen `json:"haefen"` // Häfen/Landestellen mit Hafenkreis (SIFA/Kamera-Alarm ruht darin)
	Legs        []Leg  `json:"legs"`
	GeneratedAt string `json:"generated_at"`
	Error       string `json:"error"`
}

// Hafen: Mittelpunkt + Radius des Hafenkreises (Radius je Hafen in KAI einstellbar, sonst Vorgabe).
type Hafen struct {
	Name    string  `json:"name"`
	Lat     float64 `json:"lat"`
	Lng     float64 `json:"lng"`
	RadiusM float64 `json:"radius_m"`
}

type KaiClient struct {
	base  string
	token string
	hc    *http.Client
}

func newKaiClient(base, token string) *KaiClient {
	// Tolerant gegen die häufige Fehleingabe: der Nutzer kopiert die in KAI angezeigte „Gerät-Adresse"
	// (…/api/edge) ins Adressfeld, obwohl das Gerät /api/edge SELBST anhängt. Doppeltes Suffix abschneiden.
	b := strings.TrimRight(base, "/")
	for _, suf := range []string{"/api/edge", "/api"} {
		if strings.HasSuffix(b, suf) {
			b = strings.TrimRight(strings.TrimSuffix(b, suf), "/")
			break
		}
	}
	return &KaiClient{
		base:  b,
		token: token,
		hc:    &http.Client{Timeout: 20 * time.Second},
	}
}

func (k *KaiClient) do(method, path string, body any) ([]byte, int, error) {
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, k.base+path, r)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("X-Edge-Token", k.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := k.hc.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return data, resp.StatusCode, nil
}

// GetAuftrag: schlanke Nachfrage, ob in KAI ein Auftrag für dieses Gerät wartet (z. B. Speedtest).
// KAI kann das Gerät nicht selbst ansprechen (nur ausgehende Verbindungen) — deshalb fragt das Gerät
// alle paar Sekunden nach, statt bis zum nächsten Herzschlag zu warten.
type Auftrag struct {
	Speedtest bool `json:"speedtest"`
	Update    bool `json:"update"` // Aktualisierung aus KAI angefordert
	ConfigRev int         `json:"config_rev"` // Stand der Einstellungen in KAI (s. kaiconfig.go)
	Befehl    *kaiBefehlT `json:"befehl"`     // Testschalter aus KAI (Hupe/Lampe/GPIO-Test/Quittieren)
}

func (k *KaiClient) GetAuftrag() (Auftrag, error) {
	var r Auftrag
	data, code, err := k.do("GET", "/api/edge/auftrag", nil)
	if err != nil {
		return r, err
	}
	if code != 200 {
		return r, fmt.Errorf("HTTP %d", code)
	}
	err = json.Unmarshal(data, &r)
	return r, err
}

// UpdateGestartet meldet KAI, dass die angeforderte Aktualisierung jetzt läuft (KAI nimmt das Flag zurück).
func (k *KaiClient) UpdateGestartet() { _, _, _ = k.do("POST", "/api/edge/update-gestartet", map[string]any{}) }

func (k *KaiClient) GetBundle(shipID int) (*Bundle, error) {
	p := "/api/edge/bundle"
	if shipID > 0 {
		p = fmt.Sprintf("%s?ship=%d", p, shipID)
	}
	data, code, err := k.do("GET", p, nil)
	if err != nil {
		return nil, err
	}
	if code != 200 {
		return nil, fmt.Errorf("bundle http %d: %s", code, string(data))
	}
	var b Bundle
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, err
	}
	if b.Error != "" {
		return nil, fmt.Errorf("bundle: %s", b.Error)
	}
	return &b, nil
}

// Streckennetz + Anlegestellen für den Standalone-/Offline-Modus (wird gecacht).
type NetSegment struct {
	From   string       `json:"from"`
	To     string       `json:"to"`
	Km     float64      `json:"km"`
	Points [][2]float64 `json:"points"` // [lon,lat]
}
type NetworkData struct {
	Segments []NetSegment `json:"segments"`
}

func (k *KaiClient) GetNetwork() (*NetworkData, error) {
	data, code, err := k.do("GET", "/api/edge/network", nil)
	if err != nil {
		return nil, err
	}
	if code != 200 {
		return nil, fmt.Errorf("network http %d", code)
	}
	var n NetworkData
	if err := json.Unmarshal(data, &n); err != nil {
		return nil, err
	}
	return &n, nil
}

// Hello ist der Herzschlag an KAI: meldet die Version und — sofern vorhanden — den system-Block
// (CPU-Temp/Last/RAM/Platte/Laufzeit), damit KAI die Gerätegesundheit zentral anzeigen kann. Der
// Block wird ohnehin für die HA-Integration gemessen (system.go), hier reist er nur mit.
type HelloResp struct {
	OK              bool `json:"ok"`
	ReportScan      bool `json:"report_scan"`      // Netz-Scan-Schalter aus KAI (zentral umlegbar)
	ReportTeltonika bool `json:"report_teltonika"` // Teltonika-Router-Diagnose an/aus (zentral)
	SpeedtestRequested bool `json:"speedtest_requested"` // im KAI-Monitoring angeforderter Speedtest
	LatestVersion      string `json:"latest_version"`      // neueste Release-Version (KAI fragt GitHub zentral)
	UpdateRequested    bool   `json:"update_requested"`    // Aktualisierung aus KAI angefordert
	ConfigRev          int    `json:"config_rev"`          // Stand der Einstellungen in KAI
	BordToken          string `json:"bord_token"`          // Lese-Schlüssel für die Position im Bord-Netz
	Rolle              string `json:"rolle"`               // Rolle aus KAI (bordwaechter | bildschirm | audio)
	Medien             *struct {
		Pfad string `json:"pfad"`
	} `json:"medien"` // Bildschirm/Bordmusik: Abspielseite
	Bordwaechter *struct {
		URL   string `json:"url"`
		Token string `json:"token"`
	} `json:"bordwaechter"` // Bildschirm/Bordmusik: Bordwächter desselben Schiffs
}

// Hello: Herzschlag an KAI. gpsHost/gpsPort werden mitgemeldet (für die Teltonika-Übersicht / Router-IP),
// sifa (optional) trägt den SIFA-/Totmann-Zustand (Hupe/Leuchttaster/Quittier-Taster + Deadman-Countdown).
func (k *KaiClient) Hello(version, kind string, sys *SystemData, gpsHost string, gpsPort int, sifa, anzeige map[string]any) (*HelloResp, error) {
	body := map[string]any{"version": version, "kind": kind}
	if anzeige != nil {
		body["anzeige"] = anzeige // Anzeige am Monitor: eingerichtet/läuft/Ansicht/Drehung
	}
	if sys != nil {
		body["system"] = sys
	}
	if gpsHost != "" {
		body["gps_host"] = gpsHost
		body["gps_port"] = gpsPort
	}
	if ip := localIPv4(); ip != "" {
		body["lan_ip"] = ip // echte LAN-Adresse des Geräts (statt der Verbindungs-Quelle 127.0.0.1 in KAI)
	}
	if sifa != nil {
		body["sifa"] = sifa
	}
	data, _, err := k.do("POST", "/api/edge/hello", body)
	if err != nil {
		return nil, err
	}
	var hr HelloResp
	_ = json.Unmarshal(data, &hr)
	return &hr, nil
}

// PostLog meldet gepufferte Geräte-Protokoll-Einträge an KAI.
func (k *KaiClient) PostLog(entries []logEntry) error {
	_, code, err := k.do("POST", "/api/edge/log", map[string]any{"entries": entries})
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("log http %d", code)
	}
	return nil
}

// GetTeltonikaConfig holt die Teltonika-Registerkarte aus KAI (zentral gepflegt). Leere/Fehler-Antwort
// → der Aufrufer nimmt die eingebaute Liste als Rückfall (autark).
func (k *KaiClient) GetTeltonikaConfig() (teltonikaCfg, error) {
	data, code, err := k.do("GET", "/api/edge/teltonika-config", nil)
	if err != nil {
		return teltonikaCfg{}, err
	}
	if code != 200 {
		return teltonikaCfg{}, fmt.Errorf("teltonika-config http %d", code)
	}
	var resp struct {
		Host      string `json:"host"`
		Port      int    `json:"port"`
		Registers []struct {
			Measure string  `json:"measure"`
			Label   string  `json:"label"`
			Address int     `json:"address"`
			Count   int     `json:"count"`
			Type    string  `json:"type"`
			Unit    string  `json:"unit"`
			Scale   float64 `json:"scale"`
		} `json:"registers"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return teltonikaCfg{}, err
	}
	out := make([]tRegDef, 0, len(resp.Registers))
	for _, r := range resp.Registers {
		cnt := r.Count
		if cnt <= 0 {
			if r.Type == "uint16" {
				cnt = 1
			} else {
				cnt = 2
			}
		}
		out = append(out, tRegDef{addr: r.Address, count: cnt, typ: r.Type, scale: r.Scale, measure: r.Measure, label: r.Label, unit: r.Unit})
	}
	return teltonikaCfg{Regs: out, Host: resp.Host, Port: resp.Port}, nil
}

func (k *KaiClient) PostPosition(p Position) error {
	_, code, err := k.do("POST", "/api/edge/position", map[string]any{
		"lat": p.Lat, "lng": p.Lng, "speed_kmh": p.SpeedKmh, "heading": p.Heading,
		"recorded_at": p.TS.UTC().Format(time.RFC3339),
	})
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("position http %d", code)
	}
	return nil
}

func (k *KaiClient) PostEvent(kind, state, detail string, p Position, offM float64, snapshotB64 string) error {
	body := map[string]any{
		"kind": kind, "state": state, "detail": detail,
		"occurred_at": time.Now().UTC().Format(time.RFC3339),
	}
	if p.Valid {
		body["lat"] = p.Lat
		body["lng"] = p.Lng
	}
	if offM > 0 {
		body["off_course_m"] = offM
	}
	if snapshotB64 != "" {
		body["snapshot_base64"] = snapshotB64
	}
	_, code, err := k.do("POST", "/api/edge/event", body)
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("event http %d", code)
	}
	return nil
}
