package main

// Teltonika-Router-Telemetrie: der Bordwächter liest Diagnose-Register des Routers per Modbus
// (Signal/Temperatur/Netzbetreiber/Netztyp/Uptime/Datenverbrauch/SIM/Seriennummer) und meldet sie an
// KAI (POST /api/edge/router-telemetry → Asset Manager/Monitoring). Läuft, wenn die GPS-Quelle ohnehin
// „teltonika" ist (dann IST der Router die Modbus-Quelle, host/unit stehen schon). GPS/Position holt das
// Gerät separat (gps.go) — hier nur die Router-Diagnose, keine Dopplung. Nur ausgehend, best effort.

import (
	"fmt"
	"log"
	"strings"
	"time"
)

type tRegDef struct {
	addr, count int
	typ         string // uint32 | int32 | uint16 | string | float32
	scale       float64
	measure     string
	label       string
	unit        string
}

// teltonikaCfg bündelt die zentral in KAI gepflegte Teltonika-Modbus-Konfig (Registerkarte + Adresse).
type teltonikaCfg struct {
	Regs []tRegDef
	Host string
	Port int
}

// Registerkarte aus der Teltonika-Modbus-Konfig des Nutzers (slave 1). GPS lat/lng/speed bewusst weg
// (holt gps.go), ebenso die reinen Zeit-Strings (KAI hat seine eigene Uhr).
var teltonikaRegs = []tRegDef{
	{addr: 1, count: 2, typ: "uint32", measure: "uptime", label: "System-Uptime", unit: "s"},
	{addr: 3, count: 2, typ: "int32", measure: "signal", label: "Mobilfunk-Signal", unit: "dBm"},
	{addr: 5, count: 2, typ: "int32", scale: 0.1, measure: "temp", label: "System-Temperatur", unit: "°C"},
	{addr: 7, count: 16, typ: "string", measure: "hostname", label: "Hostname"},
	{addr: 23, count: 16, typ: "string", measure: "operator", label: "Netzbetreiber"},
	{addr: 119, count: 16, typ: "string", measure: "network_type", label: "Netztyp"},
	{addr: 181, count: 2, typ: "uint32", measure: "gps_sats", label: "GPS-Satelliten"},
	{addr: 194, count: 2, typ: "int32", measure: "data_down_month", label: "Daten Download (Monat)", unit: "B"},
	{addr: 196, count: 2, typ: "uint32", measure: "bytes_sent_mtd", label: "Bytes gesendet (MTD)", unit: "B"},
	{addr: 205, count: 1, typ: "uint16", measure: "active_sim", label: "Aktive SIM"},
	{addr: 39, count: 16, typ: "string", measure: "serial", label: "Seriennummer"},
}

type routerReading struct {
	Measure string   `json:"measure"`
	Label   string   `json:"label"`
	Unit    string   `json:"unit,omitempty"`
	Num     *float64 `json:"num,omitempty"`
	Text    string   `json:"text,omitempty"`
}

func (k *KaiClient) PostRouterTelemetry(readings []routerReading) error {
	_, code, err := k.do("POST", "/api/edge/router-telemetry", map[string]any{"readings": readings})
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("router-telemetry http %d", code)
	}
	return nil
}

func (a *App) routerLoop() {
	time.Sleep(35 * time.Second)
	for {
		cfg := a.getCfg()
		k := a.getKai()
		if k != nil && cfg.EdgeToken != "" && a.getReportTeltonika() {
			// Registerliste UND Modbus-Adresse zentral aus KAI; eingebaute Liste + lokale GPS-Konfig als
			// Rückfall (autark, falls KAI weg). So lässt sich der Router rein in KAI einrichten.
			regs := teltonikaRegs
			host := strings.TrimSpace(cfg.GPS.Host)
			port := cfg.GPS.Port
			if kc, err := k.GetTeltonikaConfig(); err == nil {
				if len(kc.Regs) > 0 {
					regs = kc.Regs
				}
				if strings.TrimSpace(kc.Host) != "" {
					host = strings.TrimSpace(kc.Host)
					port = kc.Port
				}
			}
			if host != "" {
				if rr := readRouter(cfg, regs, host, port); len(rr) > 0 {
					if err := k.PostRouterTelemetry(rr); err != nil {
						log.Printf("router: %v", err)
						logEvent("warn", "Router-Telemetrie konnte nicht gesendet werden: "+err.Error())
					}
				}
			}
		}
		time.Sleep(60 * time.Second)
	}
}

func readRouter(cfg *Config, regs []tRegDef, host string, port int) []routerReading {
	if port == 0 {
		port = 502
	}
	unit := cfg.GPS.Unit
	if unit == 0 {
		unit = 1 // Teltonika-Router: Slave 1
	}
	to := 5 * time.Second
	var out []routerReading
	for _, r := range regs {
		regs, err := modbusReadHolding(host, port, unit, r.addr, r.count, to)
		if err != nil || len(regs) < r.count {
			continue // einzelner Lesefehler: Register überspringen
		}
		rr := routerReading{Measure: r.measure, Label: r.label, Unit: r.unit}
		switch r.typ {
		case "string":
			rr.Text = regsToString(regs)
			if rr.Text == "" {
				continue
			}
		case "float32":
			v := regsToFloat(regs, "float32")
			rr.Num = &v
		case "uint32":
			v := float64(uint32(regs[0])<<16 | uint32(regs[1]))
			rr.Num = &v
		case "int32":
			v := float64(int32(uint32(regs[0])<<16 | uint32(regs[1])))
			if r.scale != 0 {
				v *= r.scale
			}
			rr.Num = &v
		case "uint16":
			v := float64(regs[0])
			rr.Num = &v
		default:
			continue
		}
		out = append(out, rr)
	}
	return out
}

// regsToString: Modbus-Register (je 2 Byte, High-Byte zuerst) → ASCII, bei NUL abgeschnitten.
func regsToString(regs []uint16) string {
	b := make([]byte, 0, len(regs)*2)
	for _, r := range regs {
		b = append(b, byte(r>>8), byte(r&0xff))
	}
	s := string(b)
	if i := strings.IndexByte(s, 0); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
