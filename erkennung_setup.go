package main

// Objekterkennung aus KAI einrichten (Befehl "detect_setup"): Treiber des AI HAT+ (Hailo-8L), Docker
// und Frigate installiert das Gerät selbst — niemand muss an Bord etwas eintippen (Peter 2026-11-02,
// „20 Geräte, einfache Installation von KAI aus“). Die Kamera-Adresse samt Zugang kommt aus den
// Kamera-Einstellungen in KAI (camera.stream_url/snapshot_url), daraus wird der RTSP-Stream gebildet.
//
// Ablauf: Pi-5-Prüfung → Treiber (danach EIN Neustart, der Lauf setzt nach dem Hochfahren selbst fort)
// → Docker → Frigate-Konfiguration → Container starten → warten, bis die API antwortet. Der Stand liegt
// in /etc/bordwaechter/erkennung.json und reist mit jedem Herzschlag nach KAI.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const frigateDir = "/opt/frigate"

type erkennungStand struct {
	Phase    string `json:"phase"`    // "" | laeuft | neustart | fertig | fehler
	Schritt  string `json:"schritt"`  // lesbarer Schritt
	Meldung  string `json:"meldung"`  // Fehlertext (bei fehler)
	Neustart int    `json:"neustart"` // Treiber-Neustarts in diesem Lauf (gegen Neustart-Schleifen)
	Treiber  string `json:"treiber"`  // Hailo-Treiberversion, für die Frigate eingerichtet wurde
	Seit     string `json:"seit"`
	Am       string `json:"am"`
}

// erkennungMelden: vom Herzschlag aufgerufen (nur in der Rolle Bordwächter gesetzt).
var erkennungMelden func() map[string]any

var (
	erkMu   sync.Mutex
	erkLauf bool
)

func erkennungPfad() string {
	if _, err := os.Stat("/etc/bordwaechter"); err == nil {
		return "/etc/bordwaechter/erkennung.json"
	}
	return "erkennung.json"
}

func ladeErkennung() erkennungStand {
	var s erkennungStand
	if b, err := os.ReadFile(erkennungPfad()); err == nil {
		_ = json.Unmarshal(b, &s)
	}
	return s
}

func speichereErkennung(s erkennungStand) {
	s.Am = time.Now().UTC().Format(time.RFC3339)
	b, _ := json.Marshal(s)
	_ = os.WriteFile(erkennungPfad(), b, 0o600)
}

func istPi5() (bool, string) {
	b, _ := os.ReadFile("/proc/device-tree/model")
	m := strings.TrimSpace(strings.TrimRight(string(b), "\x00"))
	return strings.Contains(m, "Raspberry Pi 5"), m
}

func hailoDa() bool { _, err := os.Stat("/dev/hailo0"); return err == nil }

// rtspAusKamera bildet aus der Kamera-Adresse in KAI (http://user:pass@host/…) den RTSP-Stream der AXIS.
// Rein, testbar. Leer, wenn keine Adresse gepflegt ist.
func rtspAusKamera(stream, snapshot string) string {
	for _, roh := range []string{stream, snapshot} {
		u, err := url.Parse(strings.TrimSpace(roh))
		if err != nil || u.Host == "" {
			continue
		}
		r := url.URL{Scheme: "rtsp", Host: u.Hostname(), Path: "/axis-media/media.amp", RawQuery: "resolution=1280x720&fps=5"}
		if u.User != nil {
			r.User = u.User
		}
		return r.String()
	}
	return ""
}

// frigateConfig: die Frigate-Konfiguration (YAML) für den Hailo und die Klassen aus KAI. Rein, testbar.
func frigateConfig(rtsp, labels string) string {
	var track []string
	for _, l := range strings.Split(labels, ",") {
		if l = strings.TrimSpace(l); l != "" {
			track = append(track, l)
		}
	}
	if len(track) == 0 {
		track = []string{"person", "boat"}
	}
	var f strings.Builder
	for _, l := range track {
		fmt.Fprintf(&f, "        %s: { min_score: 0.5, threshold: 0.7 }\n", l)
	}
	q, _ := json.Marshal(rtsp) // JSON-String ist gültiges YAML und schützt Sonderzeichen im Passwort
	return `# Vom KAI-Bordgerät erzeugt – Änderungen hier werden überschrieben (Pflege in KAI).
mqtt:
  enabled: false
detectors:
  hailo:
    type: hailo8l
    device: PCIe
cameras:
  bordwaechter:
    enabled: true
    ffmpeg:
      inputs:
        - path: ` + string(q) + `
          roles: [detect]
    detect:
      width: 1280
      height: 720
      fps: 5
    objects:
      track: [` + strings.Join(track, ", ") + `]
      filters:
` + f.String() + `    snapshots:
      enabled: true
      bounding_box: true
    record:
      enabled: false
`
}

