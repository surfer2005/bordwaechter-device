package main

import (
	"log"
	"net/http"
	"sync"
	"time"
)

const version = "0.1.31"

type App struct {
	mu     sync.Mutex
	cfg    *Config
	kai    *KaiClient
	gpio   GPIO
	manual *ManualGPS
	src    GPSSource
	eng    *Engine
}

func (a *App) getCfg() *Config     { a.mu.Lock(); defer a.mu.Unlock(); return a.cfg }
func (a *App) getKai() *KaiClient  { a.mu.Lock(); defer a.mu.Unlock(); return a.kai }
func (a *App) getSrc() GPSSource   { a.mu.Lock(); defer a.mu.Unlock(); return a.src }

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

	log.Printf("Bordwächter %s — lokale Oberfläche auf %s (GPS-Quelle: %s, GPIO: %s)",
		version, cfg.Listen, cfg.GPS.Source, cfg.GPIO.Backend)
	if err := http.ListenAndServe(cfg.Listen, app.routes()); err != nil {
		log.Fatal(err)
	}
}
