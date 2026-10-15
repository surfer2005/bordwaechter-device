package main

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
)

// GPIO: Ausgänge (Hupe/Lampe/Relais) + Eingänge (Quittier-Taster/Schlüsselschalter).
// Zwei Backends: "sysfs" (Raspberry, dependency-frei) und "none" (Entwicklung/ohne Hardware → Log).
type GPIO interface {
	SetOutput(pin int, on bool)
	ReadInput(pin int) (bool, error)
}

// ---- sysfs ----
type sysfsGPIO struct {
	activeLow bool
	mu        sync.Mutex
	exported  map[int]string // pin -> direction ("out"/"in")
	lastErr   map[string]string // "pin:stage" -> zuletzt geloggter Fehlertext (gegen Journal-Spam)
}

func newSysfsGPIO(activeLow bool) *sysfsGPIO {
	return &sysfsGPIO{activeLow: activeLow, exported: map[int]string{}, lastErr: map[string]string{}}
}

// logErr: einen GPIO-Fehler nur beim ERSTEN Auftreten (bzw. bei geändertem Text) loggen — Tick ruft
// SetOutput jede Sekunde, sonst liefe das Journal über. Erfolg löscht die Merkung wieder.
func (g *sysfsGPIO) logErr(key string, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err == nil {
		delete(g.lastErr, key)
		return
	}
	if g.lastErr[key] != err.Error() {
		g.lastErr[key] = err.Error()
		log.Printf("gpio: %s: %v", key, err)
	}
}

func (g *sysfsGPIO) ensure(pin int, dir string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.exported[pin] == dir {
		return nil
	}
	base := fmt.Sprintf("/sys/class/gpio/gpio%d", pin)
	if _, err := os.Stat(base); err != nil {
		if err := os.WriteFile("/sys/class/gpio/export", []byte(strconv.Itoa(pin)), 0o644); err != nil {
			return err
		}
	}
	if err := os.WriteFile(base+"/direction", []byte(dir), 0o644); err != nil {
		return err
	}
	g.exported[pin] = dir
	return nil
}

func (g *sysfsGPIO) SetOutput(pin int, on bool) {
	if pin < 0 {
		return
	}
	if err := g.ensure(pin, "out"); err != nil {
		g.logErr(fmt.Sprintf("%d:ensure", pin), err)
		return
	}
	v := on
	if g.activeLow {
		v = !on
	}
	val := "0"
	if v {
		val = "1"
	}
	err := os.WriteFile(fmt.Sprintf("/sys/class/gpio/gpio%d/value", pin), []byte(val), 0o644)
	g.logErr(fmt.Sprintf("%d:value", pin), err)
}

func (g *sysfsGPIO) ReadInput(pin int) (bool, error) {
	if pin < 0 {
		return false, nil
	}
	if err := g.ensure(pin, "in"); err != nil {
		return false, err
	}
	b, err := os.ReadFile(fmt.Sprintf("/sys/class/gpio/gpio%d/value", pin))
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(b)) == "1", nil
}

// ---- none (ohne Hardware) ----
type noopGPIO struct{}

func (noopGPIO) SetOutput(pin int, on bool)        {}
func (noopGPIO) ReadInput(pin int) (bool, error)   { return false, nil }

func buildGPIO(cfg *Config) GPIO {
	if cfg.GPIO.Backend == "sysfs" {
		return newSysfsGPIO(cfg.GPIO.ActiveLow)
	}
	return noopGPIO{}
}
