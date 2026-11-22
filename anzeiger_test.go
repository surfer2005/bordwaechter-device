package main

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Rolle Anzeigerwächter (2026-11-22): Rolle aus KAI, Sendewege (aus | HTTP | TCP) mit Zeitlimit, nur Firmennetz.

func TestAnzeigerRolle(t *testing.T) {
	if !istAnzeigerRolle("anzeigerwaechter") || istAnzeigerRolle("bordwaechter") || istAnzeigerRolle("") {
		t.Fatal("istAnzeigerRolle")
	}
	if !istKaiRolle("anzeigerwaechter") || !istKaiRolle("audio") || istKaiRolle("") {
		t.Fatal("istKaiRolle")
	}
	if rolleName("anzeigerwaechter") != "Anzeigerwächter" || rolleFuerKai("anzeigerwaechter") != "anzeigerwaechter" || rolleFuerKai("") != "bordwaechter" {
		t.Fatal("rolleName/rolleFuerKai")
	}
	for _, f := range []struct {
		ist, kai, neu string
		wechsel       bool
	}{
		{"", "anzeigerwaechter", "anzeigerwaechter", true},
		{"anzeigerwaechter", "anzeigerwaechter", "anzeigerwaechter", false},
		{"anzeigerwaechter", "bordwaechter", "", true},
		{"anzeigerwaechter", "", "anzeigerwaechter", false}, // ältere KAI ohne Angabe
		{"bildschirm", "audio", "bildschirm", false},        // regelt medienAusHerzschlag
		{"", "bordwaechter", "", false},
	} {
		neu, w := rolleNachHerzschlag(f.ist, f.kai)
		if neu != f.neu || w != f.wechsel {
			t.Errorf("rolleNachHerzschlag(%q,%q) = %q,%v, erwartet %q,%v", f.ist, f.kai, neu, w, f.neu, f.wechsel)
		}
	}
}

// Speichern der Konfig-Seite verwirft die Rolle nicht (wie bei Bildschirm/Bordmusik)
func TestAnzeigerRolleBleibtBeimSpeichern(t *testing.T) {
	cur := defaultConfig()
	cur.EdgeToken, cur.Rolle = "tok", "anzeigerwaechter"
	neu := *defaultConfig()
	neu.KaiBaseURL = "https://kai.example"
	n, gesperrt := sperreAusKai(cur, neu)
	if !gesperrt || n.Rolle != "anzeigerwaechter" || n.KaiBaseURL != "https://kai.example" {
		t.Fatalf("Rolle verloren oder Adresse nicht übernommen: %+v gesperrt=%v", n.Rolle, gesperrt)
	}
}

func TestAnzeigerNurFirmennetz(t *testing.T) {
	for ip, ok := range map[string]bool{"192.168.62.27": true, "10.1.2.3": true, "172.16.0.1": true, "172.32.0.1": false, "8.8.8.8": false, "127.0.0.1": false, "": false, "DHCP": false} {
		if anzPrivateIP(ip) != ok {
			t.Errorf("anzPrivateIP(%q) != %v", ip, ok)
		}
	}
	r := telegrammSenden(anzTelegramm{ID: 1, Inhalt: "<x/>", Ziel: anzZiel{IP: "8.8.8.8", Weg: "http", Port: 80}})
	if r.Status != "fehler" || !strings.Contains(r.Fehler, "nicht im Firmennetz") {
		t.Fatalf("öffentliche IP wurde nicht abgewiesen: %+v", r)
	}
}

func lokalErlauben(t *testing.T) {
	alt := anzIPErlaubt
	anzIPErlaubt = func(ip string) bool { return ip == "127.0.0.1" || anzPrivateIP(ip) }
	t.Cleanup(func() { anzIPErlaubt = alt })
}

func hostPort(t *testing.T, adr string) (string, int) {
	h, p, err := net.SplitHostPort(adr)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := strconv.Atoi(p)
	return h, n
}

func TestAnzeigerNurProtokollieren(t *testing.T) {
	for _, weg := range []string{"", "aus"} {
		r := telegrammSenden(anzTelegramm{ID: 2, Inhalt: "<x/>", Ziel: anzZiel{IP: "192.168.62.27", Weg: weg}})
		if r.Status != "protokolliert" || r.ID != 2 {
			t.Fatalf("Sendeweg %q: %+v", weg, r)
		}
	}
	if r := telegrammSenden(anzTelegramm{ID: 3, Ziel: anzZiel{IP: "192.168.62.27", Weg: "udp", Port: 1}}); r.Status != "fehler" {
		t.Fatalf("unbekannter Sendeweg: %+v", r)
	}
	if r := telegrammSenden(anzTelegramm{ID: 4, Ziel: anzZiel{IP: "192.168.62.27", Weg: "tcp"}}); r.Status != "fehler" || r.Fehler != "Port fehlt" {
		t.Fatalf("TCP ohne Port: %+v", r)
	}
}

