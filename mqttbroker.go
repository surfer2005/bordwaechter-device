package main

// Schlanker MQTT-Empfänger (MQTT 3.1.1, nur Standardbibliothek) für Sensoren im Bord-Netz: Shelly, ESPHome,
// Web-IO melden hier statt an Home Assistant (Peter 2026-11-07). Jeder Sensor hat einen eigenen Zugang aus KAI
// (Benutzer + Kennwort, am Gerät nur als Hash) und darf nur unter seinem eigenen Thema schreiben und lesen.
// Unterstützt: CONNECT (mit Last Will), PUBLISH QoS 0/1/2, SUBSCRIBE/UNSUBSCRIBE (Wildcards + und #),
// zurückgehaltene Nachrichten, PING, DISCONNECT. Nicht unterstützt (nicht nötig): persistente Sitzungen.

import (
	"bufio"
	"errors"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"time"
)

type mqttZugang struct {
	SensorID int
	Praefix  string
}

// mqttAuth prüft Benutzer/Kennwort und liefert, unter welchem Thema der Client arbeiten darf.
type mqttAuth func(user, pass string) (mqttZugang, bool)

// mqttEingang wird für jede angenommene Nachricht eines Clients aufgerufen.
type mqttEingang func(z mqttZugang, topic string, payload []byte)

type mqttClient struct {
	conn   net.Conn
	w      *bufio.Writer
	wmu    sync.Mutex
	zugang mqttZugang
	id     string
	subs   map[string]byte
	will   *mqttNachricht
	b      *MQTTBroker
}

type mqttNachricht struct {
	topic   string
	payload []byte
	retain  bool
}

type MQTTBroker struct {
	mu        sync.Mutex
	clients   map[*mqttClient]bool
	retained  map[string][]byte
	auth      mqttAuth
	eingang   mqttEingang
	ln        net.Listener
	verbunden map[int]time.Time // SensorID → letzte Verbindung
}

func neuerMQTTBroker(auth mqttAuth, eingang mqttEingang) *MQTTBroker {
	return &MQTTBroker{clients: map[*mqttClient]bool{}, retained: map[string][]byte{}, auth: auth, eingang: eingang, verbunden: map[int]time.Time{}}
}

func (b *MQTTBroker) Starten(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	b.ln = ln
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					return
				}
				time.Sleep(200 * time.Millisecond)
				continue
			}
			go b.bedienen(c)
		}
	}()
	return nil
}

func (b *MQTTBroker) Stoppen() {
	if b.ln != nil {
		_ = b.ln.Close()
	}
	b.mu.Lock()
	for c := range b.clients {
		_ = c.conn.Close()
	}
	b.mu.Unlock()
}

// Verbunden: welche Sensoren gerade eine offene Verbindung haben.
func (b *MQTTBroker) Verbunden() map[int]bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := map[int]bool{}
	for c := range b.clients {
		out[c.zugang.SensorID] = true
	}
	return out
}

// Senden: interne Nachricht (z. B. „Firmware aktualisieren“ an einen ESPHome-Sensor).
func (b *MQTTBroker) Senden(topic string, payload []byte, retain bool) {
	b.verteilen(nil, topic, payload, retain)
}

// ── Pakete ──────────────────────────────────────────────────────────────────────────────────────────

func leseLaenge(r *bufio.Reader) (int, error) {
	mult, wert := 1, 0
	for i := 0; i < 4; i++ {
		c, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		wert += int(c&127) * mult
		if c&128 == 0 {
			return wert, nil
		}
		mult *= 128
	}
	return 0, errors.New("Länge zu groß")
}

func schreibeLaenge(n int) []byte {
	out := []byte{}
	for {
		d := byte(n % 128)
		n /= 128
		if n > 0 {
			d |= 128
		}
		out = append(out, d)
		if n == 0 {
			return out
		}
	}
}

func leseString(p []byte) (string, []byte, error) {
	if len(p) < 2 {
		return "", nil, errors.New("kurz")
	}
	n := int(p[0])<<8 | int(p[1])
	if len(p) < 2+n {
		return "", nil, errors.New("kurz")
	}
	return string(p[2 : 2+n]), p[2+n:], nil
}

