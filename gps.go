package main

import (
	"fmt"
	"math"
	"sync"
	"time"
)

type Position struct {
	Lat, Lng float64
	SpeedKmh float64
	Heading  float64
	Valid    bool
	TS       time.Time
}

type GPSSource interface {
	Read() (Position, error)
}

// ManualGPS: Position wird über die lokale UI gesetzt — für Test/Inbetriebnahme ohne Hardware.
type ManualGPS struct {
	mu sync.Mutex
	p  Position
}

func (m *ManualGPS) Set(p Position) {
	m.mu.Lock()
	p.Valid = true
	p.TS = time.Now()
	m.p = p
	m.mu.Unlock()
}

func (m *ManualGPS) Read() (Position, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.p.Valid {
		return Position{}, fmt.Errorf("keine manuelle Position gesetzt")
	}
	return m.p, nil
}

// TeltonikaGPS: liest Lat/Lng (je 2 Register) per Modbus-TCP. Speed wird im Engine aus
// aufeinanderfolgenden Positionen abgeleitet (robuster als ein unsicher skaliertes Speed-Register).
type TeltonikaGPS struct{ cfg *Config }

func regsToFloat(regs []uint16, format string) float64 {
	if len(regs) < 2 {
		return 0
	}
	raw := uint32(regs[0])<<16 | uint32(regs[1])
	switch format {
	case "int32_1e7":
		return float64(int32(raw)) / 1e7
	case "int32_1e6":
		return float64(int32(raw)) / 1e6
	default: // float32 (IEEE754, High-Word zuerst)
		return float64(math.Float32frombits(raw))
	}
}

func (t *TeltonikaGPS) Read() (Position, error) {
	g := t.cfg.GPS
	to := 5 * time.Second
	latR, err := modbusReadHolding(g.Host, g.Port, g.Unit, g.LatReg, 2, to)
	if err != nil {
		return Position{}, err
	}
	lngR, err := modbusReadHolding(g.Host, g.Port, g.Unit, g.LngReg, 2, to)
	if err != nil {
		return Position{}, err
	}
	p := Position{Lat: regsToFloat(latR, g.Format), Lng: regsToFloat(lngR, g.Format), Valid: true, TS: time.Now()}
	if g.HeadingReg >= 0 {
		if hr, err := modbusReadHolding(g.Host, g.Port, g.Unit, g.HeadingReg, 1, to); err == nil && len(hr) > 0 {
			p.Heading = float64(hr[0])
		}
	}
	if g.SpeedReg >= 0 {
		if sr, err := modbusReadHolding(g.Host, g.Port, g.Unit, g.SpeedReg, 1, to); err == nil && len(sr) > 0 {
			p.SpeedKmh = float64(sr[0]) // ggf. skalieren — an echter Teltonika verifizieren
		}
	}
	return p, nil
}

func buildGPS(cfg *Config, manual *ManualGPS) GPSSource {
	if cfg.GPS.Source == "teltonika" {
		return &TeltonikaGPS{cfg: cfg}
	}
	return manual
}
