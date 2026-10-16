package main

// Kiosk am Monitor OHNE SSH einrichten: der Dienst (läuft als root) schreibt den Autostart für die
// Desktop-Sitzung des Auto-Login-Nutzers, sodass beim Hochfahren automatisch die Anzeige (Pilot/
// Seekarte) im Vollbild erscheint — auf Raspberry Pi OS Bookworm (Wayland: labwc/wayfire) UND X/LXDE.
// Chromium ist im Desktop-Image bereits vorhanden (kein apt/Internet nötig). Alles wird protokolliert
// und an den Browser zurückgegeben, damit man ohne Shell sieht, was erkannt/getan wurde.

import (
	_ "embed"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// refreshKioskOnStartup hält das installierte Kiosk-Skript + die Keyring-Vorsorge beim Dienststart
// auf dem Stand des Programms — damit „Aktualisieren" allein reicht (das Autostart-Skript wird
// sonst beim Update NICHT miterneuert, s. Keyring-Problem). Nur aktiv, wenn der Kiosk eingerichtet ist.
func refreshKioskOnStartup() {
	u := desktopUser()
	_, aErr := os.Stat(filepath.Join(u.home, ".config/autostart/bordwaechter-kiosk.desktop"))
	_, sErr := os.Stat("/usr/local/bin/kiosk-browser.sh")
	if aErr != nil && sErr != nil {
		return // kein Kiosk eingerichtet → nichts tun
	}
	if err := os.WriteFile("/usr/local/bin/kiosk-browser.sh", []byte(kioskBrowserSh), 0o755); err != nil {
		log.Printf("kiosk-refresh: %v", err)
		return
	}
	writeChromiumPolicy()
	for _, n := range []string{"gnome-keyring-secrets", "gnome-keyring-pkcs11", "gnome-keyring-ssh"} {
		body := "[Desktop Entry]\nType=Application\nName=" + n + " (deaktiviert)\nExec=/bin/true\nHidden=true\nX-GNOME-Autostart-enabled=false\nNoDisplay=true\n"
		_ = writeUserFile(u, ".config/autostart/"+n+".desktop", body, 0o644)
	}
	if _, err := os.Stat(filepath.Join(u.home, ".local/share/keyrings/default")); os.IsNotExist(err) {
		_ = writeUserFile(u, ".local/share/keyrings/Default_keyring.keyring", "[keyring]\ndisplay-name=Default keyring\nctime=0\nmtime=0\nlock-on-idle=false\nlock-after=false\n", 0o600)
		_ = writeUserFile(u, ".local/share/keyrings/default", "Default_keyring\n", 0o600)
	}
	log.Printf("kiosk: Autostart-Skript + Keyring beim Start aufgefrischt (Version %s)", version)
}

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

// writeChromiumPolicy legt eine Enterprise-Richtlinie an, die die Übersetzen-Leiste („Deutsch/
// Englisch") und andere störende Einblendungen im Kiosk ABSCHALTET — unabhängig von den
// Kommandozeilen-Flags. Chromium liest /etc/chromium[/-browser]/policies/managed/*.json.
func writeChromiumPolicy() {
	policy := `{"TranslateEnabled": false, "DefaultBrowserSettingEnabled": false, "MetricsReportingEnabled": false, "PasswordManagerEnabled": false, "BackgroundModeEnabled": false}`
	for _, dir := range []string{"/etc/chromium/policies/managed", "/etc/chromium-browser/policies/managed"} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			continue
		}
		_ = os.WriteFile(filepath.Join(dir, "bordwaechter.json"), []byte(policy), 0o644)
	}
}

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

