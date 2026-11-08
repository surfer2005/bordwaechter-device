package main

// Licht über ein Lunatone-DALI-Gateway im Bord-Netz (MS-KONSTANZ, Peter 2026-11-07). Nur der Bordwächter spricht
// mit dem Gateway (keine Anmeldung dort — es bleibt im Bord-Netz). Ablauf:
//  - alle paar Sekunden GET http://<gateway>/devices → Zustand je Leuchte; bei Änderung (sonst minütlich) an KAI,
//  - in derselben Anfrage holt er wartende Schaltbefehle aus KAI ab und quittiert die erledigten,
//  - Lichtplan (Uhrzeit oder Sonnenauf-/-untergang aus der eigenen GPS-Position) läuft am Gerät, auch ohne Netz,
//  - Schlüsselschalter (Web-IO über den MQTT-Empfänger): AUS = Leuchten stromlos → keine Befehle ausführen,
//  - ohne Netz schaltet das Schiffsterminal direkt hier (/api/licht/*, Bedien-Schlüssel aus KAI).

import (
	_ "embed"
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type lichtAktion struct {
	Ziel   string `json:"ziel"` // alle | gruppe | leuchte
	ID     *int   `json:"id,omitempty"`
	An     *bool  `json:"an,omitempty"`
	Lvl    *int   `json:"lvl,omitempty"`
	Kelvin *int   `json:"kelvin,omitempty"`
	RGB    string `json:"rgb,omitempty"`
}
type lichtStimmung struct {
	ID         int           `json:"id"`
	Name       string        `json:"name"`
	ImTerminal bool          `json:"im_terminal"`
	Aktionen   []lichtAktion `json:"aktionen"`
}
type lichtPlanZeile struct {
	ID       int           `json:"id"`
	Tage     string        `json:"tage"`
	Zeit     string        `json:"zeit"`
	Aktionen []lichtAktion `json:"aktionen"`
}
type lichtGruppe struct {
	G          int    `json:"g"`
	Name       string `json:"name"`
	ImTerminal bool   `json:"im_terminal"`
}
type lichtConfig struct {
	Aktiv       bool   `json:"aktiv"`
	Rev         int    `json:"rev"`
	GatewayHost string `json:"gateway_host"`
	PollSek     int    `json:"poll_sek"`
	BedienToken string `json:"bedien_token"`
	Schluessel  *struct {
		Praefix    string `json:"praefix"`
		Schluessel string `json:"schluessel"`
	} `json:"schluessel"`
	Gruppen    []lichtGruppe    `json:"gruppen"`
	Stimmungen []lichtStimmung  `json:"stimmungen"`
	Plan       []lichtPlanZeile `json:"plan"`
	// für die Lichtseite /licht (0.1.113): Leuchtennamen und Reiter wie im Schiffsterminal (kommen aus KAI)
	Leuchten []struct {
		A    int    `json:"a"`
		Name string `json:"name"`
	} `json:"leuchten"`
	Reiter []struct {
		Name     string `json:"name"`
		Elemente []struct {
			Art string `json:"art"`
			ID  int    `json:"id"`
		} `json:"elemente"`
	} `json:"reiter"`
}
type leuchteStand struct {
	A            int     `json:"a"`
	Name         string  `json:"name,omitempty"` // am Display vergebener Name (Lunatone /devices „name“)
	An           bool    `json:"an"`
	Lvl          int     `json:"lvl"`
	Typ          []int   `json:"typ"`
	Gruppen      []int   `json:"gruppen"`
	Verfuegbar   bool    `json:"verfuegbar"`
	LampeDefekt  bool    `json:"lampe_defekt"`
	GeraetDefekt bool    `json:"geraet_defekt"`
	Kelvin       *int    `json:"kelvin,omitempty"`
	RGB          *string `json:"rgb,omitempty"`
	lunatoneID   int
}
type lichtQuittung struct {
	ID     int    `json:"id"`
	OK     bool   `json:"ok"`
	Fehler string `json:"fehler,omitempty"`
}

type Licht struct {
	mu        sync.Mutex
	cfg       lichtConfig
	stand     []leuchteStand
	standJSON string
	gemeldet  time.Time
	gateway   map[string]any
	fehler    string
	quittung  []lichtQuittung
	planLauf  map[string]string // planID → Tag, an dem zuletzt ausgeführt
	letzteKAI time.Time
	hc        *http.Client
}

var licht = &Licht{planLauf: map[string]string{}, hc: &http.Client{Timeout: 3 * time.Second}}

func lichtPfad() string { return filepath.Join(filepath.Dir(configPath()), "licht.json") }

func (l *Licht) laden() {
	if b, err := os.ReadFile(lichtPfad()); err == nil {
		var c lichtConfig
		if json.Unmarshal(b, &c) == nil {
			l.mu.Lock()
			l.cfg = c
			l.mu.Unlock()
		}
	}
}
func (l *Licht) sichern() {
	l.mu.Lock()
	b, _ := json.Marshal(l.cfg)
	l.mu.Unlock()
	_ = os.WriteFile(lichtPfad(), b, 0o600) // enthält den Bedien-Schlüssel
}
func (l *Licht) config() lichtConfig { l.mu.Lock(); defer l.mu.Unlock(); return l.cfg }

// ── Gateway ─────────────────────────────────────────────────────────────────────────────────────────

func (l *Licht) gw(pfad string) string {
	h := strings.TrimRight(l.config().GatewayHost, "/")
	if !strings.HasPrefix(h, "http") {
		h = "http://" + h
	}
	return h + pfad
}

// standAusGeraeten: Lunatone-/devices in den kompakten Stand für KAI übersetzen.
func standAusGeraeten(data []byte) ([]leuchteStand, error) {
	var r struct {
		Devices []struct {
			ID        int                        `json:"id"`
			Name      string                     `json:"name"`
			Address   int                        `json:"address"`
			Available bool                       `json:"available"`
			Groups    []int                      `json:"groups"`
			DaliTypes []int                      `json:"daliTypes"`
			Status    map[string]any             `json:"status"`
			Features  map[string]json.RawMessage `json:"features"`
		} `json:"devices"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	out := []leuchteStand{}
	for _, d := range r.Devices {
		st := func(k string) map[string]any {
			var o map[string]any
			_ = json.Unmarshal(d.Features[k], &o)
			return o
		}
		an, _ := st("switchable")["status"].(bool)
		lvl, _ := st("dimmable")["status"].(float64)
		e := leuchteStand{A: d.Address, Name: strings.TrimSpace(d.Name), Typ: d.DaliTypes, Gruppen: d.Groups, Verfuegbar: d.Available, lunatoneID: d.ID,
			An: an && lvl > 0, Lvl: int(math.Round(lvl))}
		if e.Typ == nil {
			e.Typ = []int{}
		}
		if e.Gruppen == nil {
			e.Gruppen = []int{}
		}
		e.LampeDefekt, _ = d.Status["lampFailure"].(bool)
		e.GeraetDefekt, _ = d.Status["controlGearFailure"].(bool)
		for _, t := range d.DaliTypes {
			if t == 8 { // Farbleuchte: Weißton und Farbe mitmelden
				if k, ok := st("colorKelvin")["status"].(float64); ok {
					ki := int(k)
					e.Kelvin = &ki
				}
				if c, ok := st("colorRGB")["status"].(map[string]any); ok {
					f := func(k string) int { v, _ := c[k].(float64); return int(math.Round(math.Max(0, math.Min(1, v)) * 255)) }
					s := fmt.Sprintf("#%02x%02x%02x", f("r"), f("g"), f("b"))
					e.RGB = &s
				}
			}
		}
		out = append(out, e)
	}
	return out, nil
}

func (l *Licht) abfragen() error {
	resp, err := l.hc.Get(l.gw("/devices"))
	if err != nil {
		return fmt.Errorf("Gateway nicht erreichbar")
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != 200 {
		return fmt.Errorf("Gateway antwortet HTTP %d", resp.StatusCode)
	}
	stand, err := standAusGeraeten(data)
	if err != nil {
		return fmt.Errorf("Gateway-Antwort unlesbar")
	}
	l.mu.Lock()
	l.stand = stand
	l.mu.Unlock()
	return nil
}

func (l *Licht) infoHolen() {
	resp, err := l.hc.Get(l.gw("/info"))
	if err != nil {
		return
	}
	defer resp.Body.Close()
	var info map[string]any
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&info) == nil {
		g := map[string]any{}
		for _, k := range []string{"name", "version", "descriptor", "device"} {
			if v, ok := info[k]; ok {
				g[k] = v
			}
		}
		// Am Display vergebene Namen von Gruppen/Zonen (falls die Firmware die Abfragen kennt) roh mitschicken
		for pfad, k := range map[string]string{"/zones": "zonen", "/groups": "gruppen"} {
			if r2, err := l.hc.Get(l.gw(pfad)); err == nil {
				var v any
				if r2.StatusCode == 200 && json.NewDecoder(io.LimitReader(r2.Body, 256<<10)).Decode(&v) == nil {
					g[k] = v
				}
				r2.Body.Close()
			}
		}
		l.mu.Lock()
		l.gateway = g
		l.mu.Unlock()
	}
}

func (l *Licht) leuchteID(adresse int) (int, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range l.stand {
		if s.A == adresse {
			return s.lunatoneID, true
		}
	}
	return 0, false
}

// steuerSchritte: eine Aktion in die Lunatone-Aufrufe zerlegen (Pfad + Rumpf).
func (l *Licht) steuerSchritte(a lichtAktion) (string, []map[string]any, error) {
	pfad := ""
	switch a.Ziel {
	case "alle":
		pfad = "/broadcast/control"
	case "gruppe":
		if a.ID == nil || *a.ID < 0 || *a.ID > 15 {
			return "", nil, errors.New("Gruppe fehlt")
		}
		pfad = fmt.Sprintf("/group/%d/control", *a.ID)
	case "leuchte":
		if a.ID == nil {
			return "", nil, errors.New("Adresse fehlt")
		}
		id, ok := l.leuchteID(*a.ID)
		if !ok {
			return "", nil, fmt.Errorf("Leuchte A%d unbekannt", *a.ID)
		}
		pfad = fmt.Sprintf("/device/%d/control", id)
	default:
		return "", nil, errors.New("Ziel unbekannt")
	}
	rumpfe := []map[string]any{}
	if a.An != nil && !*a.An {
		rumpfe = append(rumpfe, map[string]any{"switchable": false})
		return pfad, rumpfe, nil
	}
	if a.RGB != "" && len(a.RGB) == 7 {
		v := func(i int) float64 {
			n, _ := strconv.ParseUint(a.RGB[i:i+2], 16, 8)
			return math.Round(float64(n)/255*1000) / 1000
		}
		rumpfe = append(rumpfe, map[string]any{"colorRGB": map[string]float64{"r": v(1), "g": v(3), "b": v(5)}})
	}
	if a.Kelvin != nil {
		rumpfe = append(rumpfe, map[string]any{"colorKelvin": *a.Kelvin})
	}
	if a.Lvl != nil {
		if *a.Lvl == 0 {
			rumpfe = append(rumpfe, map[string]any{"switchable": false})
		} else {
			rumpfe = append(rumpfe, map[string]any{"dimmable": *a.Lvl})
		}
	} else if a.An != nil && *a.An {
		rumpfe = append(rumpfe, map[string]any{"gotoLastActive": true})
	}
	return pfad, rumpfe, nil
}

// schluesselAus: true, wenn der Schlüsselschalter gemeldet AUS steht (dann sind die Leuchten stromlos).
func (l *Licht) schluesselZustand() string {
	c := l.config()
	if c.Schluessel == nil {
		return ""
	}
	v, ok := sensorik.WertVon(c.Schluessel.Praefix, c.Schluessel.Schluessel)
	if !ok {
		return ""
	}
	z := ""
	switch x := v.(type) {
	case bool:
		z = map[bool]string{true: "ein", false: "aus"}[x]
	case float64:
		z = map[bool]string{true: "ein", false: "aus"}[x != 0]
	}
	// Sicherung gegen ein hängendes Web-IO (Peter 2026-11-07): „aus“ gilt nur, wenn das Gateway auch keine
	// Leuchte mit Strom sieht. Sonst „unklar“ — dann wird NICHT gesperrt.
	if z == "aus" && l.leuchtenMitStrom() {
		return "unklar"
	}
	return z
}

func (l *Licht) leuchtenMitStrom() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range l.stand {
		if s.Verfuegbar {
			return true
		}
	}
	return false
}

func (l *Licht) ausfuehren(aktionen []lichtAktion) error {
	// Der Schlüsselschalter ist nur eine Information (Peter 2026-11-08): auch bei „aus“ wird geschaltet —
	// die Steuerung bleibt an, die Befehle gelten als Voreinstellung für die Leuchten.
	fehler := []string{}
	for _, a := range aktionen {
		pfad, rumpfe, err := l.steuerSchritte(a)
		if err != nil {
			fehler = append(fehler, err.Error())
			continue
		}
		for _, r := range rumpfe {
			b, _ := json.Marshal(r)
			resp, err := l.hc.Post(l.gw(pfad), "application/json", bytes.NewReader(b))
			if err != nil {
				fehler = append(fehler, "Gateway nicht erreichbar")
				break
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode >= 300 {
				fehler = append(fehler, fmt.Sprintf("%s: HTTP %d", pfad, resp.StatusCode))
			}
		}
	}
	go func() { time.Sleep(700 * time.Millisecond); _ = l.abfragen() }()
	if len(fehler) > 0 {
		return errors.New(strings.Join(fehler, "; "))
	}
	return nil
}

// ── Lichtplan ───────────────────────────────────────────────────────────────────────────────────────

// planZeitpunkt: wann eine Planzeile am Tag `tag` (Ortszeit) fällig ist.
func planZeitpunkt(zeit string, tag time.Time, lat, lng float64) (time.Time, bool) {
	loc := tag.Location()
	j, m, d := tag.Date()
	if len(zeit) == 5 && zeit[2] == ':' {
		h, e1 := strconv.Atoi(zeit[:2])
		mi, e2 := strconv.Atoi(zeit[3:])
		if e1 != nil || e2 != nil {
			return time.Time{}, false
		}
		return time.Date(j, m, d, h, mi, 0, 0, loc), true
	}
	if len(zeit) >= 3 && (strings.HasPrefix(zeit, "sa") || strings.HasPrefix(zeit, "su")) {
		off, err := strconv.Atoi(zeit[2:])
		if err != nil {
			return time.Time{}, false
		}
		auf, unter, ok := sonnenzeiten(time.Date(j, m, d, 12, 0, 0, 0, loc), lat, lng)
		if !ok {
			return time.Time{}, false
		}
		t := unter
		if strings.HasPrefix(zeit, "sa") {
			t = auf
		}
		return t.Add(time.Duration(off) * time.Minute).In(loc), true
	}
	return time.Time{}, false
}

// wochentag: 1 = Montag … 7 = Sonntag (wie in KAI).
func wochentag(t time.Time) string {
	w := int(t.Weekday())
	if w == 0 {
		w = 7
	}
	return strconv.Itoa(w)
}

// planFaellig: Zeilen, die jetzt laufen sollen (Zeitpunkt erreicht, höchstens 15 Min. her, heute noch nicht gelaufen).
func planFaellig(plan []lichtPlanZeile, jetzt time.Time, lat, lng float64, gelaufen map[string]string) []lichtPlanZeile {
	tag := jetzt.Format("2006-01-02")
	out := []lichtPlanZeile{}
	for _, p := range plan {
		if !strings.Contains(p.Tage, wochentag(jetzt)) {
			continue
		}
		t, ok := planZeitpunkt(p.Zeit, jetzt, lat, lng)
		if !ok || jetzt.Before(t) || jetzt.Sub(t) > 15*time.Minute {
			continue
		}
		k := strconv.Itoa(p.ID) + "|" + p.Zeit
		if gelaufen[k] == tag {
			continue
		}
		out = append(out, p)
	}
	return out
}

var ortszeit = func() *time.Location {
	if l, err := time.LoadLocation("Europe/Berlin"); err == nil {
		return l
	}
	return time.Local
}()

func (a *App) lichtPlanPruefen() {
	c := licht.config()
	if !c.Aktiv || len(c.Plan) == 0 {
		return
	}
	lat, lng := 47.6, 9.4 // Bodensee, falls kein GPS
	if p := a.eng.CurrentPos(); p.Valid && p.Lat != 0 {
		lat, lng = p.Lat, p.Lng
	}
	jetzt := time.Now().In(ortszeit)
	licht.mu.Lock()
	faellig := planFaellig(c.Plan, jetzt, lat, lng, licht.planLauf)
	for _, p := range faellig {
		licht.planLauf[strconv.Itoa(p.ID)+"|"+p.Zeit] = jetzt.Format("2006-01-02")
	}
	licht.mu.Unlock()
	for _, p := range faellig {
		if err := licht.ausfuehren(p.Aktionen); err != nil {
			logEvent("warn", "Lichtplan "+p.Zeit+": "+err.Error())
		} else {
			logEvent("info", "Lichtplan ausgeführt: "+p.Zeit)
		}
	}
}

// ── KAI ─────────────────────────────────────────────────────────────────────────────────────────────

type lichtAntwort struct {
	Aktiv   bool         `json:"aktiv"`
	Rev     int          `json:"rev"`
	Config  *lichtConfig `json:"config"`
	Befehle []struct {
		ID       int           `json:"id"`
		Aktionen []lichtAktion `json:"aktionen"`
	} `json:"befehle"`
}

func (a *App) lichtMitKai(k *KaiClient, fehler string) {
	l := licht
	l.mu.Lock()
	body := map[string]any{"rev": l.cfg.Rev, "erledigt": l.quittung}
	sj, _ := json.Marshal(l.stand)
	if l.cfg.Aktiv && (string(sj) != l.standJSON || time.Since(l.gemeldet) > 60*time.Second || fehler != l.fehler) {
		if fehler == "" && l.stand != nil {
			body["stand"] = map[string]any{"leuchten": l.stand}
			body["gateway"] = l.gateway
		}
		body["fehler"] = fehler
	}
	quittungen := l.quittung
	l.mu.Unlock()
	data, code, err := k.do("POST", "/api/edge/licht", body)
	if err != nil || code != 200 {
		return
	}
	var r lichtAntwort
	if json.Unmarshal(data, &r) != nil {
		return
	}
	l.mu.Lock()
	l.quittung = l.quittung[len(quittungen):]
	if _, ok := body["stand"]; ok {
		l.standJSON = string(sj)
		l.gemeldet = time.Now()
	}
	if _, ok := body["fehler"]; ok {
		l.fehler = fehler
		l.gemeldet = time.Now()
	}
	l.letzteKAI = time.Now()
	neueConfig := r.Config != nil
	if neueConfig {
		l.cfg = *r.Config
	} else {
		l.cfg.Aktiv = r.Aktiv
	}
	l.mu.Unlock()
	if neueConfig {
		l.sichern()
		logEvent("info", fmt.Sprintf("Licht: Einstellungen aus KAI übernommen (Stand %d)", r.Rev))
	}
	for _, b := range r.Befehle {
		e := l.ausfuehren(b.Aktionen)
		q := lichtQuittung{ID: b.ID, OK: e == nil}
		if e != nil {
			q.Fehler = e.Error()
		}
		l.mu.Lock()
		l.quittung = append(l.quittung, q)
		l.mu.Unlock()
	}
}

// lichtLoop: Gateway abfragen, mit KAI abgleichen, Lichtplan prüfen.
func (a *App) lichtLoop() {
	licht.laden()
	time.Sleep(20 * time.Second)
	letzteInfo := time.Time{}
	letzterPlan := time.Time{}
	for {
		c := licht.config()
		fehler := ""
		if c.Aktiv && c.GatewayHost != "" {
			if err := licht.abfragen(); err != nil {
				fehler = err.Error()
			}
			if time.Since(letzteInfo) > 10*time.Minute {
				licht.infoHolen()
				letzteInfo = time.Now()
			}
			if time.Since(letzterPlan) > 20*time.Second {
				a.lichtPlanPruefen()
				letzterPlan = time.Now()
			}
		}
		if k := a.getKai(); k != nil && a.getCfg().EdgeToken != "" {
			a.lichtMitKai(k, fehler)
		}
		pause := c.PollSek
		if pause < 2 {
			pause = 3
		}
		if !c.Aktiv {
			pause = 30 // ohne Licht nur halbminütlich nachfragen, ob es eingeschaltet wurde
		}
		time.Sleep(time.Duration(pause) * time.Second)
	}
}

// ── Bedienung ohne Netz (Schiffsterminal über den KAI-Kiosk, Bedien-Schlüssel aus KAI) ─────────────

func (a *App) lichtDarf(r *http.Request) bool {
	tok := licht.config().BedienToken
	if g := r.URL.Query().Get("token"); tok != "" && g != "" && subtle.ConstantTimeCompare([]byte(g), []byte(tok)) == 1 {
		return true
	}
	return a.authOK(r, true)
}

func (a *App) lichtRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/licht/stand", func(w http.ResponseWriter, r *http.Request) {
		if !a.lichtDarf(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		c := licht.config()
		licht.mu.Lock()
		stand := append([]leuchteStand(nil), licht.stand...)
		licht.mu.Unlock()
		sch := licht.schluesselZustand()
		gruppen := []map[string]any{}
		for _, g := range c.Gruppen {
			if !g.ImTerminal {
				continue
			}
			n, summe, an := 0, 0, 0
			for _, s := range stand {
				for _, x := range s.Gruppen {
					if x == g.G {
						n++
						if s.An {
							an++
							summe += s.Lvl
						}
					}
				}
			}
			lvl := 0
			if an > 0 {
				lvl = summe / an
			}
			gruppen = append(gruppen, map[string]any{"g": g.G, "name": g.Name, "anzahl": n, "an": sch != "aus" && an > 0, "lvl": map[bool]int{true: 0, false: lvl}[sch == "aus"], "stromlos": sch == "aus"})
		}
		stimm := []map[string]any{}
		for _, s := range c.Stimmungen {
			if s.ImTerminal {
				stimm = append(stimm, map[string]any{"id": s.ID, "name": s.Name})
			}
		}
		writeJSON(w, map[string]any{"aktiv": c.Aktiv, "schluessel": sch, "gruppen": gruppen, "stimmungen": stimm, "lokal": true})
	})
	mux.HandleFunc("/api/licht/befehl", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST", http.StatusMethodNotAllowed)
			return
		}
		if !a.lichtDarf(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		var b struct {
			StimmungID int           `json:"stimmung_id"`
			Aktionen   []lichtAktion `json:"aktionen"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10)).Decode(&b) != nil {
			http.Error(w, "ungültig", 400)
			return
		}
		c := licht.config()
		// An der Geräteseite angemeldet (Lichtseite /licht): alle Gruppen, Leuchten und Stimmungen. Mit dem
		// Bedien-Schlüssel (Schiffsterminal ohne Netz): nur, was in KAI fürs Terminal freigegeben ist.
		seite := a.authOK(r, true)
		erlaubt := map[int]bool{}
		for _, g := range c.Gruppen {
			if g.ImTerminal || seite {
				erlaubt[g.G] = true
			}
		}
		aktionen := b.Aktionen
		if b.StimmungID > 0 {
			aktionen = nil
			for _, s := range c.Stimmungen {
				if s.ID == b.StimmungID && (s.ImTerminal || seite) {
					aktionen = s.Aktionen
				}
			}
			if aktionen == nil {
				http.Error(w, "Stimmung nicht freigegeben", http.StatusForbidden)
				return
			}
		} else {
			for _, x := range aktionen {
				if (x.Ziel == "leuchte" && !seite) || (x.Ziel == "gruppe" && (x.ID == nil || !erlaubt[*x.ID])) || (x.Ziel == "alle" && !seite) {
					http.Error(w, "Gruppe nicht freigegeben", http.StatusForbidden)
					return
				}
			}
		}
		if len(aktionen) == 0 {
			http.Error(w, "keine Aktion", 400)
			return
		}
		if err := licht.ausfuehren(aktionen); err != nil {
			w.WriteHeader(http.StatusConflict)
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		if seite {
			logEvent("info", "Licht über die Lichtseite am Gerät geschaltet")
		} else {
			logEvent("info", "Licht ohne Netz vom Schiffsterminal geschaltet")
		}
		writeJSON(w, map[string]any{"ok": true})
	})
}

