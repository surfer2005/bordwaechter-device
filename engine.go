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
	Verdict      string  `json:"verdict"` // ok | faster | slower | ankunft | late | none
	DiffKmh      float64 `json:"diff_kmh"`
	DelayMin     int     `json:"delay_min"`
	OffCourseM   float64 `json:"off_course_m"`
	OffLimitM    float64 `json:"off_course_limit_m"` // „Grenze" (streckenadaptiv) für die Anzeige
	OffState     string  `json:"off_state"`          // ok | warn | alarm | unknown
	MaxKmh       float64 `json:"max_kmh"`
	Alarm        bool    `json:"alarm"`
	AlarmState   string  `json:"alarm_state"` // normal | alarm | acked
	SifaDeadman  bool    `json:"sifa_deadman_active"`
	Lat          float64 `json:"lat"`
	Lng          float64 `json:"lng"`
	Heading      float64 `json:"heading"` // Kurs über Grund (°), aus der Bewegung abgeleitet
	FromLat      float64      `json:"from_lat"`
	FromLng      float64      `json:"from_lng"`
	ToLat        float64      `json:"to_lat"`
	ToLng        float64      `json:"to_lng"`
	RouteGeom        [][2]float64 `json:"route_geom"`         // [lon,lat] des aktuellen Legs (für die Karte)
	RouteGeomReverse [][2]float64 `json:"route_geom_reverse"` // Gegenrichtung (blasse Linie)
	PosAgeSec    int     `json:"pos_age_sec"`
	PosValid     bool    `json:"pos_valid"`
	CameraHazard bool    `json:"camera_hazard"`
	CamConfigured bool   `json:"cam_configured"` // Snapshot-URL hinterlegt → Live-Bild möglich
	HatBordbuch  bool    `json:"hat_bordbuch"`
	BundleAgeSec int     `json:"bundle_age_sec"`
	Hint         string  `json:"hint"`
	NextLogbookHint bool `json:"next_logbook_hint"` // am Ziel + kein weiteres Leg → Bordbuch für nächste Fahrt
	Horn         bool    `json:"horn"`
	Lamp         bool    `json:"lamp"`
	Version      string  `json:"version"`
	UpdatedAt    string  `json:"updated_at"`
}

type Engine struct {
	mu   sync.Mutex
	cfg  *Config
	kai  *KaiClient
	gpio GPIO

	bundle   *Bundle
	bundleAt time.Time
	pos         Position
	lastPos     Position
	lastHeading float64 // zuletzt abgeleiteter Kurs (wird bei Stillstand gehalten)

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
	// Kurs: Teltonika hat KEIN Heading-Register → aus der Bewegung ableiten (Peilung). Bei zu wenig
	// Bewegung (GPS-Zittern im Stand) den zuletzt bekannten Kurs halten statt wild zu springen.
	if e.cfg != nil && e.cfg.GPS.HeadingReg < 0 && e.lastPos.Valid && p.Valid {
		if haversineKm(e.lastPos.Lat, e.lastPos.Lng, p.Lat, p.Lng) > 0.004 { // > ~4 m
			p.Heading = bearingDeg(e.lastPos.Lat, e.lastPos.Lng, p.Lat, p.Lng)
			e.lastHeading = p.Heading
		} else {
			p.Heading = e.lastHeading
		}
	}
	if p.Valid {
		e.lastPos = p
	}
	e.pos = p
}