// runKioskDiag liest (nur lesend) den Ist-Zustand aus, um die Keyring-/Kiosk-Ursache ohne SSH zu sehen.
func runKioskDiag() map[string]any {
	u := desktopUser()
	log := []string{}
	add := func(f string, a ...any) { log = append(log, fmt.Sprintf(f, a...)) }
	add("Bordwächter-Version (läuft): %s", version)
	add("--- Wie wurde Chromium gestartet? (sucht --password-store) ---")
	add(runCmd("bash", "-lc", "ps -eo args 2>/dev/null | grep -i chromi | grep -v grep | head -3 | tr -s ' '"))
	add("--- Laufende Compositor/Keyring-Prozesse ---")
	add(runCmd("bash", "-lc", "pgrep -a -f 'labwc|wayfire|cage|gnome-keyring|chromi' 2>/dev/null | head -8"))
	add("--- /usr/local/bin/kiosk-browser.sh enthält password-store? ---")
	add(runCmd("bash", "-lc", "grep -c password-store /usr/local/bin/kiosk-browser.sh 2>/dev/null; grep -c 'keyrings' /usr/local/bin/kiosk-browser.sh 2>/dev/null"))
	add("--- Keyring-Verzeichnis ---")
	add(runCmd("bash", "-lc", "ls -la "+u.home+"/.local/share/keyrings 2>&1 | head"))
	add("--- Autostart-Einträge (die Chromium starten könnten) ---")
	add(runCmd("bash", "-lc", "grep -rl -i 'chromi\\|kiosk-browser' "+u.home+"/.config 2>/dev/null"))
	add("--- labwc/autostart ---")
	add(runCmd("bash", "-lc", "cat "+u.home+"/.config/labwc/autostart 2>&1 | head"))
	return map[string]any{"ok": true, "log": log}
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
	add("Bordwächter-Version: %s", version)
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

	// 6) gnome-keyring vorab mit LEEREM Passwort anlegen → der „Choose password for new keyring"-Dialog
	//    erscheint dann gar nicht (blockiert sonst den Kiosk). Nur anlegen, wenn noch keiner da ist.
	krDefault := filepath.Join(u.home, ".local/share/keyrings/default")
	if _, err := os.Stat(krDefault); os.IsNotExist(err) {
		kr := "[keyring]\ndisplay-name=Default keyring\nctime=0\nmtime=0\nlock-on-idle=false\nlock-after=false\n"
		if err := writeUserFile(u, ".local/share/keyrings/Default_keyring.keyring", kr, 0o600); err != nil {
			add("FEHLER Keyring: %v", err)
		} else if err := writeUserFile(u, ".local/share/keyrings/default", "Default_keyring\n", 0o600); err != nil {
			add("FEHLER Keyring-default: %v", err)
		} else {
			add("leerer Default-Keyring angelegt (kein Passwort-Dialog mehr)")
		}
	} else {
		add("Keyring existiert bereits (unverändert)")
	}

	// 7) gnome-keyring-Dienst beim Sitzungsstart abschalten → GAR keine App kann mehr den
	//    „Choose password for new keyring"-Dialog auslösen (chromium braucht ihn mit --password-store=basic
	//    nicht; WLAN-Passwörter liegen bei raspios in NetworkManager, nicht im User-Keyring).
	for _, n := range []string{"gnome-keyring-secrets", "gnome-keyring-pkcs11", "gnome-keyring-ssh"} {
		body := "[Desktop Entry]\nType=Application\nName=" + n + " (deaktiviert)\nExec=/bin/true\nHidden=true\nX-GNOME-Autostart-enabled=false\nNoDisplay=true\n"
		if err := writeUserFile(u, ".config/autostart/"+n+".desktop", body, 0o644); err != nil {
			add("FEHLER keyring-autostart %s: %v", n, err)
		}
	}
	add("gnome-keyring-Autostart deaktiviert (kein Passwort-Dialog mehr)")
	writeChromiumPolicy()
	add("Chromium-Richtlinie geschrieben (Übersetzen-Leiste aus)")

	if env["chromium"] == "" {
		add("⚠️ Chromium nicht gefunden — auf einem Lite-Image fehlt es (Desktop-Image hat es). Dann: sudo apt install chromium-browser")
	}
	add("Fertig. Zum Aktivieren den Pi neu starten (Knopf „Gerät (Pi) neu starten“).")
	return map[string]any{"ok": true, "env": env, "log": log}
}
