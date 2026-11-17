package main

// Fortsetzbarer Download für die Selbstaktualisierung (KAI-Bordgerät 0.1.139). Anlass MS-LINDAU 2026-11-17: das langsame
// Schiffsnetz schaffte die 6,5 MB nicht in den alten 120 s — zweimal abgebrochen („context deadline exceeded“ bzw.
// Verbindung abgerissen). Jetzt: bis zu 20 Min. gesamt, abgebrochen wird ein Versuch erst, wenn 60 s lang nichts kommt,
// und der nächste Versuch setzt mit „Range“ an der Abbruchstelle fort (GitHub liefert 206).

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	ladenStillstand = 60 * time.Second // ohne ein einziges Byte so lange → Versuch abbrechen
	ladenVersuche   = 6
	ladenWarteBasis = 5 * time.Second
)

// stillstandLeser setzt bei jedem gelesenen Stück den Wächter zurück; schlägt er an, wird der Versuch abgebrochen.
type stillstandLeser struct {
	r     io.Reader
	timer *time.Timer
	mu    sync.Mutex
}

func (s *stillstandLeser) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if n > 0 {
		s.mu.Lock()
		s.timer.Reset(ladenStillstand)
		s.mu.Unlock()
	}
	return n, err
}

// gesamtAusContentRange: „bytes 100-199/6553752“ → 6553752 (sonst -1).
func gesamtAusContentRange(v string) int64 {
	if i := strings.LastIndex(v, "/"); i >= 0 {
		if n, err := strconv.ParseInt(strings.TrimSpace(v[i+1:]), 10, 64); err == nil {
			return n
		}
	}
	return -1
}

// ladeFortsetzbar lädt url nach ziel (ziel wird zuerst entfernt). Fehler erst, wenn alle Versuche bzw. die Gesamtzeit aus sind.
func ladeFortsetzbar(url, ziel string, gesamt time.Duration) error {
	_ = os.Remove(ziel)
	ctx, cancel := context.WithTimeout(context.Background(), gesamt)
	defer cancel()
	cl := &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, ResponseHeaderTimeout: 60 * time.Second, TLSHandshakeTimeout: 30 * time.Second}}
	var groesse int64 = -1
	var letzter error
	for versuch := 1; versuch <= ladenVersuche && ctx.Err() == nil; versuch++ {
		if versuch > 1 {
			select {
			case <-ctx.Done():
			case <-time.After(time.Duration(versuch-1) * ladenWarteBasis):
			}
		}
		var hat int64
		if st, err := os.Stat(ziel); err == nil {
			hat = st.Size()
		}
		if groesse > 0 && hat >= groesse {
			return nil
		}
		actx, acancel := context.WithCancel(ctx)
		req, _ := http.NewRequestWithContext(actx, http.MethodGet, url, nil)
		req.Header.Set("User-Agent", "KAI-Bordgeraet/"+version)
		if hat > 0 {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", hat))
		}
		resp, err := cl.Do(req)
		if err != nil {
			acancel()
			letzter = err
			continue
		}
		flags := os.O_CREATE | os.O_WRONLY
		switch resp.StatusCode {
		case http.StatusOK: // kein Fortsetzen möglich (oder erster Versuch) → von vorn
			flags |= os.O_TRUNC
			groesse = resp.ContentLength
		case http.StatusPartialContent:
			flags |= os.O_APPEND
			if g := gesamtAusContentRange(resp.Header.Get("Content-Range")); g > 0 {
				groesse = g
			}
		case http.StatusRequestedRangeNotSatisfiable: // Teilstück passt nicht (mehr) → von vorn
			resp.Body.Close()
			acancel()
			_ = os.Remove(ziel)
			letzter = fmt.Errorf("HTTP 416 beim Fortsetzen")
			continue
		default:
			resp.Body.Close()
			acancel()
			return fmt.Errorf("Download fehlgeschlagen (HTTP %d) von %s", resp.StatusCode, url)
		}
		f, err := os.OpenFile(ziel, flags, 0o755)
		if err != nil {
			resp.Body.Close()
			acancel()
			return err
		}
		sl := &stillstandLeser{r: resp.Body, timer: time.AfterFunc(ladenStillstand, acancel)}
		_, err = io.Copy(f, sl)
		sl.mu.Lock()
		sl.timer.Stop()
		sl.mu.Unlock()
		f.Close()
		resp.Body.Close()
		acancel()
		st, serr := os.Stat(ziel)
		if err == nil && serr == nil && (groesse <= 0 || st.Size() == groesse) {
			return nil
		}
		if err == nil {
			err = fmt.Errorf("unvollständig (%d von %d Bytes)", st.Size(), groesse)
		}
		letzter = err
		logEvent("warn", fmt.Sprintf("Aktualisierung: Download-Versuch %d abgebrochen (%v) — wird fortgesetzt", versuch, err))
	}
	if letzter == nil {
		letzter = ctx.Err()
	}
	return fmt.Errorf("Download nach %d Versuchen abgebrochen: %v", ladenVersuche, letzter)
}

// istProgramm: die geladene Datei ist ein ELF-Programm (kein HTML-Fehlerblatt o. ä.), bevor sie das laufende ersetzt.
func istProgramm(pfad string) bool {
	f, err := os.Open(pfad)
	if err != nil {
		return false
	}
	defer f.Close()
	kopf := make([]byte, 4)
	if _, err := io.ReadFull(f, kopf); err != nil {
		return false
	}
	return string(kopf) == "\x7fELF"
}
