package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// fmt/io/runtime werden vom Self-Update (selfUpdate) genutzt.

//go:embed ui.html
var uiHTML []byte

//go:embed display.html
var displayHTML []byte

//go:embed seekarte.html
var seekarteHTML []byte

// Quelle für den Self-Update ("Aktualisieren"-Knopf): die neueste Release-Binärdatei des Geräte-Repos.
const updateBase = "https://github.com/surfer2005/bordwaechter-device/releases/latest/download"

// reexecSoon startet den Dienst neu, damit nach Selbst-Update bzw. Konfig-/GPS-/GPIO-Änderung der NEUE
// Stand läuft. ZUVERLÄSSIGSTER Weg unter systemd: sich einfach beenden — `Restart=always`
// (RestartSec=3) startet den Dienst dann automatisch mit dem neuen Binary. Der frühere Weg über
// `systemctl … restart` aus dem eigenen Prozess griff NICHT zuverlässig (Job wurde eingereiht, der alte
// Prozess lief weiter → „Aktualisieren" blieb hängen). Reines Beenden hat keine solche Eigenheit.
func reexecSoon() {
	go func() {
		time.Sleep(600 * time.Millisecond) // HTTP-Antwort rausflushen lassen
		os.Exit(0)                          // systemd (Restart=always) startet mit dem neuen Binary neu
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

// Versionsprüfung: liegt in GitHub eine neuere Version als die laufende vor? Dieselbe Quelle wie der
// „Aktualisieren"-Knopf (releases/latest). Zwischengespeichert (1 h); bei Offline/Fehler bleibt der
// letzte bekannte Stand. So sieht der Kollege beim Öffnen der Konfigseite, dass eine neue Version
// bereitsteht, statt extra informiert werden zu müssen.
const ghLatestAPI = "https://api.github.com/repos/surfer2005/bordwaechter-device/releases/latest"

var (
	relMu  sync.Mutex
	relTag string
	relAt  time.Time
)

// cachedLatestTag liefert den zuletzt bekannten Release-Tag OHNE Netzzugriff (für den Status-Tick).
func cachedLatestTag() string {
	relMu.Lock()
	defer relMu.Unlock()
	return relTag
}

// releaseCheckLoop hält den Release-Tag im Hintergrund aktuell (für den „neue Version"-Hinweis),
// ohne dass ein /api/display-Abruf je auf das Netz warten muss.
func (a *App) releaseCheckLoop() {
	for {
		latestReleaseTag()
		time.Sleep(time.Hour)
	}
}

func latestReleaseTag() string {
	relMu.Lock()
	if relTag != "" && time.Since(relAt) < time.Hour {
		t := relTag
		relMu.Unlock()
		return t
	}
	relMu.Unlock()
	cl := &http.Client{Timeout: 8 * time.Second}
	req, _ := http.NewRequest("GET", ghLatestAPI, nil)
	req.Header.Set("User-Agent", "bordwaechter")
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := cl.Do(req)
	if err != nil {
		relMu.Lock()
		t := relTag
		relMu.Unlock()
		return t
	}
	defer resp.Body.Close()
	if resp.StatusCode == 200 {
		var j struct {
			TagName string `json:"tag_name"`
		}
		if json.NewDecoder(resp.Body).Decode(&j) == nil && j.TagName != "" {
			relMu.Lock()
			relTag = j.TagName
			relAt = time.Now()
			relMu.Unlock()
			return j.TagName
		}
	}
	relMu.Lock()
	t := relTag
	relMu.Unlock()
	return t
}

// verNewer: ist latest (z. B. "v0.1.22") höher als current ("0.1.21")? Nur Zahlenteile, führendes v egal.
func verNewer(latest, current string) bool {
	norm := func(s string) []int {
		s = strings.TrimPrefix(strings.TrimSpace(s), "v")
		parts := strings.Split(s, ".")
		out := make([]int, len(parts))
		for i, p := range parts {
			out[i], _ = strconv.Atoi(p)
		}
		return out
	}
	a, b := norm(latest), norm(current)
	for i := 0; i < len(a) || i < len(b); i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			return x > y
		}
	}
	return false
}

// isLoopback: kommt die Anfrage vom Gerät selbst (127.0.0.1/::1)? Dann ist es der Kiosk auf dem
// angeschlossenen Monitor — der darf die Anzeige ohne Token sehen (der Token schützt nur das Netz).
func isLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// displayAuthOK: wie /api/display — localhost (Gerät) darf ohne Token, aus dem Netz nur mit Token.
func (a *App) displayAuthOK(r *http.Request) bool {
	if isLoopback(r) {
		return true
	}
	tok := a.getCfg().DisplayToken
	return tok != "" && r.URL.Query().Get("token") == tok
}

