package main

// Kiosk am Monitor OHNE SSH einrichten: der Dienst (läuft als root) schreibt den Autostart für die
// Desktop-Sitzung des Auto-Login-Nutzers, sodass beim Hochfahren automatisch die Anzeige (Pilot/
// Seekarte) im Vollbild erscheint — auf Raspberry Pi OS Bookworm (Wayland: labwc/wayfire) UND X/LXDE.
// Chromium ist im Desktop-Image bereits vorhanden (kein apt/Internet nötig). Alles wird protokolliert
// und an den Browser zurückgegeben, damit man ohne Shell sieht, was erkannt/getan wurde.

import (
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

//go:embed kiosk-browser.sh
var kioskBrowserSh string

type kioskUser struct {
	name string
	home string
	uid  int
	gid  int
}

// desktopUser: ersten regulären Nutzer (UID 1000, Auto-Login-Desktop-Nutzer) aus /etc/passwd lesen.
func desktopUser() kioskUser {
	u := kioskUser{name: "pi", home: "/home/pi", uid: 1000, gid: 1000}
	b, err := os.ReadFile("/etc/passwd")
	if err != nil {
		return u
	}
	for _, ln := range strings.Split(string(b), "\n") {
		f := strings.Split(ln, ":")
		if len(f) < 6 {
			continue
		}
		uid, _ := strconv.Atoi(f[2])
		if uid == 1000 {
			gid, _ := strconv.Atoi(f[3])
			return kioskUser{name: f[0], home: f[5], uid: uid, gid: gid}
		}
	}
	return u
}

func hasBin(name string) bool { _, err := exec.LookPath(name); return err == nil }

// runCmd führt einen Befehl aus und gibt eine kurze Protokollzeile zurück.
func runCmd(name string, args ...string) string {
	out, err := exec.Command(name, args...).CombinedOutput()
	o := strings.TrimSpace(string(out))
	if err != nil {
		return fmt.Sprintf("%s %s → FEHLER: %v %s", name, strings.Join(args, " "), err, o)
	}
	if o == "" {
		o = "ok"
	}
	return fmt.Sprintf("%s %s → %s", name, strings.Join(args, " "), o)
}

// runRemoteAccess schaltet SSH und/oder VNC ein (als root, kein SSH nötig — über die Weboberfläche).
func runRemoteAccess(ssh, vnc bool) map[string]any {
	log := []string{}
	if ssh {
		// Dienst aktivieren + starten; raspi-config zusätzlich (setzt auch die Boot-Freischaltung).
		log = append(log, runCmd("systemctl", "enable", "--now", "ssh"))
		if hasBin("raspi-config") {
			log = append(log, runCmd("raspi-config", "nonint", "do_ssh", "0"))
		}
	}
	if vnc {
		if hasBin("raspi-config") {
			log = append(log, runCmd("raspi-config", "nonint", "do_vnc", "0"))
		}
		// Bookworm: wayvnc; ältere: vncserver-x11-serviced — beides versuchen (best effort).
		log = append(log, runCmd("systemctl", "enable", "--now", "wayvnc"))
		log = append(log, runCmd("systemctl", "enable", "--now", "vncserver-x11-serviced"))
	}
	return map[string]any{"ok": true, "log": log}
}

func procRunning(name string) bool {
	out, err := exec.Command("pgrep", "-x", name).Output()
	return err == nil && len(strings.TrimSpace(string(out))) > 0
}

// detectKioskEnv: sammelt, was auf dem Gerät vorhanden ist (statt SSH-Diagnose).
func detectKioskEnv() map[string]any {
	osrel := ""
	if b, err := os.ReadFile("/etc/os-release"); err == nil {
		for _, ln := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(ln, "PRETTY_NAME=") {
				osrel = strings.Trim(strings.TrimPrefix(ln, "PRETTY_NAME="), `"`)
			}
		}
	}
	u := desktopUser()
	chromium := "chromium-browser"
	if !hasBin(chromium) {
		chromium = "chromium"
		if !hasBin(chromium) {
			chromium = ""
		}
	}
	return map[string]any{
		"os":          osrel,
		"user":        u.name,
		"home":        u.home,
		"chromium":    chromium,
		"wlr_randr":   hasBin("wlr-randr"),
		"xrandr":      hasBin("xrandr"),
		"labwc":       hasBin("labwc") || procRunning("labwc"),
		"wayfire":     hasBin("wayfire") || procRunning("wayfire"),
		"xorg":        procRunning("Xorg") || procRunning("X"),
	}
}

func chownTo(path string, u kioskUser) { _ = os.Chown(path, u.uid, u.gid) }

// writeUserFile: Datei unter dem Home des Desktop-Nutzers anlegen (Verzeichnisse mit chown).
func writeUserFile(u kioskUser, rel, content string, mode os.FileMode) error {
	full := filepath.Join(u.home, rel)
	dir := filepath.Dir(full)
	// Verzeichniskette anlegen und dem Nutzer geben
	parts := strings.Split(strings.TrimPrefix(dir, u.home+"/"), "/")
	cur := u.home
	for _, p := range parts {
		cur = filepath.Join(cur, p)
		if _, err := os.Stat(cur); os.IsNotExist(err) {
			if err := os.MkdirAll(cur, 0o755); err != nil {
				return err
			}
			chownTo(cur, u)
		}
	}
	if err := os.WriteFile(full, []byte(content), mode); err != nil {
		return err
	}
	chownTo(full, u)
	return nil
}

