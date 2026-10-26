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
	"strconv"
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

func (a *App) bordmusikLoop() {
	time.Sleep(20 * time.Second) // Netz und Herzschlag zuerst
	for {
		cfg := a.getCfg()
		ziel := medienStartZiel(cfg)
		soll := cfg.Rolle == "audio" && cfg.EdgeToken != "" && ziel != "" && !monitorAngeschlossen()
		if !soll {
			if bordmusikLaeuft() {
				bordmusikStop()
				logEvent("info", "Bordmusik im Hintergrund beendet")
			}
		} else if !bordmusikLaeuft() || bordmusikZiel() != ziel {
			bordmusikStop()
			if err := bordmusikStart(ziel); err != nil {
				logEvent("error", "Bordmusik ohne Monitor nicht gestartet: "+err.Error())
			} else {
				logEvent("info", "Bordmusik ohne Monitor gestartet (Abspielseite im Hintergrund)")
			}
		}
		time.Sleep(15 * time.Second)
	}
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
	cmd := exec.Command(bin, bordmusikArgs(ziel, profil)...)
	cmd.Env = []string{"HOME=" + u.home, "USER=" + u.name, "PATH=/usr/local/bin:/usr/bin:/bin"}
	// Ton über die Sitzung des Desktop-Nutzers (PipeWire/Pulse), sonst fällt Chromium auf ALSA zurück
	rt := "/run/user/" + strconv.Itoa(u.uid)
	if _, err := os.Stat(rt); err == nil {
		cmd.Env = append(cmd.Env, "XDG_RUNTIME_DIR="+rt, "DBUS_SESSION_BUS_ADDRESS=unix:path="+rt+"/bus")
	}
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
