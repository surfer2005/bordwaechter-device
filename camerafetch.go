package main

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// cameraGet holt ein Bild von der Kamera-Snapshot-URL und beherrscht sowohl Basic- (z. B. Mobotix)
// als auch Digest-Authentifizierung (AXIS/AXIS OS 11+ verlangt standardmäßig Digest — mit reinem
// Basic kommt sonst nur HTTP 401 zurück). Die Zugangsdaten stehen als userinfo in der URL
// (http://benutzer:passwort@host/pfad). Ohne userinfo wird anonym geladen.
func cameraGet(rawURL string) (*http.Response, error) {
	return cameraDo(rawURL, &http.Client{Timeout: 8 * time.Second})
}

// cameraStream wie cameraGet, aber OHNE Gesamt-Timeout (ein MJPEG-Livestream läuft dauerhaft) —
// nur ein Timeout bis zum Antwort-Header, damit ein toter Host nicht ewig hängt.
func cameraStream(rawURL string) (*http.Response, error) {
	return cameraDo(rawURL, &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 10 * time.Second}})
}

func cameraDo(rawURL string, cl *http.Client) (*http.Response, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	var user, pass string
	if u.User != nil {
		user = u.User.Username()
		pass, _ = u.User.Password()
	}
	reqURL := *u
	reqURL.User = nil // Zugangsdaten nicht in die Request-Line schreiben

	// 1) Erstversuch: anonym bzw. mit Basic.
	req, _ := http.NewRequest("GET", reqURL.String(), nil)
	if user != "" {
		req.SetBasicAuth(user, pass)
	}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized || user == "" {
		return resp, nil
	}

	// 2) 401 → die Kamera verlangt vermutlich Digest. Challenge auswerten und erneut versuchen.
	challenge := resp.Header.Get("WWW-Authenticate")
	_ = resp.Body.Close()
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(challenge)), "digest") {
		// Nur Basic angeboten (und abgelehnt) → Basic-Antwort zurückgeben, der Aufrufer meldet den Fehler.
		req2, _ := http.NewRequest("GET", reqURL.String(), nil)
		req2.SetBasicAuth(user, pass)
		return cl.Do(req2)
	}
	params := parseDigestChallenge(challenge)
	authHeader := buildDigestHeader(user, pass, "GET", reqURL.RequestURI(), params)
	req3, _ := http.NewRequest("GET", reqURL.String(), nil)
	req3.Header.Set("Authorization", authHeader)
	return cl.Do(req3)
}

func parseDigestChallenge(h string) map[string]string {
	h = strings.TrimSpace(h)
	if len(h) >= 6 && strings.EqualFold(h[:6], "digest") {
		h = strings.TrimSpace(h[6:])
	}
	m := map[string]string{}
	for _, part := range splitDigestParts(h) {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		k := strings.ToLower(strings.TrimSpace(kv[0]))
		v := strings.Trim(strings.TrimSpace(kv[1]), "\"")
		m[k] = v
	}
	return m
}

// splitDigestParts trennt an Kommas, aber nicht innerhalb von Anführungszeichen (realm/nonce stehen
// in Quotes, qop kann "auth,auth-int" enthalten).
func splitDigestParts(s string) []string {
	var out []string
	var b strings.Builder
	inQuote := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
			b.WriteRune(r)
		case r == ',' && !inQuote:
			out = append(out, b.String())
			b.Reset()
		default:
			b.WriteRune(r)
		}
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	return out
}

func md5hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func buildDigestHeader(user, pass, method, uri string, p map[string]string) string {
	realm := p["realm"]
	nonce := p["nonce"]
	qop := p["qop"]
	opaque := p["opaque"]
	algo := p["algorithm"]

	ha1 := md5hex(user + ":" + realm + ":" + pass)
	ha2 := md5hex(method + ":" + uri)

	header := fmt.Sprintf(`Digest username="%s", realm="%s", nonce="%s", uri="%s"`, user, realm, nonce, uri)
	var response string
	if strings.Contains(qop, "auth") {
		cnonce := randHex(8)
		nc := "00000001"
		response = md5hex(ha1 + ":" + nonce + ":" + nc + ":" + cnonce + ":auth:" + ha2)
		header += fmt.Sprintf(`, qop=auth, nc=%s, cnonce="%s"`, nc, cnonce)
	} else {
		response = md5hex(ha1 + ":" + nonce + ":" + ha2)
	}
	header += fmt.Sprintf(`, response="%s"`, response)
	if opaque != "" {
		header += fmt.Sprintf(`, opaque="%s"`, opaque)
	}
	if algo != "" {
		header += fmt.Sprintf(`, algorithm=%s`, algo)
	}
	return header
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
