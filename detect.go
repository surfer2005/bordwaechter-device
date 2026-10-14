package main

// Objekterkennung (optional, per Konfig zuschaltbar): fragt die HTTP-API eines lokal laufenden
// Frigate ab und meldet jeden neuen Treffer (person/boat …) über den vorhandenen Edge-Event-Weg
// an KAI — kind="detection", state=<label>, mit Standbild. Kein zweites Programm, keine externen
// Abhängigkeiten: Frigate macht die Erkennung, der Bordwächter ist die Brücke zu KAI.
//
// Läuft nur, wenn cfg.Detect.Enabled. Ist das Feature aus (oder kein AI-Beschleuniger/Frigate da),
// schläft die Schleife bloß — dasselbe Image läuft so als normaler Bordwächter weiter.

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var detectHTTP = &http.Client{Timeout: 10 * time.Second}

// Ein Ereignis, wie es Frigates /api/events liefert (nur die Felder, die wir brauchen).
type frigateEvent struct {
	ID        string  `json:"id"`
	Camera    string  `json:"camera"`
	Label     string  `json:"label"`
	TopScore  float64 `json:"top_score"`
	Score     float64 `json:"score"`
	StartTime float64 `json:"start_time"`
}

func labelDe(l string) string {
	switch l {
	case "person":
		return "Person"
	case "boat":
		return "Boot"
	default:
		return l
	}
}

func (a *App) detectLoop() {
	seen := map[string]bool{} // schon gemeldete Frigate-Event-IDs (gegen Doppelmeldung)
	for {
		cfg := a.getCfg()
		if cfg.Detect.Enabled && cfg.Detect.FrigateURL != "" {
			a.pollFrigate(cfg, seen)
		}
		sleepSec(cfg.Detect.PollSec, 3)
	}
}

func (a *App) pollFrigate(cfg *Config, seen map[string]bool) {
	base := strings.TrimRight(cfg.Detect.FrigateURL, "/")
	labels := cfg.Detect.Labels
	if labels == "" {
		labels = "person,boat"
	}
	q := fmt.Sprintf("%s/api/events?limit=50&include_thumbnails=0&labels=%s", base, url.QueryEscape(labels))
	resp, err := detectHTTP.Get(q)
	if err != nil {
		log.Printf("detect: Frigate nicht erreichbar: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		log.Printf("detect: Frigate /api/events HTTP %d", resp.StatusCode)
		return
	}
	var events []frigateEvent
	if err := json.NewDecoder(resp.Body).Decode(&events); err != nil {
		log.Printf("detect: Antwort unlesbar: %v", err)
		return
	}
	// Frigate liefert neueste zuerst; chronologisch melden (älteste zuerst).
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		if e.ID == "" || seen[e.ID] {
			continue
		}
		seen[e.ID] = true // ab jetzt als bearbeitet merken (auch wenn wir es unten verwerfen)
		score := e.TopScore
		if score == 0 {
			score = e.Score
		}
		if score < cfg.Detect.MinScore {
			continue
		}
		snap := fetchSnapshotB64(fmt.Sprintf("%s/api/events/%s/snapshot.jpg", base, url.PathEscape(e.ID)))
		detail := fmt.Sprintf("%s erkannt (%.0f%%)", labelDe(e.Label), score*100)
		k := a.getKai()
		if k == nil || cfg.EdgeToken == "" {
			continue
		}
		if err := k.PostEvent("detection", e.Label, detail, Position{}, 0, snap); err != nil {
			log.Printf("detect: melden fehlgeschlagen: %v", err)
			delete(seen, e.ID) // beim nächsten Durchlauf erneut versuchen
		}
	}
	// Speicher begrenzen: die Merkliste nicht unbegrenzt wachsen lassen.
	if len(seen) > 5000 {
		for id := range seen {
			delete(seen, id)
		}
	}
}
