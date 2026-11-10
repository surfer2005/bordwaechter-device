package main

import (
	"log"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// applyKeyboard setzt das Tastaturlayout auf Deutsch. Konsole und Kiosk (Chromium) lesen
// /etc/default/keyboard; wirkt vollständig nach einem Neustart. Best effort, als root; nur schreiben,
// wenn noch nicht "de", damit es bei jedem Start idempotent ist.
func applyKeyboard() {
	if imAddon {
		return // im Home-Assistant-Add-on verwaltet HA das System
	}
	cur, _ := os.ReadFile("/etc/default/keyboard")
	if strings.Contains(string(cur), `XKBLAYOUT="de"`) {
		return
	}
	content := "XKBMODEL=\"pc105\"\nXKBLAYOUT=\"de\"\nXKBVARIANT=\"\"\nXKBOPTIONS=\"\"\nBACKSPACE=\"guess\"\n"
	if err := os.WriteFile("/etc/default/keyboard", []byte(content), 0o644); err != nil {
		log.Printf("tastatur: /etc/default/keyboard: %v", err)
		return
	}
	if _, err := exec.LookPath("setupcon"); err == nil {
		_ = exec.Command("setupcon", "--save", "--force").Run() // Konsole
	}
	if _, err := exec.LookPath("localectl"); err == nil {
		_ = exec.Command("localectl", "set-x11-keymap", "de", "pc105").Run() // X/Wayland (Kiosk)
	}
	log.Printf("tastatur: Layout auf Deutsch gesetzt (voll wirksam nach Neustart)")
}

// Fernzugriff/Identität: Hostname und das SSH-Passwort des Benutzers "pi" werden aus der Konfig
// angewandt (best effort, als root). So ist das Gerät als <hostname>.local erreichbar und der
// SSH-Zugang ist einheitlich (Benutzer "pi"). Beide greifen beim Dienststart UND beim Speichern.
const sshUser = "pi"

var hostnameOK = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)

// applyHostname setzt den Gerätenamen (hostnamectl + /etc/hosts), wenn er sich geändert hat. Wirkt
// vollständig nach einem Neustart; der mDNS-Name <hostname>.local folgt dem gesetzten Hostnamen.
func applyHostname(cfg *Config) {
	if imAddon {
		return // im Home-Assistant-Add-on gehört der Rechner Home Assistant
	}
	hn := strings.TrimSpace(cfg.Hostname)
	if hn == "" || !hostnameOK.MatchString(hn) {
		return
	}
	cur, _ := os.ReadFile("/etc/hostname")
	if strings.TrimSpace(string(cur)) == hn {
		return // schon gesetzt
	}
	if _, err := exec.LookPath("hostnamectl"); err == nil {
		if err := exec.Command("hostnamectl", "set-hostname", hn).Run(); err != nil {
			log.Printf("hostname: hostnamectl fehlgeschlagen: %v", err)
		}
	}
	_ = os.WriteFile("/etc/hostname", []byte(hn+"\n"), 0o644)
	// 127.0.1.1-Zeile in /etc/hosts auf den neuen Namen bringen (Debian-Konvention).
	if b, err := os.ReadFile("/etc/hosts"); err == nil {
		lines := strings.Split(string(b), "\n")
		found := false
		for i, l := range lines {
			if strings.HasPrefix(strings.TrimSpace(l), "127.0.1.1") {
				lines[i] = "127.0.1.1\t" + hn
				found = true
			}
		}
		if !found {
			lines = append(lines, "127.0.1.1\t"+hn)
		}
		_ = os.WriteFile("/etc/hosts", []byte(strings.Join(lines, "\n")), 0o644)
	}
	log.Printf("hostname: auf %q gesetzt (voll wirksam nach Neustart)", hn)
}

// applySSHPassword setzt das Passwort des Benutzers "pi" (einheitlicher SSH-Zugang). Leer = unverändert.
func applySSHPassword(cfg *Config) {
	if imAddon {
		return
	}
	pw := strings.TrimSpace(cfg.SSHPassword)
	if pw == "" {
		return
	}
	cmd := exec.Command("chpasswd")
	cmd.Stdin = strings.NewReader(sshUser + ":" + pw + "\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("ssh: Passwort für %q konnte nicht gesetzt werden: %v (%s)", sshUser, err, strings.TrimSpace(string(out)))
		return
	}
	log.Printf("ssh: Passwort für Benutzer %q gesetzt", sshUser)
}

// primaryIP liefert die erste nicht-lokale IPv4-Adresse (für den SSH-Hinweis auf der Konfig-Seite).
func primaryIP() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				ip4 := ipn.IP.To4()
				if ip4 != nil && !ip4.IsLoopback() {
					return ip4.String()
				}
			}
		}
	}
	return ""
}

// sshInfo: Daten für den Fernzugriff-Block der Konfig-Seite (Benutzer/Passwort/Adresse/Befehl).
func sshInfo(cfg *Config) map[string]any {
	hn := strings.TrimSpace(cfg.Hostname)
	ip := primaryIP()
	// Das Passwort steht bewusst NICHT mehr auf der Geräteseite — es wird in KAI verwaltet.
	host := ip
	if hn != "" {
		host = hn + ".local"
	}
	return map[string]any{"user": sshUser, "password_aus_kai": strings.TrimSpace(cfg.SSHPassword) != "", "host": host, "ip": ip, "hostname": hn,
		"command": "ssh " + sshUser + "@" + host}
}
