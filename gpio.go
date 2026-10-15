package main

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
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
	inputBias string // "" = sysfs; "pullup"/"pulldown"/"disabled" = Eingang übers character device
	base      int    // Basis des Header-Chips; sysfs-Leitung = base + BCM-Pin (0 auf alten Kerneln)
	mu        sync.Mutex
	exported  map[int]string    // pin -> direction ("out"/"in")
	lastErr   map[string]string // "pin:stage" -> zuletzt geloggter Fehlertext (gegen Journal-Spam)
}

func newSysfsGPIO(activeLow bool, inputBias string) *sysfsGPIO {
	b := gpioHeaderBase()
	if b != 0 {
		log.Printf("gpio: sysfs-Chip-Basis %d erkannt — BCM-Pins werden um %d verschoben", b, b)
	}
	if inputBias != "" {
		log.Printf("gpio: Eingänge über character device mit Bias %q", inputBias)
	}
	return &sysfsGPIO{activeLow: activeLow, inputBias: inputBias, base: b, exported: map[int]string{}, lastErr: map[string]string{}}
}

// update: Polung/Eingangs-Bias zur Laufzeit übernehmen (bei Config-Änderung, ohne Neustart).
func (g *sysfsGPIO) update(activeLow bool, inputBias string) {
	g.mu.Lock()
	g.activeLow = activeLow
	g.inputBias = inputBias
	g.mu.Unlock()
}

// line übersetzt einen BCM-Pin in die globale sysfs-Leitungsnummer (Basis + Pin).
func (g *sysfsGPIO) line(pin int) int { return pin + g.base }

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
	ln := g.line(pin)
	dirPath := fmt.Sprintf("/sys/class/gpio/gpio%d", ln)
	if _, err := os.Stat(dirPath); err != nil {
		if err := os.WriteFile("/sys/class/gpio/export", []byte(strconv.Itoa(ln)), 0o644); err != nil {
			return err
		}
		time.Sleep(120 * time.Millisecond) // udev braucht einen Moment, bis direction/value da sind
	}
	if err := os.WriteFile(dirPath+"/direction", []byte(dir), 0o644); err != nil {
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
	g.mu.Lock()
	activeLow := g.activeLow
	g.mu.Unlock()
	v := on
	if activeLow {
		v = !on
	}
	val := "0"
	if v {
		val = "1"
	}
	err := os.WriteFile(fmt.Sprintf("/sys/class/gpio/gpio%d/value", g.line(pin)), []byte(val), 0o644)
	g.logErr(fmt.Sprintf("%d:value", pin), err)
}

func (g *sysfsGPIO) ReadInput(pin int) (bool, error) {
	if pin < 0 {
		return false, nil
	}
	g.mu.Lock()
	bias := g.inputBias
	g.mu.Unlock()
	if bias != "" {
		// character device mit interner Vorspannung (Pull-up für Taster gegen GND)
		v, err := gpiodReadLine(pin, bias)
		g.logErr(fmt.Sprintf("%d:in-gpiod", pin), err)
		return v, err
	}
	if err := g.ensure(pin, "in"); err != nil {
		return false, err
	}
	b, err := os.ReadFile(fmt.Sprintf("/sys/class/gpio/gpio%d/value", g.line(pin)))
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
		return newSysfsGPIO(cfg.GPIO.ActiveLow, cfg.GPIO.InputBias)
	}
	return noopGPIO{}
}
