package main

// Diagnose aus KAI (Befehl "diagnose"): ein FESTER Satz Prüfungen am Gerät — was man sonst per SSH
// nachsehen müsste (Peter 2026-11-02, „dann bräuchte niemand mehr SSH“). Bewusst keine beliebigen
// Befehle aus KAI: wer KAI bedienen kann, soll nicht beliebig auf dem Gerät ausführen können.
// Das Ergebnis geht an KAI (POST /api/edge/diagnose) und steht dort beim Gerät.

import (
	"context"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"
)

type diagPunkt struct {
	Titel   string `json:"titel"`
	Befehl  string `json:"befehl"`
	Ausgabe string `json:"ausgabe"`
	OK      *bool  `json:"ok,omitempty"` // nil = nur Information
}

func diagShell(cmd string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "sh", "-c", cmd).CombinedOutput()
	t := strings.TrimSpace(string(out))
	if err != nil && t == "" {
		t = "(" + err.Error() + ")"
	}
	if len(t) > 6000 {
		t = t[:6000] + "\n…(gekürzt)"
	}
	return t
}

func janein(b bool) *bool { return &b }

var diagApp *App

// kameraHost: Adresse der Kamera aus den Einstellungen (Livestream, sonst Standbild).
func kameraHost() string {
	if diagApp == nil {
		return ""
	}
	cfg := diagApp.getCfg()
	for _, u := range []string{cfg.Camera.StreamURL, cfg.Camera.SnapshotURL} {
		if x, err := url.Parse(strings.TrimSpace(u)); err == nil && x.Hostname() != "" {
			return x.Hostname()
		}
	}
	return ""
}

