package main

// AirPlay-Empfänger aus KAI (Festlegung 2026-11-12, Konzept/Klick-Dummy mit Peter abgestimmt):
//   - Rolle Bildschirm  → Bild spiegeln über uxplay
//   - Rolle Bordmusik   → Ton vom iPad über shairport-sync
// Aus KAI kommt nur, OB der Empfänger läuft, OB eine PIN nötig ist und wie er HEISST (Schiff + Rolle).
// Bild/Ton gehen direkt vom iPad ans Gerät, nie über KAI. Alles standardmäßig AUS — ein Flotten-Update
// ändert nichts, bis es je Gerät in KAI eingeschaltet wird.
//
// Einrichten (Befehl "airplay_setup"): das Werkzeug je Rolle installieren (wie bei der Objekterkennung
// installiert das Gerät selbst, niemand tippt an Bord etwas). Anwenden (beim Start + nach jedem
// Config-Abgleich): den Empfänger starten/stoppen, mit Name und PIN. Der Stand reist mit dem Herzschlag.
//
// ⚠️ AUF ECHTER HARDWARE ZU PRÜFEN/ABSTIMMEN (wie damals Frigate auf der MS-Lindau):
//   (1) Bild-Übergabe: uxplay muss sich vor den Kiosk-Browser legen, wenn ein iPad verbindet, und ihn
//       danach wieder freigeben. Die Display-Umgebung (Wayland/X) hängt am Geräte-Image.
//   (2) PIN beim Bildschirm: uxplay zeigt die PIN beim Verbinden selbst am Schirm an (`-pin`); die in KAI
//       gesetzte feste PIN ist dort vorerst nur informativ. Bei der Bordmusik ist die PIN ein Passwort.
//   (3) Ton bei der Bordmusik: shairport-sync muss denselben Ausgang wie die geplante Bordmusik nutzen
//       und diese beim Einspielen pausieren (Ducking) — am Gerät festzulegen.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

const airplayUnit = "/etc/systemd/system/bordwaechter-airplay.service"

type airplayStand struct {
	Phase    string `json:"phase"`    // "" | laeuft | fertig | fehler
	Meldung  string `json:"meldung"`  // Fehlertext
	Werkzeug string `json:"werkzeug"` // uxplay | shairport-sync
	Am       string `json:"am"`
}

var (
	airplayMu   sync.Mutex
	airplayLauf bool
	// airplayMelden: vom Herzschlag aufgerufen (nur in der Rolle Bildschirm/Bordmusik gesetzt).
	airplayMelden func() map[string]any
)

func airplayPfad() string {
	if _, err := os.Stat("/etc/bordwaechter"); err == nil {
		return "/etc/bordwaechter/airplay.json"
	}
	return "airplay.json"
}

func ladeAirplayStand() airplayStand {
	var s airplayStand
	if b, err := os.ReadFile(airplayPfad()); err == nil {
		_ = json.Unmarshal(b, &s)
	}
	return s
}

func speichereAirplayStand(s airplayStand) {
	s.Am = time.Now().UTC().Format(time.RFC3339)
	b, _ := json.Marshal(s)
	_ = os.WriteFile(airplayPfad(), b, 0o600)
}

// airplayWerkzeug: Programm je Rolle. Bildschirm = Bild (uxplay), Bordmusik = Ton (shairport-sync).
func airplayWerkzeug(rolle string) string {
	if rolle == "audio" {
		return "shairport-sync"
	}
	return "uxplay"
}

// airplayDienst: Name des systemd-Dienstes je Rolle (uxplay als eigener Dienst, shairport-sync bringt seinen mit).
func airplayDienst(rolle string) string {
	if rolle == "audio" {
		return "shairport-sync"
	}
	return "bordwaechter-airplay"
}

func airplayBinaer(rolle string) bool {
	_, err := exec.LookPath(airplayWerkzeug(rolle))
	return err == nil
}

