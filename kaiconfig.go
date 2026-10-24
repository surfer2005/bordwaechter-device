package main

// Einstellungen aus KAI (Nutzerentscheidung 2026-10-24): alle Einstellungen werden in KAI je Schiff
// gepflegt und hier übernommen; am Gerät bleiben nur KAI-Adresse und Netzwerk. Gekoppelt wird mit einem
// Code AUS KAI (30 Min., einmal) — derselbe Weg tauscht ein defektes Gerät.
//
// Ablauf des Abgleichs (kaiSync):
//  1. das Gerät meldet seinen eigenen Stand (POST /api/edge/config-ist) — hat KAI ein Feld noch nicht,
//     füllt KAI es damit (KAI bleibt führend, überschreibt also nichts, was dort gepflegt ist);
//  2. das Gerät holt die Einstellungen (GET /api/edge/config) und übernimmt NUR die Felder, die KAI
//     liefert — was KAI nicht kennt, bleibt wie es ist;
//  3. das Gerät meldet den übernommenen Stand zurück.
// Ausgelöst, sobald KAI einen neueren Stand meldet (Auftrags-Abruf alle 10 s bzw. Herzschlag).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Felder, die NIE aus KAI kommen: Verbindung zu KAI selbst, lokaler Port und Netzwerk (sonst käme das
// Gerät nach einem Fehler nicht mehr ins Netz).
var nurAmGeraet = map[string]bool{"kai_base_url": true, "edge_token": true, "listen": true, "network": true, "bundle_refresh_sec": true, "kai_config_rev": true,
	"rolle": true, "medien_pfad": true, "bordwaechter": true, "bordwaechter_token": true}

type koppelAntwort struct {
	OK        bool   `json:"ok"`
	Rolle     string `json:"rolle"`
	Token     string `json:"token"`
	Device    string `json:"device"`
	ShipID    int    `json:"ship_id"`
	ConfigRev int    `json:"config_rev"`
	Error     string `json:"error"`
	Medien    *struct {
		Name string `json:"name"`
		Pfad string `json:"pfad"`
	} `json:"medien"`
}

