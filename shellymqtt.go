package main

// „MQTT-Steuerung einschalten“ (KAI-Bordgerät 0.1.138, Peter 2026-11-17 „Weg 2“): ohne die MQTT-Steuerung meldet ein Shelly
// zwar seine Werte, nimmt aber keine Schaltbefehle (Switch.Set über <praefix>/rpc) an. KAI schickt dafür einen Gerätebefehl
// mit der IP eines in KAI eingetragenen Shelly; der Bordwächter ruft dort im Schiffsnetz MQTT.SetConfig mit enable_rpc +
// enable_control auf und auf Wunsch Shelly.Reboot. Ziel ist nur eine private Adresse — die IP kommt aus KAI, nicht frei.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

var shellyHTTP = &http.Client{Timeout: 6 * time.Second}

// istBordAdresse: nur private IPv4-Adressen im Bord-/Landestellennetz (10/8, 172.16/12, 192.168/16).
func istBordAdresse(ip string) bool {
	p := net.ParseIP(ip)
	if p == nil || p.To4() == nil || p.IsLoopback() {
		return false
	}
	return p.IsPrivate()
}

type shellyFehler struct {
	Code    int
	Meldung string
}

func (e *shellyFehler) Error() string { return e.Meldung }

// shellyRPC: JSON-RPC über HTTP an „<basis>/rpc“ (Shelly Gen2+). 401 = Kennwort am Shelly.
func shellyRPC(basis, methode string, params any) (json.RawMessage, error) {
	body, _ := json.Marshal(map[string]any{"id": 1, "src": "kai-bordwaechter", "method": methode, "params": params})
	resp, err := shellyHTTP.Post(basis+"/rpc", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, &shellyFehler{Meldung: "Shelly nicht erreichbar"}
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, &shellyFehler{Code: 401, Meldung: "Shelly verlangt ein Kennwort — MQTT-Steuerung am Shelly selbst einschalten (Einstellungen → MQTT)"}
	case resp.StatusCode == http.StatusNotFound:
		return nil, &shellyFehler{Code: 404, Meldung: "kein Shelly der Generation 2 oder neuer (keine RPC-Schnittstelle)"}
	case resp.StatusCode != http.StatusOK:
		return nil, &shellyFehler{Code: resp.StatusCode, Meldung: fmt.Sprintf("Shelly antwortet mit %d", resp.StatusCode)}
	}
	var r struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &r) != nil {
		return nil, &shellyFehler{Meldung: "unverständliche Antwort vom Shelly"}
	}
	if r.Error != nil {
		return nil, &shellyFehler{Code: r.Error.Code, Meldung: "Shelly: " + r.Error.Message}
	}
	return r.Result, nil
}

// shellyMqttSteuerungAn: Stand lesen, falls nötig enable_rpc + enable_control einschalten, auf Wunsch neu starten.
// Rückgabe: Klartext für KAI (Quittung „antwort“).
func shellyMqttSteuerungAn(basis string, neustart bool) (string, error) {
	var cfg struct {
		Enable        *bool `json:"enable"`
		EnableRPC     *bool `json:"enable_rpc"`
		EnableControl *bool `json:"enable_control"`
	}
	if raw, err := shellyRPC(basis, "MQTT.GetConfig", map[string]any{}); err != nil {
		return "", err
	} else {
		_ = json.Unmarshal(raw, &cfg)
	}
	hinweis := ""
	if cfg.Enable != nil && !*cfg.Enable {
		hinweis = " — Achtung: MQTT selbst ist am Shelly aus (Server/Zugang dort eintragen)"
	}
	if cfg.EnableRPC != nil && *cfg.EnableRPC && cfg.EnableControl != nil && *cfg.EnableControl {
		return "MQTT-Steuerung war schon eingeschaltet" + hinweis, nil
	}
	raw, err := shellyRPC(basis, "MQTT.SetConfig", map[string]any{"config": map[string]any{"enable_rpc": true, "enable_control": true}})
	if err != nil {
		return "", err
	}
	var set struct {
		RestartRequired bool `json:"restart_required"`
	}
	_ = json.Unmarshal(raw, &set)
	if !set.RestartRequired {
		return "MQTT-Steuerung eingeschaltet" + hinweis, nil
	}
	if !neustart {
		return "MQTT-Steuerung eingeschaltet — wirkt nach dem nächsten Neustart des Shelly" + hinweis, nil
	}
	if _, err := shellyRPC(basis, "Shelly.Reboot", map[string]any{"delay_ms": 1000}); err != nil {
		return "", &shellyFehler{Meldung: "MQTT-Steuerung eingeschaltet, Neustart fehlgeschlagen: " + err.Error()}
	}
	return "MQTT-Steuerung eingeschaltet, Shelly startet neu" + hinweis, nil
}

// mqttSteuerungBefehl: Befehl aus KAI ausführen (nur Bordnetz-Adresse) → Quittung.
func mqttSteuerungBefehl(b sensorBefehl) sensorQuittung {
	q := sensorQuittung{ID: b.ID}
	if !istBordAdresse(b.IP) {
		q.Fehler = "Adresse ist nicht im Bordnetz — abgelehnt"
		return q
	}
	antwort, err := shellyMqttSteuerungAn("http://"+b.IP, b.Neustart)
	if err != nil {
		q.Fehler = err.Error()
		logEvent("warn", fmt.Sprintf("MQTT-Steuerung %s: %s", b.IP, err.Error()))
		return q
	}
	q.OK, q.Antwort = true, antwort
	logEvent("info", fmt.Sprintf("MQTT-Steuerung %s: %s", b.IP, antwort))
	return q
}
