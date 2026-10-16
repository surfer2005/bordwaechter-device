package main

// Geräte-Protokoll: lokale Ereignisse/Fehler + Neustarts werden gepuffert (auch offline, auf Platte)
// und beim Wiederverbinden an KAI gemeldet (POST /api/edge/log). So sieht man zentral im Monitoring,
// was auf dem Gerät passiert — statt per SSH ins systemd-Journal zu müssen. Nur ausgehend, best effort.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type logEntry struct {
	At         string `json:"at"`
	Level      string `json:"level"` // info | warn | error
	Message    string `json:"message"`
	Key        string `json:"key"`
	WasOffline bool   `json:"was_offline"`
}

var (
	logMu  sync.Mutex
	logBuf []logEntry
	logSeq int64
)

// kaiReachable wird vom Herzschlag gepflegt (hello erfolgreich = true), damit ein Ereignis als
// „offline aufgetreten" markiert werden kann.
var kaiReachable = true

func devlogPath() string {
	if p := os.Getenv("BORDWAECHTER_DEVLOG"); p != "" {
		return p
	}
	if _, err := os.Stat("/etc/bordwaechter"); err == nil {
		return "/etc/bordwaechter/devlog.json"
	}
	return "devlog.json"
}

func persistLog() { // Aufrufer hält logMu
	data, err := json.Marshal(logBuf)
	if err != nil {
		return
	}
	p := devlogPath()
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	tmp := p + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		_ = os.Rename(tmp, p)
	}
}

func loadLog() {
	data, err := os.ReadFile(devlogPath())
	if err != nil {
		return
	}
	logMu.Lock()
	defer logMu.Unlock()
	_ = json.Unmarshal(data, &logBuf)
}

// logEvent puffert ein Ereignis (dedup-Schlüssel eindeutig, damit ein erneutes Nachreichen nicht doppelt zählt).
func logEvent(level, msg string) {
	logMu.Lock()
	defer logMu.Unlock()
	logSeq++
	logBuf = append(logBuf, logEntry{
		At:         time.Now().UTC().Format(time.RFC3339),
		Level:      level,
		Message:    msg,
		Key:        fmt.Sprintf("%d-%d", time.Now().UnixNano(), logSeq),
		WasOffline: !kaiReachable,
	})
	if len(logBuf) > 500 {
		logBuf = logBuf[len(logBuf)-500:]
	}
	persistLog()
}

// devlogLoop meldet die gepufferten Einträge an KAI, sobald erreichbar; gesendete werden entfernt.
func (a *App) devlogLoop() {
	loadLog()
	logEvent("info", "Bordwächter gestartet, Version "+version)
	for {
		k := a.getKai()
		if k != nil && a.getCfg().EdgeToken != "" {
			logMu.Lock()
			batch := append([]logEntry(nil), logBuf...)
			logMu.Unlock()
			if len(batch) > 0 {
				if err := k.PostLog(batch); err == nil {
					sent := make(map[string]bool, len(batch))
					for _, e := range batch {
						sent[e.Key] = true
					}
					logMu.Lock()
					rest := logBuf[:0]
					for _, e := range logBuf {
						if !sent[e.Key] {
							rest = append(rest, e)
						}
					}
					logBuf = append([]logEntry(nil), rest...)
					persistLog()
					logMu.Unlock()
				}
			}
		}
		time.Sleep(60 * time.Second)
	}
}