func dienstLaeuft(dienst string) bool {
	return exec.Command("systemctl", "is-active", "--quiet", dienst).Run() == nil
}

// airplayEinrichten (Befehl "airplay_setup"): Werkzeug je Rolle + avahi (mDNS) installieren, danach anwenden.
func (a *App) airplayEinrichten() {
	airplayMu.Lock()
	if airplayLauf {
		airplayMu.Unlock()
		return
	}
	airplayLauf = true
	airplayMu.Unlock()
	defer func() { airplayMu.Lock(); airplayLauf = false; airplayMu.Unlock() }()

	cfg := a.getCfg()
	if !istMedienRolle(cfg.Rolle) {
		logEvent("warn", "AirPlay einrichten nur in der Rolle Bildschirm/Bordmusik möglich")
		return
	}
	wz := airplayWerkzeug(cfg.Rolle)
	s := airplayStand{Phase: "laeuft", Werkzeug: wz, Meldung: wz + " installieren"}
	speichereAirplayStand(s)
	logEvent("info", "AirPlay einrichten: "+wz+" + avahi installieren")

	if err := lauf(10*time.Minute, "apt-get", "update"); err != nil {
		s.Phase, s.Meldung = "fehler", err.Error()
		speichereAirplayStand(s)
		logEvent("error", "AirPlay: apt-get update fehlgeschlagen: "+err.Error())
		return
	}
	if err := lauf(30*time.Minute, "apt-get", "install", "-y", "avahi-daemon", wz); err != nil {
		s.Phase, s.Meldung = "fehler", err.Error()
		speichereAirplayStand(s)
		logEvent("error", "AirPlay: Installation fehlgeschlagen: "+err.Error())
		return
	}
	s.Phase, s.Meldung = "fertig", ""
	speichereAirplayStand(s)
	logEvent("info", "AirPlay: "+wz+" installiert")
	a.airplayAnwenden()
}

// airplayAnwenden: Empfänger je Rolle + Einstellung starten/stoppen. Beim Start und nach jedem Config-Abgleich.
func (a *App) airplayAnwenden() {
	cfg := a.getCfg()
	if !istMedienRolle(cfg.Rolle) {
		return
	}
	dienst := airplayDienst(cfg.Rolle)
	ap := cfg.Airplay
	// Aus oder noch nicht installiert → Empfänger stoppen (schadet nicht, wenn es ihn nicht gibt).
	if !ap.An || !airplayBinaer(cfg.Rolle) {
		_ = lauf(30*time.Second, "systemctl", "disable", "--now", dienst)
		return
	}
	name := strings.TrimSpace(ap.Name)
	if name == "" {
		name = "KAI-Bordgerät"
	}
	var err error
	if cfg.Rolle == "audio" {
		err = airplayShairport(name, ap.PinVerlangen, ap.Pin)
	} else {
		err = airplayUxplay(name, ap.PinVerlangen)
	}
	if err != nil {
		s := ladeAirplayStand()
		s.Phase, s.Meldung = "fehler", err.Error()
		speichereAirplayStand(s)
		logEvent("error", "AirPlay anwenden fehlgeschlagen: "+err.Error())
	}
}