// statusWithInfo: Engine-Status plus das optionale Wetter/Pegel-Objekt (info) für die Anzeige.
// Additiv — fehlt info, ist es einfach nicht enthalten; der bestehende Status bleibt unverändert.
func (a *App) statusWithInfo() map[string]any {
	b, _ := json.Marshal(a.eng.Status())
	m := map[string]any{}
	_ = json.Unmarshal(b, &m)
	if inf := a.getInfo(); inf != nil {
		m["info"] = inf
	}
	if sys := a.getSys(); sys != nil {
		m["system"] = sys
	}
	if lt := cachedLatestTag(); lt != "" {
		m["latest_version"] = lt
		m["update_available"] = verNewer(lt, version)
	}
	return m
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
		writeJSON(w, a.statusWithInfo())
	}))

	// Kiosk-Anzeige (schöne Vollbild-Seite für einen Monitor an Bord): ohne Login, dafür Token in der
	// Adresse. Die Seite selbst ist harmlos; die Daten dahinter (/api/display) prüfen den Token.
	mux.HandleFunc("/anzeige", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(displayHTML)
	})
	// Seekarten-Ansicht (Hochkant: Pilot-Infos oben, große Karte unten) — gleiche Daten/Token wie /anzeige.
	mux.HandleFunc("/seekarte", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(seekarteHTML)
	})
	mux.HandleFunc("/api/display", func(w http.ResponseWriter, r *http.Request) {
		// Der Kiosk AUF DEM GERÄT selbst (localhost) darf ohne Token lesen — physisch sicher;
		// der Token schützt nur den Zugriff aus dem Netz.
		if !isLoopback(r) {
			tok := a.getCfg().DisplayToken
			if tok == "" || r.URL.Query().Get("token") != tok {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		}
		writeJSON(w, a.statusWithInfo())
	})

	// Startziel für den Geräte-Kiosk (kiosk-browser.sh) — NUR localhost, nicht sensibel: nur der
	// Pfad der gewünschten Startansicht (Pilot/Seekarte). So braucht das Startskript den Token nicht.
	mux.HandleFunc("/api/kiosk-target", func(w http.ResponseWriter, r *http.Request) {
		if !isLoopback(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		cfg := a.getCfg()
		path := "/anzeige"
		if cfg.DisplayView == "seekarte" {
			path = "/seekarte"
		}
		rot := cfg.DisplayRotate
		if rot == "" {
			rot = "normal"
		}
		writeJSON(w, map[string]any{"path": path, "rotate": rot})
	})

	// Standalone-/Offline-Navigation: Anlegestellen-Liste (für das „Ziel"-Auswahlfeld der Anzeige).
	mux.HandleFunc("/api/landings", func(w http.ResponseWriter, r *http.Request) {
		if !a.displayAuthOK(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		// Nur im Standalone-Modus (Betrieb ohne KAI) anbieten; sonst kommt die Route aus dem Bundle.
		if !a.getCfg().Standalone {
			writeJSON(w, map[string]any{"landings": []any{}, "has_net": false})
			return
		}
		n := a.getNet()
		ls := landingsFromNet(n)
		writeJSON(w, map[string]any{"landings": ls, "has_net": n != nil && len(n.Segments) > 0})
	})
	// Route von der aktuellen Position zum gewählten Ziel (Luftlinie oder Fahrrinne) — auch offline.
	mux.HandleFunc("/api/manual-route", func(w http.ResponseWriter, r *http.Request) {
		if !a.displayAuthOK(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if !a.getCfg().Standalone {
			writeJSON(w, map[string]any{"ok": false, "note": "Standalone-Modus aus"})
			return
		}
		pos := a.eng.CurrentPos()
		if !pos.Valid {
			writeJSON(w, map[string]any{"ok": false, "note": "keine Position"})
			return
		}
		mode := r.URL.Query().Get("mode")
		to := r.URL.Query().Get("to")
		writeJSON(w, manualRoute(a.getNet(), pos.Lat, pos.Lng, to, mode))
	})

	// Kamera-Snapshot für die Anzeige: holt das Bild von der konfigurierten Kamera-Snapshot-URL
	// (Mobotix per Basic, AXIS per Digest — s. cameraGet) und reicht es weiter — so bleibt die
	// Kamera-Adresse/der Zugang auf dem Gerät, nicht im Monitor-Browser.
	// Token wie bei /api/display (die Anzeige ist ohne Login, aber mit Token in der Adresse).
	mux.HandleFunc("/api/snapshot", func(w http.ResponseWriter, r *http.Request) {
		cfg := a.getCfg()
		if !isLoopback(r) && (cfg.DisplayToken == "" || r.URL.Query().Get("token") != cfg.DisplayToken) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if cfg.Camera.SnapshotURL == "" {
			http.Error(w, "keine Kamera konfiguriert", http.StatusNotFound)
			return
		}
		resp, err := cameraGet(cfg.Camera.SnapshotURL)
		if err != nil {
			http.Error(w, "Kamera nicht erreichbar", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			http.Error(w, "Kamera-Fehler", http.StatusBadGateway)
			return
		}
		ct := resp.Header.Get("Content-Type")
		if ct == "" {
			ct = "image/jpeg"
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.Copy(w, io.LimitReader(resp.Body, 10*1024*1024))
	})

	// Kamera-LIVESTREAM (MJPEG) für ein flüssiges Bild: holt den fortlaufenden Multipart-Strom von der
	// konfigurierten Livestream-URL (AXIS: /axis-cgi/mjpg/video.cgi, Basic/Digest s. cameraStream) und
	// reicht ihn durch — Zugang bleibt am Gerät. Dauerverbindung (kein Write-Timeout, s. ListenAndServe);
	// endet, wenn der Browser die Verbindung schließt (r.Context()).
	mux.HandleFunc("/api/stream", func(w http.ResponseWriter, r *http.Request) {
		cfg := a.getCfg()
		if !isLoopback(r) && (cfg.DisplayToken == "" || r.URL.Query().Get("token") != cfg.DisplayToken) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if cfg.Camera.StreamURL == "" {
			http.Error(w, "kein Livestream konfiguriert", http.StatusNotFound)
			return
		}
		resp, err := cameraStream(cfg.Camera.StreamURL)
		if err != nil {
			http.Error(w, "Kamera nicht erreichbar", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			http.Error(w, "Kamera-Fehler (Livestream)", http.StatusBadGateway)
			return
		}
		ct := resp.Header.Get("Content-Type")
		if ct == "" {
			ct = "multipart/x-mixed-replace"
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Connection", "close")
		flusher, _ := w.(http.Flusher)
		buf := make([]byte, 32*1024)
		for {
			select {
			case <-r.Context().Done():
				return
			default:
			}
			n, er := resp.Body.Read(buf)
			if n > 0 {
				if _, we := w.Write(buf[:n]); we != nil {
					return
				}
				if flusher != nil {
					flusher.Flush()
				}
			}
			if er != nil {
				return
			}
		}
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

	// Fernzugriff-Infos für die Konfig-Seite: SSH-Benutzer/Passwort/Adresse + fertiger ssh-Befehl.
	mux.HandleFunc("/api/netinfo", a.guard(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, sshInfo(a.getCfg()))
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

	// Versionsprüfung: meldet die laufende und die neueste verfügbare Version (GitHub releases/latest).
	mux.HandleFunc("/api/update-check", a.guard(func(w http.ResponseWriter, r *http.Request) {
		// Den STÜNDLICH gecachten Tag nehmen (wie die Anzeige-Fußzeile), nicht bei jedem Aufruf frisch
		// die GitHub-API fragen — die ist unauthentifiziert rate-limitiert und antwortete dann leer,
		// sodass die Konfigseite „aktuell" zeigte, obwohl es eine neue Version gab.
		latest := cachedLatestTag()
		writeJSON(w, map[string]any{
			"current":         version,
			"latest":          latest,
			"updateAvailable": latest != "" && verNewer(latest, version),
		})
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

	// Anzeige am Monitor einrichten (Kiosk-Autostart + Drehung) — ohne SSH, Dienst läuft als root.
	mux.HandleFunc("/api/kiosk-setup", a.guardUI(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, runKioskSetup())
	}))
	// Fernzugriff (SSH/VNC) einschalten — über die Weboberfläche, da man sonst nicht hineinkommt.
	mux.HandleFunc("/api/remote-access", a.guardUI(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		writeJSON(w, runRemoteAccess(q.Get("ssh") == "1", q.Get("vnc") == "1"))
	}))
	// Kiosk-Diagnose (nur lesend): zeigt, wie Chromium wirklich gestartet wurde + Autostart-Lage.
	mux.HandleFunc("/api/kiosk-diag", a.guardUI(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, runKioskDiag())
	}))

	// Test: Hupe/Lampe kurz schalten
	mux.HandleFunc("/api/test", a.guard(func(w http.ResponseWriter, r *http.Request) {
		a.eng.TestPulse(r.URL.Query().Get("out"), 3)
		writeJSON(w, map[string]any{"ok": true})
	}))

	// GPIO-Selbsttest: zeigt den ECHTEN Grund, warum ein Ausgang nicht schaltet (sonst still verschluckt).
	mux.HandleFunc("/api/gpio-selftest", a.guard(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, runGpioSelftest(a.getCfg()))
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