// koppelnMitKai löst einen Kopplungscode aus KAI ein und liefert den Geräte-Schlüssel.
func koppelnMitKai(base, code string) (*koppelAntwort, error) {
	k := newKaiClient(base, "")
	// Rolle kommt aus dem Code (Bordwächter, Bildschirm oder Bordmusik) — „geraet“: mit Gerätesoftware gekoppelt
	body, _ := json.Marshal(map[string]any{"code": code, "geraet": true, "version": version, "serial": geraeteSerial()})
	req, err := http.NewRequest("POST", k.base+"/api/edge/koppeln", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := k.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("KAI nicht erreichbar: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var a koppelAntwort
	_ = json.Unmarshal(data, &a)
	if resp.StatusCode != 200 || a.Token == "" {
		if a.Error == "" {
			a.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("%s", a.Error)
	}
	return &a, nil
}

// geraeteSerial: Seriennummer des Raspberry (für KAI zur Wiedererkennung), sonst leer.
func geraeteSerial() string {
	raw, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(l, "Serial") {
			if i := strings.Index(l, ":"); i > 0 {
				return strings.TrimSpace(l[i+1:])
			}
		}
	}
	return ""
}

type kaiConfig struct {
	Rev    int            `json:"rev"`
	Config map[string]any `json:"config"`
	ShipID int            `json:"ship_id"`
	Name   string         `json:"name"`
}

func (k *KaiClient) GetConfig() (*kaiConfig, error) {
	data, code, err := k.do("GET", "/api/edge/config", nil)
	if err != nil {
		return nil, err
	}
	if code != 200 {
		return nil, fmt.Errorf("config http %d", code)
	}
	var c kaiConfig
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func (k *KaiClient) PostConfigIst(rev int, cfg *Config) error {
	m := configAlsMap(cfg)
	delete(m, "edge_token")
	_, code, err := k.do("POST", "/api/edge/config-ist", map[string]any{"rev": rev, "config": m})
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("config-ist http %d", code)
	}
	return nil
}

func (k *KaiClient) BefehlErledigt(n int, ok bool, fehler string) {
	_, _, _ = k.do("POST", "/api/edge/befehl-erledigt", map[string]any{"n": n, "ok": ok, "fehler": fehler})
}

func configAlsMap(c *Config) map[string]any {
	b, _ := json.Marshal(c)
	m := map[string]any{}
	_ = json.Unmarshal(b, &m)
	return m
}

// mischeKaiConfig legt die von KAI gelieferten Felder über den aktuellen Stand (rekursiv), ohne die
// Felder, die nur am Gerät gepflegt werden. Reine Funktion (testbar).
func mischeKaiConfig(aktuell *Config, kai map[string]any) (*Config, error) {
	basis := configAlsMap(aktuell)
	for k, v := range kai {
		if nurAmGeraet[k] {
			continue
		}
		if sub, ok := v.(map[string]any); ok {
			ziel, _ := basis[k].(map[string]any)
			if ziel == nil {
				ziel = map[string]any{}
			}
			for sk, sv := range sub {
				ziel[sk] = sv
			}
			basis[k] = ziel
			continue
		}
		basis[k] = v
	}
	b, err := json.Marshal(basis)
	if err != nil {
		return nil, err
	}
	n := defaultConfig()
	if err := json.Unmarshal(b, n); err != nil {
		return nil, err
	}
	return n, nil
}

var kaiSyncMu sync.Mutex

// kaiSync gleicht die Einstellungen mit KAI ab, wenn KAI einen neueren Stand hat (oder erzwingen=true).
func (a *App) kaiSync(kaiRev int, erzwingen bool) {
	if !kaiSyncMu.TryLock() {
		return
	}
	defer kaiSyncMu.Unlock()
	cfg := a.getCfg()
	k := a.getKai()
	if k == nil || cfg.EdgeToken == "" {
		return
	}
	if !erzwingen && kaiRev <= cfg.KaiConfigRev {
		return
	}
	// 1) eigenen Stand melden — füllt in KAI nur die Lücken
	_ = k.PostConfigIst(cfg.KaiConfigRev, cfg)
	// 2) Einstellungen holen und übernehmen
	kc, err := k.GetConfig()
	if err != nil {
		logEvent("warn", "Einstellungen aus KAI nicht abholbar: "+err.Error())
		return
	}
	if kc.Rev == 0 || (kc.Rev <= cfg.KaiConfigRev && !erzwingen) {
		return // in KAI ist (noch) nichts gepflegt bzw. nichts Neues
	}
	n, err := mischeKaiConfig(cfg, kc.Config)
	if err != nil {
		logEvent("error", "Einstellungen aus KAI nicht lesbar: "+err.Error())
		return
	}
	n.KaiBaseURL, n.EdgeToken, n.Listen, n.Network, n.BundleRefreshSec = cfg.KaiBaseURL, cfg.EdgeToken, cfg.Listen, cfg.Network, cfg.BundleRefreshSec
	if kc.ShipID > 0 {
		n.ShipID = kc.ShipID
	}
	n.KaiConfigRev = kc.Rev
	if err := a.applyConfig(n); err != nil {
		logEvent("error", "Einstellungen aus KAI nicht gespeichert: "+err.Error())
		return
	}
	logEvent("info", fmt.Sprintf("Einstellungen aus KAI übernommen (Stand %d)", kc.Rev))
	// 3) übernommenen Stand zurückmelden
	_ = k.PostConfigIst(kc.Rev, a.getCfg())
}

// kaiBefehl führt einen Testbefehl aus KAI aus (Testschalter gibt es in KAI UND am Gerät).
var letzterBefehl int

func (a *App) kaiBefehl(b *kaiBefehlT) {
	if b == nil || b.N == 0 || b.N == letzterBefehl {
		return
	}
	letzterBefehl = b.N
	k := a.getKai()
	ok, fehler := true, ""
	switch b.Cmd {
	case "horn":
		a.eng.TestPulse("horn", 3)
	case "lamp":
		a.eng.TestPulse("lamp", 3)
	case "ack":
		a.eng.Ack()
	case "gpio":
		r := runGpioSelftest(a.getCfg())
		for _, p := range r.Pins {
			if !p.OK {
				ok = false
			}
		}
		if !ok {
			fehler = "GPIO-Selbsttest mit Fehlern – Details auf der Konfig-Seite am Gerät"
		}
	default:
		ok, fehler = false, "unbekannter Befehl "+b.Cmd
	}
	logEvent("info", "Testbefehl aus KAI: "+b.Cmd)
	if k != nil {
		k.BefehlErledigt(b.N, ok, fehler)
	}
}

type kaiBefehlT struct {
	N   int    `json:"n"`
	Cmd string `json:"cmd"`
}

// kaiStartSync: nach dem Start einmal abgleichen (der Stand kann sich offline geändert haben).
func (a *App) kaiStartSync() {
	time.Sleep(15 * time.Second)
	a.kaiSync(0, true)
}
