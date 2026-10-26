package main

// Bordmusik ohne Monitor (2026-10-26, live MS-Lindau): ein Audio-Player hängt an der ELA-Anlage, meist ohne
// Bildschirm. Der Anzeige-Autostart greift aber nur mit Monitor — ohne ihn spielte das Gerät nichts. Hier
// startet der Dienst die Abspielseite aus KAI deshalb selbst in einem unsichtbaren Chromium (headless) als
// Desktop-Nutzer, mit Ton über PipeWire/ALSA, und hält ihn am Laufen. Hängt doch ein Monitor dran, übernimmt
// der normale Kiosk (anzeige.go) und dieser Weg tritt zurück.

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var (
	bmMu   sync.Mutex
	bmProz *exec.Cmd
	bmZiel string
	bmAn   bool // Prozess läuft (vom Wait-Goroutine zurückgesetzt)
)

func bordmusikLaeuft() bool {
	bmMu.Lock()
	defer bmMu.Unlock()
	return bmAn
}

func bordmusikZiel() string {
	bmMu.Lock()
	defer bmMu.Unlock()
	return bmZiel
}

func bordmusikStop() {
	bmMu.Lock()
	defer bmMu.Unlock()
	if bmProz != nil && bmProz.Process != nil && bmAn {
		_ = syscall.Kill(-bmProz.Process.Pid, syscall.SIGKILL) // ganze Prozessgruppe (Chromium-Kinder)
	}
	bmProz, bmZiel, bmAn = nil, "", false
}

// bordmusikArgs: Aufruf für den unsichtbaren Browser (rein, testbar).
func bordmusikArgs(ziel, profil string) []string {
	return []string{"--headless=new", "--autoplay-policy=no-user-gesture-required",
		"--user-data-dir=" + profil, "--password-store=basic", "--no-first-run", "--no-default-browser-check",
		"--disable-gpu", "--noerrdialogs", "--disable-features=TranslateUI,Translate", "--window-size=1280,720", ziel}
}

// bordmusikArgsSitzung: Chromium als normales App-Fenster in der laufenden Desktop-Sitzung (rein, testbar).
func bordmusikArgsSitzung(ziel, profil string) []string {
	return []string{"--ozone-platform=wayland", "--autoplay-policy=no-user-gesture-required",
		"--user-data-dir=" + profil, "--password-store=basic", "--no-first-run", "--no-default-browser-check",
		"--noerrdialogs", "--disable-features=TranslateUI,Translate", "--kiosk", "--app=" + ziel}
}

// anzeigeNachholen: Bildschirm mit Monitor, Anzeige eingerichtet, aber kein Browser (eingerichtet wird ohne Neustart,
// der Autostart greift erst beim nächsten Hochfahren — MS-Lindau 2026-10-26): Kiosk in der laufenden Sitzung starten.
var anzeigeVersucht time.Time

func anzeigeNachholen(cfg *Config) {
	if cfg.Rolle != "bildschirm" || !monitorAngeschlossen() || !kioskEingerichtet() || browserLaeuft() {
		return
	}
	if time.Since(anzeigeVersucht) < 2*time.Minute {
		return // dem Browser Zeit zum Starten lassen, nicht im Takt neu starten
	}
	u := desktopUser()
	rt := "/run/user/" + strconv.Itoa(u.uid)
	if _, err := os.Stat(rt + "/wayland-0"); err != nil {
		return // keine Desktop-Sitzung — dann bleibt es beim Autostart
	}
	if _, err := os.Stat("/usr/local/bin/kiosk-browser.sh"); err != nil {
		return
	}
	anzeigeVersucht = time.Now()
	cmd := exec.Command("/usr/local/bin/kiosk-browser.sh")
	cmd.Env = []string{"HOME=" + u.home, "USER=" + u.name, "PATH=/usr/local/bin:/usr/bin:/bin", "XDG_RUNTIME_DIR=" + rt,
		"DBUS_SESSION_BUS_ADDRESS=unix:path=" + rt + "/bus", "WAYLAND_DISPLAY=wayland-0", "XDG_SESSION_TYPE=wayland"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(u.uid), Gid: uint32(u.gid)}, Setpgid: true}
	if err := cmd.Start(); err != nil {
		logEvent("error", "Anzeige konnte nicht gestartet werden: "+err.Error())
		return
	}
	go func() { _ = cmd.Wait() }()
	logEvent("info", "Anzeige am Monitor gestartet (ohne Neustart)")
}

