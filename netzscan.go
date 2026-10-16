package main

// Netz-Lebenszeichen: der Bordwächter sitzt im Schiffsnetz und prüft, welche Geräte leben — statt der
// Home-Assistant-Netzwerk-Integration. Er holt die Scan-Ziele aus KAI (GET /api/asset-manager/
// scan-targets), pingt sie (+ ARP-Tabelle für DHCP-Geräte) und meldet online/offline zurück
// (POST /api/asset-manager/ha-connectivity). Beide Endpunkte mit dem Asset-Manager-/HA-Schlüssel
// (X-API-Key, cfg.AMKey). Nur lesend/ausgehend, best effort; leerer Schlüssel = Scan aus.

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

type scanTarget struct {
	Name string   `json:"name"`
	IP   string   `json:"ip"`
	Macs []string `json:"macs"`
}
type connEntry struct {
	Name     string `json:"name"`
	EntityID string `json:"entity_id"`
	Status   string `json:"status"`
	IP       string `json:"ip,omitempty"`
	Mac      string `json:"mac,omitempty"`
}

// amBase schneidet ein evtl. mitkonfiguriertes /api(/edge)-Suffix ab (wie kai.go), damit die
// Asset-Manager-Pfade korrekt an die KAI-Wurzel gehängt werden.
func amBase(cfg *Config) string {
	b := strings.TrimRight(strings.TrimSpace(cfg.KaiBaseURL), "/")
	for _, suf := range []string{"/api/edge", "/api"} {
		b = strings.TrimSuffix(b, suf)
	}
	return b
}

func (a *App) netScanLoop() {
	time.Sleep(40 * time.Second) // Netz erst hochkommen lassen
	for {
		cfg := a.getCfg()
		if strings.TrimSpace(cfg.AMKey) != "" && strings.TrimSpace(cfg.KaiBaseURL) != "" {
			if err := runNetScan(cfg); err != nil {
				log.Printf("netzscan: %v", err)
			}
		}
		time.Sleep(5 * time.Minute)
	}
}

func runNetScan(cfg *Config) error {
	base := amBase(cfg)
	key := strings.TrimSpace(cfg.AMKey)
	cl := &http.Client{Timeout: 25 * time.Second}

	req, _ := http.NewRequest("GET", base+"/api/asset-manager/scan-targets", nil)
	req.Header.Set("X-API-Key", key)
	resp, err := cl.Do(req)
	if err != nil {
		return err
	}
	var tr struct {
		Items []scanTarget `json:"items"`
	}
	derr := json.NewDecoder(resp.Body).Decode(&tr)
	resp.Body.Close()
	if derr != nil {
		return derr
	}
	if len(tr.Items) == 0 {
		return nil
	}

	arp := readArpMacs()
	entries := make([]connEntry, 0, len(tr.Items))
	for _, tgt := range tr.Items {
		online := false
		if tgt.IP != "" {
			online = pingHost(tgt.IP)
		}
		if !online {
			for _, m := range tgt.Macs {
				if arp[normMacStr(m)] {
					online = true
					break
				}
			}
		}
		mac := ""
		if len(tgt.Macs) > 0 {
			mac = tgt.Macs[0]
		}
		st := "off"
		if online {
			st = "on"
		}
		entries = append(entries, connEntry{Name: tgt.Name, EntityID: "ping", Status: st, IP: tgt.IP, Mac: mac})
	}

	body, _ := json.Marshal(map[string]any{"readings": entries})
	req2, _ := http.NewRequest("POST", base+"/api/asset-manager/ha-connectivity", bytes.NewReader(body))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-API-Key", key)
	resp2, err := cl.Do(req2)
	if err != nil {
		return err
	}
	resp2.Body.Close()
	return nil
}

// pingHost: ein ICMP-Paket, 1 s Timeout (iputils-ping, auf dem Pi/NUC vorhanden).
func pingHost(ip string) bool {
	return exec.Command("ping", "-c", "1", "-W", "1", ip).Run() == nil
}

func normMacStr(s string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s), "-", ":"))
}

// readArpMacs liest die MAC-Adressen aus der Kernel-ARP-Tabelle (wer wurde zuletzt im LAN gesehen).
func readArpMacs() map[string]bool {
	out := map[string]bool{}
	b, err := os.ReadFile("/proc/net/arp")
	if err != nil {
		return out
	}
	for i, ln := range strings.Split(string(b), "\n") {
		if i == 0 {
			continue // Kopfzeile
		}
		f := strings.Fields(ln)
		if len(f) >= 4 {
			mac := normMacStr(f[3])
			if mac != "" && mac != "00:00:00:00:00:00" {
				out[mac] = true
			}
		}
	}
	return out
}
