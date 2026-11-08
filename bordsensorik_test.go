package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ── MQTT-Empfänger ─────────────────────────────────────────────────────────────────────────────────

func mqttPaket(kopf byte, rumpf []byte) []byte { return append(append([]byte{kopf}, schreibeLaenge(len(rumpf))...), rumpf...) }

func mqttConnect(user, pass string) []byte {
	r := append(mqttStr("MQTT"), 4, 0xC2, 0, 60) // Benutzer + Kennwort + clean session, Keepalive 60
	r = append(r, mqttStr("client-"+user)...)
	r = append(r, mqttStr(user)...)
	r = append(r, mqttStr(pass)...)
	return mqttPaket(0x10, r)
}

func lesePaket(t *testing.T, r *bufio.Reader) (byte, []byte) {
	t.Helper()
	k, err := r.ReadByte()
	if err != nil {
		t.Fatalf("lesen: %v", err)
	}
	n, _ := leseLaenge(r)
	p := make([]byte, n)
	_, _ = io.ReadFull(r, p)
	return k, p
}

func TestMQTTBrokerZugangUndThemen(t *testing.T) {
	var mu sync.Mutex
	empfangen := map[string]string{}
	b := neuerMQTTBroker(func(u, p string) (mqttZugang, bool) {
		if u == "kn1178-kuehlraum" && p == "geheim" {
			return mqttZugang{SensorID: 7, Praefix: "kn1178-kuehlraum"}, true
		}
		return mqttZugang{}, false
	}, func(z mqttZugang, topic string, payload []byte) {
		mu.Lock()
		empfangen[topic] = string(payload)
		mu.Unlock()
	})
	if err := b.Starten("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	defer b.Stoppen()
	addr := b.ln.Addr().String()

	// falsches Kennwort → CONNACK 4
	c1, _ := net.Dial("tcp", addr)
	_, _ = c1.Write(mqttConnect("kn1178-kuehlraum", "falsch"))
	k, p := lesePaket(t, bufio.NewReader(c1))
	if k != 0x20 || p[1] != 4 {
		t.Fatalf("erwartet Ablehnung, bekam %x %v", k, p)
	}
	c1.Close()

	c, _ := net.Dial("tcp", addr)
	defer c.Close()
	r := bufio.NewReader(c)
	_, _ = c.Write(mqttConnect("kn1178-kuehlraum", "geheim"))
	if k, p := lesePaket(t, r); k != 0x20 || p[1] != 0 {
		t.Fatalf("Anmeldung abgelehnt: %x %v", k, p)
	}
	// eigenes Thema: angenommen (QoS 1 → PUBACK), fremdes Thema: verworfen
	pub := append(mqttStr("kn1178-kuehlraum/sensor/temperatur/state"), 0, 9)
	_, _ = c.Write(mqttPaket(0x32, append(pub, []byte("4.2")...)))
	if k, _ := lesePaket(t, r); k != 0x40 {
		t.Fatalf("PUBACK erwartet, bekam %x", k)
	}
	_, _ = c.Write(mqttPaket(0x30, append(mqttStr("anderer/sensor/x/state"), []byte("1")...)))
	// Abonnieren: eigenes ja, fremdes nein
	sub := append([]byte{0, 1}, append(mqttStr("kn1178-kuehlraum/button/#"), 0)...)
	sub = append(sub, append(mqttStr("homeassistant/status"), 0)...)
	_, _ = c.Write(mqttPaket(0x82, sub))
	k, p = lesePaket(t, r)
	if k != 0x90 || p[2] != 0 || p[3] != 0x80 {
		t.Fatalf("SUBACK falsch: %x %v", k, p)
	}
	b.Senden("kn1178-kuehlraum/button/firmware_aktualisieren/command", []byte("PRESS"), false)
	k, p = lesePaket(t, r)
	if k>>4 != 3 || !strings.HasSuffix(string(p), "PRESS") {
		t.Fatalf("Befehl nicht zugestellt: %x %q", k, p)
	}
	_, _ = c.Write([]byte{0xC0, 0}) // PING
	if k, _ := lesePaket(t, r); k != 0xD0 {
		t.Fatalf("PINGRESP erwartet")
	}
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if empfangen["kn1178-kuehlraum/sensor/temperatur/state"] != "4.2" {
		t.Fatalf("Wert fehlt: %v", empfangen)
	}
	if _, ok := empfangen["anderer/sensor/x/state"]; ok {
		t.Fatal("fremdes Thema durfte nicht durch")
	}
	if !b.Verbunden()[7] {
		t.Fatal("Sensor 7 sollte verbunden sein")
	}
}

func TestPasst(t *testing.T) {
	for _, f := range [][3]string{{"a/+/c", "a/b/c", "1"}, {"a/#", "a/b/c", "1"}, {"a/+", "a/b/c", ""}, {"a/b", "a/b", "1"}} {
		if passt(f[0], f[1]) != (f[2] == "1") {
			t.Errorf("passt(%s, %s)", f[0], f[1])
		}
	}
}

// ── Werte aus Nachrichten ──────────────────────────────────────────────────────────────────────────

func TestWerteAusNachricht(t *testing.T) {
	st := &sensorStatus{}
	w := werteAusNachricht("shelly", "kn1178-bar", "kn1178-bar/events/rpc",
		[]byte(`{"src":"shellyhtg3","method":"NotifyFullStatus","params":{"ts":1.7e9,"temperature:0":{"id":0,"tC":4.9},"humidity:0":{"id":0,"rh":61},"devicepower:0":{"battery":{"V":5.9,"percent":84}},"wifi":{"sta_ip":"192.168.75.52"}}}`), st)
	got := map[string]any{}
	for _, x := range w {
		got[x.Schluessel] = x.Wert
	}
	if got["temperatur"] != 4.9 || got["feuchte"] != 61.0 || got["batterie"] != 84.0 {
		t.Fatalf("Shelly: %v", got)
	}
	if st.IP != "192.168.75.52" || st.Batterie == nil || *st.Batterie != 84 {
		t.Fatalf("Shelly-Status: %+v", st)
	}
	if w := werteAusNachricht("esphome", "kn1178-kuehl", "kn1178-kuehl/binary_sensor/tuer/state", []byte("ON"), st); len(w) != 1 || w[0].Wert != true || w[0].Schluessel != "tuer" {
		t.Fatalf("ESPHome binär: %v", w)
	}
	if w := werteAusNachricht("esphome", "kn1178-kuehl", "kn1178-kuehl/sensor/temperatur/state", []byte("8.6"), st); w[0].Wert != 8.6 {
		t.Fatalf("ESPHome: %v", w)
	}
	// Web-IO wie auf der MS-KONSTANZ (MQTT Explorer 07.11.)
	if w := werteAusNachricht("webio", "BSBWEBIO000001", "BSBWEBIO000001/get/input/1", []byte("ON"), st); w[0].Schluessel != "input_1" || w[0].Wert != true {
		t.Fatalf("Web-IO: %v", w)
	}
	if w := werteAusNachricht("webio", "BSBWEBIO000001", "BSBWEBIO000001/get/counter/1", []byte("859"), st); w[0].Wert != 859.0 {
		t.Fatalf("Web-IO Zähler: %v", w)
	}
	st.Online = false
	werteAusNachricht("webio", "BSBWEBIO000001", "BSBWEBIO000001", []byte("online"), st)
	if !st.Online {
		t.Fatal("Web-IO online")
	}
}

func TestSensorZugangUndLampe(t *testing.T) {
	s := &Sensorik{defs: map[int]*sensorDef{}, aktuell: map[string]any{}, verletzt: map[string]time.Time{}, status: map[int]*sensorStatus{}, fwStand: map[int]string{}}
	h := sha256.Sum256([]byte("salz" + "kennwort"))
	max := 7.0
	s.defs[3] = &sensorDef{ID: 3, Art: "esphome", User: "kuehl", Salt: "salz", Hash: hex.EncodeToString(h[:]), Praefix: "kuehl",
		Werte: []sensorWertDef{{Schluessel: "temperatur", Max: &max, DauerMin: 15, Lampe: true}}}
	if _, ok := s.pruefeZugang("kuehl", "falsch"); ok {
		t.Fatal("falsches Kennwort angenommen")
	}
	z, ok := s.pruefeZugang("kuehl", "kennwort")
	if !ok || z.Praefix != "kuehl" {
		t.Fatal("Zugang abgelehnt")
	}
	s.eingang(z, "kuehl/sensor/temperatur/state", []byte("8.6"))
	t0 := time.Now()
	if s.lampeBerechnen(t0) {
		t.Fatal("Lampe zu früh")
	}
	if !s.lampeBerechnen(t0.Add(16 * time.Minute)) {
		t.Fatal("Lampe nach 16 Minuten erwartet")
	}
	s.eingang(z, "kuehl/sensor/temperatur/state", []byte("4.0"))
	if s.lampeBerechnen(t0.Add(17 * time.Minute)) {
		t.Fatal("Lampe muss aus sein, wenn der Wert zurück ist")
	}
	if len(s.puffer) != 2 {
		t.Fatalf("Puffer: %d", len(s.puffer))
	}
}

// ── Licht ──────────────────────────────────────────────────────────────────────────────────────────

// Ausschnitt aus GET /devices des Gateways der MS-KONSTANZ (07.11.2026)
const devicesBeispiel = `{"devices":[
{"id":1,"name":"Line 0 - DALI#00","available":false,"status":{"lampFailure":false,"controlGearFailure":false,"lampOn":false},"features":{"switchable":{"status":true},"dimmable":{"status":83.07086614173228},"colorKelvin":{"status":2700},"colorRGB":{"status":{"r":1.0,"g":1.0,"b":1.0}}},"groups":[0,2,3,9],"address":0,"line":0,"daliTypes":[254]},
{"id":11,"name":"Line 0 - DALI#10","available":false,"status":{"lampFailure":false},"features":{"switchable":{"status":true},"dimmable":{"status":81.49606299212599}},"groups":[0,6,7],"address":10,"line":0,"daliTypes":[7]},
{"id":32,"name":"Line 0 - DALI#31","available":true,"status":{"lampFailure":true},"features":{"switchable":{"status":false},"dimmable":{"status":0.0},"colorKelvin":{"status":2700},"colorRGB":{"status":{"r":0.0,"g":0.051181102362204724,"b":1.0}}},"groups":[0,2,4,13],"address":31,"line":0,"daliTypes":[8]}
]}`

func TestStandAusGeraeten(t *testing.T) {
	st, err := standAusGeraeten([]byte(devicesBeispiel))
	if err != nil || len(st) != 3 {
		t.Fatalf("%v %d", err, len(st))
	}
	if !st[0].An || st[0].Lvl != 83 || st[0].Verfuegbar || st[0].RGB != nil {
		t.Fatalf("A0: %+v", st[0])
	}
	f := st[2]
	if f.A != 31 || f.An || !f.LampeDefekt || f.RGB == nil || *f.RGB != "#000dff" || f.Kelvin == nil || *f.Kelvin != 2700 || f.lunatoneID != 32 {
		t.Fatalf("A31: %+v rgb=%v", f, f.RGB)
	}
}

func TestSteuerSchritteUndAusfuehren(t *testing.T) {
	var mu sync.Mutex
	aufrufe := []string{}
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/devices" {
			_, _ = w.Write([]byte(devicesBeispiel))
			return
		}
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		aufrufe = append(aufrufe, r.URL.Path+" "+string(b))
		mu.Unlock()
	}))
	defer gw.Close()
	l := &Licht{planLauf: map[string]string{}, hc: gw.Client()}
	l.cfg = lichtConfig{Aktiv: true, GatewayHost: gw.URL}
	licht = l
	if err := l.abfragen(); err != nil {
		t.Fatal(err)
	}
	g, lvl, k, adr := 2, 50, 2700, 31
	aus := false
	if err := l.ausfuehren([]lichtAktion{{Ziel: "gruppe", ID: &g, Lvl: &lvl}, {Ziel: "leuchte", ID: &adr, RGB: "#ff0000", Kelvin: &k}, {Ziel: "alle", An: &aus}}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{`/group/2/control {"dimmable":50}`, `/device/32/control {"colorRGB":{"b":0,"g":0,"r":1}}`, `/device/32/control {"colorKelvin":2700}`, `/broadcast/control {"switchable":false}`}
	if strings.Join(aufrufe, "\n") != strings.Join(want, "\n") {
		t.Fatalf("Aufrufe:\n%s", strings.Join(aufrufe, "\n"))
	}
}

