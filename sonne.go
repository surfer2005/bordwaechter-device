package main

// Sonnenauf- und -untergang für den Lichtplan (NOAA-Näherung, auf etwa eine Minute genau) — der Bordwächter
// rechnet sie aus der eigenen GPS-Position, damit „bei Sonnenuntergang“ auch ohne Netz stimmt.

import (
	"math"
	"time"
)

// sonnenzeiten liefert Aufgang und Untergang (UTC) am Kalendertag von `tag` (in dessen Zeitzone).
// ok = false bei Polartag/-nacht (am Bodensee nie).
func sonnenzeiten(tag time.Time, lat, lng float64) (auf, unter time.Time, ok bool) {
	j, m, d := tag.Date()
	mittag := time.Date(j, m, d, 12, 0, 0, 0, time.UTC)
	n := float64(mittag.YearDay())
	rad := math.Pi / 180
	gamma := 2 * math.Pi / 365 * (n - 1)
	eqt := 229.18 * (0.000075 + 0.001868*math.Cos(gamma) - 0.032077*math.Sin(gamma) - 0.014615*math.Cos(2*gamma) - 0.040849*math.Sin(2*gamma))
	decl := 0.006918 - 0.399912*math.Cos(gamma) + 0.070257*math.Sin(gamma) - 0.006758*math.Cos(2*gamma) + 0.000907*math.Sin(2*gamma) - 0.002697*math.Cos(3*gamma) + 0.00148*math.Sin(3*gamma)
	cosHa := math.Cos(90.833*rad)/(math.Cos(lat*rad)*math.Cos(decl)) - math.Tan(lat*rad)*math.Tan(decl)
	if cosHa < -1 || cosHa > 1 {
		return time.Time{}, time.Time{}, false
	}
	ha := math.Acos(cosHa) / rad
	aufMin := 720 - 4*(lng+ha) - eqt
	unterMin := 720 - 4*(lng-ha) - eqt
	basis := time.Date(j, m, d, 0, 0, 0, 0, time.UTC)
	return basis.Add(time.Duration(aufMin * float64(time.Minute))), basis.Add(time.Duration(unterMin * float64(time.Minute))), true
}
