package main

// Smartmeter-Bridge: holt die zentrale Zähler-Konfig von KAI (GET /api/edge/smartmeter-config),
// liest die Register ROH per Modbus TCP (modbusReadHolding, s. modbus.go) und pusht die Werte an KAI
// (POST /api/edge/telemetry). Gefiltert wird ZENTRAL in KAI — das Gerät ist nur Bridge. Nur ausgehend,
// best effort: ein einzelner Lesefehler überspringt den Zähler, bricht nicht den ganzen Lauf ab.

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type smRegister struct {
	Address  int    `json:"address"`
	Type     string `json:"type"`
	Measure  string `json:"measure"`
	Unit     string `json:"unit"`
	Kind     string `json:"kind"`
	EntityID string `json:"entity_id"`
	Name     string `json:"name"`
}
type smMeter struct {
	Name      string       `json:"name"`
	Slave     int          `json:"slave"`
	Host      string       `json:"host"` // leer = Geräte-Host (sc.Host); gesetzt = eigener Modbus-Host (Zähler auf anderer IP)
	Port      int          `json:"port"` // 0 = Geräte-Port (sc.Port)
	Registers []smRegister `json:"registers"`
}
type smConfig struct {
	Enabled            bool      `json:"enabled"`
	Host               string    `json:"host"`
	Port               int       `json:"port"`
	PushIntervalSec    int       `json:"push_interval_sec"`
	OfflineIntervalSec int       `json:"offline_interval_sec"` // ohne Netz nur alle N s einen Wert puffern (Vorgabe 300)
	Meters             []smMeter `json:"meters"`
}
type smReading struct {
	EntityID   string  `json:"entity_id"`
	Name       string  `json:"name"`
	Unit       string  `json:"unit"`
	Value      float64 `json:"value"`
	RecordedAt string  `json:"recorded_at,omitempty"` // Messzeit (UTC) — damit KAI nachgelieferte Werte richtig einordnet
}

func (k *KaiClient) GetSmartmeterConfig() (*smConfig, error) {
	data, code, err := k.do("GET", "/api/edge/smartmeter-config", nil)
	if err != nil {
		return nil, err
	}
	if code != 200 {
		return nil, fmt.Errorf("smartmeter-config http %d", code)
	}
	var c smConfig
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func (k *KaiClient) PostTelemetry(readings []smReading) error {
	_, code, err := k.do("POST", "/api/edge/telemetry", map[string]any{"readings": readings})
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("telemetry http %d", code)
	}
	return nil
}

// ── Puffer ohne Netz (Peter 2026-11-18: „keine Verbindung zum Land → Werte bei sich speichern und nachliefern“) ──
// Jeder gelesene Wert geht in eine Warteschlange und wird von vorne (älteste zuerst) an KAI geschickt; erst nach
// dessen Bestätigung fällt er heraus. Ohne Netz wird nur alle offline_interval_sec (Vorgabe 5 min) ein Satz aufgenommen
// — beim Verbrauch zählt die Differenz der Zählerstände, die Lücke dazwischen schadet nicht. Die Warteschlange liegt
// auch auf der Platte (übersteht einen Neustart), höchstens smPufferMax Werte (danach fallen die ältesten heraus).
const (
	smPufferMax      = 120000 // ≈ 14 Tage bei 30 Werten alle 5 min
	smPaket          = 500    // Werte je Sendung
	smPaketeJeRunde  = 40     // höchstens so viele Pakete je Runde (Nachholen nach langer Pause in wenigen Minuten)
	smOfflineVorgabe = 300 * time.Second
)

type smPuffer struct {
	mu      sync.Mutex
	werte   []smReading
	letzte  time.Time // letzte Aufnahme
	offline bool      // letzter Versand gescheitert
}

var smWarteschlange = &smPuffer{}

func smOrdner() string { return filepath.Join(filepath.Dir(configPath()), "smartmeter") }

