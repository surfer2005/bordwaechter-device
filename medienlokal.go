package main

// Bedienung von Bildschirm/Bordmusik OHNE Verbindung zu KAI (Festlegung 2026-10-24). Die Knöpfe wirken
// immer direkt auf das Gerät, nie über den Bordwächter. Zwei Wege führen hierher:
//   - das Schiffsterminal (KAI-Kiosk) reicht einen Knopfdruck aus „Bordmedien“ im Bord-Netz weiter
//     (mit dem Abspiel-Schlüssel dieses Geräts, den es aus KAI kennt),
//   - die kleine Bedienseite /bedienen auf diesem Gerät (für Geräte ohne unsere Software, z. B. ein Tablet;
//     geschützt wie die Konfig-Seite).
// Das Gerät merkt sich den Befehl, die Abspielseite holt ihn hier ab (localhost) und führt ihn aus. Sie
// meldet umgekehrt, was gerade läuft und welche Playlisten/Ereignisse es gibt — das zeigt die Bedienseite.

import (
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

//go:embed bedienen.html
var bedienenHTML []byte

type medienBefehl struct {
	N        int    `json:"n"`
	Art      string `json:"art"`                   // befehl | steuerung | zeitplan | ereignis
	Cmd      string `json:"cmd,omitempty"`         // vor | zurueck | anfang | lautstaerke | zufall | schleife
	Value    any    `json:"value,omitempty"`       // z. B. Lautstärke
	Paused   bool   `json:"paused,omitempty"`      // steuerung: Pause
	Playlist int    `json:"playlist_id,omitempty"` // steuerung: Playlist
	Volume   *int   `json:"volume,omitempty"`      // steuerung: Lautstärke
	Ereignis int    `json:"ereignis_id,omitempty"`
	Zeit     string `json:"at"`
	Quelle   string `json:"quelle,omitempty"` // terminal | geraeteseite
}

var (
	medMu      sync.Mutex
	medN       int
	medLetzt   []medienBefehl // die letzten Befehle (die Seite holt alle mit n > ihrem Stand)
	medStand   json.RawMessage
	medStandAt time.Time
)

// medienToken: der Abspiel-Schlüssel aus der Abspielseite (…?token=…) — dient zugleich als Bedien-Schlüssel.
func medienToken(c *Config) string {
	if i := strings.Index(c.MedienPfad, "?"); i >= 0 {
		if q, err := url.ParseQuery(c.MedienPfad[i+1:]); err == nil {
			return q.Get("token")
		}
	}
	return ""
}

var erlaubteArten = map[string]bool{"befehl": true, "steuerung": true, "zeitplan": true, "ereignis": true}
var erlaubteCmds = map[string]bool{"vor": true, "zurueck": true, "anfang": true, "lautstaerke": true, "zufall": true, "schleife": true}

// medienBefehlMerken prüft und speichert einen Befehl (rein, testbar).
func medienBefehlMerken(b medienBefehl) (medienBefehl, bool) {
	if !erlaubteArten[b.Art] || (b.Art == "befehl" && !erlaubteCmds[b.Cmd]) {
		return b, false
	}
	medMu.Lock()
	defer medMu.Unlock()
	medN++
	b.N = medN
	b.Zeit = time.Now().UTC().Format(time.RFC3339)
	medLetzt = append(medLetzt, b)
	if len(medLetzt) > 20 {
		medLetzt = medLetzt[len(medLetzt)-20:]
	}
	return b, true
}

func medienBefehleSeit(n int) []medienBefehl {
	medMu.Lock()
	defer medMu.Unlock()
	out := []medienBefehl{}
	for _, b := range medLetzt {
		if b.N > n {
			out = append(out, b)
		}
	}
	return out
}

// darfBedienen: Bedien-Schlüssel (Terminal) ODER angemeldet wie auf der Konfig-Seite (Bedienseite).
func (a *App) darfBedienen(r *http.Request) bool {
	tok := medienToken(a.getCfg())
	if g := r.URL.Query().Get("token"); tok != "" && g != "" && subtle.ConstantTimeCompare([]byte(g), []byte(tok)) == 1 {
		return true
	}
	return a.authOK(r, true)
}

func (a *App) medienRoutes(mux *http.ServeMux) {
	// Bedienseite (nur HTML; Daten/Aktionen dahinter sind geschützt)
	mux.HandleFunc("/bedienen", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(bedienenHTML)
	})
	// Befehl entgegennehmen (Terminal mit Schlüssel oder Bedienseite)
	mux.HandleFunc("/api/medien/befehl", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Allow-Private-Network", "true")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "POST erwartet", 405)
			return
		}
		if !istMedienRolle(a.getCfg().Rolle) {
			http.Error(w, "kein Bildschirm/keine Bordmusik", 409)
			return
		}
		if !a.darfBedienen(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var b medienBefehl
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			http.Error(w, "ungültiger Befehl", 400)
			return
		}
		if b.Quelle == "" {
			b.Quelle = "geraeteseite"
		}
		nb, ok := medienBefehlMerken(b)
		if !ok {
			http.Error(w, "unbekannter Befehl", 400)
			return
		}
		logEvent("info", "Bedienung an Bord: "+nb.Art+" "+nb.Cmd+" ("+nb.Quelle+")")
		writeJSON(w, map[string]any{"ok": true, "n": nb.N})
	})
	// Stand fürs Terminal/die Bedienseite: was läuft, welche Playlisten/Ereignisse gibt es
	mux.HandleFunc("/api/medien/stand", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Allow-Private-Network", "true")
		w.Header().Set("Cache-Control", "no-store")
		switch r.Method {
		case http.MethodOptions:
			w.WriteHeader(http.StatusNoContent)
		case http.MethodPost: // nur die Abspielseite auf diesem Gerät meldet
			if !isLoopback(r) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			var raw json.RawMessage
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&raw); err != nil {
				http.Error(w, "ungültig", 400)
				return
			}
			medMu.Lock()
			medStand, medStandAt = raw, time.Now()
			medMu.Unlock()
			writeJSON(w, map[string]any{"ok": true})
		default:
			if !a.darfBedienen(r) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			medMu.Lock()
			st, at := medStand, medStandAt
			medMu.Unlock()
			cfg := a.getCfg()
			writeJSON(w, map[string]any{"rolle": cfg.Rolle, "name": cfg.Hostname, "stand": st, "stand_alter_s": time.Since(at).Seconds(), "seit_start": at.IsZero()})
		}
	})
	// Abspielseite (localhost) holt neue Befehle ab
	mux.HandleFunc("/api/medien/lokal", func(w http.ResponseWriter, r *http.Request) {
		if !lokaleAntwort(w, r) {
			return
		}
		n := 0
		if v := r.URL.Query().Get("seit"); v != "" {
			_ = json.Unmarshal([]byte(v), &n)
		}
		medMu.Lock()
		akt := medN
		medMu.Unlock()
		writeJSON(w, map[string]any{"n": akt, "befehle": medienBefehleSeit(n)})
	})
}
