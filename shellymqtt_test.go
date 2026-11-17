package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Shelly-Attrappe: merkt sich die MQTT-Konfiguration und ob neu gestartet wurde.
func shellyAttrappe(t *testing.T, rpc, control bool, kennwort bool) (*httptest.Server, *[]string) {
	aufrufe := &[]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if kennwort {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var req struct {
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		*aufrufe = append(*aufrufe, req.Method)
		switch req.Method {
		case "MQTT.GetConfig":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "result": map[string]any{"enable": true, "enable_rpc": rpc, "enable_control": control}})
		case "MQTT.SetConfig":
			c, _ := req.Params["config"].(map[string]any)
			if c["enable_rpc"] != true || c["enable_control"] != true {
				t.Errorf("SetConfig ohne enable_rpc/enable_control: %v", req.Params)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "result": map[string]any{"restart_required": true}})
		case "Shelly.Reboot":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "result": nil})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "error": map[string]any{"code": -114, "message": "unbekannt"}})
		}
	}))
	return srv, aufrufe
}

func TestMqttSteuerungEinschaltenMitNeustart(t *testing.T) {
	srv, aufrufe := shellyAttrappe(t, false, false, false)
	defer srv.Close()
	a, err := shellyMqttSteuerungAn(srv.URL, true)
	if err != nil || !strings.Contains(a, "startet neu") {
		t.Fatalf("antwort=%q err=%v", a, err)
	}
	if strings.Join(*aufrufe, ",") != "MQTT.GetConfig,MQTT.SetConfig,Shelly.Reboot" {
		t.Fatalf("aufrufe %v", *aufrufe)
	}
}

func TestMqttSteuerungOhneNeustartUndSchonAn(t *testing.T) {
	srv, aufrufe := shellyAttrappe(t, false, false, false)
	defer srv.Close()
	if a, _ := shellyMqttSteuerungAn(srv.URL, false); !strings.Contains(a, "nächsten Neustart") || len(*aufrufe) != 2 {
		t.Fatalf("ohne Neustart: %q %v", a, *aufrufe)
	}
	srv2, aufrufe2 := shellyAttrappe(t, true, true, false)
	defer srv2.Close()
	if a, _ := shellyMqttSteuerungAn(srv2.URL, true); !strings.Contains(a, "schon eingeschaltet") || len(*aufrufe2) != 1 {
		t.Fatalf("schon an: %q %v", a, *aufrufe2)
	}
}

func TestMqttSteuerungKennwort(t *testing.T) {
	srv, _ := shellyAttrappe(t, false, false, true)
	defer srv.Close()
	if _, err := shellyMqttSteuerungAn(srv.URL, true); err == nil || !strings.Contains(err.Error(), "Kennwort") {
		t.Fatalf("erwartet Kennwort-Hinweis, err=%v", err)
	}
}

func TestMqttSteuerungNurBordnetz(t *testing.T) {
	for _, ip := range []string{"192.168.74.80", "10.0.8.18", "172.16.0.5"} {
		if !istBordAdresse(ip) {
			t.Errorf("%s sollte erlaubt sein", ip)
		}
	}
	for _, ip := range []string{"8.8.8.8", "127.0.0.1", "", "kai-bsb.de", "192.168.74.80:80", "::1"} {
		if istBordAdresse(ip) {
			t.Errorf("%s darf nicht erlaubt sein", ip)
		}
	}
	q := mqttSteuerungBefehl(sensorBefehl{ID: 7, Art: "mqtt_steuerung", IP: "8.8.8.8"})
	if q.OK || !strings.Contains(q.Fehler, "nicht im Bordnetz") {
		t.Fatalf("fremde Adresse nicht abgelehnt: %+v", q)
	}
}
