package main

import (
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"
)

type Status struct {
	Ship         string  `json:"ship"`
	Reg          string  `json:"registration_number"`
	ShipID       int     `json:"ship_id"`
	Phase        string  `json:"phase"` // none | vor_abfahrt | unterwegs | beendet
	From         string  `json:"from"`
	To           string  `json:"to"`
	Soll         string  `json:"soll"`
	Abfahrt      string  `json:"abfahrt"`
	WartenMin    int     `json:"warten_min"`
	RestMin      int     `json:"rest_min"`
	RemainKm     float64 `json:"remain_km"`
	IstKmh       float64 `json:"ist_kmh"`
	SollKmh      float64 `json:"soll_kmh"`
	Verdict      string  `json:"verdict"` // ok | faster | slower | none
	DiffKmh      float64 `json:"diff_kmh"`
	OffCourseM   float64 `json:"off_course_m"`
	OffState     string  `json:"off_state"` // ok | warn | alarm | unknown
	Alarm        bool    `json:"alarm"`
	AlarmState   string  `json:"alarm_state"` // normal | alarm | acked
	SifaDeadman  bool    `json:"sifa_deadman_active"`
	Lat          float64 `json:"lat"`
	Lng          float64 `json:"lng"`
	PosAgeSec    int     `json:"pos_age_sec"`
	PosValid     bool    `json:"pos_valid"`
	CameraHazard bool    `json:"camera_hazard"`
	HatBordbuch  bool    `json:"hat_bordbuch"`
	BundleAgeSec int     `json:"bundle_age_sec"`
	Hint         string  `json:"hint"`
	Horn         bool    `json:"horn"`
	Lamp         bool    `json:"lamp"`
	UpdatedAt    string  `json:"updated_at"`
}

type Engine struct {
	mu   sync.Mutex
	cfg  *Config
	kai  *KaiClient
	gpio GPIO

	bundle   *Bundle
	bundleAt time.Time
	pos      Position
	lastPos  Position

	// Ereignis-SIFA
	alarmState string // normal | alarm | acked
	lastAck    time.Time
	// Totmann-SIFA
	lastDeadmanAck time.Time
	// Kamera
	camHazardUntil time.Time
	prevAck        bool

	// Test-Puls (Hupe/Lampe kurz schalten)
	testHornUntil time.Time
	testLampUntil time.Time

	status Status
}

// TestPulse: Ausgang für ein paar Sekunden erzwingen (Test-Knopf in der UI).
func (e *Engine) TestPulse(kind string, sec int) {
	e.mu.Lock()
	until := time.Now().Add(time.Duration(sec) * time.Second)
	switch kind {
	case "horn":
		e.testHornUntil = until
	case "lamp":
		e.testLampUntil = until
	}
	e.mu.Unlock()
}

func NewEngine(cfg *Config, kai *KaiClient, gpio GPIO) *Engine {
	return &Engine{cfg: cfg, kai: kai, gpio: gpio, alarmState: "normal", lastDeadmanAck: time.Now()}
}

func (e *Engine) SetConfig(cfg *Config, kai *KaiClient) {
	e.mu.Lock()
	e.cfg = cfg
	e.kai = kai
	e.mu.Unlock()
}

func (e *Engine) SetBundle(b *Bundle) {
	e.mu.Lock()
	e.bundle = b
	e.bundleAt = time.Now()
	e.mu.Unlock()
}

func (e *Engine) SetPosition(p Position) {
	e.mu.Lock()
	defer e.mu.Unlock()
	// Fehlt eine Geschwindigkeit, aus der Bewegung ableiten (robuster als unsicheres Speed-Register).
	if p.SpeedKmh == 0 && e.lastPos.Valid && p.Valid {
		dt := p.TS.Sub(e.lastPos.TS).Hours()
		if dt > 0 && dt < 0.2 { // nur plausible kurze Abstände
			d := haversineKm(e.lastPos.Lat, e.lastPos.Lng, p.Lat, p.Lng)
			p.SpeedKmh = d / dt
		}
	}
	if p.Valid {
		e.lastPos = p
	}
	e.pos = p
}

func (e *Engine) CameraHazard() {
	e.mu.Lock()
	hold := e.cfg.Camera.HoldSec
	if hold <= 0 {
		hold = 20
	}
	e.camHazardUntil = time.Now().Add(time.Duration(hold) * time.Second)
	e.mu.Unlock()
}

// Ack: quittiert einen laufenden Ereignis-Alarm UND bestätigt Anwesenheit (Totmann).
func (e *Engine) Ack() {
	e.mu.Lock()
	e.lastDeadmanAck = time.Now()
	if e.alarmState == "alarm" || e.alarmState == "acked" {
		e.alarmState = "acked"
		e.lastAck = time.Now()
	}
	e.mu.Unlock()
}

func hhmmToMin(s string) int {
	var h, m int
	if _, err := fmt.Sscanf(s, "%d:%d", &h, &m); err != nil {
		return -1
	}
	return h*60 + m
}