func (a *App) bordmusikLoop() {
	time.Sleep(20 * time.Second) // Netz und Herzschlag zuerst
	for {
		cfg := a.getCfg()
		anzeigeNachholen(cfg)
		ziel := medienStartZiel(cfg)
		soll := cfg.Rolle == "audio" && cfg.EdgeToken != "" && ziel != "" && !monitorAngeschlossen()
		if !soll {
			if bordmusikLaeuft() {
				bordmusikStop()
				logEvent("info", "Bordmusik im Hintergrund beendet")
			}
		} else if !bordmusikLaeuft() || bordmusikZiel() != ziel {
			bordmusikStop()
			tonVorbereiten(cfg.MedienTon)
			if err := bordmusikStart(ziel); err != nil {
				logEvent("error", "Bordmusik ohne Monitor nicht gestartet: "+err.Error())
			} else {
				logEvent("info", "Bordmusik ohne Monitor gestartet (Abspielseite im Hintergrund)")
			}
		}
		time.Sleep(15 * time.Second)
	}
}

// alsNutzer führt einen Befehl als Desktop-Nutzer in dessen Sitzung aus (PipeWire gehört dem Nutzer, nicht root).
func alsNutzer(u kioskUser, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	rt := "/run/user/" + strconv.Itoa(u.uid)
	cmd.Env = []string{"HOME=" + u.home, "USER=" + u.name, "PATH=/usr/local/bin:/usr/bin:/bin", "XDG_RUNTIME_DIR=" + rt,
		"DBUS_SESSION_BUS_ADDRESS=unix:path=" + rt + "/bus"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(u.uid), Gid: uint32(u.gid)}}
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

var senkeZeile = regexp.MustCompile(`^[\s│├└─]*(\*)?\s*(\d+)\.\s+(.+?)(\s+\[vol:.*\])?$`)

// senken: Tonausgänge aus „wpctl status“ (Abschnitt Audio → Sinks), mit Kennzeichen für den Standard (rein, testbar).
type senke struct {
	ID       string
	Name     string
	Standard bool
}

func senken(status string) []senke {
	var out []senke
	inAudio, inSinks := false, false
	for _, z := range strings.Split(status, "\n") {
		t := strings.TrimSpace(z)
		switch {
		case strings.HasPrefix(t, "Audio"):
			inAudio, inSinks = true, false
			continue
		case strings.HasPrefix(t, "Video") || strings.HasPrefix(t, "Settings"):
			inAudio, inSinks = false, false
			continue
		}
		if !inAudio {
			continue
		}
		if strings.Contains(t, "Sinks:") {
			inSinks = true
			continue
		}
		if strings.Contains(t, "Sources:") || strings.Contains(t, "Filters:") || strings.Contains(t, "Streams:") || strings.Contains(t, "Devices:") {
			inSinks = false
			continue
		}
		if inSinks {
			if m := senkeZeile.FindStringSubmatch(z); m != nil {
				out = append(out, senke{ID: m[2], Name: strings.TrimSpace(m[3]), Standard: m[1] == "*"})
			}
		}
	}
	return out
}

// passendeSenke: Ausgang laut KAI wählen — „hdmi“ einen HDMI-Ausgang, sonst (analog) den ersten Nicht-HDMI-Ausgang
// (Kopfhörerbuchse, USB-Soundkarte). Leer, wenn keiner passt (rein, testbar).
func passendeSenke(l []senke, ton string) string {
	for _, s := range l {
		h := strings.Contains(strings.ToLower(s.Name), "hdmi")
		if (ton == "hdmi") == h && !strings.Contains(strings.ToLower(s.Name), "dummy") {
			return s.ID
		}
	}
	return ""
}

