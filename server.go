package main

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"syscall"
	"time"
)

//go:embed ui.html
var uiHTML []byte

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

// basicAuth: schützt die lokale Oberfläche/API mit dem UI-Passwort (Benutzer egal). HACS nutzt dasselbe.
func (a *App) guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pw := a.getCfg().UIPassword
		if pw != "" {
			_, pass, ok := r.BasicAuth()
			if !ok || pass != pw {
				w.Header().Set("WWW-Authenticate", `Basic realm="Bordwächter"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		h(w, r)
	}
}

func (a *App) routes() *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("/", a.guard(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(uiHTML)
	}))

	// Status — auch für die HACS-Integration (Basic-Auth mit UI-Passwort).
	mux.HandleFunc("/api/status", a.guard(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, a.eng.Status())
	}))

	mux.HandleFunc("/api/config", a.guard(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			var c Config
			if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			if err := a.applyConfig(&c); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			writeJSON(w, map[string]any{"ok": true})
			return
		}
		writeJSON(w, a.getCfg())
	}))

	// Neustart aus der Oberfläche: Prozess per Re-Exec frisch starten (liest die Config neu ein,
	// setzt GPIO-Backend/alle Loops neu auf). systemd-unabhängig — ersetzt das laufende Programm.
	mux.HandleFunc("/api/restart", a.guard(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true})
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		go func() {
			time.Sleep(400 * time.Millisecond)
			exe, err := os.Executable()
			if err != nil {
				exe = os.Args[0]
			}
			_ = syscall.Exec(exe, os.Args, os.Environ())
			os.Exit(0) // nur erreichbar, wenn Exec fehlschlägt → systemd startet neu
		}()
	}))

	mux.HandleFunc("/api/ack", a.guard(func(w http.ResponseWriter, r *http.Request) {
		a.eng.Ack()
		writeJSON(w, map[string]any{"ok": true})
	}))

	// Test: Hupe/Lampe kurz schalten
	mux.HandleFunc("/api/test", a.guard(func(w http.ResponseWriter, r *http.Request) {
		a.eng.TestPulse(r.URL.Query().Get("out"), 3)
		writeJSON(w, map[string]any{"ok": true})
	}))

	// Manuelle Position (Test, nur bei GPS-Quelle "manual")
	mux.HandleFunc("/api/position", a.guard(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		lat, _ := strconv.ParseFloat(q.Get("lat"), 64)
		lng, _ := strconv.ParseFloat(q.Get("lng"), 64)
		sp, _ := strconv.ParseFloat(q.Get("speed_kmh"), 64)
		a.manual.Set(Position{Lat: lat, Lng: lng, SpeedKmh: sp})
		writeJSON(w, map[string]any{"ok": true})
	}))

	// Kamera-Webhook (MxMessage): ?secret=… → setzt einen Kamera-Hinweis. KEIN UI-Passwort (eigenes Secret).
	mux.HandleFunc("/api/camera/event", func(w http.ResponseWriter, r *http.Request) {
		sec := a.getCfg().Camera.WebhookSecret
		if sec == "" || r.URL.Query().Get("secret") != sec {
			http.Error(w, "forbidden", 403)
			return
		}
		a.eng.CameraHazard()
		writeJSON(w, map[string]any{"ok": true})
	})

	return mux
}
