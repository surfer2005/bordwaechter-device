package main

import "fmt"

// Hafenkreis + Mindestgeschwindigkeit (Nutzervorgabe 2026-10-22): SIFA und Kamera-Alarm sollen erst
// scharf werden, wenn das Schiff den Hafen verlassen hat bzw. schnell genug fährt — und beim Einlaufen
// wieder ruhen. Je getrennt einstellbar. Fail-safe: ohne frische Position bleibt alles SCHARF.

const (
	hafenHystereseM = 30.0 // raus erst bei Radius + 30 m (kein Flattern an der Kreisgrenze)
	speedHystKmh    = 1.0  // wieder stumm erst bei Mindestgeschwindigkeit − 1 km/h
)

type gateState struct {
	inHafen   string // Name des Hafens, in dessen Kreis wir liegen ("" = draußen)
	langsamS  bool   // unter SIFA-Mindestgeschwindigkeit
	langsamK  bool   // unter Kamera-Mindestgeschwindigkeit
}

type gateResult struct {
	hafen, sifaRuht, kameraRuht string
}

func (g *gateState) update(cfg *Config, haefen []Hafen, pos Position, posFresh bool) gateResult {
	if !posFresh {
		// Position unbekannt → sicherheitshalber scharf (lieber ein Alarm zu viel als keiner)
		g.inHafen, g.langsamS, g.langsamK = "", false, false
		return gateResult{}
	}
	// Hafenkreis mit Hysterese: drin bleiben, bis Radius + 30 m überschritten
	if g.inHafen != "" {
		drin := false
		for _, h := range haefen {
			if h.Name == g.inHafen && haversineKm(pos.Lat, pos.Lng, h.Lat, h.Lng)*1000 <= radius(h)+hafenHystereseM {
				drin = true
				break
			}
		}
		if !drin {
			g.inHafen = ""
		}
	}
	if g.inHafen == "" {
		for _, h := range haefen {
			if haversineKm(pos.Lat, pos.Lng, h.Lat, h.Lng)*1000 <= radius(h) {
				g.inHafen = h.Name
				break
			}
		}
	}
	g.langsamS = langsam(g.langsamS, pos.SpeedKmh, cfg.SIFA.MinKmh)
	g.langsamK = langsam(g.langsamK, pos.SpeedKmh, cfg.SIFA.CameraMinKmh)

	r := gateResult{hafen: g.inHafen}
	r.sifaRuht = grund(cfg.SIFA.HarborOff, g.inHafen, g.langsamS, cfg.SIFA.MinKmh)
	r.kameraRuht = grund(cfg.SIFA.CameraHarborOff, g.inHafen, g.langsamK, cfg.SIFA.CameraMinKmh)
	return r
}

func radius(h Hafen) float64 {
	if h.RadiusM > 0 {
		return h.RadiusM
	}
	return 300
}

// langsam: unter min → stumm; scharf ab min, wieder stumm erst unter min − Hysterese.
func langsam(war bool, kmh, min float64) bool {
	if min <= 0 {
		return false
	}
	if war {
		return kmh < min
	}
	return kmh < min-speedHystKmh
}

func grund(hafenAus bool, hafen string, langsam bool, min float64) string {
	if hafenAus && hafen != "" {
		return "im Hafen " + hafen
	}
	if langsam {
		return fmt.Sprintf("unter %g km/h", min)
	}
	return ""
}

func posStaleSec(b *Bundle) float64 {
	if b != nil && b.Thresholds.PosStaleS > 0 {
		return b.Thresholds.PosStaleS
	}
	return 300
}
