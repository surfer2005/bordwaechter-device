package main

// Wetter + Pegel für die Anzeige (optional, standardmäßig an). Der Bordwächter holt die Werte aus
// DENSELBEN öffentlichen Quellen wie KAI — Wetter von Open-Meteo, Pegel von PEGELONLINE — also
// identische Zahlen, aber OHNE Eingriff ins Live-Pilot-Bundle: die bestehenden Geräte bleiben
// unberührt, und es funktioniert mit UND ohne KAI-Verbindung.
//
// Die reine Rechenlogik (Parsen, Pegel-Rate/Trend, Downsample) ist bewusst in freien Funktionen,
// damit sie ohne Netz testbar ist (info_test.go).

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"time"
)

var infoHTTP = &http.Client{Timeout: 12 * time.Second}

type WeatherData struct {
	TempC    *float64 `json:"temp_c,omitempty"`
	WindKmh  *float64 `json:"wind_kmh,omitempty"`
	WindDir  *float64 `json:"wind_dir_deg,omitempty"`
	PrecipMm *float64 `json:"precip_mm,omitempty"`
	Code     *int     `json:"code,omitempty"`
	AgeSec   int      `json:"age_sec"`
}

type PegelData struct {
	Cm       *float64  `json:"cm,omitempty"`
	Unit     string    `json:"unit,omitempty"`
	Trend    int       `json:"trend"`                   // -1 fallend, 0 stabil, 1 steigend
	RatePerH *float64  `json:"rate_cm_per_h,omitempty"` // Änderung pro Stunde
	Station  string    `json:"station,omitempty"`
	AgeSec   int       `json:"age_sec"`
	Spark    []float64 `json:"spark,omitempty"` // letzte Werte für die Mini-Verlaufslinie
}

type InfoData struct {
	Weather *WeatherData `json:"weather,omitempty"`
	Pegel   *PegelData   `json:"pegel,omitempty"`
}

// --- reine, testbare Logik ---------------------------------------------------

type omCurrent struct {
	Current struct {
		Temp    *float64 `json:"temperature_2m"`
		Wind    *float64 `json:"wind_speed_10m"`
		WindDir *float64 `json:"wind_direction_10m"`
		Code    *int     `json:"weather_code"`
		Precip  *float64 `json:"precipitation"`
		Time    string   `json:"time"`
	} `json:"current"`
}

// parseOpenMeteo liest den current-Block von Open-Meteo. age = Alter des Messzeitpunkts.
func parseOpenMeteo(b []byte, now time.Time) (*WeatherData, error) {
	var o omCurrent
	if err := json.Unmarshal(b, &o); err != nil {
		return nil, err
	}
	w := &WeatherData{TempC: o.Current.Temp, WindKmh: o.Current.Wind, WindDir: o.Current.WindDir, PrecipMm: o.Current.Precip, Code: o.Current.Code}
	if t, err := time.Parse("2006-01-02T15:04", o.Current.Time); err == nil {
		if s := int(now.Sub(t).Seconds()); s >= 0 {
			w.AgeSec = s
		}
	}
	return w, nil
}

type pegelPoint struct {
	Timestamp string  `json:"timestamp"`
	Value     float64 `json:"value"`
}

// parsePegelSeries liest die PEGELONLINE-Messreihe (Zeit + Wert).
func parsePegelSeries(b []byte) ([]pegelPoint, error) {
	var pts []pegelPoint
	if err := json.Unmarshal(b, &pts); err != nil {
		return nil, err
	}
	return pts, nil
}

// pegelRateCmPerH: mittlere Änderung pro Stunde über die Reihe (erster → letzter Wert).
// Gibt (rate, ok). ok=false, wenn zu wenig/zeitlich untauglich.
func pegelRateCmPerH(pts []pegelPoint) (float64, bool) {
	if len(pts) < 2 {
		return 0, false
	}
	t0, err0 := time.Parse(time.RFC3339, pts[0].Timestamp)
	t1, err1 := time.Parse(time.RFC3339, pts[len(pts)-1].Timestamp)
	if err0 != nil || err1 != nil {
		return 0, false
	}
	h := t1.Sub(t0).Hours()
	if h <= 0 {
		return 0, false
	}
	return (pts[len(pts)-1].Value - pts[0].Value) / h, true
}

