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
	Geometry [][2]float64 `json:"geometry"` // [lon,lat]
	LengthKm *float64    `json:"length_km"`
}

type Bundle struct {
	Ship struct {
		ID                 int    `json:"id"`
		Name               string `json:"name"`
		RegistrationNumber string `json:"registration_number"`
	} `json:"ship"`
	Date       string `json:"date"`
	Thresholds struct {
		OffCourseM     float64 `json:"offCourseM"`
		ToleranceKmh   float64 `json:"toleranceKmh"`
		MaxKmh         float64 `json:"maxKmh"`
		ArrivalRadiusM float64 `json:"arrivalRadiusM"`
		Unit           string  `json:"unit"`
	} `json:"thresholds"`
	HatBordbuch bool   `json:"hatBordbuch"`
	Legs        []Leg  `json:"legs"`
	GeneratedAt string `json:"generated_at"`
	Error       string `json:"error"`
}

type KaiClient struct {
	base  string
	token string
	hc    *http.Client
}

func newKaiClient(base, token string) *KaiClient {
	return &KaiClient{
		base:  strings.TrimRight(base, "/"),
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

func (k *KaiClient) Hello(version string) error {
	_, _, err := k.do("POST", "/api/edge/hello", map[string]any{"version": version})
	return err
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
