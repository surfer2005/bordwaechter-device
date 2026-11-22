package main

// Rolle Anzeigerwächter (Peter 2026-11-22): EIN zentrales KAI-Bordgerät im Firmennetz, das die Zielanzeiger und Sprachboxen
// (Asset Manager in KAI, z. B. IB-ZIEL-V1 / IB-TTS-V1) laufend anpingt und ihnen die Telegramme aus KAI weiterreicht.
// Keine eigene Software: dieselbe Gerätesoftware wie Bordwächter/Bildschirm/Bordmusik, die Rolle kommt aus KAI (Kopplung +
// Herzschlag). Die Verbindung baut immer das GERÄT zu KAI auf (nur ausgehend) — KAI spricht nie ins Firmennetz.
//
// In dieser Rolle laufen GPS, SIFA, Hupe, Smartmeter, Sensoren, Licht und Anzeige NICHT. Es laufen: Herzschlag, Auftrags-
// Nachfrage (Neustart/Herunterfahren/Aktualisieren aus KAI), Geräte-Protokoll, Gerätezustand, Anpingen der Zielgeräte (über
// die vorhandene Ping-Senke von KAI, gleiche Schnittstelle wie der Netz-Scan der Bordwächter) und der Telegramm-Kanal:
//   KAI legt Telegramme ab → der Wächter holt sie sofort ab (Long-Poll, wartet bis 25 s; Rückfall: alle 10 s) → schickt
//   sie je nach Sendeweg aus KAI (aus = nur protokollieren | HTTP POST | TCP, mit Zeitlimit) → meldet Antwort/Fehler zurück.
// ⚠️ Das echte Telegramm-Format, der Port und die Quittung der Geräte (Nause / IB Datentechnik) sind noch offen — hier ist
// nichts davon geraten: ohne in KAI eingetragenen Sendeweg wird nur protokolliert, ein Port steht nirgends vor.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const rolleAnzeiger = "anzeigerwaechter"

func istAnzeigerRolle(r string) bool { return r == rolleAnzeiger }

// istKaiRolle: Rollen, die KAI vergibt und die nicht „Bordwächter“ sind — ein so gekoppeltes Gerät gilt ab der Kopplung als
// aus KAI gesteuert (die Konfig-Seite am Gerät ändert dann nur KAI-Adresse und Netzwerk) und hat kein GPS/SIFA.
func istKaiRolle(r string) bool { return istMedienRolle(r) || istAnzeigerRolle(r) }

// rolleNachHerzschlag: Rolle am Gerät nach der Angabe aus KAI — nur Anzeigerwächter ↔ Bordwächter (Bildschirm/Bordmusik regelt
// medienAusHerzschlag). Liefert die neue Rolle und ob sie wechselt. Rein.
func rolleNachHerzschlag(ist, kai string) (string, bool) {
	switch {
	case kai == "":
		return ist, false // ältere KAI ohne Angabe: nichts ändern
	case istAnzeigerRolle(kai) && !istAnzeigerRolle(ist):
		return rolleAnzeiger, true
	case istAnzeigerRolle(ist) && kai == "bordwaechter":
		return "", true
	}
	return ist, false
}

// anzeigerRolleAusHerzschlag übernimmt die Rolle Anzeigerwächter aus dem Herzschlag (oder gibt sie ab) und startet den Dienst
// neu, damit die passenden Teile laufen.
func (a *App) anzeigerRolleAusHerzschlag(hr *HelloResp) {
	if hr == nil {
		return
	}
	cfg := a.getCfg()
	neu, wechsel := rolleNachHerzschlag(cfg.Rolle, hr.Rolle)
	if !wechsel {
		return
	}
	n := *cfg
	n.Rolle = neu
	if err := saveConfig(&n); err != nil {
		log.Printf("anzeiger: Rolle aus KAI nicht gespeichert: %v", err)
		return
	}
	a.mu.Lock()
	a.cfg = &n
	a.mu.Unlock()
	logEvent("info", "Rolle aus KAI: "+rolleName(neu)+" — Dienst startet neu")
	reexecSoon()
}

