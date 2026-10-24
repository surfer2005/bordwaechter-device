package main

// Position fürs Bord-Netz (Festlegung 2026-10-24): der Bordwächter ist an Bord der einzige Empfänger der
// Positionsdaten (Modbus/GPS) und gibt sie im Bord-Netz an Bildschirme und Bordmusik weiter. Er entscheidet
// NICHTS über Medien — welche Ereignisse wann laufen, prüft jedes Gerät mit seinen eigenen Einstellungen
// aus KAI selbst. Zugriff nur mit dem Lese-Schlüssel, den KAI mit dem Herzschlag vergibt (bord_token) und
// den Geräten desselben Schiffs mitteilt. Der Schlüssel liegt neben der Konfiguration, damit er auch nach
// einem Neustart ohne Netz gilt.

import (
	"crypto/subtle"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	bordTokMu      sync.Mutex
	bordTok        string
	bordTokGeladen bool
)

func bordTokenPfad() string { return filepath.Join(filepath.Dir(configPath()), "bord-token") }

func getBordToken() string {
	bordTokMu.Lock()
	defer bordTokMu.Unlock()
	if !bordTokGeladen {
		if b, err := os.ReadFile(bordTokenPfad()); err == nil {
			bordTok = strings.TrimSpace(string(b))
		}
		bordTokGeladen = true
	}
	return bordTok
}

// setBordToken übernimmt den Schlüssel aus dem Herzschlag (nur bei Änderung auf die Platte).
func setBordToken(t string) {
	t = strings.TrimSpace(t)
	if t == "" || t == getBordToken() {
		return
	}
	bordTokMu.Lock()
	bordTok = t
	bordTokMu.Unlock()
	if err := os.WriteFile(bordTokenPfad(), []byte(t+"\n"), 0o600); err != nil {
		logEvent("warn", "Lese-Schlüssel fürs Bord-Netz nicht gespeichert: "+err.Error())
	}
}

// bordPositionAntwort: was Bildschirme/Bordmusik bekommen. „valid“ nur bei frischem Fix (≤ 30 s).
func bordPositionAntwort(p Position, jetzt time.Time) map[string]any {
	alter := -1.0
	if !p.TS.IsZero() {
		alter = jetzt.Sub(p.TS).Seconds()
	}
	gueltig := p.Valid && alter >= 0 && alter <= 30
	out := map[string]any{"valid": gueltig, "age_s": alter, "quelle": "bordwaechter", "version": version}
	if gueltig {
		out["lat"], out["lng"], out["speed_kmh"], out["heading"] = p.Lat, p.Lng, p.SpeedKmh, p.Heading
	}
	return out
}

func (a *App) handleBordPosition(w http.ResponseWriter, r *http.Request) {
	tok := getBordToken()
	gegeben := r.URL.Query().Get("token")
	if tok == "" || subtle.ConstantTimeCompare([]byte(gegeben), []byte(tok)) != 1 {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, bordPositionAntwort(a.eng.CurrentPos(), time.Now()))
}
