package main

import (
	"log"
	"os/exec"
	"strings"
)

// applyVNC setzt das VNC-Passwort für den Bildschirm-Fernzugriff und stellt den VNC-Dienst sicher —
// best effort, als root über die Weboberfläche. So lässt sich VNC wie beim Kiosk über ein Passwortfeld
// einstellen (statt eines separaten „VNC einschalten"-Knopfes). Leer = nichts tun (der Aufrufer wendet
// es nur an, wenn tatsächlich ein Passwort eingegeben wurde).
//   - Raspberry Pi OS (RealVNC): `vncpasswd -service`.
//   - Fallback Debian/x11vnc: Passwortdatei schreiben.
func applyVNC(cfg *Config) {
	pw := strings.TrimSpace(cfg.VNCPassword)
	if pw == "" {
		return
	}
	switch {
	case hasCmd("vncpasswd"):
		cmd := exec.Command("vncpasswd", "-service")
		cmd.Stdin = strings.NewReader(pw + "\n" + pw + "\n")
		if out, err := cmd.CombinedOutput(); err != nil {
			log.Printf("vnc: vncpasswd fehlgeschlagen: %v (%s)", err, strings.TrimSpace(string(out)))
		} else {
			log.Printf("vnc: RealVNC-Passwort gesetzt")
		}
	case hasCmd("x11vnc"):
		if err := exec.Command("x11vnc", "-storepasswd", pw, "/etc/bordwaechter/vncpasswd").Run(); err != nil {
			log.Printf("vnc: x11vnc -storepasswd fehlgeschlagen: %v", err)
		} else {
			log.Printf("vnc: x11vnc-Passwort gesetzt")
		}
	default:
		log.Printf("vnc: kein VNC-Server gefunden (vncpasswd/x11vnc) — Passwort nicht gesetzt")
	}
	// VNC-Dienst sicherstellen (best effort — je nach OS wayvnc, RealVNC oder x11vnc).
	_ = exec.Command("raspi-config", "nonint", "do_vnc", "0").Run()
	for _, unit := range []string{"vncserver-x11-serviced", "wayvnc", "x11vnc.service"} {
		_ = exec.Command("systemctl", "enable", "--now", unit).Run()
	}
}

func hasCmd(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}