func TestSchluesselAusSperrt(t *testing.T) {
	sensorik = &Sensorik{defs: map[int]*sensorDef{5: {ID: 5, Art: "webio", Praefix: "BSBWEBIO000001"}}, aktuell: map[string]any{"5|input_1": false}, verletzt: map[string]time.Time{}, status: map[int]*sensorStatus{}, fwStand: map[int]string{}}
	l := &Licht{planLauf: map[string]string{}, hc: http.DefaultClient}
	l.cfg = lichtConfig{Aktiv: true, GatewayHost: "127.0.0.1:1"}
	l.cfg.Schluessel = &struct {
		Praefix    string `json:"praefix"`
		Schluessel string `json:"schluessel"`
	}{"BSBWEBIO000001", "input_1"}
	an := true
	if err := l.ausfuehren([]lichtAktion{{Ziel: "alle", An: &an}}); err == nil || !strings.Contains(err.Error(), "Schlüsselschalter") {
		t.Fatalf("erwartet Sperre, bekam %v", err)
	}
	if l.schluesselZustand() != "aus" {
		t.Fatal("Schlüssel aus")
	}
	// Hängt das Web-IO auf „aus“, die Leuchten sind aber erreichbar: unklar, nicht sperren
	l.stand = []leuchteStand{{A: 0, Verfuegbar: true}}
	if l.schluesselZustand() != "unklar" {
		t.Fatalf("erwartet unklar, bekam %q", l.schluesselZustand())
	}
	if err := l.ausfuehren([]lichtAktion{{Ziel: "alle", An: &an}}); err != nil && strings.Contains(err.Error(), "Schlüsselschalter") {
		t.Fatalf("darf nicht sperren: %v", err)
	}
}

