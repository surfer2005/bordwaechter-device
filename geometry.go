package main

import "math"

// Reine Geometrie — Spiegel von server/src/route-geometry.js. Punkte sind [lon, lat].

func haversineKm(aLat, aLng, bLat, bLng float64) float64 {
	const R = 6371.0088
	dLat := (bLat - aLat) * math.Pi / 180
	dLng := (bLng - aLng) * math.Pi / 180
	la1 := aLat * math.Pi / 180
	la2 := bLat * math.Pi / 180
	h := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(la1)*math.Cos(la2)*math.Sin(dLng/2)*math.Sin(dLng/2)
	return 2 * R * math.Asin(math.Min(1, math.Sqrt(h)))
}

type projResult struct {
	RemainKm     float64
	DistToLineKm float64
	OK           bool
}

// projectOntoPolyline: Querabstand zur Linie + Reststrecke von der Projektion bis zum Ende (Ziel).
func projectOntoPolyline(pts [][2]float64, lat, lng float64) projResult {
	if len(pts) < 2 {
		return projResult{}
	}
	latRad := lat * math.Pi / 180
	mLat := 111320.0
	mLng := 111320.0 * math.Cos(latRad)
	toXY := func(lo, la float64) (float64, float64) { return (lo - lng) * mLng, (la - lat) * mLat }
	px, py := 0.0, 0.0 // Abfragepunkt im Ursprung

	bestD := math.MaxFloat64
	bestSeg := 0
	bestT := 0.0
	for i := 0; i < len(pts)-1; i++ {
		ax, ay := toXY(pts[i][0], pts[i][1])
		bx, by := toXY(pts[i+1][0], pts[i+1][1])
		dx, dy := bx-ax, by-ay
		seg2 := dx*dx + dy*dy
		t := 0.0
		if seg2 > 0 {
			t = ((px-ax)*dx + (py-ay)*dy) / seg2
			if t < 0 {
				t = 0
			} else if t > 1 {
				t = 1
			}
		}
		cx, cy := ax+t*dx, ay+t*dy
		d := math.Hypot(px-cx, py-cy)
		if d < bestD {
			bestD = d
			bestSeg = i
			bestT = t
		}
	}
	remain := haversineKm(pts[bestSeg][1], pts[bestSeg][0], pts[bestSeg+1][1], pts[bestSeg+1][0]) * (1 - bestT)
	for i := bestSeg + 1; i < len(pts)-1; i++ {
		remain += haversineKm(pts[i][1], pts[i][0], pts[i+1][1], pts[i+1][0])
	}
	return projResult{RemainKm: remain, DistToLineKm: bestD / 1000.0, OK: true}
}

func requiredSpeedKmh(remainKm, restMin float64) float64 {
	if restMin <= 0 {
		return 0
	}
	return remainKm / (restMin / 60.0)
}

// speedVerdict: "ok" | "faster" | "slower"; diff = Ist − Soll (positiv = zu schnell).
func speedVerdict(ist, soll, tol float64) (string, float64) {
	diff := ist - soll
	if math.Abs(diff) <= tol {
		return "ok", diff
	}
	if ist < soll {
		return "faster", diff
	}
	return "slower", diff
}
