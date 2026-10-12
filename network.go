package main

import (
	"fmt"
	"net"
	"os"
	"strings"
)

// Feste IP ohne Shell: wir schreiben ein NetworkManager-Keyfile. NetworkManager liest Keyfiles
// beim Start ein — die Änderung wird also nach einem Neustart des Geräts wirksam (kein nmcli nötig).
// Bei "dhcp" entfernen wir unser Keyfile wieder, dann greift die Standard-DHCP-Verbindung.
const (
	nmDir     = "/etc/NetworkManager/system-connections"
	nmFile    = nmDir + "/bordwaechter.nmconnection"
	nmUUID    = "b07dwa3c-0000-4000-8000-626f726477ff" // fest: es gibt nur dieses eine Keyfile
	nmPrioStr = "100"                                   // höhere Priorität als die Standardverbindung
)

// validateNetwork prüft die Eingaben für den statischen Modus (CIDR, Gateway, DNS).
func validateNetwork(n *struct {
	Mode, Interface, Address, Gateway, DNS string
}) error {
	if n.Mode != "static" {
		return nil
	}
	if strings.TrimSpace(n.Interface) == "" {
		return fmt.Errorf("Schnittstelle fehlt (z. B. eth0)")
	}
	if _, _, err := net.ParseCIDR(strings.TrimSpace(n.Address)); err != nil {
		return fmt.Errorf("Adresse muss CIDR sein, z. B. 192.168.1.50/24")
	}
	if net.ParseIP(strings.TrimSpace(n.Gateway)) == nil {
		return fmt.Errorf("Gateway ist keine gültige IP")
	}
	for _, d := range splitDNS(n.DNS) {
		if net.ParseIP(d) == nil {
			return fmt.Errorf("DNS-Eintrag %q ist keine gültige IP", d)
		}
	}
	return nil
}

func splitDNS(s string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' || r == ' ' }) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// applyNetwork schreibt (static) bzw. entfernt (dhcp) das Keyfile. Wirkt nach Geräte-Neustart.
func applyNetwork(cfg *Config) error {
	n := &struct{ Mode, Interface, Address, Gateway, DNS string }{
		cfg.Network.Mode, cfg.Network.Interface, cfg.Network.Address, cfg.Network.Gateway, cfg.Network.DNS,
	}
	if err := validateNetwork(n); err != nil {
		return err
	}
	if cfg.Network.Mode != "static" {
		// DHCP: eigenes Keyfile entfernen → Standardverbindung übernimmt wieder.
		if err := os.Remove(nmFile); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	iface := strings.TrimSpace(cfg.Network.Interface)
	dns := strings.Join(splitDNS(cfg.Network.DNS), ";")
	if dns != "" {
		dns += ";"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[connection]\n")
	fmt.Fprintf(&b, "id=bordwaechter\n")
	fmt.Fprintf(&b, "uuid=%s\n", nmUUID)
	fmt.Fprintf(&b, "type=ethernet\n")
	fmt.Fprintf(&b, "interface-name=%s\n", iface)
	fmt.Fprintf(&b, "autoconnect=true\n")
	fmt.Fprintf(&b, "autoconnect-priority=%s\n\n", nmPrioStr)
	fmt.Fprintf(&b, "[ipv4]\n")
	fmt.Fprintf(&b, "method=manual\n")
	fmt.Fprintf(&b, "address1=%s,%s\n", strings.TrimSpace(cfg.Network.Address), strings.TrimSpace(cfg.Network.Gateway))
	if dns != "" {
		fmt.Fprintf(&b, "dns=%s\n", dns)
	}
	fmt.Fprintf(&b, "\n[ipv6]\nmethod=auto\n")

	if err := os.MkdirAll(nmDir, 0o700); err != nil {
		return err
	}
	// NetworkManager verlangt 0600 und root-Eigentum, sonst wird das Keyfile ignoriert.
	tmp := nmFile + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, nmFile)
}