// trendFromRate: Vorzeichen mit Totband (kleine Schwankungen = stabil).
func trendFromRate(rate float64, deadband float64) int {
	if rate > deadband {
		return 1
	}
	if rate < -deadband {
		return -1
	}
	return 0
}

// downsample: höchstens n gleichmäßig verteilte Werte für die Mini-Linie.
func downsample(pts []pegelPoint, n int) []float64 {
	if n <= 0 || len(pts) == 0 {
		return nil
	}
	if len(pts) <= n {
		out := make([]float64, len(pts))
		for i, p := range pts {
			out[i] = p.Value
		}
		return out
	}
	out := make([]float64, n)
	for i := 0; i < n; i++ {
		idx := i * (len(pts) - 1) / (n - 1)
		out[i] = pts[idx].Value
	}
	return out
}

// --- Netz-Abruf --------------------------------------------------------------

func fetchWeather(lat, lng string) (*WeatherData, error) {
	u := fmt.Sprintf("https://api.open-meteo.com/v1/forecast?latitude=%s&longitude=%s&current=temperature_2m,wind_speed_10m,wind_direction_10m,weather_code,precipitation&timezone=Europe%%2FBerlin",
		url.QueryEscape(lat), url.QueryEscape(lng))
	b, err := httpGet(u)
	if err != nil {
		return nil, err
	}
	return parseOpenMeteo(b, time.Now())
}

func fetchPegel(uuid, label string) (*PegelData, error) {
	u := "https://www.pegelonline.wsv.de/webservices/rest-api/v2/stations/" + url.PathEscape(uuid) + "/W/measurements.json?start=PT3H"
	b, err := httpGet(u)
	if err != nil {
		return nil, err
	}
	pts, err := parsePegelSeries(b)
	if err != nil || len(pts) == 0 {
		return nil, fmt.Errorf("pegel: keine Messwerte")
	}
	last := pts[len(pts)-1]
	pd := &PegelData{Cm: &last.Value, Unit: "cm", Station: label, Spark: downsample(pts, 16)}
	if t, err := time.Parse(time.RFC3339, last.Timestamp); err == nil {
		if s := int(time.Since(t).Seconds()); s >= 0 {
			pd.AgeSec = s
		}
	}
	if rate, ok := pegelRateCmPerH(pts); ok {
		r := round1(rate)
		pd.RatePerH = &r
		pd.Trend = trendFromRate(rate, 0.3)
	}
	return pd, nil
}

func httpGet(u string) ([]byte, error) {
	resp, err := infoHTTP.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
}

// infoLoop: periodisch Wetter/Pegel holen (nur die aktivierten) und am App-Zustand ablegen.
// Fehler sind unkritisch — dann bleibt der letzte Stand bzw. die Anzeige leer.
func (a *App) infoLoop() {
	for {
		cfg := a.getCfg()
		if cfg.Info.Weather || cfg.Info.Pegel {
			inf := &InfoData{}
			if cfg.Info.Weather {
				if w, err := fetchWeather(cfg.Info.Lat, cfg.Info.Lng); err == nil {
					inf.Weather = w
				} else {
					log.Printf("info: Wetter: %v", err)
				}
			}
			if cfg.Info.Pegel {
				if p, err := fetchPegel(cfg.Info.PegelUUID, cfg.Info.PegelLabel); err == nil {
					inf.Pegel = p
				} else {
					log.Printf("info: Pegel: %v", err)
				}
			}
			a.setInfo(inf)
		} else {
			a.setInfo(nil)
		}
		sleepSec(cfg.Info.PollSec, 600)
	}
}