// anzeigerStart: was in der Rolle Anzeigerwächter läuft (aus main()).
func (a *App) anzeigerStart() {
	go a.sysLoop()
	go a.auftragLoop()
	go a.heartbeatLoop()
	go a.devlogLoop()
	go a.releaseCheckLoop()
	go a.anzeigerPingLoop()
	go a.anzeigerTelegrammLoop()
}

// ---------------------------------------------------------------------------------------------
// Anpingen: die Zielgeräte (scan-targets liefert dem Anzeigerwächter genau diese) laufend prüfen und über die Ping-Senke
// melden (ha-connectivity) — derselbe Weg wie beim Netz-Scan der Bordwächter, aber OHNE Durchsuchen des eigenen Netzes:
// der Anzeigerwächter steht im Firmennetz, dessen übrige Geräte prüft weiter der zuständige Bordwächter.
// ---------------------------------------------------------------------------------------------

func (a *App) anzeigerPingLoop() {
	time.Sleep(15 * time.Second) // Netz erst hochkommen lassen
	for {
		cfg := a.getCfg()
		k := a.getKai()
		if a.getReportScan() && k != nil && strings.TrimSpace(cfg.EdgeToken) != "" {
			if err := meldeKonnektivitaet(k, targetedPings(k)); err != nil {
				log.Printf("anzeiger: anpingen: %v", err)
			}
		}
		time.Sleep(time.Duration(a.getScanInterval()) * time.Second) // Takt aus KAI (Vorgabe 60 s, mindestens 30 s)
	}
}

// ---------------------------------------------------------------------------------------------
// Telegramme
// ---------------------------------------------------------------------------------------------

type anzZiel struct {
	AssetID int    `json:"asset_id"`
	Name    string `json:"name"`
	IP      string `json:"ip"`
	Weg     string `json:"weg"` // aus | http | tcp (aus KAI, je Zielgerät)
	Port    int    `json:"port"`
	Pfad    string `json:"pfad"`
	// Content-Type der HTTP-Anfrage aus KAI (ab 0.1.148), leer = application/xml; charset=utf-8
	ContentType string `json:"content_type"`
}

const anzContentTypeVorgabe = "application/xml; charset=utf-8"

// anzContentType: eingestellter Content-Type, nur druckbare Zeichen (nie ein Zeilenumbruch in der Kopfzeile), sonst Vorgabe.
func anzContentType(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 120 || !strings.Contains(s, "/") {
		return anzContentTypeVorgabe
	}
	for _, r := range s {
		if r < 0x20 || r > 0x7e {
			return anzContentTypeVorgabe
		}
	}
	return s
}

type anzTelegramm struct {
	ID     int     `json:"id"`
	Art    string  `json:"art"` // zielanzeiger | durchsage
	Test   bool    `json:"test"`
	Inhalt string  `json:"inhalt"`
	Ziel   anzZiel `json:"ziel"`
}

type anzAbruf struct {
	Stand      string         `json:"stand"`
	Zustaendig bool           `json:"zustaendig"`
	Telegramme []anzTelegramm `json:"telegramme"`
}

type anzErgebnis struct {
	ID      int    `json:"id"`
	Status  string `json:"status"` // gesendet | protokolliert | fehler
	Antwort string `json:"antwort,omitempty"`
	Fehler  string `json:"fehler,omitempty"`
	DauerMs int64  `json:"dauer_ms"`
}

// Zeitlimits beim Senden (variabel nur für die Tests)
var (
	anzVerbindenZeit = 5 * time.Second // Verbindungsaufbau
	anzGesamtZeit    = 8 * time.Second // ganze Übertragung
	anzAntwortZeit   = 2 * time.Second // TCP: so lange auf eine Antwort/Quittung warten
	anzAntwortMax    = 4000            // Zeichen der Antwort, die an KAI zurückgeht
	anzWarten        = 25              // Sekunden, die KAI einen Abruf offen hält
	anzRueckfall     = 10 * time.Second
)