// computeAdvisory: Phase/nächster Halt/Kursabweichung/Empfehlung aus Bundle + Position (reine Rechnung).
func computeAdvisory(b *Bundle, pos Position, now time.Time, tolDefault float64) Status {
	var st Status
	st.OffState = "unknown"
	st.Verdict = "none"
	st.Phase = "none"
	st.WartenMin, st.RestMin = -1, -1
	if b == nil {
		st.Hint = "kein Bundle"
		return st
	}
	st.Ship = b.Ship.Name
	st.Reg = b.Ship.RegistrationNumber
	st.ShipID = b.Ship.ID
	st.HatBordbuch = b.HatBordbuch
	tol := b.Thresholds.ToleranceKmh
	if tol <= 0 {
		tol = tolDefault
	}
	offLimit := b.Thresholds.OffCourseM
	if offLimit <= 0 {
		offLimit = 50
	}
	if !b.HatBordbuch || len(b.Legs) == 0 {
		st.Hint = "kein Bordbuch für heute"
		return st
	}
	nowMin := float64(now.Hour()*60+now.Minute()) + float64(now.Second())/60

	// offene Abschnitte
	type cand struct {
		leg  Leg
		dep  int
		arr  int
		rang float64
	}
	var offen, running, upcoming []cand
	for _, l := range b.Legs {
		if l.Done {
			continue
		}
		dep, arr := hhmmToMin(l.Dep), hhmmToMin(l.Arr)
		c := cand{leg: l, dep: dep, arr: arr}
		if dep >= 0 {
			c.rang = absf(float64(dep) - nowMin)
		} else {
			c.rang = 1e9
		}
		offen = append(offen, c)
		if dep >= 0 && arr >= 0 && float64(dep) <= nowMin && nowMin <= float64(arr) {
			running = append(running, c)
		} else if dep >= 0 && float64(dep) > nowMin {
			upcoming = append(upcoming, c)
		}
	}
	if len(offen) == 0 {
		st.Phase = "beendet"
		st.Hint = "Dienst beendet — alle Halte erreicht"
		return st
	}
	pool := offen
	if len(running) > 0 {
		pool = running
	} else if len(upcoming) > 0 {
		pool = upcoming
	}
	best := pool[0]
	for _, c := range pool {
		if c.rang < best.rang {
			best = c
		}
	}
	leg := best.leg
	st.From, st.To = leg.From, leg.To
	st.Soll = leg.Arr
	st.Abfahrt = leg.Dep

	phase := "unterwegs"
	if best.dep >= 0 && float64(best.dep) > nowMin {
		phase = "vor_abfahrt"
	}
	st.Phase = phase

	if phase == "vor_abfahrt" {
		if best.dep >= 0 {
			st.WartenMin = int(float64(best.dep) - nowMin + 0.5)
		}
		st.Hint = "Abfahrt " + leg.Dep
		return st
	}

	// unterwegs
	arr := best.arr
	if arr >= 0 && float64(arr) < nowMin {
		arr += 1440
	}
	if arr >= 0 {
		rm := float64(arr) - nowMin
		if rm < 0.5 {
			rm = 0.5
		}
		st.RestMin = int(rm + 0.5)
	}
	if pos.Valid && len(leg.Geometry) >= 2 {
		pr := projectOntoPolyline(leg.Geometry, pos.Lat, pos.Lng)
		if pr.OK {
			st.RemainKm = round2(pr.RemainKm)
			st.OffCourseM = float64(int(pr.DistToLineKm*1000 + 0.5))
			if st.OffCourseM <= offLimit {
				st.OffState = "ok"
			} else if st.OffCourseM <= offLimit*2 {
				st.OffState = "warn"
			} else {
				st.OffState = "alarm"
			}
		}
	}
	if pos.Valid {
		st.IstKmh = round1(pos.SpeedKmh)
	}
	if st.RemainKm > 0 && st.RestMin > 0 {
		soll := requiredSpeedKmh(st.RemainKm, float64(st.RestMin))
		st.SollKmh = round1(soll)
		if pos.Valid {
			v, diff := speedVerdict(pos.SpeedKmh, soll, tol)
			st.Verdict = v
			st.DiffKmh = round1(diff)
		}
	}
	return st
}

