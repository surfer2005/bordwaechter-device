package main

// Smartmeter-Bridge: holt die zentrale Zähler-Konfig von KAI (GET /api/edge/smartmeter-config),
// liest die Register ROH per Modbus TCP (modbusReadHolding, s. modbus.go) und pusht die Werte an KAI
// (POST /api/edge/telemetry). Gefiltert wird ZENTRAL in KAI — das Gerät ist nur Bridge. Nur ausgehend,
// best effort: ein einzelner Lesefehler überspringt den Zähler, bricht nicht den ganzen Lauf ab.

import (
	"encoding/json"
	"fmt"
	"log"
	"time"
)

type smRegister struct {
	Address  int    `json:"address"`
	Type     string `json:"type"`
	Measure  string `json:"measure"`
	Unit     string `json:"unit"`
	Kind     string `json:"kind"`
	EntityID string `json:"entity_id"`
	Name     string `json:"name"`
}
type smMeter struct {
	Name      string       `json:"name"`
	Slave     int          `json:"slave"`
	Registers []smRegister `json:"registers"`
}
type smConfig struct {
	Enabled         bool      `json:"enabled"`
	Host            string    `json:"host"`
	Port            int       `json:"port"`
	PushIntervalSec int       `json:"push_interval_sec"`
	Meters          []smMeter `json:"meters"`
}
type smReading struct {
	EntityID string  `json:"entity_id"`
	Name     string  `json:"name"`
	Unit     string  `json:"unit"`
	Value    float64 `json:"value"`
}

func (k *KaiClient) GetSmartmeterConfig() (*smConfig, error) {
	data, code, err := k.do("GET", "/api/edge/smartmeter-config", nil)
	if err != nil {
		return nil, err
	}
	if code != 200 {
		return nil, fmt.Errorf("smartmeter-config http %d", code)
	}
	var c smConfig
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func (k *KaiClient) PostTelemetry(readings []smReading) error {
	_, code, err := k.do("POST", "/api/edge/telemetry", map[string]any{"readings": readings})
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("telemetry http %d", code)
	}
	return nil
}

// smartmeterLoop: Konfig holen → Register lesen → pushen, im von KAI vorgegebenen Takt.
func (a *App) smartmeterLoop() {
	time.Sleep(25 * time.Second) // Netz/Token erst hochkommen lassen
	for {
		interval := 30 * time.Second
		cfg := a.getCfg()
		k := a.getKai()
		if k != nil && cfg.EdgeToken != "" {
			if sc, err := k.GetSmartmeterConfig(); err != nil {
				log.Printf("smartmeter: Konfig: %v", err)
			} else if sc.Enabled && sc.Host != "" && len(sc.Meters) > 0 {
				if sc.PushIntervalSec > 0 {
					interval = time.Duration(sc.PushIntervalSec) * time.Second
				}
				if readings := readSmartmeter(sc); len(readings) > 0 {
					if err := k.PostTelemetry(readings); err != nil {
						log.Printf("smartmeter: push: %v", err)
					}
				}
			}
		}
		time.Sleep(interval)
	}
}

// readSmartmeter liest alle Register aller Zähler. Das Standard-Profil ist durchweg float32 (2 Register,
// High-Word zuerst — wie HA; regsToFloat deckt das ab). Gemischte Typen (Teltonika) kommen separat.
func readSmartmeter(sc *smConfig) []smReading {
	var out []smReading
	port := sc.Port
	if port == 0 {
		port = 502
	}
	to := 5 * time.Second
	for _, m := range sc.Meters {
		for _, r := range m.Registers {
			regs, err := modbusReadHolding(sc.Host, port, m.Slave, r.Address, 2, to)
			if err != nil || len(regs) < 2 {
				continue // einzelner Lesefehler: Zähler/Register überspringen
			}
			out = append(out, smReading{
				EntityID: r.EntityID, Name: r.Name, Unit: r.Unit,
				Value: regsToFloat(regs, "float32"),
			})
		}
	}
	return out
}
