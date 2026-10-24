package main

// Anzeige am Monitor aus KAI (Festlegung 2026-10-24): Startansicht, Drehung und Display-Token kommen aus
// KAI. Ist das Gerät aus KAI gesteuert und hängt ein Monitor dran, richtet es die Anzeige (Browser im
// Vollbild beim Hochfahren) selbst ein — der Knopf auf der Geräteseite bleibt nur als Notbehelf. Der
// Zustand („läuft / läuft nicht“) geht mit dem Herzschlag an KAI; die rohe Kiosk-Diagnose bleibt am Gerät.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// monitorAngeschlossen: meldet irgendein Bildschirmausgang „connected“ (HDMI/DSI)?
func monitorAngeschlossen() bool {
	files, _ := filepath.Glob("/sys/class/drm/card*-*/status")
	for _, f := range files {
		if b, err := os.ReadFile(f); err == nil && strings.TrimSpace(string(b)) == "connected" {
			return true
		}
	}
	return false
}

func kioskEingerichtet() bool {
	_, err := os.Stat(filepath.Join(desktopUser().home, ".config/autostart/bordwaechter-kiosk.desktop"))
	return err == nil
}

func browserLaeuft() bool {
	return exec.Command("pgrep", "-f", "chromi.*--kiosk").Run() == nil
}

// anzeigeGesteuert: Anzeige wird aus KAI bestimmt (Bordwächter mit KAI-Einstellungen bzw. Bildschirm/Bordmusik).
func anzeigeGesteuert(c *Config) bool {
	return c != nil && c.EdgeToken != "" && (c.KaiConfigRev > 0 || istMedienRolle(c.Rolle))
}

// anzeigeZustand: was KAI im Monitoring zeigt.
func anzeigeZustand(c *Config) map[string]any {
	ansicht := "Pilot"
	if c.DisplayView == "seekarte" {
		ansicht = "Seekarte"
	}
	if istMedienRolle(c.Rolle) {
		ansicht = "Abspielseite"
	}
	dreh := c.DisplayRotate
	if dreh == "" {
		dreh = "normal"
	}
	return map[string]any{"monitor": monitorAngeschlossen(), "eingerichtet": kioskEingerichtet(), "laeuft": browserLaeuft(), "ansicht": ansicht, "drehung": dreh}
}

// anzeigeAutomatisch richtet die Anzeige einmal selbst ein, wenn ein Monitor dranhängt. Kein Neustart —
// an Bord läuft das Gerät womöglich gerade; wirksam wird es beim nächsten Hochfahren (KAI zeigt das an).
func (a *App) anzeigeAutomatisch() {
	cfg := a.getCfg()
	if !anzeigeGesteuert(cfg) || !monitorAngeschlossen() || kioskEingerichtet() {
		return
	}
	runKioskSetup()
	logEvent("info", "Anzeige am Monitor aus KAI eingerichtet – wirksam nach dem nächsten Neustart")
}

func (a *App) anzeigeAutomatischSpaeter() {
	time.Sleep(25 * time.Second)
	a.anzeigeAutomatisch()
}