// tonVorbereiten: PipeWire-Ausgang wählen, laut stellen, Stummschaltung aufheben — und festhalten, was gefunden wurde,
// damit in KAI im Geräte-Protokoll steht, warum ggf. nichts zu hören ist.
func tonVorbereiten(ton string) {
	u := desktopUser()
	karten, _ := exec.Command("aplay", "-l").CombinedOutput()
	var namen []string
	for _, z := range strings.Split(string(karten), "\n") {
		if strings.HasPrefix(z, "card ") {
			if i := strings.Index(z, "["); i > 0 {
				if j := strings.Index(z[i:], "]"); j > 0 {
					namen = append(namen, z[i+1:i+j])
				}
			}
		}
	}
	info := "Ton: Karten " + strings.Join(namen, ", ")
	if len(namen) == 0 {
		info = "Ton: keine Soundkarte gefunden"
	}
	if exec.Command("pgrep", "-u", strconv.Itoa(u.uid), "-x", "pipewire").Run() != nil {
		logEvent("warn", info+" · PipeWire läuft nicht (Ausgabe direkt über ALSA, Standardkarte)")
		return
	}
	st, err := alsNutzer(u, "wpctl", "status")
	if err != nil {
		logEvent("warn", info+" · wpctl nicht nutzbar: "+err.Error())
		return
	}
	l := senken(st)
	var liste []string
	for _, s := range l {
		liste = append(liste, s.Name)
	}
	if id := passendeSenke(l, ton); id != "" {
		_, _ = alsNutzer(u, "wpctl", "set-default", id)
	}
	_, _ = alsNutzer(u, "wpctl", "set-mute", "@DEFAULT_AUDIO_SINK@", "0")
	_, _ = alsNutzer(u, "wpctl", "set-volume", "@DEFAULT_AUDIO_SINK@", "1.0")
	akt, _ := alsNutzer(u, "wpctl", "inspect", "@DEFAULT_AUDIO_SINK@")
	gewaehlt := ""
	for _, z := range strings.Split(akt, "\n") {
		if strings.Contains(z, "node.description") {
			gewaehlt = strings.Trim(strings.TrimSpace(z[strings.Index(z, "=")+1:]), `"`)
		}
	}
	logEvent("info", info+" · Ausgänge: "+strings.Join(liste, ", ")+" · gewählt: "+gewaehlt+" (laut KAI: "+ton+")")
}

func bordmusikStart(ziel string) error {
	bin := "chromium"
	if !hasBin(bin) {
		bin = "chromium-browser"
	}
	writeChromiumPolicy() // Zugriff der Abspielseite auf den eigenen Gerätedienst (Position, lokale Befehle)
	u := desktopUser()
	profil := filepath.Join(u.home, ".config/bordwaechter-bordmusik")
	_ = os.MkdirAll(profil, 0o755)
	chownTo(profil, u)
	rt := "/run/user/" + strconv.Itoa(u.uid)
	// Läuft die Desktop-Sitzung (auch ohne Monitor), spielt ein normales Chromium-Fenster darin — mit dem echten
	// Tonweg der Sitzung. Nur ohne Sitzung der unsichtbare (headless) Browser.
	sitzung := ""
	if _, err := os.Stat(rt + "/wayland-0"); err == nil {
		sitzung = "wayland-0"
	}
	args := bordmusikArgs(ziel, profil)
	if sitzung != "" {
		args = bordmusikArgsSitzung(ziel, profil)
	}
	cmd := exec.Command(bin, args...)
	cmd.Env = []string{"HOME=" + u.home, "USER=" + u.name, "PATH=/usr/local/bin:/usr/bin:/bin"}
	if _, err := os.Stat(rt); err == nil {
		cmd.Env = append(cmd.Env, "XDG_RUNTIME_DIR="+rt, "DBUS_SESSION_BUS_ADDRESS=unix:path="+rt+"/bus")
	}
	if sitzung != "" {
		cmd.Env = append(cmd.Env, "WAYLAND_DISPLAY="+sitzung)
	}
	logEvent("info", "Bordmusik-Browser: "+map[bool]string{true: "in der Desktop-Sitzung", false: "unsichtbar (ohne Desktop-Sitzung)"}[sitzung != ""])
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(u.uid), Gid: uint32(u.gid)}, Setpgid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	bmMu.Lock()
	bmProz, bmZiel, bmAn = cmd, ziel, true
	bmMu.Unlock()
	go func() {
		_ = cmd.Wait()
		bmMu.Lock()
		if bmProz == cmd {
			bmAn = false // beendet → die Schleife startet ihn neu
		}
		bmMu.Unlock()
	}()
	return nil
}