// appendMarkerLine: Zeile an eine (Shell-)Autostart-Datei anhängen, idempotent über einen Marker.
func appendMarkerLine(u kioskUser, rel, line string) (string, error) {
	full := filepath.Join(u.home, rel)
	existing := ""
	if b, err := os.ReadFile(full); err == nil {
		existing = string(b)
	}
	if strings.Contains(existing, "kiosk-browser.sh") {
		return "schon vorhanden", nil
	}
	content := existing
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	content += line + "\n"
	if err := writeUserFile(u, rel, content, 0o755); err != nil {
		return "", err
	}
	return "geschrieben", nil
}

// runKioskSetup: Autostart an allen gängigen Stellen setzen + kiosk-browser.sh installieren.
func runKioskSetup() map[string]any {
	log := []string{}
	add := func(f string, a ...any) { log = append(log, fmt.Sprintf(f, a...)) }
	env := detectKioskEnv()
	u := desktopUser()
	add("Desktop-Nutzer: %s (%s)", u.name, u.home)
	add("OS: %v · Chromium: %v · wlr-randr: %v · xrandr: %v · labwc: %v · wayfire: %v",
		env["os"], env["chromium"], env["wlr_randr"], env["xrandr"], env["labwc"], env["wayfire"])

	// 1) kiosk-browser.sh nach /usr/local/bin
	if err := os.WriteFile("/usr/local/bin/kiosk-browser.sh", []byte(kioskBrowserSh), 0o755); err != nil {
		add("FEHLER kiosk-browser.sh: %v", err)
	} else {
		add("kiosk-browser.sh → /usr/local/bin (ok)")
	}

	// 2) XDG-Autostart (.desktop) — von LXDE/lxsession u. a. gelesen
	desktop := "[Desktop Entry]\nType=Application\nName=Bordwächter Kiosk\nExec=/usr/local/bin/kiosk-browser.sh\nX-GNOME-Autostart-enabled=true\nComment=bordwaechter-kiosk\n"
	if err := writeUserFile(u, ".config/autostart/bordwaechter-kiosk.desktop", desktop, 0o644); err != nil {
		add("FEHLER XDG-autostart: %v", err)
	} else {
		add(".config/autostart/bordwaechter-kiosk.desktop (ok)")
	}

	// 3) labwc-Autostart (aktueller Raspberry-Pi-OS-Standard) — reines Shell-Skript
	if s, err := appendMarkerLine(u, ".config/labwc/autostart", "/usr/local/bin/kiosk-browser.sh &  # bordwaechter-kiosk"); err != nil {
		add("FEHLER labwc/autostart: %v", err)
	} else {
		add(".config/labwc/autostart (%s)", s)
	}

	// 4) wayfire.ini — nur wenn noch KEIN [autostart] existiert (bestehende Config nicht zerstören)
	wf := filepath.Join(u.home, ".config/wayfire.ini")
	if b, err := os.ReadFile(wf); err == nil {
		if strings.Contains(string(b), "[autostart]") {
			add("wayfire.ini hat schon [autostart] — bitte manuell ergänzen: bordwaechter = /usr/local/bin/kiosk-browser.sh")
		} else {
			content := string(b)
			if !strings.HasSuffix(content, "\n") {
				content += "\n"
			}
			content += "\n[autostart]\nbordwaechter = /usr/local/bin/kiosk-browser.sh\n"
			if err := writeUserFile(u, ".config/wayfire.ini", content, 0o644); err != nil {
				add("FEHLER wayfire.ini: %v", err)
			} else {
				add("wayfire.ini [autostart] ergänzt (ok)")
			}
		}
	} else if env["wayfire"] == true {
		if err := writeUserFile(u, ".config/wayfire.ini", "[autostart]\nbordwaechter = /usr/local/bin/kiosk-browser.sh\n", 0o644); err != nil {
			add("FEHLER wayfire.ini neu: %v", err)
		} else {
			add("wayfire.ini neu angelegt (ok)")
		}
	}

	// 5) lxsession-Autostart (ältere LXDE-Sitzungen)
	if _, err := os.Stat(filepath.Join(u.home, ".config/lxsession/LXDE-pi")); err == nil {
		if s, err := appendMarkerLine(u, ".config/lxsession/LXDE-pi/autostart", "@/usr/local/bin/kiosk-browser.sh"); err == nil {
			add("lxsession/LXDE-pi/autostart (%s)", s)
		}
	}

	if env["chromium"] == "" {
		add("⚠️ Chromium nicht gefunden — auf einem Lite-Image fehlt es (Desktop-Image hat es). Dann: sudo apt install chromium-browser")
	}
	add("Fertig. Zum Aktivieren den Pi neu starten (Knopf „Gerät (Pi) neu starten“).")
	return map[string]any{"ok": true, "env": env, "log": log}
}