// smZeit: Messzeit im selben Format wie KAI (ISO, ms, Z). Uhr offensichtlich falsch (vor 2024) → leer, KAI nimmt die Ankunft.
func smZeit(t time.Time) string {
	if t.Year() < 2024 {
		return ""
	}
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// smAufnehmen (rein, testbar): neue Werte an die Warteschlange hängen. Online jedes Mal; offline nur, wenn seit der
// letzten Aufnahme mindestens `intervall` vergangen ist. Über `max` fallen die ältesten heraus.
func smAufnehmen(warte, neu []smReading, offline bool, letzte, jetzt time.Time, intervall time.Duration, max int) ([]smReading, bool) {
	if len(neu) == 0 {
		return warte, false
	}
	if offline && !letzte.IsZero() && jetzt.Sub(letzte) < intervall {
		return warte, false
	}
	warte = append(warte, neu...)
	if max > 0 && len(warte) > max {
		warte = append([]smReading(nil), warte[len(warte)-max:]...)
	}
	return warte, true
}

func (p *smPuffer) aufnehmen(neu []smReading, intervall time.Duration) {
	p.mu.Lock()
	w, genommen := smAufnehmen(p.werte, neu, p.offline, p.letzte, time.Now(), intervall, smPufferMax)
	p.werte = w
	if genommen {
		p.letzte = time.Now()
	}
	offline := p.offline
	p.mu.Unlock()
	if genommen && offline {
		p.sichern() // ohne Netz: Stand auf die Platte (alle paar Minuten)
	}
}

// senden: Pakete von vorne schicken, nach jeder Bestätigung vorne entfernen. Fehler → offline, Stand sichern.
func (p *smPuffer) senden(k *KaiClient) {
	for runde := 0; runde < smPaketeJeRunde; runde++ {
		p.mu.Lock()
		n := len(p.werte)
		if n == 0 {
			p.mu.Unlock()
			break
		}
		if n > smPaket {
			n = smPaket
		}
		stueck := append([]smReading(nil), p.werte[:n]...)
		p.mu.Unlock()
		if err := k.PostTelemetry(stueck); err != nil {
			p.mu.Lock()
			war := p.offline
			p.offline = true
			rest := len(p.werte)
			p.mu.Unlock()
			if !war {
				log.Printf("smartmeter: keine Verbindung zu KAI (%v) — puffere, alle %v ein Satz", err, smOfflineVorgabe)
				logEvent("warn", fmt.Sprintf("Smartmeter: keine Verbindung zu KAI — Werte werden gepuffert (%d im Puffer)", rest))
			}
			p.sichern()
			return
		}
		p.mu.Lock()
		p.werte = p.werte[len(stueck):]
		war := p.offline
		p.offline = false
		rest := len(p.werte)
		p.mu.Unlock()
		if war {
			logEvent("info", fmt.Sprintf("Smartmeter: Verbindung zu KAI wieder da — liefere gepufferte Werte nach (%d offen)", rest+len(stueck)))
		}
	}
	p.mu.Lock()
	leer := len(p.werte) == 0
	p.mu.Unlock()
	if leer {
		_ = os.Remove(filepath.Join(smOrdner(), "puffer.json"))
	} else {
		p.sichern()
	}
}

func (p *smPuffer) anzahl() int { p.mu.Lock(); defer p.mu.Unlock(); return len(p.werte) }

func (p *smPuffer) sichern() {
	p.mu.Lock()
	daten, err := json.Marshal(p.werte)
	p.mu.Unlock()
	if err != nil {
		return
	}
	_ = os.MkdirAll(smOrdner(), 0o700)
	tmp := filepath.Join(smOrdner(), "puffer.json.tmp")
	if os.WriteFile(tmp, daten, 0o600) == nil {
		_ = os.Rename(tmp, filepath.Join(smOrdner(), "puffer.json"))
	}
}

func (p *smPuffer) laden() {
	b, err := os.ReadFile(filepath.Join(smOrdner(), "puffer.json"))
	if err != nil {
		return
	}
	var alt []smReading
	if json.Unmarshal(b, &alt) == nil && len(alt) > 0 {
		p.mu.Lock()
		p.werte = append(alt, p.werte...)
		p.offline = true // bis der erste Versand klappt
		p.mu.Unlock()
		log.Printf("smartmeter: %d gepufferte Werte von der Platte geladen", len(alt))
	}
}

// Zuletzt bekannte Zähler-Einstellung auf der Platte: ohne Netz weiterlesen, statt gar nichts zu messen.
func smKonfigSichern(c *smConfig) {
	if daten, err := json.Marshal(c); err == nil {
		_ = os.MkdirAll(smOrdner(), 0o700)
		_ = os.WriteFile(filepath.Join(smOrdner(), "konfig.json"), daten, 0o600)
	}
}
func smKonfigLaden() *smConfig {
	b, err := os.ReadFile(filepath.Join(smOrdner(), "konfig.json"))
	if err != nil {
		return nil
	}
	var c smConfig
	if json.Unmarshal(b, &c) != nil {
		return nil
	}
	return &c
}

// smartmeterLoop: Konfig holen (ohne Netz: die zuletzt bekannte) → Register lesen → in die Warteschlange → nachliefern.
func (a *App) smartmeterLoop() {
	time.Sleep(25 * time.Second) // Netz/Token erst hochkommen lassen
	p := smWarteschlange
	p.laden()
	merk := smKonfigLaden()
	for {
		interval := 30 * time.Second
		cfg := a.getCfg()
		k := a.getKai()
		verbunden := k != nil && cfg.EdgeToken != ""
		sc := merk
		if verbunden {
			if c, err := k.GetSmartmeterConfig(); err != nil {
				if p.anzahl() == 0 && merk == nil {
					log.Printf("smartmeter: Konfig: %v", err)
				}
			} else {
				sc = c
				merk = c
				smKonfigSichern(c)
			}
		}
		if sc != nil && sc.Enabled && len(sc.Meters) > 0 {
			if sc.PushIntervalSec > 0 {
				interval = time.Duration(sc.PushIntervalSec) * time.Second
			}
			offlineIv := smOfflineVorgabe
			if sc.OfflineIntervalSec > 0 {
				offlineIv = time.Duration(sc.OfflineIntervalSec) * time.Second
			}
			p.aufnehmen(readSmartmeter(sc), offlineIv)
		}
		if verbunden && p.anzahl() > 0 {
			p.senden(k) // auch wenn der Smartmeter inzwischen aus ist: Gepuffertes noch abliefern
		}
		time.Sleep(interval)
	}
}

// smLesen ist austauschbar, damit die Tests ohne echtes Gateway laufen.
var smLesen = modbusReadHolding

// smKeineAntwort: der Zähler hat gar nicht geantwortet (Zeitüberschreitung, Verbindung abgelehnt oder
// abgebrochen). Eine Modbus-Ausnahme ist dagegen eine Antwort — der Zähler ist da, nur das Register nicht.
func smKeineAntwort(err error) bool {
	if err == nil {
		return false
	}
	var a modbusAusnahme
	return !errors.As(err, &a)
}

// smStumm merkt sich je Zähler, ob er im letzten Durchgang geschwiegen hat — nur für eine Log-Zeile beim
// Wechsel (sonst stünde bei einem stromlosen Landzähler alle 30 s dieselbe Meldung im Protokoll).
// readSmartmeter läuft nur in der einen smartmeterLoop-Goroutine, daher ohne Sperre.
var smStumm = map[string]bool{}

// readSmartmeter liest alle Register aller Zähler. Das Standard-Profil ist durchweg float32 (2 Register,
// High-Word zuerst — wie HA; regsToFloat deckt das ab). Gemischte Typen (Teltonika) kommen separat.
//
// ⚠️ Stummer Zähler (Peter 2026-11-18): bekommt schon die ERSTE Anfrage an einen Zähler gar keine Antwort,
// werden dessen übrige Register in diesem Durchgang ausgelassen. Vorher wartete der Bordwächter bei jedem der
// 11–13 Register die volle Zeit (5 s) — ein stromloser oder abgeklemmter Zähler (MF-FRIEDRICHSHAFEN Land RO,
// MS-LINDAU Landzähler in Fahrt) dehnte so den Takt des GANZEN Schiffs von 30 auf 90–155 s, auch für die Zähler,
// die antworten. Der Zähler bleibt eingeschaltet und wird im nächsten Durchgang wieder gefragt. Antwortet ein
// Zähler, ändert sich nichts: dieselben Anfragen in derselben Reihenfolge, ein späterer Einzelfehler überspringt
// wie bisher nur sein Register.
func readSmartmeter(sc *smConfig) []smReading {
	var out []smReading
	to := 5 * time.Second
	for _, m := range sc.Meters {
		// Host/Port je Zähler (für Zähler auf eigener IP); leer/0 → Geräte-Host/-Port, sonst 502.
		host := m.Host
		if host == "" {
			host = sc.Host
		}
		port := m.Port
		if port == 0 {
			port = sc.Port
		}
		if port == 0 {
			port = 502
		}
		if host == "" {
			continue // kein Host für diesen Zähler → überspringen
		}
		schluessel := fmt.Sprintf("%s|%s:%d|%d", m.Name, host, port, m.Slave)
		geantwortet := false
		for i, r := range m.Registers {
			regs, err := smLesen(host, port, m.Slave, r.Address, 2, to)
			if i == 0 && smKeineAntwort(err) {
				if !smStumm[schluessel] {
					log.Printf("smartmeter: Zähler %q (%s:%d, Slave %d) antwortet nicht (%v) — in diesem Durchgang übersprungen, wird weiter gefragt", m.Name, host, port, m.Slave, err)
				}
				smStumm[schluessel] = true
				break
			}
			if !smKeineAntwort(err) {
				geantwortet = true
			}
			if err != nil || len(regs) < 2 {
				continue // einzelner Lesefehler: Register überspringen
			}
			out = append(out, smReading{
				EntityID: r.EntityID, Name: r.Name, Unit: r.Unit,
				Value: regsToFloat(regs, "float32"), RecordedAt: smZeit(time.Now()),
			})
		}
		if geantwortet && smStumm[schluessel] {
			log.Printf("smartmeter: Zähler %q (%s:%d, Slave %d) antwortet wieder", m.Name, host, port, m.Slave)
			delete(smStumm, schluessel)
		}
	}
	return out
}
