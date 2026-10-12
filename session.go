package main

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"
)

// Schlanke Sitzungsverwaltung für die Anmeldemaske: nach erfolgreichem Login bekommt der Browser
// ein Cookie mit einem zufälligen Token; die API akzeptiert dieses Cookie (neben Basic-Auth, das HA
// weiter nutzt). In-Memory — nach einem Neustart meldet man sich neu an (für ein Bordgerät genügt das).
var (
	sessMu   sync.Mutex
	sessions = map[string]time.Time{} // Token -> Ablaufzeit
)

const sessionTTL = 12 * time.Hour

func newSession() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	tok := base64.RawURLEncoding.EncodeToString(b)
	sessMu.Lock()
	sessions[tok] = time.Now().Add(sessionTTL)
	// Gelegenheit zum Aufräumen abgelaufener Einträge
	now := time.Now()
	for k, exp := range sessions {
		if now.After(exp) {
			delete(sessions, k)
		}
	}
	sessMu.Unlock()
	return tok
}

func validSession(tok string) bool {
	if tok == "" {
		return false
	}
	sessMu.Lock()
	defer sessMu.Unlock()
	exp, ok := sessions[tok]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(sessions, tok)
		return false
	}
	return true
}

func dropSession(tok string) {
	sessMu.Lock()
	delete(sessions, tok)
	sessMu.Unlock()
}
