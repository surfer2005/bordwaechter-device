package main

// Rollen Bildschirm und Bordmusik (Festlegung 2026-10-24): dieselbe Gerätesoftware wie der Bordwächter,
// nur mit anderer Rolle. Gekoppelt wird mit dem Code aus KAI (Monitoring → Schiffe & Landestellen); KAI
// sagt dabei, welche Rolle das Gerät hat. In der Rolle Bildschirm/Bordmusik laufen GPS, SIFA, Hupe und
// Smartmeter NICHT — der Browser öffnet die Abspielseite aus KAI, und dieser Dienst reicht ihr die
// Position des Bordwächters desselben Schiffs durch (/api/medien/position). Direkt aus der Seite ginge
// das nicht: der Browser sperrt Anfragen einer https-Seite an eine einfache Adresse im lokalen Netz.
// Erlaubt ist nur das eigene Gerät, und das erst mit der Chromium-Freigabe „Zugriff aufs lokale Netz“
// für die KAI-Adresse (writeChromiumPolicy).

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func istMedienRolle(r string) bool { return r == "bildschirm" || r == "audio" }

// rolleName: Rolle in Worten für Protokoll und Anzeige (leer = Bordwächter).
func rolleName(r string) string {
	switch r {
	case "bildschirm":
		return "Bildschirm"
	case "audio":
		return "Bordmusik"
	}
	return "Bordwächter"
}

// rolleFuerKai: Art, die das Gerät im Herzschlag meldet.
func rolleFuerKai(r string) string {
	if istMedienRolle(r) {
		return r
	}
	return "bordwaechter"
}

// kaiAdresse: relativer Pfad (kiosk/medien/…) an der KAI-Adresse, auch unter einem Unterpfad wie /test.
func kaiAdresse(kaiBase, rel string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(kaiBase))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("KAI-Adresse fehlt oder ist ungültig")
	}
	u.Fragment, u.RawQuery = "", ""
	if !strings.HasSuffix(u.Path, "/") {
		u.Path += "/"
	}
	r, err := url.Parse(strings.TrimLeft(rel, "/"))
	if err != nil {
		return "", err
	}
	return u.ResolveReference(r).String(), nil
}

// kaiHerkunft: https://host der KAI-Adresse (für die Chromium-Freigabe).
func kaiHerkunft(kaiBase string) string {
	u, err := url.Parse(strings.TrimSpace(kaiBase))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// medienStartZiel: Abspielseite für den Geräte-Browser, leer = (noch) keine.
func medienStartZiel(c *Config) string {
	if !istMedienRolle(c.Rolle) || c.MedienPfad == "" {
		return ""
	}
	u, err := kaiAdresse(c.KaiBaseURL, c.MedienPfad)
	if err != nil {
		return ""
	}
	return u
}

// medienAusHerzschlag übernimmt Rolle, Abspielseite und Bordwächter-Adresse aus dem Herzschlag.
func (a *App) medienAusHerzschlag(hr *HelloResp) {
	cfg := a.getCfg()
	if hr == nil || (!istMedienRolle(cfg.Rolle) && !istMedienRolle(hr.Rolle)) {
		return
	}
	n := *cfg
	if istMedienRolle(hr.Rolle) {
		n.Rolle = hr.Rolle
	}
	if hr.Medien != nil && hr.Medien.Pfad != "" {
		n.MedienPfad = hr.Medien.Pfad
	}
	if hr.Medien != nil && hr.Medien.Ton != "" {
		n.MedienTon = hr.Medien.Ton
	}
	// leer = KAI kennt (gerade) keinen Bordwächter mit Adresse: den letzten bekannten behalten
	if hr.Bordwaechter != nil && hr.Bordwaechter.URL != "" {
		n.Bordwaechter, n.BordwaechterToken = hr.Bordwaechter.URL, hr.Bordwaechter.Token
	}
	if n.Rolle != cfg.Rolle || n.MedienPfad != cfg.MedienPfad || n.MedienTon != cfg.MedienTon || n.Bordwaechter != cfg.Bordwaechter || n.BordwaechterToken != cfg.BordwaechterToken {
		if err := saveConfig(&n); err != nil {
			log.Printf("medien: Stand aus KAI nicht gespeichert: %v", err)
			return
		}
		a.mu.Lock()
		a.cfg = &n
		a.mu.Unlock()
		// Rolle gewechselt (z. B. von KAI zurückgegeben, nachdem sie am Gerät verloren ging): neu starten,
		// damit die passenden Teile laufen — sonst liefen GPS/SIFA weiter, ohne dass etwas abgespielt wird.
		if n.Rolle != cfg.Rolle {
			logEvent("info", "Rolle aus KAI: "+rolleName(n.Rolle)+" — Dienst startet neu")
			reexecSoon()
		}
	}
}

// lokaleAntwort: nur Anfragen vom Gerät selbst; CORS + Freigabe fürs lokale Netz.
func lokaleAntwort(w http.ResponseWriter, r *http.Request) bool {
	if !isLoopback(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	w.Header().Set("Access-Control-Allow-Private-Network", "true")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return false
	}
	return true
}

var bordClient = &http.Client{Timeout: 1500 * time.Millisecond}

// handleMedienPosition: Position vom Bordwächter desselben Schiffs durchreichen. Ohne Bordwächter → valid:false.
func (a *App) handleMedienPosition(w http.ResponseWriter, r *http.Request) {
	if !lokaleAntwort(w, r) {
		return
	}
	cfg := a.getCfg()
	if cfg.Bordwaechter == "" || cfg.BordwaechterToken == "" {
		writeJSON(w, map[string]any{"valid": false, "fehler": "kein Bordwächter bekannt"})
		return
	}
	resp, err := bordClient.Get(strings.TrimRight(cfg.Bordwaechter, "/") + "/api/bord/position?token=" + url.QueryEscape(cfg.BordwaechterToken))
	if err != nil {
		writeJSON(w, map[string]any{"valid": false, "fehler": "Bordwächter nicht erreichbar"})
		return
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if resp.StatusCode != 200 || !json.Valid(b) {
		writeJSON(w, map[string]any{"valid": false, "fehler": fmt.Sprintf("Bordwächter antwortet %d", resp.StatusCode)})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}
