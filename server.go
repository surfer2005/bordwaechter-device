package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"syscall"
	"time"
)

// fmt/io/runtime werden vom Self-Update (selfUpdate) genutzt.

//go:embed ui.html
var uiHTML []byte

//go:embed display.html
var displayHTML []byte

// Quelle für den Self-Update ("Aktualisieren"-Knopf): die neueste Release-Binärdatei des Geräte-Repos.
const updateBase = "https://github.com/surfer2005/bordwaechter-device/releases/latest/download"

// reexecSoon ersetzt den laufenden Prozess nach kurzer Verzögerung durch sich selbst (liest die Config
// neu, setzt alle Loops neu auf). Die Verzögerung lässt die HTTP-Antwort noch raus.
func reexecSoon() {
	go func() {
		time.Sleep(400 * time.Millisecond)
		exe, err := os.Executable()
		if err != nil {
			exe = os.Args[0]
		}
		_ = syscall.Exec(exe, os.Args, os.Environ())
		os.Exit(0) // nur erreichbar, wenn Exec fehlschlägt → systemd startet neu
	}()
}

// selfUpdate lädt die passende Release-Binärdatei und schiebt sie per Rename über die eigene Datei.
// Rename ist atomar und funktioniert auch, während die alte Binärdatei noch läuft (neuer Inode).
func selfUpdate() error {
	arch := runtime.GOARCH
	if arch == "arm" {
		arch = "armv7"
	}
	url := updateBase + "/bordwaechter-" + arch
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cl := &http.Client{Timeout: 120 * time.Second}
	resp, err := cl.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("Download fehlgeschlagen (HTTP %d) von %s", resp.StatusCode, url)
	}
	tmp := exe + ".new"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	f.Close()
	if err := os.Rename(tmp, exe); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

// authOK prüft die Berechtigung: Cookie-Sitzung (aus der Anmeldemaske) ODER Basic-Auth. Bewusst
// KEIN WWW-Authenticate mehr — so erscheint kein Browser-Popup; die Oberfläche zeigt stattdessen eine
// eigene Anmeldemaske. Basic-Auth bleibt für Home Assistant (eigenes HA-Passwort) erhalten.
// uiOnly=true: nur das UI-Passwort zählt (Admin-Aktionen wie Netzwerk/Neustart); sonst auch HA-Passwort.
func (a *App) authOK(r *http.Request, uiOnly bool) bool {
	cfg := a.getCfg()
	ui, ha := cfg.UIPassword, cfg.HAPassword
	if ui == "" && (uiOnly || ha == "") {
		return true // kein relevantes Passwort gesetzt → offen im Bordnetz
	}
	if c, err := r.Cookie("bw_session"); err == nil && validSession(c.Value) {
		return true
	}
	if _, pass, ok := r.BasicAuth(); ok {
		if ui != "" && pass == ui {
			return true
		}
		if !uiOnly && ha != "" && pass == ha {
			return true
		}
	}
	return false
}

// guard: schützt API/Aktionen; akzeptiert UI- ODER HA-Passwort (bzw. gültige Sitzung).
func (a *App) guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.authOK(r, false) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

// guardUI schützt Admin-Aktionen (Netzwerk, Geräte-Neustart) mit dem UI-Passwort — NICHT mit dem
// HA-Passwort (das ist nur fürs Mitlesen gedacht). Leeres UI-Passwort = offen im Bordnetz.
func (a *App) guardUI(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.authOK(r, true) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

func (a *App) routes() *http.ServeMux {
	mux := http.NewServeMux()

	// Die Seite selbst ist nicht geschützt (sie enthält nur HTML/JS). Alle Daten/Aktionen dahinter
	// sind weiterhin geschützt; ist man nicht angemeldet, zeigt die Seite ihre eigene Anmeldemaske.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(uiHTML)
	})

	// Anmelden: Benutzername (z. B. "admin") egal, Passwort muss dem UI-Passwort entsprechen.
	// Erfolg → Sitzungs-Cookie. Kein UI-Passwort gesetzt → immer erfolgreich (offen im Bordnetz).
	mux.HandleFunc("/api/login", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST", http.StatusMethodNotAllowed)
			return
		}
		var in struct{ Username, Password string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		ui := a.getCfg().UIPassword
		if ui == "" || in.Password == ui {
			http.SetCookie(w, &http.Cookie{
				Name: "bw_session", Value: newSession(), Path: "/",
				HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: int(sessionTTL / time.Second),
			})
			writeJSON(w, map[string]any{"ok": true})
			return
		}
		writeJSON(w, map[string]any{"ok": false, "error": "Benutzername oder Passwort falsch"})
	})

	mux.HandleFunc("/api/logout", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("bw_session"); err == nil {
			dropSession(c.Value)
		}
		http.SetCookie(w, &http.Cookie{Name: "bw_session", Value: "", Path: "/", HttpOnly: true, MaxAge: -1})
		writeJSON(w, map[string]any{"ok": true})
	})

	// Status — auch für die HACS-Integration (Basic-Auth mit UI-Passwort).
	mux.HandleFunc("/api/status", a.guard(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, a.eng.Status())
	}))

	// Kiosk-Anzeige (schöne Vollbild-Seite für einen Monitor an Bord): ohne Login, dafür Token in der
	// Adresse. Die Seite selbst ist harmlos; die Daten dahinter (/api/display) prüfen den Token.
	mux.HandleFunc("/anzeige", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(displayHTML)
	})
	mux.HandleFunc("/api/display", func(w http.ResponseWriter, r *http.Request) {
		tok := a.getCfg().DisplayToken
		if tok == "" || r.URL.Query().Get("token") != tok {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		writeJSON(w, a.eng.Status())
	})

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
		reexecSoon()
	}))

	// Aktualisieren aus der Oberfläche: neueste Release-Binärdatei holen, über die eigene schieben,
	// dann neu starten. Scheitert der Download, bleibt die alte Version unangetastet.
	mux.HandleFunc("/api/update", a.guard(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST", http.StatusMethodNotAllowed)
			return
		}
		if err := selfUpdate(); err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"ok": true})
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		reexecSoon()
	}))

	// Feste IP / DHCP umstellen (schreibt ein NetworkManager-Keyfile, kein Shell). Wirkt nach
	// einem Geräte-Neustart. Nur mit UI-Passwort.
	mux.HandleFunc("/api/network", a.guardUI(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST", http.StatusMethodNotAllowed)
			return
		}
		var in struct {
			Mode, Interface, Address, Gateway, DNS string
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		a.mu.Lock()
		a.cfg.Network.Mode = in.Mode
		a.cfg.Network.Interface = in.Interface
		a.cfg.Network.Address = in.Address
		a.cfg.Network.Gateway = in.Gateway
		a.cfg.Network.DNS = in.DNS
		cfg := a.cfg
		a.mu.Unlock()
		if err := applyNetwork(cfg); err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		if err := saveConfig(cfg); err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	}))

	// Gerät (Raspberry) komplett neu starten — nötig, damit eine geänderte feste IP greift.
	mux.HandleFunc("/api/reboot", a.guardUI(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true})
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		go func() {
			time.Sleep(500 * time.Millisecond)
			syscall.Sync()
			_ = syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART)
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