func TestSonneUndPlan(t *testing.T) {
	// Konstanz, 7.11.2026: Untergang gegen 16:50 Uhr MEZ
	tag := time.Date(2026, 11, 7, 12, 0, 0, 0, ortszeit)
	_, unter, ok := sonnenzeiten(tag, 47.66, 9.18)
	u := unter.In(ortszeit)
	if !ok || u.Hour() != 16 || u.Minute() < 40 || u.Minute() > 59 {
		t.Fatalf("Sonnenuntergang %v", u)
	}
	an := true
	plan := []lichtPlanZeile{{ID: 1, Tage: "6", Zeit: "su+0", Aktionen: []lichtAktion{{Ziel: "alle", An: &an}}}, {ID: 2, Tage: "1234567", Zeit: "23:30"}}
	jetzt := u.Add(5 * time.Minute) // 7.11.2026 ist ein Samstag (6)
	f := planFaellig(plan, jetzt, 47.66, 9.18, map[string]string{})
	if len(f) != 1 || f[0].ID != 1 {
		t.Fatalf("fällig: %+v", f)
	}
	if len(planFaellig(plan, jetzt, 47.66, 9.18, map[string]string{"1|su+0": "2026-11-07"})) != 0 {
		t.Fatal("schon gelaufen")
	}
	if len(planFaellig(plan, u.Add(30*time.Minute), 47.66, 9.18, map[string]string{})) != 0 {
		t.Fatal("zu spät (über 15 Min.) darf nicht mehr laufen")
	}
	_ = json.Marshal
}
