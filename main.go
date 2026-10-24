package main

import (
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

const version = "0.1.91"

type App struct {
	mu     sync.Mutex
	cfg    *Config
	kai    *KaiClient
	gpio   GPIO
	manual *ManualGPS
	src    GPSSource
	eng    *Engine
	info   *InfoData
	sys    *SystemData
	sysAt  time.Time
	net    *NetworkData
	scan   *bool // Netz-Scan-Schalter aus KAI (hello-Antwort); nil = noch nicht gemeldet → cfg.ReportScan
	telt   *bool // Teltonika-Router-Diagnose an/aus aus KAI; nil = noch nicht gemeldet → an (bei GPS=teltonika)
}

// getReportScan: maßgeblich ist der KAI-Schalter (hello-Antwort); solange KAI noch nichts gemeldet hat,
// gilt der lokale Wert als Rückfall (autark).
func (a *App) getReportScan() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.scan != nil {
		return *a.scan
	}
	return a.cfg.ReportScan
}
func (a *App) setReportScan(v bool) { a.mu.Lock(); a.scan = &v; a.mu.Unlock() }

// getReportTeltonika: KAI-Schalter; bis KAI etwas meldet, ist die Router-Diagnose an (Rückfall).
func (a *App) getReportTeltonika() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.telt != nil {
		return *a.telt
	}
	return true
}
func (a *App) setReportTeltonika(v bool) { a.mu.Lock(); a.telt = &v; a.mu.Unlock() }

// gpsHostForHello: die Router-/Modbus-Adresse, wenn GPS per Teltonika läuft (für die Teltonika-Übersicht).
func (a *App) gpsHostForHello() (string, int) {
	c := a.getCfg()
	if c.GPS.Source == "teltonika" {
		return c.GPS.Host, c.GPS.Port
	}
	return "", 0
}

func (a *App) getCfg() *Config     { a.mu.Lock(); defer a.mu.Unlock(); return a.cfg }
func (a *App) getKai() *KaiClient  { a.mu.Lock(); defer a.mu.Unlock(); return a.kai }
func (a *App) getSrc() GPSSource   { a.mu.Lock(); defer a.mu.Unlock(); return a.src }
func (a *App) getInfo() *InfoData  { a.mu.Lock(); defer a.mu.Unlock(); return a.info }
func (a *App) setInfo(i *InfoData) { a.mu.Lock(); a.info = i; a.mu.Unlock() }
func (a *App) setSys(s *SystemData) { a.mu.Lock(); a.sys = s; a.sysAt = time.Now(); a.mu.Unlock() }
func (a *App) getNet() *NetworkData { a.mu.Lock(); defer a.mu.Unlock(); return a.net }
func (a *App) setNet(n *NetworkData) { a.mu.Lock(); a.net = n; a.mu.Unlock() }

// netLoop: Streckennetz + Anlegestellen von KAI holen und cachen (für den Standalone-/Offline-Modus).
// Beim Start aus dem Platten-Cache laden, dann bei Verbindung gelegentlich auffrischen.
func (a *App) netLoop() {
	if n := loadNetwork(); n != nil && len(n.Segments) > 0 {
		a.setNet(n)
	}
	for {
		if k := a.getKai(); k != nil && a.getCfg().EdgeToken != "" {
			if n, err := k.GetNetwork(); err == nil && len(n.Segments) > 0 {
				a.setNet(n)
				saveNetwork(n)
			}
		}
		sleepSec(6*60*60, 6*60*60) // alle ~6 h auffrischen
	}
}
func (a *App) getSys() *SystemData {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sys == nil {
		return nil
	}
	cp := *a.sys
	cp.AgeSec = int(time.Since(a.sysAt).Seconds())
	return &cp
}

func (a *App) applyConfig(n *Config) error {
	// SSH-Passwort wird in der Oberfläche nie zurückgespielt (leer = unverändert) → bestehenden Wert
	// bewahren, damit ein Speichern ohne Eingabe ihn nicht verwirft.
	if strings.TrimSpace(n.SSHPassword) == "" {
		n.SSHPassword = a.getCfg().SSHPassword
	}
	// VNC-Passwort: nur anwenden, wenn in der Oberfläche eines eingegeben wurde; leer = bestehenden
	// Wert bewahren (gleiche Regel wie SSH — die Oberfläche spielt es nie zurück).
	// (aus KAI kommt das Passwort bei jedem Abgleich mit — angewandt wird es nur, wenn es sich ändert)
	applyVNCNow := strings.TrimSpace(n.VNCPassword) != "" && n.VNCPassword != a.getCfg().VNCPassword
	if !applyVNCNow {
		n.VNCPassword = a.getCfg().VNCPassword
	}
	if err := saveConfig(n); err != nil {
		return err
	}
	a.mu.Lock()
	a.cfg = n
	a.kai = newKaiClient(n.KaiBaseURL, n.EdgeToken)
	a.src = buildGPS(n, a.manual) // Hinweis: GPIO-Backend-Wechsel greift erst nach Neustart
	a.mu.Unlock()
	a.eng.SetConfig(n, a.getKai())
	applyHostname(n)    // Hostname beim Speichern anwenden (voll wirksam nach Neustart)
	applySSHPassword(n) // SSH-Passwort beim Speichern anwenden
	if applyVNCNow {
		applyVNC(n) // VNC-Passwort setzen + VNC-Dienst sicherstellen (nur bei Eingabe)
	}
	return nil
}

