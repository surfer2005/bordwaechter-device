package main

import (
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

const version = "0.1.77"

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
	applyVNCNow := strings.TrimSpace(n.VNCPassword) != ""
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
		if hr, err := k.Hello(version, a.getSys()); err == nil && hr != nil {
			a.setReportScan(hr.ReportScan)
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

// heartbeatLoop meldet KAI minütlich Version + Gesundheits-Block (CPU-Temp/Last/RAM/Platte/Laufzeit),
// damit das Monitoring in KAI online/offline und den Zustand sieht, ohne an Bord zu gehen. Nur
// ausgehend (wie alles hier) und best effort — ein Fehlschlag wird geloggt, nicht weitergereicht.
func (a *App) heartbeatLoop() {
	for {
		cfg := a.getCfg()
		if k := a.getKai(); k != nil && cfg.EdgeToken != "" {
			if hr, err := k.Hello(version, a.getSys()); err != nil {
				log.Printf("heartbeat: %v", err)
			} else if hr != nil {
				a.setReportScan(hr.ReportScan) // Netz-Scan-Schalter aus KAI übernehmen
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

	go app.bundleLoop()
	go app.gpsLoop()
	go app.tickLoop()
	go app.detectLoop() // Objekterkennung (nur aktiv, wenn cfg.Detect.Enabled)
	go app.infoLoop()   // Wetter + Pegel für die Anzeige (nur aktiv, wenn cfg.Info.Weather/Pegel)
	go app.sysLoop()    // Pi-Zustand (CPU-Temp/Last/RAM/Platte) für die HA-Integration
	go app.heartbeatLoop()  // minütlicher Herzschlag an KAI (Version + Gesundheit fürs Monitoring)
	go app.smartmeterLoop() // Smartmeter-Bridge: Zähler per Modbus lesen (Konfig aus KAI) → an KAI pushen
	go app.netScanLoop()    // Netz-Lebenszeichen: Scan-Ziele aus KAI, Ping+ARP, Status zurück (Asset Manager)
	go app.routerLoop()     // Teltonika-Router-Diagnose per Modbus → KAI (Asset Manager/Monitoring)
	go app.kmLoop()     // Tageskilometer vom/auf die Platte (reboot-fest)
	go app.releaseCheckLoop() // GitHub-Release-Tag stündlich cachen (für „neue Version"-Hinweis)
	go app.netLoop()    // Streckennetz + Anlegestellen cachen (Standalone-/Offline-Navigation)

	log.Printf("Bordwächter %s — lokale Oberfläche auf %s (GPS-Quelle: %s, GPIO: %s)",
		version, cfg.Listen, cfg.GPS.Source, cfg.GPIO.Backend)
	if err := http.ListenAndServe(cfg.Listen, app.routes()); err != nil {
		log.Fatal(err)
	}
}
