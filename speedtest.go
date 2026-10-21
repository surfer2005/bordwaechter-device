package main

// Speedtest (2026-10-21): im KAI-Monitoring je Bordwächter anforderbar. KAI setzt die Anforderung,
// der nächste Herzschlag (heartbeatLoop) bekommt speedtest_requested=true zurück, und das Gerät misst:
//   • Verbindung Gerät → KAI-Server: Ping (Median aus 5), Download und Upload (Probengröße passt sich an),
//   • Erreichbarkeit der Modbus-Geräte an Bord (Smartmeter-Zähler aus der KAI-Konfig, Teltonika-Router):
//     TCP-Verbindungsaufbau + eine Register-Abfrage je Gerät mit Antwortzeit. Modbus überträgt nur wenige
//     Bytes — dort zählt, OB und WIE SCHNELL ein Gerät antwortet, nicht die Bandbreite.
// Nur ausgehend, best effort; ein Fehler steht im Ergebnis, statt den Lauf abzubrechen.

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

var speedtestLaeuft sync.Mutex

type modbusProbe struct {
	Name      string   `json:"name"`
	Host      string   `json:"host"`
	Port      int      `json:"port"`
	Unit      int      `json:"unit"`
	OK        bool     `json:"ok"`
	ConnectMs *float64 `json:"connect_ms,omitempty"`
	ReadMs    *float64 `json:"read_ms,omitempty"`
	Fehler    string   `json:"fehler,omitempty"`
}

func ms(d time.Duration) *float64 { v := float64(d.Microseconds()) / 1000; return &v }

func (k *KaiClient) rohAnfrage(method, path string, body io.Reader, ctype string, timeout time.Duration) (int64, time.Duration, error) {
	req, err := http.NewRequest(method, k.base+path, body)
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("X-Edge-Token", k.token)
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	cl := &http.Client{Timeout: timeout}
	t0 := time.Now()
	resp, err := cl.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	n, _ := io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != 200 {
		return 0, 0, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return n, time.Since(t0), nil
}

func mbit(bytes int64, d time.Duration) *float64 {
	if d <= 0 {
		return nil
	}
	v := float64(bytes) * 8 / d.Seconds() / 1e6
	return &v
}

// messen: erst 1 MB; ist das schneller als 1,5 s, eine größere Probe (bis 16 MB).
func messen(fn func(n int) (int64, time.Duration, error)) (*float64, error) {
	probe := 1 << 20
	n, d, err := fn(probe)
	for err == nil && d < 1500*time.Millisecond && probe < 16<<20 {
		probe *= 4
		if probe > 16<<20 {
			probe = 16 << 20
		}
		n, d, err = fn(probe)
	}
	if err != nil {
		return nil, err
	}
	return mbit(n, d), nil
}

func probeModbus(name, host string, port, unit, reg int) modbusProbe {
	p := modbusProbe{Name: name, Host: host, Port: port, Unit: unit}
	if port <= 0 {
		p.Port, port = 502, 502
	}
	t0 := time.Now()
	c, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", host, port), 3*time.Second)
	if err != nil {
		p.Fehler = "nicht erreichbar: " + err.Error()
		return p
	}
	p.ConnectMs = ms(time.Since(t0))
	c.Close()
	t1 := time.Now()
	_, err = modbusReadHolding(host, port, unit, reg, 1, 3*time.Second)
	p.ReadMs = ms(time.Since(t1))
	if err != nil && !strings.Contains(err.Error(), "ausnahme") {
		p.Fehler = "keine Antwort auf die Abfrage: " + err.Error()
		return p
	}
	// Eine Modbus-AUSNAHME (z. B. unbekanntes Register) ist auch eine Antwort — das Gerät ist da.
	p.OK = true
	if err != nil {
		p.Fehler = "antwortet (Ausnahme auf Testregister)"
	}
	return p
}

func (a *App) modbusZiele() []modbusProbe {
	var out []modbusProbe
	cfg := a.getCfg()
	if cfg.GPS.Source == "teltonika" && cfg.GPS.Host != "" {
		out = append(out, probeModbus("Teltonika-Router (GPS)", cfg.GPS.Host, cfg.GPS.Port, cfg.GPS.Unit, cfg.GPS.LatReg))
	}
	if k := a.getKai(); k != nil {
		if sc, err := k.GetSmartmeterConfig(); err == nil && sc != nil && sc.Host != "" {
			for _, m := range sc.Meters {
				reg := 0
				if len(m.Registers) > 0 {
					reg = m.Registers[0].Address
				}
				out = append(out, probeModbus("Smartmeter "+m.Name, sc.Host, sc.Port, m.Slave, reg))
			}
		}
	}
	return out
}

// runSpeedtest misst und meldet das Ergebnis an KAI (POST /api/edge/speedtest). Nur ein Lauf gleichzeitig.
func (a *App) runSpeedtest() {
	if !speedtestLaeuft.TryLock() {
		return
	}
	defer speedtestLaeuft.Unlock()
	k := a.getKai()
	if k == nil {
		return
	}
	res := map[string]any{}
	var fehler []string
	// Ping: Median aus 5
	var pings []time.Duration
	for i := 0; i < 5; i++ {
		if _, d, err := k.rohAnfrage("GET", "/api/edge/speedtest/ping", nil, "", 10*time.Second); err == nil {
			pings = append(pings, d)
		}
	}
	if len(pings) > 0 {
		sort.Slice(pings, func(i, j int) bool { return pings[i] < pings[j] })
		res["ping_ms"] = ms(pings[len(pings)/2])
	} else {
		fehler = append(fehler, "KAI-Server nicht erreichbar")
	}
	if len(pings) > 0 {
		if v, err := messen(func(n int) (int64, time.Duration, error) {
			return k.rohAnfrage("GET", fmt.Sprintf("/api/edge/speedtest/download?bytes=%d", n), nil, "", 120*time.Second)
		}); err == nil {
			res["download_mbit"] = v
		} else {
			fehler = append(fehler, "Download: "+err.Error())
		}
		if v, err := messen(func(n int) (int64, time.Duration, error) {
			buf := make([]byte, n)
			_, _ = rand.Read(buf)
			_, d, err := k.rohAnfrage("POST", "/api/edge/speedtest/upload", bytes.NewReader(buf), "application/octet-stream", 120*time.Second)
			return int64(n), d, err
		}); err == nil {
			res["upload_mbit"] = v
		} else {
			fehler = append(fehler, "Upload: "+err.Error())
		}
	}
	res["modbus"] = a.modbusZiele()
	if len(fehler) > 0 {
		res["fehler"] = strings.Join(fehler, "; ")
	}
	if _, code, err := k.do("POST", "/api/edge/speedtest", res); err != nil || code != 200 {
		log.Printf("speedtest: Ergebnis nicht gemeldet (%v, http %d)", err, code)
		return
	}
	b, _ := json.Marshal(res)
	log.Printf("speedtest: %s", string(b))
}