func sleepSec(v, def int) {
	if v <= 0 {
		v = def
	}
	time.Sleep(time.Duration(v) * time.Second)
}

func (a *App) bundleLoop() {
	// Beim Start das zuletzt gespeicherte Bundle von der Platte laden → nach Offline-Neustart gelten
	// sofort wieder die KAI-Werte (statt der Code-Defaults), auch bevor KAI erreichbar ist.
	if b := loadBundle(); b != nil {
		a.eng.SetBundle(b)
		log.Printf("bundle: aus Platten-Cache geladen (%s)", b.Date)
	}
	if k := a.getKai(); k != nil && a.getCfg().EdgeToken != "" {
		gh, gp := a.gpsHostForHello()
		if hr, err := k.Hello(version, "bordwaechter", a.getSys(), gh, gp, a.sifaBlock(), anzeigeZustand(a.getCfg())); err == nil && hr != nil {
			kaiReachable = true
			a.setReportScan(hr.ReportScan)
			a.setReportTeltonika(hr.ReportTeltonika)
		} else {
			kaiReachable = false
		}
	}
	for {
		cfg := a.getCfg()
		if k := a.getKai(); k != nil && cfg.EdgeToken != "" {
			if b, err := k.GetBundle(cfg.ShipID); err == nil {
				a.eng.SetBundle(b)
				saveBundle(b) // für Offline-Neustart auf die Platte sichern
			} else {
				log.Printf("bundle: %v", err)
			}
		}
		sleepSec(cfg.BundleRefreshSec, 300)
	}
}

// sifaBlock baut den SIFA-/Totmann-Zustand für den Heartbeat aus dem Engine-Status + der SIFA-Konfig:
// Hupe/Leuchttaster/Quittier-Taster an/aus, Zustand (ok|vorwarnung|alarm), Zeit bis zur nächsten
// Auslösung (Deadman-Countdown) sowie die konfigurierten Intervalle. KAI zeigt ihn je Bordwächter.
func (a *App) sifaBlock() map[string]any {
	st := a.eng.Status()
	cfg := a.getCfg()
	state := "ok"
	if st.AlarmState == "alarm" {
		state = "alarm"
	} else if st.SifaActive && cfg.SIFA.DeadmanPrewarnSec > 0 && st.SifaCountdownSec > 0 && st.SifaCountdownSec <= cfg.SIFA.DeadmanPrewarnSec {
		state = "vorwarnung"
	}
	return map[string]any{
		"active":      st.SifaActive || st.AlarmState != "normal",
		"horn":        st.Horn,
		"lamp":        st.Lamp,
		"ack":         st.AckPressed,
		"state":       state,
		"next_sec":    st.SifaCountdownSec,
		"deadman_sec": cfg.SIFA.DeadmanSec,
		"prewarn_sec": cfg.SIFA.DeadmanPrewarnSec,
		"rearm_sec":   cfg.SIFA.ReAlarmSec,
		"ruht":        st.SifaRuht,
		"kamera_ruht": st.KameraRuht,
		"im_hafen":    st.ImHafen,
		// Lage wie die Status-Kacheln der Geräteseite — KAI zeigt dasselbe (Abgleich Gerät ↔ KAI)
		"lage": map[string]any{
			"ship": st.Ship, "reg": st.Reg, "phase": st.Phase, "from": st.From, "to": st.To, "soll": st.Soll,
			"remain_km": st.RemainKm, "ist_kmh": st.IstKmh, "soll_kmh": st.SollKmh, "verdict": st.Verdict,
			"off_course_m": st.OffCourseM, "off_state": st.OffState, "pos_valid": st.PosValid, "pos_age_sec": st.PosAgeSec,
			"alarm_state": st.AlarmState, "ack_level_high": st.AckLevelHigh,
		},
	}
}