//go:embed licht.html
var lichtHTML []byte

// Lichtseite am Gerät (Peter 2026-11-08): DALI-Leuchten direkt im Bord-Netz schalten — auch ohne KAI/Internet, z. B. zum
// Prüfen vor Ort. Sieht aus wie das Bordlicht im Schiffsterminal (Kacheln, Reiter, Stimmungen). Nur mit Anmeldung an der
// Geräteseite. Belastung: liest nur den ohnehin abgefragten Stand aus dem Speicher; die Seite fragt höchstens alle 3 s.
func (a *App) lichtSeiteRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/licht", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(lichtHTML)
	})
	mux.HandleFunc("/api/licht/seite", func(w http.ResponseWriter, r *http.Request) {
		if !a.authOK(r, true) {
			http.Error(w, "Anmeldung nötig", http.StatusUnauthorized)
			return
		}
		c := licht.config()
		licht.mu.Lock()
		stand := append([]leuchteStand(nil), licht.stand...)
		licht.mu.Unlock()
		sch := licht.schluesselZustand()
		namen := map[int]string{}
		for _, l := range c.Leuchten {
			namen[l.A] = l.Name
		}
		hat := func(t []int, x int) bool {
			for _, v := range t {
				if v == x {
					return true
				}
			}
			return false
		}
		leuchten := []map[string]any{}
		for _, s := range stand {
			n := namen[s.A]
			if n == "" {
				n = s.Name
			}
			if n == "" {
				n = fmt.Sprintf("Leuchte A%d", s.A)
			}
			leuchten = append(leuchten, map[string]any{"a": s.A, "name": n, "an": s.An && sch != "aus", "lvl": s.Lvl,
				"farbe": hat(s.Typ, 8), "relais": hat(s.Typ, 7) && !hat(s.Typ, 6), "defekt": s.LampeDefekt || s.GeraetDefekt, "verfuegbar": s.Verfuegbar})
		}
		gruppen := []map[string]any{}
		for _, g := range c.Gruppen {
			n, summe, an, farbe := 0, 0, 0, false
			for _, s := range stand {
				if hat(s.Gruppen, g.G) {
					n++
					farbe = farbe || hat(s.Typ, 8)
					if s.An {
						an++
						summe += s.Lvl
					}
				}
			}
			lvl := 0
			if an > 0 {
				lvl = summe / an
			}
			gruppen = append(gruppen, map[string]any{"g": g.G, "name": g.Name, "anzahl": n, "an": sch != "aus" && an > 0, "lvl": lvl, "farbe": farbe})
		}
		stimm := []map[string]any{}
		for _, s := range c.Stimmungen {
			stimm = append(stimm, map[string]any{"id": s.ID, "name": s.Name, "im_terminal": s.ImTerminal})
		}
		writeJSON(w, map[string]any{"aktiv": c.Aktiv, "schluessel": sch, "gruppen": gruppen, "leuchten": leuchten,
			"stimmungen": stimm, "reiter": c.Reiter})
	})
}
