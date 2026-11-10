package main

import (
	"regexp"
	"testing"
)

// Regressionsschutz: Das Keyfile braucht eine GÜLTIGE Hex-UUID. Ein Nicht-Hex-Zeichen (früher „w“ in
// nmUUID) lässt NetworkManager das ganze Profil verwerfen → die feste IP greift nie, das Gerät bleibt
// stillschweigend auf DHCP. Siehe Vorfall 2026-11-10 (MS-GRAF ZEPPELIN).
var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func TestNmUUIDIstGueltigeHexUUID(t *testing.T) {
	if !uuidRe.MatchString(nmUUID) {
		t.Fatalf("nmUUID ist keine gültige Hex-UUID: %q", nmUUID)
	}
}