func mqttStr(s string) []byte { return append([]byte{byte(len(s) >> 8), byte(len(s))}, s...) }

func (c *mqttClient) senden(kopf byte, rumpf []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := c.w.WriteByte(kopf); err != nil {
		return err
	}
	if _, err := c.w.Write(schreibeLaenge(len(rumpf))); err != nil {
		return err
	}
	if _, err := c.w.Write(rumpf); err != nil {
		return err
	}
	return c.w.Flush()
}

func (c *mqttClient) publish(topic string, payload []byte, retain bool) {
	kopf := byte(0x30)
	if retain {
		kopf |= 1
	}
	_ = c.senden(kopf, append(mqttStr(topic), payload...))
}

// thema gehört dem Client, wenn es mit seinem Präfix beginnt.
func darfThema(praefix, thema string) bool {
	return praefix != "" && (thema == praefix || strings.HasPrefix(thema, praefix+"/"))
}

// passt: MQTT-Filter (+ eine Ebene, # Rest) gegen ein Thema.
func passt(filter, thema string) bool {
	f := strings.Split(filter, "/")
	t := strings.Split(thema, "/")
	for i, teil := range f {
		if teil == "#" {
			return true
		}
		if i >= len(t) {
			return false
		}
		if teil != "+" && teil != t[i] {
			return false
		}
	}
	return len(f) == len(t)
}

func (b *MQTTBroker) verteilen(von *mqttClient, topic string, payload []byte, retain bool) {
	b.mu.Lock()
	if retain {
		if len(payload) == 0 {
			delete(b.retained, topic)
		} else {
			b.retained[topic] = append([]byte(nil), payload...)
		}
	}
	ziele := []*mqttClient{}
	for c := range b.clients {
		if c == von {
			continue
		}
		for f := range c.subs {
			if passt(f, topic) {
				ziele = append(ziele, c)
				break
			}
		}
	}
	b.mu.Unlock()
	for _, c := range ziele {
		c.publish(topic, payload, false)
	}
}

func (b *MQTTBroker) bedienen(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReaderSize(conn, 4096)
	c := &mqttClient{conn: conn, w: bufio.NewWriter(conn), subs: map[string]byte{}, b: b}
	sauber := false
	angemeldet := false
	keepalive := 60 * time.Second
	defer func() {
		if angemeldet {
			b.mu.Lock()
			delete(b.clients, c)
			b.mu.Unlock()
			if !sauber && c.will != nil {
				b.eingangMelden(c, c.will.topic, c.will.payload)
				b.verteilen(c, c.will.topic, c.will.payload, c.will.retain)
			}
		}
	}()
	for {
		frist := keepalive + keepalive/2
		if !angemeldet {
			frist = 10 * time.Second
		}
		_ = conn.SetReadDeadline(time.Now().Add(frist))
		kopf, err := r.ReadByte()
		if err != nil {
			return
		}
		n, err := leseLaenge(r)
		if err != nil || n > 256*1024 {
			return
		}
		p := make([]byte, n)
		if _, err := io.ReadFull(r, p); err != nil {
			return
		}
		typ := kopf >> 4
		if !angemeldet && typ != 1 {
			return
		}
		switch typ {
		case 1: // CONNECT
			if angemeldet {
				return
			}
			ok, ka := c.connect(p)
			if !ok {
				return
			}
			if ka > 0 {
				keepalive = time.Duration(ka) * time.Second
			}
			angemeldet = true
			b.mu.Lock()
			b.clients[c] = true
			b.verbunden[c.zugang.SensorID] = time.Now()
			b.mu.Unlock()
		case 3: // PUBLISH
			qos := (kopf >> 1) & 3
			retain := kopf&1 == 1
			topic, rest, err := leseString(p)
			if err != nil {
				return
			}
			var pid []byte
			if qos > 0 {
				if len(rest) < 2 {
					return
				}
				pid, rest = rest[:2], rest[2:]
			}
			if darfThema(c.zugang.Praefix, topic) {
				b.eingangMelden(c, topic, rest)
				b.verteilen(c, topic, rest, retain)
			}
			if qos == 1 {
				_ = c.senden(0x40, pid)
			} else if qos == 2 {
				_ = c.senden(0x50, pid) // PUBREC
			}
		case 6: // PUBREL → PUBCOMP
			if len(p) >= 2 {
				_ = c.senden(0x70, p[:2])
			}
		case 4, 5, 7: // PUBACK/PUBREC/PUBCOMP auf eigene Nachrichten (wir senden nur QoS 0)
		case 8: // SUBSCRIBE
			if len(p) < 2 {
				return
			}
			pid, rest := p[:2], p[2:]
			antwort := append([]byte(nil), pid...)
			neu := []string{}
			for len(rest) > 0 {
				f, r2, err := leseString(rest)
				if err != nil || len(r2) < 1 {
					return
				}
				rest = r2[1:]
				if darfThema(c.zugang.Praefix, f) { // Filter muss unter dem eigenen Thema liegen (Wildcards dahinter erlaubt)
					b.mu.Lock()
					c.subs[f] = 0
					b.mu.Unlock()
					antwort = append(antwort, 0)
					neu = append(neu, f)
				} else {
					antwort = append(antwort, 0x80)
				}
			}
			_ = c.senden(0x90, antwort)
			// zurückgehaltene Nachrichten für die neuen Filter
			b.mu.Lock()
			alt := map[string][]byte{}
			for t, v := range b.retained {
				for _, f := range neu {
					if passt(f, t) {
						alt[t] = v
					}
				}
			}
			b.mu.Unlock()
			for t, v := range alt {
				c.publish(t, v, true)
			}
		case 10: // UNSUBSCRIBE
			if len(p) < 2 {
				return
			}
			pid, rest := p[:2], p[2:]
			for len(rest) > 0 {
				f, r2, err := leseString(rest)
				if err != nil {
					break
				}
				rest = r2
				b.mu.Lock()
				delete(c.subs, f)
				b.mu.Unlock()
			}
			_ = c.senden(0xB0, pid)
		case 12: // PINGREQ
			_ = c.senden(0xD0, nil)
		case 14: // DISCONNECT
			sauber = true
			return
		default:
			return
		}
	}
}