// eigener Client für den Abruf: KAI hält ihn bis 25 s offen (der normale Client bricht nach 20 s ab)
var anzAbrufClient = &http.Client{Timeout: 40 * time.Second}

func (k *KaiClient) AnzeigerAbholen(stand string, warten int) (*anzAbruf, error) {
	p := fmt.Sprintf("/api/edge/anzeiger/telegramme?warten=%d&stand=%s", warten, url.QueryEscape(stand))
	data, code, err := k.doMit(anzAbrufClient, "GET", p, nil)
	if err != nil {
		return nil, err
	}
	if code != 200 {
		return nil, fmt.Errorf("telegramme http %d", code)
	}
	var r anzAbruf
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func (k *KaiClient) AnzeigerErgebnis(e anzErgebnis) error {
	_, code, err := k.do("POST", "/api/edge/anzeiger/ergebnis", e)
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("ergebnis http %d", code)
	}
	return nil
}

// anzeigerTelegrammLoop: abholen (Long-Poll) → senden → Ergebnis melden. Fällt der Abruf aus (Netz, ältere KAI), wird alle
// 10 s erneut gefragt.
func (a *App) anzeigerTelegrammLoop() {
	stand := ""
	for {
		k := a.getKai()
		if k == nil || strings.TrimSpace(a.getCfg().EdgeToken) == "" {
			time.Sleep(anzRueckfall)
			continue
		}
		t0 := time.Now()
		r, err := k.AnzeigerAbholen(stand, anzWarten)
		if err != nil {
			gedrosseltMelden("anzeiger-abruf", 30*time.Minute, "warn", "Telegramme aus KAI nicht abrufbar: "+err.Error())
			time.Sleep(anzRueckfall)
			continue
		}
		stand = r.Stand
		if !r.Zustaendig {
			// ein zweiter Anzeigerwächter steht bereit, zuständig ist ein anderer (KAI: der älteste aktive, gekoppelte)
			gedrosseltMelden("anzeiger-zustaendig", 6*time.Hour, "info", "Nicht zuständig — KAI schickt die Telegramme an einen anderen Anzeigerwächter")
			time.Sleep(anzRueckfall)
			continue
		}
		for _, t := range r.Telegramme {
			e := telegrammSenden(t)
			if e.Status == "fehler" {
				gedrosseltMelden("anzeiger-fehler-"+t.Ziel.Name, 30*time.Minute, "warn", fmt.Sprintf("Telegramm %d an %s (%s) fehlgeschlagen: %s", t.ID, t.Ziel.Name, t.Ziel.IP, e.Fehler))
			} else if t.Test {
				logEvent("info", fmt.Sprintf("Testtelegramm %d an %s (%s): %s", t.ID, t.Ziel.Name, t.Ziel.IP, ergebnisText(e)))
			}
			if err := k.AnzeigerErgebnis(e); err != nil {
				log.Printf("anzeiger: Ergebnis %d nicht gemeldet: %v", t.ID, err)
			}
		}
		// Kam der Abruf ohne Telegramm sofort zurück (z. B. Weckruf ohne neues Telegramm), nicht im Kreis fragen
		if len(r.Telegramme) == 0 && time.Since(t0) < time.Second {
			time.Sleep(time.Second)
		}
	}
}

func ergebnisText(e anzErgebnis) string {
	switch e.Status {
	case "gesendet":
		return "gesendet"
	case "protokolliert":
		return "nur protokolliert (Sendeweg in KAI: aus)"
	}
	return "Fehler: " + e.Fehler
}

