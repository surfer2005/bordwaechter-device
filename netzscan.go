package main

// Netz-Lebenszeichen: der Bordwächter sitzt im Schiffsnetz und prüft, welche Geräte leben — statt der
// Home-Assistant-Netzwerk-Integration. Er holt die Scan-Ziele aus KAI (GET /api/asset-manager/
// scan-targets), pingt sie (+ ARP-Tabelle für DHCP-Geräte) und meldet online/offline zurück
// (POST /api/asset-manager/ha-connectivity). Beide Aufrufe über den Edge-Token (KaiClient.do; die
// Asset-Manager-Endpunkte akzeptieren ihn). AN/AUS steuert der zentrale Schalter in KAI
// (hello-Antwort report_scan, s. main.go) — ohne lokalen Schlüssel. Nur lesend/ausgehend, best effort.

import (
	"encoding/json"
	"fmt"
	"log"
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

func (a *App) netScanLoop() {
	time.Sleep(40 * time.Second) // Netz erst hochkommen lassen
	for {
		cfg := a.getCfg()
		k := a.getKai()
		// AN/AUS kommt aus KAI (report_scan via hello); kein lokaler Schlüssel mehr nötig.
		if a.getReportScan() && k != nil && strings.TrimSpace(cfg.EdgeToken) != "" {
			if err := runNetScan(k); err != nil {
				log.Printf("netzscan: %v", err)
			}
		}
		time.Sleep(5 * time.Minute)
	}
}

func runNetScan(k *KaiClient) error {
	data, code, err := k.do("GET", "/api/asset-manager/scan-targets", nil)
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("scan-targets http %d", code)
	}
	var tr struct {
		Items []scanTarget `json:"items"`
	}
	if err := json.Unmarshal(data, &tr); err != nil {
		return err
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

	_, code2, err := k.do("POST", "/api/asset-manager/ha-connectivity", map[string]any{"readings": entries})
	if err != nil {
		return err
	}
	if code2 != 200 {
		return fmt.Errorf("ha-connectivity http %d", code2)
	}
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
