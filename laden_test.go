package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Server bricht den ersten Download nach der Hälfte ab; danach liefert er mit Range (http.ServeContent) den Rest.
func TestLadeFortsetzbarSetztNachAbbruchFort(t *testing.T) {
	inhalt := append([]byte("\x7fELF"), bytes.Repeat([]byte("KAI-"), 250000)...)
	versuche := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		versuche++
		if versuche == 1 {
			w.Header().Set("Content-Length", "1000004")
			w.WriteHeader(http.StatusOK)
			w.Write(inhalt[:400000])
			hj, _ := w.(http.Hijacker)
			c, _, _ := hj.Hijack()
			c.Close()
			return
		}
		if r.Header.Get("Range") == "" {
			t.Errorf("zweiter Versuch ohne Range")
		}
		http.ServeContent(w, r, "bordwaechter", time.Now(), bytes.NewReader(inhalt))
	}))
	defer srv.Close()
	alt := ladenWarteBasis
	ladenWarteBasis = 10 * time.Millisecond
	defer func() { ladenWarteBasis = alt }()
	ziel := filepath.Join(t.TempDir(), "bw.new")
	if err := ladeFortsetzbar(srv.URL, ziel, time.Minute); err != nil {
		t.Fatalf("Fehler: %v", err)
	}
	got, _ := os.ReadFile(ziel)
	if !bytes.Equal(got, inhalt) {
		t.Fatalf("Inhalt weicht ab: %d statt %d Bytes", len(got), len(inhalt))
	}
	if versuche != 2 {
		t.Fatalf("erwartet 2 Versuche, waren %d", versuche)
	}
	if !istProgramm(ziel) {
		t.Fatal("ELF nicht erkannt")
	}
}

func TestLadeFortsetzbarHttpFehler(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) }))
	defer srv.Close()
	if err := ladeFortsetzbar(srv.URL, filepath.Join(t.TempDir(), "x"), time.Minute); err == nil {
		t.Fatal("404 muss als Fehler kommen")
	}
	if gesamtAusContentRange("bytes 100-199/6553752") != 6553752 || gesamtAusContentRange("") != -1 {
		t.Fatal("Content-Range falsch gelesen")
	}
}

func TestIstProgrammLehntHtmlAb(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x")
	os.WriteFile(p, []byte("<html>Fehler</html>"), 0o644)
	if istProgramm(p) {
		t.Fatal("HTML darf nicht als Programm gelten")
	}
}