// Frigate-Version fest (nicht „stable“): die Hailo-Bibliothek im Image muss zum Treiber am Gerät passen.
// Frigate 0.17/0.18 bringen HailoRT 4.21.0 mit; Raspberry Pi OS liefert mit hailo-all einen neueren Treiber
// (MS-Lindau 2026-11-02: 4.23.0 → Frigate stürzte mit HAILO_INVALID_DRIVER_VERSION ab). Weicht der Treiber ab,
// baut das Gerät ein eigenes Image mit der passenden HailoRT-Version (Builds von frigate-nvr/hailort).
const frigateBasis = "ghcr.io/blakeblackshear/frigate:0.18.2"
const frigateHailoRT = "4.21.0"

const frigateCompose = `services:
  frigate:
    image: __IMAGE__
    container_name: frigate
    restart: unless-stopped
    privileged: true
    shm_size: "128mb"
    devices:
      - /dev/hailo0
    volumes:
      - ./config:/config
      - ./storage:/media/frigate
      - /etc/localtime:/etc/localtime:ro
    ports:
      - "127.0.0.1:5000:5000"
`

func lauf(timeout time.Duration, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
	done := make(chan error, 1)
	var out []byte
	go func() { var err error; out, err = cmd.CombinedOutput(); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t := strings.TrimSpace(string(out))
			if len(t) > 400 {
				t = "…" + t[len(t)-400:]
			}
			return fmt.Errorf("%s: %v %s", name, err, t)
		}
		return nil
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		return fmt.Errorf("%s: Zeitüberschreitung nach %s", name, timeout)
	}
}

// schreibeFrigate legt Konfiguration + Compose-Datei ab; true, wenn sich die Konfiguration geändert hat.
func schreibeFrigate(cfg *Config) (bool, error) {
	rtsp := rtspAusKamera(cfg.Camera.StreamURL, cfg.Camera.SnapshotURL)
	if rtsp == "" {
		return false, fmt.Errorf("in KAI ist keine Kamera-Adresse gepflegt (Kamera → Livestream/Standbild)")
	}
	if err := os.MkdirAll(filepath.Join(frigateDir, "config"), 0o700); err != nil {
		return false, err
	}
	_ = os.MkdirAll(filepath.Join(frigateDir, "storage"), 0o755)
	_ = os.WriteFile(filepath.Join(frigateDir, "docker-compose.yml"), []byte(strings.Replace(frigateCompose, "__IMAGE__", frigateImage(hailoTreiberVersion()), 1)), 0o644)
	neu := frigateConfig(rtsp, cfg.Detect.Labels)
	p := filepath.Join(frigateDir, "config", "config.yml")
	alt, _ := os.ReadFile(p)
	if string(alt) == neu {
		return false, nil
	}
	return true, os.WriteFile(p, []byte(neu), 0o600) // enthält das Kamera-Passwort
}

func frigateAntwortet(base string) bool {
	c := &http.Client{Timeout: 4 * time.Second}
	r, err := c.Get(strings.TrimRight(base, "/") + "/api/version")
	if err != nil {
		return false
	}
	r.Body.Close()
	return r.StatusCode == 200
}

// hailoTreiberVersion: Version des geladenen Hailo-Treibers am Gerät ("" = unbekannt).
func hailoTreiberVersion() string {
	if b, err := os.ReadFile("/sys/module/hailo_pci/version"); err == nil {
		return strings.TrimSpace(string(b))
	}
	out, err := exec.Command("modinfo", "-F", "version", "hailo_pci").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// frigateImage: das Image, das zum Treiber passt — das Original, wenn der Treiber zur mitgelieferten
// HailoRT passt (oder unbekannt ist), sonst das am Gerät gebaute mit der Treiber-Version. Rein, testbar.
func frigateImage(treiber string) string {
	if treiber == "" || treiber == frigateHailoRT {
		return frigateBasis
	}
	return "kai-frigate:hailort-" + treiber
}

// frigateImageBauen: Frigate-Image mit der HailoRT-Version des Treibers (Bibliothek + Python-Modul aus den
// Builds des Frigate-Projekts). Ohne pip: das Rad ist ein Zip und wird über das alte Modul gelegt.
func frigateImageBauen(treiber string) error {
	dir := filepath.Join(frigateDir, "build")
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	rel := "https://github.com/frigate-nvr/hailort/releases/download/v" + treiber + "/"
	rad := "hailort-" + treiber + "-cp311-cp311-linux_aarch64.whl"
	for _, f := range []string{"hailort-debian12-arm64.tar.gz", rad} {
		if err := lauf(15*time.Minute, "curl", "-fsSL", "-o", filepath.Join(dir, f), rel+f); err != nil {
			return fmt.Errorf("HailoRT %s für Frigate nicht verfügbar (%s): %v", treiber, f, err)
		}
	}
	df := "FROM " + frigateBasis + "\n" +
		"COPY hailort-debian12-arm64.tar.gz " + rad + " /tmp/hailort/\n" +
		"RUN tar -C / -xzf /tmp/hailort/hailort-debian12-arm64.tar.gz && \\\n" +
		"    rm -rf /usr/local/lib/python3.11/dist-packages/hailo_platform* && \\\n" +
		"    python3 -c \"import zipfile; zipfile.ZipFile('/tmp/hailort/" + rad + "').extractall('/usr/local/lib/python3.11/dist-packages')\" && \\\n" +
		"    ldconfig && rm -rf /tmp/hailort\n"
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(df), 0o644); err != nil {
		return err
	}
	return lauf(60*time.Minute, "docker", "build", "-t", frigateImage(treiber), dir)
}