// anzPrivateIP: nur 10/8, 172.16/12, 192.168/16 — der Anzeigerwächter schickt nie ins Internet (prüft KAI ebenso).
// anzIPErlaubt ist die benutzte Prüfung (in den Tests auf 127.0.0.1 erweitert).
var anzIPErlaubt = anzPrivateIP

func anzPrivateIP(ip string) bool {
	p := net.ParseIP(strings.TrimSpace(ip))
	if p == nil || p.To4() == nil {
		return false
	}
	return p.IsPrivate()
}

// telegrammSenden schickt ein Telegramm auf dem Sendeweg aus KAI. Rein bis auf die Netzwerk-Anfrage selbst.
func telegrammSenden(t anzTelegramm) anzErgebnis {
	t0 := time.Now()
	e := anzErgebnis{ID: t.ID}
	fertig := func(status, antwort, fehler string) anzErgebnis {
		e.Status, e.Antwort, e.Fehler = status, kuerzen(antwort, anzAntwortMax), kuerzen(fehler, 1000)
		e.DauerMs = time.Since(t0).Milliseconds()
		return e
	}
	z := t.Ziel
	if !anzIPErlaubt(z.IP) {
		return fertig("fehler", "", "IP "+z.IP+" liegt nicht im Firmennetz — nicht gesendet")
	}
	switch z.Weg {
	case "", "aus":
		return fertig("protokolliert", "nur protokolliert — Sendeweg in KAI steht auf „aus“", "")
	case "http":
		if z.Port <= 0 {
			return fertig("fehler", "", "Port fehlt")
		}
		pfad := z.Pfad
		if pfad == "" {
			pfad = "/"
		}
		if !strings.HasPrefix(pfad, "/") {
			pfad = "/" + pfad
		}
		adr := "http://" + net.JoinHostPort(z.IP, strconv.Itoa(z.Port)) + pfad
		req, err := http.NewRequest("POST", adr, bytes.NewReader([]byte(t.Inhalt)))
		if err != nil {
			return fertig("fehler", "", err.Error())
		}
		req.Header.Set("Content-Type", anzContentType(z.ContentType))
		cl := &http.Client{
			Timeout:       anzGesamtZeit,
			Transport:     &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: anzVerbindenZeit}).DialContext},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, // nie weiterleiten lassen
		}
		resp, err := cl.Do(req)
		if err != nil {
			return fertig("fehler", "", err.Error())
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, int64(anzAntwortMax)))
		antwort := fmt.Sprintf("HTTP %d", resp.StatusCode)
		if s := strings.TrimSpace(string(b)); s != "" {
			antwort += " · " + s
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return fertig("fehler", antwort, fmt.Sprintf("Gerät antwortet HTTP %d", resp.StatusCode))
		}
		return fertig("gesendet", antwort, "")
	case "tcp":
		if z.Port <= 0 {
			return fertig("fehler", "", "Port fehlt")
		}
		c, err := net.DialTimeout("tcp", net.JoinHostPort(z.IP, strconv.Itoa(z.Port)), anzVerbindenZeit)
		if err != nil {
			return fertig("fehler", "", err.Error())
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(anzGesamtZeit))
		if _, err := c.Write([]byte(t.Inhalt)); err != nil {
			return fertig("fehler", "", err.Error())
		}
		// Antwort/Quittung lesen, soweit das Gerät eine schickt — ob und wie es quittiert, ist noch offen (Nause/IB)
		_ = c.SetReadDeadline(time.Now().Add(anzAntwortZeit))
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, io.LimitReader(c, int64(anzAntwortMax)))
		antwort := strings.TrimSpace(buf.String())
		if antwort == "" {
			antwort = fmt.Sprintf("gesendet (%d Bytes), keine Antwort innerhalb %d s", len(t.Inhalt), int(anzAntwortZeit.Seconds()))
		}
		return fertig("gesendet", antwort, "")
	}
	return fertig("fehler", "", "unbekannter Sendeweg „"+z.Weg+"“")
}

func kuerzen(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
