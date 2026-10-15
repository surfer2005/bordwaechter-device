package main

import (
	"log"
	"net/http"
	"sync"
	"time"
)

const version = "0.1.63"

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
}

func (a *App) getCfg() *Config     { a.mu.Lock(); defer a.mu.Unlock(); return a.cfg }
func (a *App) getKai() *KaiClient  { a.mu.Lock(); defer a.mu.Unlock(); return a.kai }
func (a *App) getSrc() GPSSource   { a.mu.Lock(); defer a.mu.Unlock(); return a.src }
func (a *App) getInfo() *InfoData  { a.mu.Lock(); defer a.mu.Unlock(); return a.info }
func (a *App) setInfo(i *InfoData) { a.mu.Lock(); a.info = i; a.mu.Unlock() }
func (a *App) setSys(s *SystemData) { a.mu.Lock(); a.sys = s; a.sysAt = time.Now(); a.mu.Unlock() }
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
	if err := saveConfig(n); err != nil {
		return err
	}
	a.mu.Lock()
	a.cfg = n
	a.kai = newKaiClient(n.KaiBaseURL, n.EdgeToken)
	a.src = buildGPS(n, a.manual) // Hinweis: GPIO-Backend-Wechsel greift erst nach Neustart
	a.mu.Unlock()
	a.eng.SetConfig(n, a.getKai())
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
		_ = k.Hello(version)
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
	go app.kmLoop()     // Tageskilometer vom/auf die Platte (reboot-fest)

	log.Printf("Bordwächter %s — lokale Oberfläche auf %s (GPS-Quelle: %s, GPIO: %s)",
		version, cfg.Listen, cfg.GPS.Source, cfg.GPIO.Backend)
	if err := http.ListenAndServe(cfg.Listen, app.routes()); err != nil {
		log.Fatal(err)
	}
}