func TestAnzeigerHTTP(t *testing.T) {
	lokalErlauben(t)
	var pfad, art, inhalt string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pfad, art = r.URL.Path, r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		inhalt = string(b)
		switch r.URL.Path {
		case "/kaputt":
			http.Error(w, "nein", 500)
		case "/weiter":
			http.Redirect(w, r, "http://example.org/", http.StatusFound)
		default:
			_, _ = w.Write([]byte("<quittung ok=\"ja\"/>"))
		}
	}))
	defer srv.Close()
	h, p := hostPort(t, srv.Listener.Addr().String())
	r := telegrammSenden(anzTelegramm{ID: 5, Inhalt: "<zielanzeiger test=\"ja\"/>", Ziel: anzZiel{IP: h, Weg: "http", Port: p, Pfad: "xml"}})
	if r.Status != "gesendet" || pfad != "/xml" || !strings.HasPrefix(art, "application/xml") || inhalt != "<zielanzeiger test=\"ja\"/>" {
		t.Fatalf("HTTP: %+v pfad=%q art=%q inhalt=%q", r, pfad, art, inhalt)
	}
	if r.Antwort != "HTTP 200 · <quittung ok=\"ja\"/>" {
		t.Fatalf("Antwort: %q", r.Antwort)
	}
	if r := telegrammSenden(anzTelegramm{ID: 6, Inhalt: "x", Ziel: anzZiel{IP: h, Weg: "http", Port: p, Pfad: "/kaputt"}}); r.Status != "fehler" || !strings.Contains(r.Fehler, "500") {
		t.Fatalf("HTTP 500: %+v", r)
	}
	if r := telegrammSenden(anzTelegramm{ID: 7, Inhalt: "x", Ziel: anzZiel{IP: h, Weg: "http", Port: p, Pfad: "/weiter"}}); r.Status != "fehler" || !strings.Contains(r.Fehler, "302") {
		t.Fatalf("Weiterleitung wurde befolgt: %+v", r)
	}
}

func TestAnzeigerTCP(t *testing.T) {
	lokalErlauben(t)
	alt := anzAntwortZeit
	anzAntwortZeit = 300 * time.Millisecond
	defer func() { anzAntwortZeit = alt }()

	// Gerät quittiert
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	empfangen := make(chan string, 2)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_ = c.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
				b, _ := io.ReadAll(bufio.NewReader(c))
				empfangen <- string(b)
				if strings.Contains(string(b), "quittieren") {
					_, _ = c.Write([]byte("ACK"))
				}
			}(c)
		}
	}()
	h, p := hostPort(t, ln.Addr().String())
	r := telegrammSenden(anzTelegramm{ID: 8, Inhalt: "<durchsage>quittieren</durchsage>", Ziel: anzZiel{IP: h, Weg: "tcp", Port: p}})
	if r.Status != "gesendet" || r.Antwort != "ACK" || <-empfangen != "<durchsage>quittieren</durchsage>" {
		t.Fatalf("TCP mit Quittung: %+v", r)
	}
	r = telegrammSenden(anzTelegramm{ID: 9, Inhalt: "<still/>", Ziel: anzZiel{IP: h, Weg: "tcp", Port: p}})
	if r.Status != "gesendet" || !strings.Contains(r.Antwort, "keine Antwort") {
		t.Fatalf("TCP ohne Quittung: %+v", r)
	}
	<-empfangen

	// niemand hört zu → Fehler
	ln2, _ := net.Listen("tcp", "127.0.0.1:0")
	_, p2 := hostPort(t, ln2.Addr().String())
	ln2.Close()
	if r := telegrammSenden(anzTelegramm{ID: 10, Inhalt: "x", Ziel: anzZiel{IP: h, Weg: "tcp", Port: p2}}); r.Status != "fehler" {
		t.Fatalf("TCP ohne Gegenstelle: %+v", r)
	}
}

func TestAnzeigerAbholenUndErgebnis(t *testing.T) {
	var token, query, ergebnis string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token = r.Header.Get("X-Edge-Token")
		switch r.URL.Path {
		case "/api/edge/anzeiger/telegramme":
			query = r.URL.RawQuery
			_, _ = w.Write([]byte(`{"stand":"b.2","zustaendig":true,"telegramme":[{"id":4,"art":"zielanzeiger","test":true,"inhalt":"<z/>","ziel":{"asset_id":9,"name":"BSBZ000001","ip":"192.168.62.27","weg":"aus","port":null,"pfad":null}}]}`))
		case "/api/edge/anzeiger/ergebnis":
			b, _ := io.ReadAll(r.Body)
			ergebnis = string(b)
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	k := newKaiClient(srv.URL, "geheim")
	r, err := k.AnzeigerAbholen("b.1", 25)
	if err != nil || token != "geheim" || query != "warten=25&stand=b.1" || r.Stand != "b.2" || len(r.Telegramme) != 1 || r.Telegramme[0].Ziel.Name != "BSBZ000001" {
		t.Fatalf("Abholen: %+v err=%v query=%q", r, err, query)
	}
	e := telegrammSenden(r.Telegramme[0])
	if err := k.AnzeigerErgebnis(e); err != nil || !strings.Contains(ergebnis, `"status":"protokolliert"`) || !strings.Contains(ergebnis, `"id":4`) {
		t.Fatalf("Ergebnis: %s err=%v", ergebnis, err)
	}
}