func (b *MQTTBroker) eingangMelden(c *mqttClient, topic string, payload []byte) {
	if b.eingang != nil {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("mqtt: Verarbeitung %s: %v", topic, r)
			}
		}()
		b.eingang(c.zugang, topic, payload)
	}
}

// connect wertet CONNECT aus und antwortet mit CONNACK. Rückgabe: angenommen?, Keepalive in Sekunden.
func (c *mqttClient) connect(p []byte) (bool, int) {
	name, rest, err := leseString(p)
	if err != nil || len(rest) < 4 {
		return false, 0
	}
	stufe, flags := rest[0], rest[1]
	ka := int(rest[2])<<8 | int(rest[3])
	rest = rest[4:]
	if (name != "MQTT" && name != "MQIsdp") || (stufe != 4 && stufe != 3) {
		_ = c.senden(0x20, []byte{0, 1}) // falsche Protokollversion
		return false, 0
	}
	if c.id, rest, err = leseString(rest); err != nil {
		return false, 0
	}
	if flags&0x04 != 0 { // Last Will
		var t string
		var m string
		if t, rest, err = leseString(rest); err != nil {
			return false, 0
		}
		if m, rest, err = leseString(rest); err != nil {
			return false, 0
		}
		c.will = &mqttNachricht{topic: t, payload: []byte(m), retain: flags&0x20 != 0}
	}
	user, pass := "", ""
	if flags&0x80 != 0 {
		if user, rest, err = leseString(rest); err != nil {
			return false, 0
		}
	}
	if flags&0x40 != 0 {
		if pass, _, err = leseString(rest); err != nil {
			return false, 0
		}
	}
	z, ok := c.b.auth(user, pass)
	if !ok {
		_ = c.senden(0x20, []byte{0, 4}) // Benutzer/Kennwort falsch
		logEvent("warn", "Sensor-Anmeldung abgelehnt: "+user)
		return false, 0
	}
	c.zugang = z
	if c.will != nil && !darfThema(z.Praefix, c.will.topic) {
		c.will = nil
	}
	_ = c.senden(0x20, []byte{0, 0})
	return true, ka
}