// frigateKiOK: Frigate antwortet UND der KI-Beschleuniger rechnet (Inferenzzeit > 0). Die API allein reicht
// nicht — bei falschem Treiber antwortet sie zwischen zwei Abstürzen des Detektors trotzdem.
func frigateKiOK(base string) bool {
	st := frigateStats(base)
	if st == nil {
		return false
	}
	v, _ := st["inferenz_ms"].(float64)
	return v > 0
}

func frigateFehlerZeile() string {
	out, _ := exec.Command("sh", "-c", "docker logs --tail 80 frigate 2>&1 | grep -iE 'error|exception' | tail -2").Output()
	t := strings.TrimSpace(string(out))
	if len(t) > 300 {
		t = t[len(t)-300:]
	}
	return t
}

// erkennungEinrichten: der eigentliche Lauf (im Hintergrund, nie zweimal gleichzeitig).
func (a *App) erkennungEinrichten() {
	erkMu.Lock()
	if erkLauf {
		erkMu.Unlock()
		return
	}
	erkLauf = true
	erkMu.Unlock()
	defer func() { erkMu.Lock(); erkLauf = false; erkMu.Unlock() }()

	s := ladeErkennung()
	if s.Phase != "neustart" { // neuer Lauf (nach einem Treiber-Neustart wird fortgesetzt)
		s = erkennungStand{Seit: time.Now().UTC().Format(time.RFC3339)}
	}
	schritt := func(t string) { s.Phase, s.Schritt, s.Meldung = "laeuft", t, ""; speichereErkennung(s); logEvent("info", "Objekterkennung: "+t) }
	fehler := func(err error) {
		s.Phase, s.Meldung = "fehler", err.Error()
		speichereErkennung(s)
		logEvent("error", "Objekterkennung einrichten fehlgeschlagen ("+s.Schritt+"): "+err.Error())
	}

	schritt("Gerät prüfen")
	if ok, m := istPi5(); !ok {
		fehler(fmt.Errorf("der AI HAT+ braucht einen Raspberry Pi 5 (gefunden: %s)", m))
		return
	}
	if !hailoDa() {
		if s.Neustart >= 1 {
			fehler(fmt.Errorf("Treiber installiert und neu gestartet, aber /dev/hailo0 fehlt – ist der AI HAT+ richtig aufgesteckt (PCIe-Kabel)?"))
			return
		}
		schritt("Treiber für den AI HAT+ installieren")
		if err := lauf(10*time.Minute, "apt-get", "update"); err != nil {
			fehler(err)
			return
		}
		if err := lauf(30*time.Minute, "apt-get", "install", "-y", "hailo-all"); err != nil {
			fehler(err)
			return
		}
		s.Phase, s.Schritt, s.Neustart = "neustart", "Neustart für den Treiber", s.Neustart+1
		speichereErkennung(s)
		logEvent("info", "Objekterkennung: Treiber installiert, Gerät startet neu und richtet danach weiter ein")
		time.Sleep(5 * time.Second) // Protokoll noch an KAI geben
		syscall.Sync()
		_ = syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART)
		return
	}
	if _, err := exec.LookPath("docker"); err != nil {
		schritt("Docker installieren")
		if err := lauf(30*time.Minute, "sh", "-c", "curl -fsSL https://get.docker.com | sh"); err != nil {
			fehler(err)
			return
		}
	}
	_ = lauf(time.Minute, "systemctl", "enable", "--now", "docker")

	schritt("Frigate-Konfiguration schreiben")
	cfg := a.getCfg()
	if _, err := schreibeFrigate(cfg); err != nil {
		fehler(err)
		return
	}
	treiber := hailoTreiberVersion()
	s.Treiber = treiber
	schritt("Frigate laden (rund 1 GB, das dauert)")
	if err := lauf(60*time.Minute, "docker", "pull", frigateBasis); err != nil {
		fehler(err)
		return
	}
	if frigateImage(treiber) != frigateBasis {
		schritt("Frigate an den Hailo-Treiber " + treiber + " anpassen")
		if err := frigateImageBauen(treiber); err != nil {
			fehler(err)
			return
		}
	}
	schritt("Frigate starten")
	if err := lauf(5*time.Minute, "docker", "compose", "-f", filepath.Join(frigateDir, "docker-compose.yml"), "up", "-d"); err != nil {
		fehler(err)
		return
	}
	base := cfg.Detect.FrigateURL
	if base == "" {
		base = "http://127.0.0.1:5000"
	}
	for i := 0; i < 60 && !frigateAntwortet(base); i++ {
		time.Sleep(5 * time.Second)
	}
	if !frigateAntwortet(base) {
		fehler(fmt.Errorf("Frigate startet nicht – %s", frigateFehlerZeile()))
		return
	}
	schritt("Prüfen, ob der KI-Beschleuniger rechnet")
	for i := 0; i < 36 && !frigateKiOK(base); i++ {
		time.Sleep(5 * time.Second)
	}
	if !frigateKiOK(base) {
		fehler(fmt.Errorf("Frigate läuft, aber der KI-Beschleuniger startet nicht (Treiber %s) – %s", treiber, frigateFehlerZeile()))
		return
	}
	s.Phase, s.Schritt, s.Meldung = "fertig", "eingerichtet", ""
	speichereErkennung(s)
	logEvent("info", "Objekterkennung eingerichtet – Frigate läuft")
}

