package main

// Netz-Lebenszeichen: der Bordwächter sitzt im Schiffsnetz und prüft, welche Geräte leben — statt der
// Home-Assistant-Netzwerk-Integration. Er holt die Scan-Ziele aus KAI (GET /api/asset-manager/
// scan-targets), pingt sie (+ ARP-Tabelle für DHCP-Geräte) und meldet online/offline zurück
// (POST /api/asset-manager/ha-connectivity). Beide Aufrufe über den Edge-Token (KaiClient.do; die
// Asset-Manager-Endpunkte akzeptieren ihn). AN/AUS steuert der zentrale Schalter in KAI
// (hello-Antwort report_scan, s. main.go) — ohne lokalen Schlüssel. Nur lesend/ausgehend, best effort.

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

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

// runNetScan meldet den Konnektivitäts-Stand des Bordnetzes an KAI (POST ha-connectivity) — als Ersatz
// für die frühere Home-Assistant-Netzwerk-Integration. OHNE gepflegte Soll-/Zielliste (Nutzervorgabe):
// der Bordwächter entdeckt einfach SEIN eigenes Subnetz (Ping-Sweep + ARP) und meldet die gefundenen
// Online-Hosts. „Offline" entsteht in KAI aus dem Scan-Gedächtnis (zuvor gesehen, jetzt weg), nicht hier.
// Die Zuordnung zum Schiff macht KAI über den Edge-Token des meldenden Geräts.
func runNetScan(k *KaiClient) error {
	entries := discoverLocalHosts()
	if len(entries) == 0 {
		return nil
	}
	_, code, err := k.do("POST", "/api/asset-manager/ha-connectivity", map[string]any{"readings": entries})
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("ha-connectivity http %d", code)
	}
	return nil
}

// discoverLocalHosts durchsucht das lokale Subnetz des Bordwächters (Ping-Sweep, der zugleich die
// ARP-Tabelle füllt) und liefert die gefundenen Hosts als online — mit MAC aus ARP und Namen aus
// Reverse-DNS, soweit vorhanden. Best effort, auf ein /24 begrenzt.
func discoverLocalHosts() []connEntry {
	hosts, self := localSubnetHosts()
	if len(hosts) == 0 {
		return nil
	}
	up := map[string]bool{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 48)
	for _, ip := range hosts {
		wg.Add(1)
		sem <- struct{}{}
		go func(ip string) {
			defer wg.Done()
			defer func() { <-sem }()
			if pingHost(ip) {
				mu.Lock()
				up[ip] = true
				mu.Unlock()
			}
		}(ip)
	}
	wg.Wait()

	arp := readArpTable()
	live := map[string]bool{}
	for ip := range up {
		live[ip] = true
	}
	for ip := range arp {
		live[ip] = true
	}
	if self != "" {
		live[self] = true
	}
	out := make([]connEntry, 0, len(live))
	for ip := range live {
		name := reverseName(ip)
		if name == "" {
			name = ip
		}
		out = append(out, connEntry{Name: name, EntityID: "ping", Status: "on", IP: ip, Mac: arp[ip]})
	}
	return out
}

// localSubnetHosts ermittelt aus der ersten passenden Schnittstelle (up, nicht Loopback, IPv4) alle
// Host-Adressen des Subnetzes (ohne Netz-/Broadcast-/eigene Adresse) sowie die eigene IP. Große Netze
// werden auf ein /24 begrenzt, damit der Sweep überschaubar bleibt.
func localSubnetHosts() ([]string, string) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, ""
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipnet.IP.To4()
			if ip4 == nil {
				continue
			}
			ones, bits := ipnet.Mask.Size()
			if bits != 32 {
				continue
			}
			if ones < 24 {
				ones = 24 // große Netze auf /24 begrenzen
			}
			mask := net.CIDRMask(ones, 32)
			network := ip4.Mask(mask)
			self := ip4.String()
			base := ipToUint(network)
			n := 1 << uint(32-ones)
			hosts := make([]string, 0, n)
			for i := 1; i < n-1; i++ { // Netz- und Broadcast-Adresse auslassen
				hip := uintToIP(base + uint32(i))
				if hip == self {
					continue
				}
				hosts = append(hosts, hip)
			}
			return hosts, self
		}
	}
	return nil, ""
}

// localIPv4 liefert die primäre LAN-IPv4 des Geräts (erste nicht-Loopback-IPv4) — für die Anzeige in
// KAI (Geräte-IP), damit dort nicht die Verbindungs-Quelle 127.0.0.1 steht.
func localIPv4() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if ipnet, ok := a.(*net.IPNet); ok {
				if ip4 := ipnet.IP.To4(); ip4 != nil && !ip4.IsLoopback() {
					return ip4.String()
				}
			}
		}
	}
	return ""
}

func ipToUint(ip net.IP) uint32 {
	ip = ip.To4()
	if ip == nil {
		return 0
	}
	return uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
}
func uintToIP(n uint32) string {
	return fmt.Sprintf("%d.%d.%d.%d", byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
}

// readArpTable liefert IP→MAC aus der Kernel-ARP-Tabelle.
func readArpTable() map[string]string {
	out := map[string]string{}
	b, err := os.ReadFile("/proc/net/arp")
	if err != nil {
		return out
	}
	for i, ln := range strings.Split(string(b), "\n") {
		if i == 0 {
			continue
		}
		f := strings.Fields(ln)
		if len(f) >= 4 {
			mac := normMacStr(f[3])
			if mac != "" && mac != "00:00:00:00:00:00" {
				out[f[0]] = mac
			}
		}
	}
	return out
}

// reverseName löst eine IP best effort per Reverse-DNS zu einem Namen auf (kurzer Timeout).
func reverseName(ip string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()
	names, err := net.DefaultResolver.LookupAddr(ctx, ip)
	if err != nil || len(names) == 0 {
		return ""
	}
	return strings.TrimSuffix(names[0], ".")
}

// pingHost: ein ICMP-Paket, 1 s Timeout (iputils-ping, auf dem Pi/NUC vorhanden).
func pingHost(ip string) bool {
	return exec.Command("ping", "-c", "1", "-W", "1", ip).Run() == nil
}

func normMacStr(s string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s), "-", ":"))
}