// diagnoseLaufen sammelt die Prüfungen. Reihenfolge = Anzeige in KAI.
func diagnoseLaufen() []diagPunkt {
	var p []diagPunkt
	add := func(titel, befehl string, ok *bool) {
		p = append(p, diagPunkt{Titel: titel, Befehl: befehl, Ausgabe: diagShell(befehl), OK: ok})
	}
	_, model := istPi5()
	p = append(p, diagPunkt{Titel: "Gerät", Befehl: "Modell / Version", Ausgabe: model + "\nKAI-Bordgerät " + version + "\n" + diagShell("uname -r; uptime")})

	// Internet-Namen (DNS)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, dnsErr := net.DefaultResolver.LookupHost(ctx, "github.com")
	cancel()
	dnsTxt := "github.com wird aufgelöst"
	if dnsErr != nil {
		dnsTxt = "github.com NICHT auflösbar: " + dnsErr.Error()
	}
	rc, _ := os.ReadFile("/etc/resolv.conf")
	p = append(p, diagPunkt{Titel: "Internet-Namen (DNS)", Befehl: "Namensauflösung + /etc/resolv.conf",
		Ausgabe: dnsTxt + "\n\n" + strings.TrimSpace(string(rc)), OK: janein(dnsErr == nil)})
	add("Netzwerk", "ip -br addr; ip route", nil)

	// KI-Beschleuniger / PCIe
	_, hErr := os.Stat("/dev/hailo0")
	add("PCIe-Geräte", "lspci 2>/dev/null || echo 'lspci fehlt (pciutils)'", nil)
	p = append(p, diagPunkt{Titel: "AI HAT+ (Hailo)", Befehl: "ls /dev/hailo*; hailortcli fw-control identify",
		Ausgabe: diagShell("ls -l /dev/hailo* 2>&1; command -v hailortcli >/dev/null && hailortcli fw-control identify 2>&1 | head -20"), OK: janein(hErr == nil)})
	add("Kernel-Meldungen PCIe/Hailo/NVMe", "dmesg | grep -iE 'hailo|pcie|nvme' | tail -40", nil)
	add("Boot-Einstellungen PCIe", "grep -iE 'pcie|pciex1|nvme|hailo|dtoverlay|dtparam' /boot/firmware/config.txt", nil)
	add("HAT-Kennung (EEPROM)", "cat /proc/device-tree/hat/product 2>/dev/null; echo; cat /proc/device-tree/hat/vendor 2>/dev/null", nil)

	// Kamera: Port 554 (RTSP für Frigate) und 80 (Standbild/Livestream für die Anzeige)
	if h := kameraHost(); h != "" {
		add("Kamera "+h+" (RTSP 554 / HTTP 80)", "for p in 554 80; do if timeout 4 bash -c \"</dev/tcp/"+h+"/$p\" 2>/dev/null; then echo \"Port $p offen\"; else echo \"Port $p NICHT erreichbar\"; fi; done; ping -c 2 -W 2 "+h+" | tail -2", nil)
	}

	// Docker / Frigate
	add("Docker / Frigate", "docker ps -a --format '{{.Names}}  {{.Status}}  {{.Image}}' 2>&1", nil)
	if _, err := os.Stat("/opt/frigate"); err == nil {
		add("Frigate-Werte (Kamera, Bewegung, Erkennung)", "curl -s http://127.0.0.1:5000/api/stats | python3 -c 'import json,sys; s=json.load(sys.stdin); print(json.dumps({\"cameras\": s.get(\"cameras\"), \"detectors\": s.get(\"detectors\")}, indent=1))' 2>&1 | head -60", nil)
		add("Frigate-Treffer (letzte 10, auch unter dem Mindest-Score)", "curl -s 'http://127.0.0.1:5000/api/events?limit=10&include_thumbnails=0' | python3 -c 'import json,sys,datetime; [print(datetime.datetime.fromtimestamp(e[\"start_time\"]).strftime(\"%d.%m. %H:%M:%S\"), e[\"label\"], round(e.get(\"top_score\") or e.get(\"score\") or 0,2)) for e in json.load(sys.stdin)]' 2>&1", nil)
		add("Frigate-Fehler (letzte 15 Minuten)", "docker logs --since 15m frigate 2>&1 | grep -v -e go2rtc_stream_proxy -e 'GET /api/' | grep -iE 'error|fatal|exit|crash|hailo|ffmpeg|stopp|restart|starting|killed|watchdog' | grep -v 'ForkAwareLocal' | tail -40", nil)
		add("Frigate-Neustarts", "docker inspect -f 'gestartet {{.State.StartedAt}} · Neustarts {{.RestartCount}}' frigate 2>&1", nil)
		add("Frigate-Protokoll (letzte Zeilen, ohne Webserver-Rauschen)", "docker logs --tail 400 frigate 2>&1 | grep -v -e go2rtc_stream_proxy -e 'GET /api/' | tail -40", nil)
	}

	// SSH
	sshAktiv := diagShell("systemctl is-active ssh 2>&1 || systemctl is-active sshd 2>&1")
	p = append(p, diagPunkt{Titel: "SSH-Dienst", Befehl: "systemctl is-active ssh; sshd -t; Host-Schlüssel",
		Ausgabe: sshAktiv + "\n\nsshd -t:\n" + diagShell("sshd -t 2>&1 || /usr/sbin/sshd -t 2>&1; echo '(Rückgabe '$?')'") +
			"\n\nHost-Schlüssel:\n" + diagShell("ls -l /etc/ssh/ssh_host_*_key 2>&1") +
			"\n\nLetzte Meldungen:\n" + diagShell("journalctl -u ssh -u sshd -n 15 --no-pager 2>&1"),
		OK: janein(strings.HasPrefix(sshAktiv, "active"))})

	add("Speicher", "df -h / /boot/firmware 2>/dev/null; free -h", nil)
	return p
}

func (a *App) diagnoseSenden(n int) {
	diagApp = a
	k := a.getKai()
	erg := diagnoseLaufen()
	if k == nil {
		return
	}
	if _, code, err := k.do("POST", "/api/edge/diagnose", map[string]any{"n": n, "punkte": erg}); err != nil || code != 200 {
		logEvent("warn", "Diagnose konnte nicht an KAI gesendet werden")
		return
	}
	logEvent("info", "Diagnose an KAI gesendet")
}
