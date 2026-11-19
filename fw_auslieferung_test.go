package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// Büro FN, 19.11.2026: der ESP8266 bekam die Firmware ohne Content-Length (chunked) und brach das OTA ab
// („Flash erase failed at 0x00100000“). Die Firmware muss MIT Größenangabe ausgeliefert werden.
func TestFirmwareMitGroessenangabe(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BORDWAECHTER_CONFIG", filepath.Join(dir, "config.json"))
	ordner := filepath.Join(sensorOrdner(), "fw", "bw-buero00001")
	if err := os.MkdirAll(ordner, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := bytes.Repeat([]byte{0xE9, 1, 2, 3}, 120000) // ~480 KB wie ein ESP-01S-Abbild
	if err := os.WriteFile(filepath.Join(ordner, "firmware.ota.bin"), bin, 0o644); err != nil {
		t.Fatal(err)
	}
	sensorik.mu.Lock()
	sensorik.defs[42] = &sensorDef{ID: 42, User: "bw-buero00001"}
	sensorik.mu.Unlock()
	defer func() {
		sensorik.mu.Lock()
		delete(sensorik.defs, 42)
		delete(sensorik.status, 42)
		sensorik.mu.Unlock()
	}()
	mux := http.NewServeMux()
	(&App{}).sensorRoutes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	r, err := http.Get(srv.URL + "/fw/bw-buero00001/firmware.ota.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	got, _ := io.ReadAll(r.Body)
	if r.StatusCode != 200 || len(got) != len(bin) {
		t.Fatalf("Status %d, %d Bytes statt %d", r.StatusCode, len(got), len(bin))
	}
	if r.ContentLength != int64(len(bin)) || r.Header.Get("Content-Length") != strconv.Itoa(len(bin)) {
		t.Fatalf("Content-Length fehlt oder falsch: %q (TransferEncoding %v)", r.Header.Get("Content-Length"), r.TransferEncoding)
	}
	if r.Header.Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("Content-Type %q", r.Header.Get("Content-Type"))
	}
	// Fortschritt für KAI: vollständig geladen = 100 %
	sensorik.mu.Lock()
	st := sensorik.status[42]
	sensorik.mu.Unlock()
	if st == nil || st.OtaProzent == nil || *st.OtaProzent != 100 || st.OtaAt == "" {
		t.Fatalf("OTA-Fortschritt fehlt: %+v", st)
	}
}
