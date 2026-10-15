package main

// GPIO-Eingang über das character device (/dev/gpiochip*) mit interner Vorspannung (Bias).
// Nötig, weil das alte sysfs keinen Pull-up setzen kann: ein Taster gegen GND bleibt dort in beiden
// Zuständen LOW. Mit internem Pull-up ist die Ruhe HIGH, der Druck zieht auf LOW → erkennbar.
// Reine Linux-UAPI (GPIO v2), kein externes Paket, CGO aus — ioctls über syscall + ein Byte-Puffer
// mit festen Offsets (Struktur-Layout aus linux/gpio.h).

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

const (
	gpioV2FlagInput        = 1 << 2  // GPIO_V2_LINE_FLAG_INPUT
	gpioV2FlagBiasPullUp   = 1 << 8  // GPIO_V2_LINE_FLAG_BIAS_PULL_UP
	gpioV2FlagBiasPullDown = 1 << 9  // GPIO_V2_LINE_FLAG_BIAS_PULL_DOWN
	gpioV2FlagBiasDisabled = 1 << 10 // GPIO_V2_LINE_FLAG_BIAS_DISABLED

	// ioctl-Nummern (_IOWR/_IOR, type 0xB4), Größen aus den Strukturen von linux/gpio.h
	gpioV2GetLineIoctl   = (3 << 30) | (592 << 16) | (0xB4 << 8) | 0x07 // _IOWR gpio_v2_line_request
	gpioV2GetValuesIoctl = (3 << 30) | (16 << 16) | (0xB4 << 8) | 0x0E  // _IOWR gpio_v2_line_values
	gpioGetChipInfoIoctl = (2 << 30) | (68 << 16) | (0xB4 << 8) | 0x01  // _IOR gpiochip_info
)

func biasFlag(bias string) uint64 {
	switch bias {
	case "pullup":
		return gpioV2FlagBiasPullUp
	case "pulldown":
		return gpioV2FlagBiasPullDown
	case "disabled":
		return gpioV2FlagBiasDisabled
	}
	return 0
}

func gpioIoctl(fd int, req uintptr, arg unsafe.Pointer) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, uintptr(arg))
	if errno != 0 {
		return errno
	}
	return nil
}

func gpioChipLabel(path string) (string, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	buf := make([]byte, 68) // gpiochip_info: name[32] label[32] lines(u32)
	if err := gpioIoctl(int(f.Fd()), gpioGetChipInfoIoctl, unsafe.Pointer(&buf[0])); err != nil {
		return "", err
	}
	return string(trimZero(buf[32:64])), nil
}

func trimZero(b []byte) []byte {
	for i, c := range b {
		if c == 0 {
			return b[:i]
		}
	}
	return b
}

var (
	chipMu       sync.Mutex
	chipResolved bool
	chipPath     string
)

// gpiodHeaderChip findet das /dev/gpiochip* des 40-poligen Headers (pinctrl-bcm2711 / pinctrl-rp1).
func gpiodHeaderChip() string {
	chipMu.Lock()
	defer chipMu.Unlock()
	if chipResolved {
		return chipPath
	}
	chipResolved = true
	paths, _ := filepath.Glob("/dev/gpiochip*")
	sort.Strings(paths)
	for _, p := range paths {
		lbl, err := gpioChipLabel(p)
		if err != nil {
			continue
		}
		l := strings.ToLower(lbl)
		if strings.Contains(l, "pinctrl-bcm") || strings.Contains(l, "pinctrl-rp1") {
			chipPath = p
			return p
		}
	}
	return ""
}

// gehaltene Line-fds je BCM-Pin — die Vorspannung gilt nur, solange die Line angefordert ist,
// deshalb halten wir sie offen (einmal anfordern, danach nur noch Werte lesen).
var (
	lineMu    sync.Mutex
	lineFds   = map[int]int{}
	lineBias  = map[int]string{}
)

func gpiodRequestLine(chip string, bcmPin int, bias string) (int, error) {
	cf, err := os.OpenFile(chip, os.O_RDWR, 0)
	if err != nil {
		return -1, err
	}
	defer cf.Close()
	req := make([]byte, 592)
	binary.LittleEndian.PutUint32(req[0:], uint32(bcmPin))                          // offsets[0]
	copy(req[256:288], "bordwaechter")                                              // consumer[32]
	binary.LittleEndian.PutUint64(req[288:], gpioV2FlagInput|biasFlag(bias))        // config.flags
	binary.LittleEndian.PutUint32(req[560:], 1)                                     // num_lines
	if err := gpioIoctl(int(cf.Fd()), gpioV2GetLineIoctl, unsafe.Pointer(&req[0])); err != nil {
		return -1, err
	}
	return int(int32(binary.LittleEndian.Uint32(req[588:]))), nil // request.fd (Line-fd)
}

// gpiodReadLine liest den Pegel eines BCM-Pins mit der gewünschten Vorspannung (true = HIGH).
func gpiodReadLine(bcmPin int, bias string) (bool, error) {
	chip := gpiodHeaderChip()
	if chip == "" {
		return false, fmt.Errorf("kein Header-gpiochip gefunden")
	}
	lineMu.Lock()
	fd, ok := lineFds[bcmPin]
	if ok && lineBias[bcmPin] != bias { // Bias geändert → alte Line schließen
		syscall.Close(fd)
		delete(lineFds, bcmPin)
		ok = false
	}
	lineMu.Unlock()

	if !ok {
		nfd, err := gpiodRequestLine(chip, bcmPin, bias)
		if err == syscall.EBUSY {
			// evtl. noch per sysfs exportiert → freigeben und erneut anfordern
			_ = os.WriteFile("/sys/class/gpio/unexport", []byte(strconv.Itoa(gpioHeaderBase()+bcmPin)), 0o644)
			nfd, err = gpiodRequestLine(chip, bcmPin, bias)
		}
		if err != nil {
			return false, err
		}
		lineMu.Lock()
		lineFds[bcmPin] = nfd
		lineBias[bcmPin] = bias
		lineMu.Unlock()
		fd = nfd
	}

	vals := make([]byte, 16)                        // gpio_v2_line_values: bits, mask
	binary.LittleEndian.PutUint64(vals[8:], 1)      // mask bit0 = diese Line
	if err := gpioIoctl(fd, gpioV2GetValuesIoctl, unsafe.Pointer(&vals[0])); err != nil {
		lineMu.Lock() // fd ungültig geworden → verwerfen, nächster Lauf fordert neu an
		syscall.Close(fd)
		delete(lineFds, bcmPin)
		lineMu.Unlock()
		return false, err
	}
	return binary.LittleEndian.Uint64(vals[0:])&1 == 1, nil
}