// Tick: Status neu rechnen, SIFA-Zustandsmaschine fahren, Ausgänge setzen, Ereignisse melden.
func (e *Engine) Tick(now time.Time) {
	e.mu.Lock()
	cfg := e.cfg
	b := e.bundle
	pos := e.pos
	kai := e.kai

	st := computeAdvisory(b, pos, now, 1.5)
	if pos.Valid {
		st.PosValid = true
		st.Lat, st.Lng = pos.Lat, pos.Lng
		st.PosAgeSec = int(now.Sub(pos.TS).Seconds())
	}
	if !e.bundleAt.IsZero() {
		st.BundleAgeSec = int(now.Sub(e.bundleAt).Seconds())
	}
	camHazard := now.Before(e.camHazardUntil)
	st.CameraHazard = camHazard

	// Ursache (Fusion je nach Kombi)
	offAlarm := st.OffState == "alarm"
	var cause bool
	switch cfg.SIFA.TriggerCombo {
	case "gps":
		cause = offAlarm
	case "camera":
		cause = camHazard
	case "both":
		cause = offAlarm && camHazard
	default: // either
		cause = offAlarm || camHazard
	}

	reAlarm := cfg.SIFA.ReAlarmSec
	if reAlarm <= 0 {
		reAlarm = 15
	}
	eventRising, eventCleared := false, false
	if cause {
		switch e.alarmState {
		case "normal":
			e.alarmState = "alarm"
			eventRising = true
		case "acked":
			if now.Sub(e.lastAck) >= time.Duration(reAlarm)*time.Second {
				e.alarmState = "alarm" // Wiederholung, bis geklärt
			}
		}
	} else if e.alarmState != "normal" {
		e.alarmState = "normal"
		eventCleared = true
	}
	eventHorn := e.alarmState == "alarm"
	eventLamp := e.alarmState == "alarm" || e.alarmState == "acked"

	// Totmann-SIFA
	deadmanHorn, deadmanLamp := false, false
	if cfg.SIFA.DeadmanOn {
		sec := cfg.SIFA.DeadmanSec
		if sec <= 0 {
			sec = 30
		}
		pre := cfg.SIFA.DeadmanPrewarnSec
		elapsed := now.Sub(e.lastDeadmanAck).Seconds()
		if elapsed >= float64(sec) {
			deadmanHorn, deadmanLamp = true, true
		} else if elapsed >= float64(sec-pre) {
			deadmanLamp = true
		}
		st.SifaDeadman = deadmanHorn
	}

	horn := eventHorn || deadmanHorn || now.Before(e.testHornUntil)
	lamp := eventLamp || deadmanLamp || now.Before(e.testLampUntil)
	st.Alarm = eventHorn
	st.AlarmState = e.alarmState
	st.Horn, st.Lamp = horn, lamp
	st.UpdatedAt = now.Format(time.RFC3339)
	e.status = st
	gpio := e.gpio
	hornPin, lampPin := cfg.GPIO.HornPin, cfg.GPIO.LampPin
	ackPin := cfg.GPIO.AckPin
	// Snapshot-Bedarf (nur beim Aufsteigen + Kamera)
	wantSnapshot := eventRising && cfg.Camera.Enabled && cfg.Camera.SnapshotURL != ""
	snapURL := cfg.Camera.SnapshotURL
	reasonDetail := alarmReason(st, camHazard)
	e.mu.Unlock()

	// Ausgänge setzen
	gpio.SetOutput(hornPin, horn)
	gpio.SetOutput(lampPin, lamp)

	// Quittier-Taster (steigende Flanke)
	if ackPin >= 0 {
		pressed, _ := gpio.ReadInput(ackPin)
		if pressed && !e.prevAck {
			e.Ack()
		}
		e.prevAck = pressed
	}

	// Ereignisse an KAI (best effort, asynchron)
	if kai != nil {
		if eventRising {
			snap := ""
			if wantSnapshot {
				snap = fetchSnapshotB64(snapURL)
			}
			go func(p Position, off float64, d, sn string) {
				if err := kai.PostEvent("alarm", "alarm", d, p, off, sn); err != nil {
					log.Printf("edge-event (alarm): %v", err)
				}
			}(pos, st.OffCourseM, reasonDetail, snap)
		} else if eventCleared {
			go func(p Position) {
				if err := kai.PostEvent("status", "ok", "Kurs wieder geklärt", p, 0, ""); err != nil {
					log.Printf("edge-event (clear): %v", err)
				}
			}(pos)
		}
	}
}

func alarmReason(st Status, cam bool) string {
	parts := []string{}
	if st.OffState == "alarm" {
		parts = append(parts, fmt.Sprintf("Kursabweichung %.0f m", st.OffCourseM))
	}
	if cam {
		parts = append(parts, "Kamera-Hinweis")
	}
	if len(parts) == 0 {
		return "Alarm"
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += " + " + p
	}
	return out
}

func (e *Engine) Status() Status {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.status
}

func fetchSnapshotB64(u string) string {
	cl := &http.Client{Timeout: 8 * time.Second}
	resp, err := cl.Get(u)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return ""
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024))
	return base64.StdEncoding.EncodeToString(b)
}

func absf(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
func round1(x float64) float64 { return float64(int(x*10+signhalf(x))) / 10 }
func round2(x float64) float64 { return float64(int(x*100+signhalf(x))) / 100 }
func signhalf(x float64) float64 {
	if x < 0 {
		return -0.5
	}
	return 0.5
}
