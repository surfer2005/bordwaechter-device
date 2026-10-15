package main

// GPIO-Selbsttest: zeigt im Browser (statt im verschluckten Fehler), warum ein Ausgang nicht schaltet.
// Liest die Umgebung (Pi-Modell, Kernel, ob das alte /sys/class/gpio existiert, welche gpiochips es
// gibt — damit sieht man die Pi-5-/RP1-Verschiebung sofort) und fährt für jeden Ausgangs-Pin die
// echte sysfs-Sequenz (export → direction=out → value), Schritt für Schritt mit Fehlertext, und pulst
// den Pin kurz an (active_low beachtet), damit man das Relais hören/sehen kann.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type gpioStep struct {
	Step   string `json:"step"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

type gpioPinResult struct {
	Pin   int        `json:"pin"`
	Name  string     `json:"name"`
	Steps []gpioStep `json:"steps"`
	OK    bool       `json:"ok"`
}

type gpioChipInfo struct {
	Dir   string `json:"dir"`
	Label string `json:"label,omitempty"`
	Base  string `json:"base,omitempty"`
	Ngpio string `json:"ngpio,omitempty"`
}

type gpioSelftestResult struct {
	Backend      string         `json:"backend"`
	ActiveLow    bool           `json:"active_low"`
	Model        string         `json:"model"`
	Kernel       string         `json:"kernel"`
	SysfsPresent bool           `json:"sysfs_present"`
	Chardevs     []string       `json:"chardevs"`
	Chips        []gpioChipInfo `json:"chips"`
	Pins         []gpioPinResult `json:"pins"`
	Hinweis      string         `json:"hinweis,omitempty"`
}

func readTrim(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimRight(strings.TrimRight(string(b), "\x00"), "\n")
}

// gpioEnv sammelt Modell/Kernel/sysfs/gpiochips.
func gpioEnv() (model, kernel string, sysfs bool, chardevs []string, chips []gpioChipInfo) {
	model = readTrim("/proc/device-tree/model")
	kernel = readTrim("/proc/sys/kernel/osrelease")
	if _, err := os.Stat("/sys/class/gpio/export"); err == nil {
		sysfs = true
	}
	chardevs, _ = filepath.Glob("/dev/gpiochip*")
	sort.Strings(chardevs)
	dirs, _ := filepath.Glob("/sys/class/gpio/gpiochip*")
	sort.Strings(dirs)
	for _, d := range dirs {
		chips = append(chips, gpioChipInfo{
			Dir:   d,
			Label: readTrim(d + "/label"),
			Base:  readTrim(d + "/base"),
			Ngpio: readTrim(d + "/ngpio"),
		})
	}
	return
}

// selftestPin fährt die sysfs-Sequenz für einen Ausgangs-Pin und pulst ihn kurz.
func selftestPin(pin int, name string, activeLow bool) gpioPinResult {
	res := gpioPinResult{Pin: pin, Name: name}
	add := func(step string, err error) bool {
		s := gpioStep{Step: step, OK: err == nil}
		if err != nil {
			s.Detail = err.Error()
		}
		res.Steps = append(res.Steps, s)
		return err == nil
	}
	base := fmt.Sprintf("/sys/class/gpio/gpio%d", pin)
	if _, err := os.Stat(base); err != nil {
		if !add("export", os.WriteFile("/sys/class/gpio/export", []byte(strconv.Itoa(pin)), 0o644)) {
			return res
		}
		time.Sleep(150 * time.Millisecond) // udev braucht einen Moment, bis die value/direction-Datei da ist
	} else {
		res.Steps = append(res.Steps, gpioStep{Step: "export", OK: true, Detail: "schon exportiert"})
	}
	if !add("direction=out", os.WriteFile(base+"/direction", []byte("out"), 0o644)) {
		return res
	}
	// „an" = logisch ein; bei active_low ist das physisch 0.
	onVal, offVal := "1", "0"
	if activeLow {
		onVal, offVal = "0", "1"
	}
	if !add("value=an (Puls)", os.WriteFile(base+"/value", []byte(onVal), 0o644)) {
		return res
	}
	time.Sleep(900 * time.Millisecond) // kurz halten → Relais hörbar
	add("value=aus", os.WriteFile(base+"/value", []byte(offVal), 0o644))
	res.OK = true
	for _, s := range res.Steps {
		if !s.OK {
			res.OK = false
		}
	}
	return res
}

func runGpioSelftest(cfg *Config) gpioSelftestResult {
	model, kernel, sysfs, chardevs, chips := gpioEnv()
	r := gpioSelftestResult{
		Backend: cfg.GPIO.Backend, ActiveLow: cfg.GPIO.ActiveLow,
		Model: model, Kernel: kernel, SysfsPresent: sysfs, Chardevs: chardevs, Chips: chips,
	}
	pins := []struct {
		pin  int
		name string
	}{{cfg.GPIO.HornPin, "Hupe"}, {cfg.GPIO.LampPin, "Lampe"}}
	for _, p := range pins {
		if p.pin < 0 {
			continue
		}
		r.Pins = append(r.Pins, selftestPin(p.pin, p.name, cfg.GPIO.ActiveLow))
	}
	// Hinweis auf die typische Pi-5-Falle, wenn sysfs fehlt oder der RP1-Chip nicht bei Basis 0 sitzt.
	if !sysfs {
		r.Hinweis = "Das alte /sys/class/gpio fehlt (neuer Kernel/Pi 5). GPIO läuft hier nur über das character device (/dev/gpiochip*). Ein gpiod-Backend ist nötig."
	} else {
		for _, c := range chips {
			if strings.Contains(strings.ToLower(c.Label), "rp1") && c.Base != "0" {
				r.Hinweis = "Pi 5 (RP1): die sysfs-Pin-Nummern sind um die Chip-Basis " + c.Base + " verschoben — export " + strconv.Itoa(cfg.GPIO.HornPin) + " trifft NICHT GPIO" + strconv.Itoa(cfg.GPIO.HornPin) + ". Entweder Basis aufaddieren oder ein gpiod-Backend nutzen."
			}
		}
	}
	return r
}