// updateAusKai: im KAI-Monitoring angeforderte Aktualisierung — wie der Knopf „Aktualisieren“ am Gerät
// (Nutzervorgabe 2026-10-22: von beiden Seiten aktualisieren). Erst KAI melden (Flag weg), dann laden + neu starten.
var updateLaeuft sync.Mutex

func updateAusKai(k *KaiClient) {
	if !updateLaeuft.TryLock() {
		return
	}
	defer updateLaeuft.Unlock()
	k.UpdateGestartet()
	if err := selfUpdate(); err != nil {
		logEvent("error", "Aktualisierung (aus KAI) fehlgeschlagen: "+err.Error())
		return
	}
	logEvent("info", "Aktualisierung aus KAI geladen, Neustart (bisher Version "+version+")")
	reexecSoon()
}

// auftragLoop fragt alle 10 s bei KAI nach, ob ein Auftrag wartet (Speedtest) — vorher kam er erst mit dem
// minütlichen Herzschlag an (Nutzerhinweis 2026-10-22 „warum müssen wir beim Speedtest auf den Herzschlag warten“).
func (a *App) auftragLoop() {
	for {
		time.Sleep(10 * time.Second)
		cfg := a.getCfg()
		if k := a.getKai(); k != nil && cfg.EdgeToken != "" {
			if !istMedienRolle(cfg.Rolle) {
				_ = k.SendSifa(a.sifaBlock()) // SIFA-Restzeit für KAI aktuell halten (ältere KAI antworten 404, harmlos)
			}
			if au, err := k.GetAuftrag(); err == nil {
				if au.Speedtest {
					go a.runSpeedtest()
				}
				if au.Update {
					go updateAusKai(k)
				}
				if au.ConfigRev > cfg.KaiConfigRev {
					go a.kaiSync(au.ConfigRev, false) // neuer Stand der Einstellungen in KAI
				}
				if au.Befehl != nil {
					go a.kaiBefehl(au.Befehl) // Testschalter aus KAI
				}
			}
		}
	}
}

// heartbeatLoop meldet KAI minütlich Version + Gesundheits-Block (CPU-Temp/Last/RAM/Platte/Laufzeit),
// damit das Monitoring in KAI online/offline und den Zustand sieht, ohne an Bord zu gehen. Nur
// ausgehend (wie alles hier) und best effort — ein Fehlschlag wird geloggt, nicht weitergereicht.
func (a *App) heartbeatLoop() {
	for {
		cfg := a.getCfg()
		if k := a.getKai(); k != nil && cfg.EdgeToken != "" {
			gh, gp := a.gpsHostForHello()
			var sifa map[string]any
			if istMedienRolle(cfg.Rolle) {
				gh, gp = "", 0 // Bildschirm/Bordmusik: kein GPS, keine SIFA
			} else {
				sifa = a.sifaBlock()
			}
			if hr, err := k.Hello(version, rolleFuerKai(cfg.Rolle), a.getSys(), gh, gp, sifa, anzeigeZustand(cfg)); err != nil {
				kaiReachable = false
				log.Printf("heartbeat: %v", err)
			} else if hr != nil {
				kaiReachable = true
				a.setReportScan(hr.ReportScan)           // Netz-Scan-Schalter aus KAI
				a.setReportTeltonika(hr.ReportTeltonika)  // Teltonika-Schalter aus KAI
				if hr.SpeedtestRequested {
					go a.runSpeedtest() // im KAI-Monitoring angefordert
				}
				if hr.UpdateRequested {
					go updateAusKai(k)
				}
				if hr.LatestVersion != "" {
					setLatestTag(hr.LatestVersion) // „Neue Version verfügbar“ binnen einer Minute statt stündlich
				}
				if hr.ConfigRev > cfg.KaiConfigRev {
					go a.kaiSync(hr.ConfigRev, false)
				}
				setBordToken(hr.BordToken) // Position fürs Bord-Netz (Bildschirme/Bordmusik)
				a.medienAusHerzschlag(hr)  // Bildschirm/Bordmusik: Abspielseite + Bordwächter-Adresse
			}
		}
		time.Sleep(60 * time.Second)
	}
}

func (a *App) gpsLoop() {
	for {
		cfg := a.getCfg()
		src := a.getSrc()
		if src != nil {
			if p, err := src.Read(); err == nil && p.Valid {
				a.eng.SetPosition(p)
				if cfg.SendPosition {
					if k := a.getKai(); k != nil && cfg.EdgeToken != "" {
						// die ANGEREICHERTE Position posten (mit aus der Bewegung abgeleitetem Tempo),
						// nicht die rohe p (die trägt bei Teltonika ohne Speed-Register 0).
						go func(pp Position) { _ = k.PostPosition(pp) }(a.eng.CurrentPos())
					}
				}
			}
		}
		sleepSec(cfg.GPS.PollSec, 5)
	}
}