// CurrentPos: die zuletzt gesetzte, mit abgeleiteter Geschwindigkeit ANGEREICHERTE Position.
// Wichtig fürs Posten an KAI — die rohe Position aus der Quelle trägt oft Speed 0.
func (e *Engine) CurrentPos() Position {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.pos
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
	// "Offen" ZEITbasiert, nicht über l.Done: RMS-Bordbücher haben die Ist-Ankunft je Leg vorbefüllt
	// (= Soll), sonst gälten alle Legs sofort als erreicht → den ganzen Tag "Dienst beendet".
	lateGrace := b.Thresholds.LateGraceMin
	if lateGrace <= 0 {
		lateGrace = 120
	}
	var offen, running, upcoming []cand
	for _, l := range b.Legs {
		dep, arr := hhmmToMin(l.Dep), hhmmToMin(l.Arr)
		if arr >= 0 && float64(arr) < nowMin-lateGrace {
			continue // Soll-Ankunft liegt deutlich zurück → erledigt
		}
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
	// Route + Start/Ziel für die Karte (gilt auch „vor Abfahrt").
	if len(leg.Geometry) >= 2 {
		st.RouteGeom = leg.Geometry
		st.RouteGeomReverse = leg.GeometryReverse
		a := leg.Geometry[0]
		z := leg.Geometry[len(leg.Geometry)-1]
		st.FromLng, st.FromLat = a[0], a[1]
		st.ToLng, st.ToLat = z[0], z[1]
	}

	phase := "unterwegs"
	if best.dep >= 0 && float64(best.dep) > nowMin {
		phase = "vor_abfahrt"
	}
	st.Phase = phase

	if phase == "vor_abfahrt" {
		if best.dep >= 0 {
			st.WartenMin = int(float64(best.dep) - nowMin + 0.5)
		}
		// Reststrecke auch VOR der Abfahrt zeigen (wie der KAI-Kiosk) — Projektion der Position auf die Route.
		posStaleS := b.Thresholds.PosStaleS
		if posStaleS <= 0 {
			posStaleS = 300
		}
		if pos.Valid && now.Sub(pos.TS).Seconds() <= posStaleS && len(leg.Geometry) >= 2 {
			if pr := projectOntoPolyline(leg.Geometry, pos.Lat, pos.Lng); pr.OK {
				st.RemainKm = round2(pr.RemainKm)
			}
		}
		st.Hint = "Abfahrt " + leg.Dep
		return st
	}

	// unterwegs
	// Mitternachts-Leg nur, wenn die Ankunft vor der Abfahrt DESSELBEN Legs liegt (nicht bloß, weil
	// die Soll-Zeit schon vorbei ist) — sonst würde eine vergangene Ankunft um 24 h nach vorn gerechnet.
	arr := best.arr
	if arr >= 0 && best.dep >= 0 && arr < best.dep {
		arr += 1440
	}
	if arr >= 0 {
		rm := float64(arr) - nowMin
		if rm < 0.5 {
			rm = 0.5
		}
		st.RestMin = int(rm + 0.5)
	}
	portZoneKm := b.Thresholds.OffCoursePortM / 1000
	if portZoneKm <= 0 {
		portZoneKm = 0.5
	}
	maneuverKmh := b.Thresholds.ManeuverKmh
	if maneuverKmh <= 0 {
		maneuverKmh = 8
	}
	// Kurs-Ampel ZONENBASIERT (wie Server, mit dem Nutzer festgelegt): in Hafennähe eng, auf offener
	// See weit. Werte aus dem Bundle (KAI-Einstellungen), Code-Defaults nur als Notnagel (fabrikneues,
	// nie-online-Gerät). Löst das frühere spread×Faktor ab.
	hafenRadiusM := b.Thresholds.HafenRadiusM
	if hafenRadiusM <= 0 {
		hafenRadiusM = 300
	}
	hafenOrange := b.Thresholds.HafenOrangeM
	if hafenOrange <= 0 {
		hafenOrange = 30
	}
	hafenRot := b.Thresholds.HafenRotM
	if hafenRot <= 0 {
		hafenRot = 50
	}
	seeOrange := b.Thresholds.SeeOrangeM
	if seeOrange <= 0 {
		seeOrange = 700
	}
	seeRot := b.Thresholds.SeeRotM
	if seeRot <= 0 {
		seeRot = 1050
	}
	st.OffLimitM = float64(int(seeOrange + 0.5)) // Vorbelegung; in der Hafen-Zone unten überschrieben
	// Position-Veraltung: ein altes Signal (Ausfall) erzeugt KEINE Live-Bewertung mehr —
	// sonst 0,0 km/h + „Schneller" aus altem Stillstand. Nur eine frische Position gilt als live.
	posStaleS := b.Thresholds.PosStaleS
	if posStaleS <= 0 {
		posStaleS = 300
	}
	posLive := pos.Valid && now.Sub(pos.TS).Seconds() <= posStaleS
	if pos.Valid && !posLive {
		st.Hint = "Position veraltet — keine aktuelle Geschwindigkeit"
	}
	remainKnown := false
	if posLive && len(leg.Geometry) >= 2 {
		pr := projectOntoPolyline(leg.Geometry, pos.Lat, pos.Lng)
		if pr.OK {
			st.RemainKm = round2(pr.RemainKm)
			remainKnown = true
			st.OffCourseM = float64(int(pr.DistToLineKm*1000 + 0.5))
			// Zone nach Abstand zum nächsten Hafen (Anfang/Ende der Linie = Anlegestellen):
			// in der Zone eng (hafenOrange/Rot), draußen weit (seeOrange/Rot). Grenze = Beginn Orange.
			a := leg.Geometry[0]
			z := leg.Geometry[len(leg.Geometry)-1]
			distHarborM := haversineKm(pos.Lat, pos.Lng, a[1], a[0]) * 1000
			if d2 := haversineKm(pos.Lat, pos.Lng, z[1], z[0]) * 1000; d2 < distHarborM {
				distHarborM = d2
			}
			orange, rot := seeOrange, seeRot
			if distHarborM <= hafenRadiusM {
				orange, rot = hafenOrange, hafenRot
			}
			st.OffLimitM = float64(int(orange + 0.5))
			if st.OffCourseM < orange {
				st.OffState = "ok"
			} else if st.OffCourseM < rot {
				st.OffState = "warn"
			} else {
				st.OffState = "alarm"
			}
		}
	}
	if posLive {
		st.IstKmh = round1(pos.SpeedKmh)
	}
	// Überfällige Abfahrt: steht das Schiff noch am Abfahrtssteg (Ist ~0, nah an leg.from) und die
	// Soll-Abfahrt ist vorbei → keine Aufhol-Geschwindigkeit, sondern "wartet am Steg".
	wartetAbfahrt := false
	if posLive && len(leg.Geometry) >= 2 && best.dep >= 0 && float64(best.dep) <= nowMin && pos.SpeedKmh < maneuverKmh {
		a := leg.Geometry[0]
		if haversineKm(pos.Lat, pos.Lng, a[1], a[0]) <= portZoneKm {
			wartetAbfahrt = true
			st.Hint = "Abfahrt überfällig — Schiff wartet am Steg"
		}
	}
	maxKmh := b.Thresholds.MaxKmh
	if maxKmh <= 0 {
		maxKmh = 40
	}
	st.MaxKmh = maxKmh
	arrivalRadiusKm := b.Thresholds.ArrivalRadiusM / 1000
	if arrivalRadiusKm <= 0 {
		arrivalRadiusKm = 0.15
	}
	// Zwei Sicherungen gegen unsinnige Tempo-Werte am Rand (siehe Server kiosk-gps.js):
	//  - praktisch am Ziel → "ankunft", keine Empfehlung
	//  - auch bei Vollgas nicht pünktlich → "late" + Verspätung statt absurder km/h
	if wartetAbfahrt {
		// keine Tempo-Empfehlung, solange überfällig am Steg
	} else if remainKnown && st.RemainKm <= arrivalRadiusKm {
		// „Am Ziel" (steht am Steg) vs. „Gleich am Ziel" (fährt noch an) — wie der Server
		// über das Tempo, nicht über den Restweg (die Linie endet nicht zwingend am Anleger).
		if posLive && pos.SpeedKmh <= 2 {
			st.Verdict = "angekommen"
		} else {
			st.Verdict = "ankunft"
		}
		st.SollKmh = 0
	} else if remainKnown && st.RemainKm > 0 && st.RestMin > 0 {
		soll := requiredSpeedKmh(st.RemainKm, float64(st.RestMin))
		if soll > maxKmh {
			st.Verdict = "late"
			st.SollKmh = 0
			tFull := (st.RemainKm / maxKmh) * 60
			dm := int(tFull - float64(st.RestMin) + 0.5)
			if dm < 0 {
				dm = 0
			}
			st.DelayMin = dm
		} else {
			st.SollKmh = round1(soll)
			if posLive {
				v, diff := speedVerdict(pos.SpeedKmh, soll, tol)
				st.Verdict = v
				st.DiffKmh = round1(diff)
			}
		}
	}
	// Nach Ankunft am LETZTEN Halt (kein weiteres Leg) nach hintMin Minuten: Bordbuch für nächste Fahrt.
	hintMin := b.Thresholds.NextLogbookHintMin
	if hintMin <= 0 {
		hintMin = 5
	}
	if st.Verdict == "angekommen" && len(upcoming) == 0 && best.arr >= 0 && nowMin >= float64(best.arr)+hintMin {
		st.NextLogbookHint = true
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
		st.Heading = pos.Heading
		st.PosAgeSec = int(now.Sub(pos.TS).Seconds())
	}
	if !e.bundleAt.IsZero() {
		st.BundleAgeSec = int(now.Sub(e.bundleAt).Seconds())
	}
	camHazard := now.Before(e.camHazardUntil)
	st.CameraHazard = camHazard
	st.CamConfigured = cfg.Camera.SnapshotURL != ""

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
	case "manuell":
		cause = false // kein automatischer Auslöser — nur die Totmann-/Zeit-Aufforderung (s. u.)
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

	// Totmann-SIFA (auch erzwungen im Auslöser-Modus "manuell" — dann IST der Totmann die SIFA)
	deadmanHorn, deadmanLamp := false, false
	if cfg.SIFA.DeadmanOn || cfg.SIFA.TriggerCombo == "manuell" {
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
	st.Version = version
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