// erkennungPflege: nach einem Treiber-Neustart den Lauf fortsetzen; danach Konfigurationsänderungen
// aus KAI (Kamera-Adresse/Passwort, Klassen) in Frigate nachziehen.
func (a *App) erkennungPflege() {
	time.Sleep(60 * time.Second) // Netz und KAI-Abgleich nach dem Start abwarten
	if ladeErkennung().Phase == "neustart" {
		go a.erkennungEinrichten()
	}
	for {
		if s := ladeErkennung(); s.Phase == "fertig" {
			// Treiber geändert (z. B. Systemaktualisierung von hailo-all) → Frigate passend neu einrichten
			if t := hailoTreiberVersion(); t != "" && s.Treiber != "" && t != s.Treiber {
				logEvent("info", "Objekterkennung: Hailo-Treiber "+s.Treiber+" → "+t+", Frigate wird angepasst")
				go a.erkennungEinrichten()
				time.Sleep(5 * time.Minute)
				continue
			}
			if geaendert, err := schreibeFrigate(a.getCfg()); err == nil && geaendert {
				if err := lauf(2*time.Minute, "docker", "restart", "frigate"); err == nil {
					logEvent("info", "Objekterkennung: geänderte Einstellungen aus KAI in Frigate übernommen")
				}
			}
		}
		time.Sleep(5 * time.Minute)
	}
}

// erkennungZustand: reist mit dem Herzschlag nach KAI (Stand der Einrichtung + Live-Prüfung).
func (a *App) erkennungZustand() map[string]any {
	s := ladeErkennung()
	cfg := a.getCfg()
	pi5, modell := istPi5()
	z := map[string]any{"phase": s.Phase, "schritt": s.Schritt, "meldung": s.Meldung, "seit": s.Seit, "am": s.Am,
		"pi5": pi5, "modell": modell, "hailo": hailoDa(), "aktiv": cfg.Detect.Enabled, "treiber": hailoTreiberVersion()}
	base := cfg.Detect.FrigateURL
	if base == "" {
		base = "http://127.0.0.1:5000"
	}
	if s.Phase == "fertig" || cfg.Detect.Enabled {
		z["frigate"] = frigateAntwortet(base)
		if st := frigateStats(base); st != nil {
			z["stats"] = st
		}
	}
	return z
}

// frigateStats: Inferenzzeit des Beschleunigers und Bilder/s der Kamera (kurz, für die Anzeige in KAI).
func frigateStats(base string) map[string]any {
	c := &http.Client{Timeout: 4 * time.Second}
	r, err := c.Get(strings.TrimRight(base, "/") + "/api/stats")
	if err != nil {
		return nil
	}
	defer r.Body.Close()
	var raw struct {
		Detectors map[string]struct {
			Inference float64 `json:"inference_speed"`
		} `json:"detectors"`
		Cameras map[string]struct {
			CameraFps    float64 `json:"camera_fps"`
			DetectionFps float64 `json:"detection_fps"`
		} `json:"cameras"`
	}
	if json.NewDecoder(r.Body).Decode(&raw) != nil {
		return nil
	}
	out := map[string]any{}
	for _, d := range raw.Detectors {
		out["inferenz_ms"] = d.Inference
	}
	for _, c := range raw.Cameras {
		out["kamera_fps"], out["erkennung_fps"] = c.CameraFps, c.DetectionFps
	}
	return out
}