func (a *App) tickLoop() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for range t.C {
		a.eng.Tick(time.Now())
	}
}

// kmLoop: den Tageskilometer-Stand beim Start von der Platte übernehmen und danach periodisch sichern,
// damit ein Neustart mitten am Tag den Zähler nicht verliert.
func (a *App) kmLoop() {
	if d, km := loadTodayKm(); d != "" {
		a.eng.SeedTodayKm(d, km)
	}
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for range t.C {
		d, km := a.eng.TodayKm()
		if d != "" {
			saveTodayKm(d, km)
		}
	}
}

func main() {
	cfg := loadConfig()
	refreshKioskOnStartup() // Kiosk-Skript + Keyring beim Start auf den Programmstand bringen
	applyHostname(cfg)      // Gerätename (hostnamectl + /etc/hosts) → erreichbar als <hostname>.local
	applySSHPassword(cfg)   // einheitliches SSH-Passwort für Benutzer "pi"
	manual := &ManualGPS{}
	kai := newKaiClient(cfg.KaiBaseURL, cfg.EdgeToken)
	gpio := buildGPIO(cfg)
	eng := NewEngine(cfg, kai, gpio)
	app := &App{cfg: cfg, kai: kai, gpio: gpio, manual: manual, src: buildGPS(cfg, manual), eng: eng}

	if istMedienRolle(cfg.Rolle) {
		// Bildschirm/Bordmusik: nur Herzschlag, Protokoll, Aktualisierung und Gerätezustand — kein GPS,
		// keine SIFA/Hupe, kein Smartmeter. Der Browser spielt die Abspielseite aus KAI.
		go app.sysLoop()
		go app.auftragLoop()
		go app.heartbeatLoop()
		go app.devlogLoop()
		go app.releaseCheckLoop()
		go app.anzeigeAutomatischSpaeter() // Abspielseite am Monitor einrichten (einmalig)
		log.Printf("KAI-Bordgerät %s in der Rolle %s — lokale Oberfläche auf %s", version, cfg.Rolle, cfg.Listen)
		if err := http.ListenAndServe(cfg.Listen, app.routes()); err != nil {
			log.Fatal(err)
		}
		return
	}
	go app.bundleLoop()
	go app.gpsLoop()
	go app.tickLoop()
	go app.detectLoop() // Objekterkennung (nur aktiv, wenn cfg.Detect.Enabled)
	go app.infoLoop()   // Wetter + Pegel für die Anzeige (nur aktiv, wenn cfg.Info.Weather/Pegel)
	go app.sysLoop()    // Pi-Zustand (CPU-Temp/Last/RAM/Platte) für die HA-Integration
	go app.auftragLoop()    // alle 10 s: wartet ein Auftrag (Speedtest)?
	go app.heartbeatLoop()  // minütlicher Herzschlag an KAI (Version + Gesundheit fürs Monitoring)
	go app.devlogLoop()     // Geräte-Protokoll (Ereignisse/Fehler, offline gepuffert) → KAI
	go app.smartmeterLoop() // Smartmeter-Bridge: Zähler per Modbus lesen (Konfig aus KAI) → an KAI pushen
	go app.netScanLoop()    // Netz-Lebenszeichen: Scan-Ziele aus KAI, Ping+ARP, Status zurück (Asset Manager)
	go app.routerLoop()     // Teltonika-Router-Diagnose per Modbus → KAI (Asset Manager/Monitoring)
	go app.kmLoop()     // Tageskilometer vom/auf die Platte (reboot-fest)
	go app.releaseCheckLoop() // GitHub-Release-Tag stündlich cachen (für „neue Version"-Hinweis)
	go app.netLoop()    // Streckennetz + Anlegestellen cachen (Standalone-/Offline-Navigation)
	go app.kaiStartSync() // Einstellungen einmal mit KAI abgleichen (Stand kann sich offline geändert haben)

	log.Printf("KAI-Bordgerät %s, Rolle Bordwächter — lokale Oberfläche auf %s (GPS-Quelle: %s, GPIO: %s)",
		version, cfg.Listen, cfg.GPS.Source, cfg.GPIO.Backend)
	if err := http.ListenAndServe(cfg.Listen, app.routes()); err != nil {
		log.Fatal(err)
	}
}