// airplayUxplay: uxplay als systemd-Dienst in der Desktop-Sitzung (Wayland) des Auto-Login-Nutzers.
// ⚠️ Display-Umgebung je Image zu prüfen (s. Kopf). PIN zeigt uxplay beim Verbinden selbst am Schirm.
func airplayUxplay(name string, pinVerlangen bool) error {
	u := desktopUser()
	rt := "/run/user/" + strconv.Itoa(u.uid)
	args := "-n " + schlangeArg(name)
	if pinVerlangen {
		args += " -pin" // uxplay verlangt dann eine PIN (am Schirm angezeigt)
	}
	unit := "[Unit]\n" +
		"Description=KAI AirPlay-Empfaenger (uxplay)\n" +
		"After=graphical.target network-online.target\n" +
		"[Service]\n" +
		"User=" + u.name + "\n" +
		"Environment=XDG_RUNTIME_DIR=" + rt + "\n" +
		"Environment=WAYLAND_DISPLAY=wayland-0\n" +
		"Environment=DBUS_SESSION_BUS_ADDRESS=unix:path=" + rt + "/bus\n" +
		"ExecStart=/usr/bin/uxplay " + args + "\n" +
		"Restart=always\nRestartSec=5\n" +
		"[Install]\nWantedBy=graphical.target\n"
	if err := os.WriteFile(airplayUnit, []byte(unit), 0o644); err != nil {
		return fmt.Errorf("Dienst schreiben: %w", err)
	}
	_ = lauf(30*time.Second, "systemctl", "daemon-reload")
	if err := lauf(30*time.Second, "systemctl", "enable", "--now", "bordwaechter-airplay"); err != nil {
		return err
	}
	// Bei Änderung neu starten, damit Name/PIN wirken.
	_ = lauf(30*time.Second, "systemctl", "restart", "bordwaechter-airplay")
	logEvent("info", "AirPlay (Bildschirm): uxplay läuft als „"+name+"“")
	return nil
}

// airplayShairport: shairport-sync (eigener Paket-Dienst) über /etc/shairport-sync.conf einstellen.
// ⚠️ Tonausgang/Ducking zur Bordmusik am Gerät zu prüfen (s. Kopf).
func airplayShairport(name string, pinVerlangen bool, pin string) error {
	pw := ""
	if pinVerlangen && pin != "" {
		pw = "  password = " + shairportStr(pin) + ";\n"
	}
	conf := "// von KAI geschrieben (AirPlay-Einstellung des Bordgeräts) — nicht von Hand ändern\n" +
		"general =\n{\n" +
		"  name = " + shairportStr(name) + ";\n" +
		pw +
		"};\n"
	if err := os.WriteFile("/etc/shairport-sync.conf", []byte(conf), 0o644); err != nil {
		return fmt.Errorf("shairport-sync.conf schreiben: %w", err)
	}
	if err := lauf(30*time.Second, "systemctl", "enable", "--now", "shairport-sync"); err != nil {
		return err
	}
	_ = lauf(30*time.Second, "systemctl", "restart", "shairport-sync")
	logEvent("info", "AirPlay (Bordmusik): shairport-sync läuft als „"+name+"“")
	return nil
}

// schlangeArg: ein Argument für die ExecStart-Zeile in Anführungszeichen (systemd-tauglich, keine Shell).
func schlangeArg(s string) string {
	return "\"" + strings.ReplaceAll(s, "\"", "'") + "\""
}

// shairportStr: Zeichenkette für die shairport-sync-Konfig (eigene Anführungszeichen, Backslash/Quote entschärft).
func shairportStr(s string) string {
	s = strings.ReplaceAll(s, "\\", "")
	s = strings.ReplaceAll(s, "\"", "'")
	return "\"" + s + "\""
}

// airplayZustand: Stand für den Herzschlag (installiert/läuft/Name/Werkzeug + Phase/Meldung aus der Einrichtung).
func (a *App) airplayZustand() map[string]any {
	cfg := a.getCfg()
	if !istMedienRolle(cfg.Rolle) {
		return nil
	}
	s := ladeAirplayStand()
	wz := airplayWerkzeug(cfg.Rolle)
	return map[string]any{
		"installiert": airplayBinaer(cfg.Rolle),
		"laeuft":      dienstLaeuft(airplayDienst(cfg.Rolle)),
		"aktiv":       false, // ob gerade ein iPad verbunden ist, meldet die erste Fassung noch nicht
		"werkzeug":    wz,
		"name":        strings.TrimSpace(cfg.Airplay.Name),
		"phase":       s.Phase,
		"meldung":     s.Meldung,
	}
}
