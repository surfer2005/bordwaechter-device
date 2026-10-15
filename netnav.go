package main

// Standalone-/Offline-Navigation ohne KAI: aus dem gecachten Streckennetz (ship_route_geometry von
// KAI) werden die Anlegestellen (Segment-Endpunkte) und ein Routen-Graph abgeleitet. Der Nutzer wählt
// ein Ziel; die Route ist entweder Luftlinie oder — über den Graphen (Dijkstra) — entlang der Fahrrinne.
// Reine Rechenfunktionen (landingsFromNet/buildGraph/manualRoute) sind ohne Netz/DB testbar.
// (Eigene Datei, NICHT network.go — das ist die IP/NetworkManager-Konfiguration.)

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sort"
)

type Landing struct {
	Name string  `json:"name"`
	Lat  float64 `json:"lat"`
	Lng  float64 `json:"lng"`
}

// landingsFromNet: eindeutige Anlegestellen mit Koordinate (From = erster Punkt, To = letzter Punkt).
func landingsFromNet(n *NetworkData) []Landing {
	if n == nil {
		return nil
	}
	m := map[string][2]float64{} // name -> [lon,lat]
	for _, s := range n.Segments {
		if len(s.Points) < 2 {
			continue
		}
		if s.From != "" {
			if _, ok := m[s.From]; !ok {
				m[s.From] = s.Points[0]
			}
		}
		if s.To != "" {
			if _, ok := m[s.To]; !ok {
				m[s.To] = s.Points[len(s.Points)-1]
			}
		}
	}
	out := make([]Landing, 0, len(m))
	for name, p := range m {
		out = append(out, Landing{Name: name, Lat: p[1], Lng: p[0]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

type netEdge struct {
	to  string
	km  float64
	pts [][2]float64 // [lon,lat], in Fahrtrichtung von -> to
}

func polyKm(pts [][2]float64) float64 {
	tot := 0.0
	for i := 1; i < len(pts); i++ {
		tot += haversineKm(pts[i-1][1], pts[i-1][0], pts[i][1], pts[i][0])
	}
	return tot
}

// buildGraph: ungerichteter Graph (beide Richtungen) über die Anlegestellen.
func buildGraph(n *NetworkData) map[string][]netEdge {
	g := map[string][]netEdge{}
	if n == nil {
		return g
	}
	for _, s := range n.Segments {
		if s.From == "" || s.To == "" || len(s.Points) < 2 {
			continue
		}
		km := s.Km
		if km <= 0 {
			km = polyKm(s.Points)
		}
		g[s.From] = append(g[s.From], netEdge{to: s.To, km: km, pts: s.Points})
		rev := make([][2]float64, len(s.Points))
		for i, p := range s.Points {
			rev[len(s.Points)-1-i] = p
		}
		g[s.To] = append(g[s.To], netEdge{to: s.From, km: km, pts: rev})
	}
	return g
}

func nearestLanding(ls []Landing, lat, lng float64) *Landing {
	var best *Landing
	bestD := math.MaxFloat64
	for i := range ls {
		d := haversineKm(lat, lng, ls[i].Lat, ls[i].Lng)
		if d < bestD {
			bestD = d
			best = &ls[i]
		}
	}
	return best
}

// dijkstra: kürzester Weg (nach km) von start zu ziel; liefert die verkettete Polylinie + km.
func dijkstra(g map[string][]netEdge, start, ziel string) ([][2]float64, float64, bool) {
	if start == ziel {
		return nil, 0, true
	}
	dist := map[string]float64{start: 0}
	prev := map[string]string{}
	prevPts := map[string][][2]float64{}
	visited := map[string]bool{}
	for {
		cur := ""
		curD := math.MaxFloat64
		for node, d := range dist {
			if !visited[node] && d < curD {
				curD = d
				cur = node
			}
		}
		if cur == "" {
			return nil, 0, false
		}
		if cur == ziel {
			break
		}
		visited[cur] = true
		for _, e := range g[cur] {
			nd := curD + e.km
			if old, ok := dist[e.to]; !ok || nd < old {
				dist[e.to] = nd
				prev[e.to] = cur
				prevPts[e.to] = e.pts
			}
		}
	}
	segs := [][][2]float64{}
	for n := ziel; n != start; {
		segs = append([][][2]float64{prevPts[n]}, segs...)
		n = prev[n]
	}
	route := [][2]float64{}
	for i, seg := range segs {
		if i > 0 && len(seg) > 0 {
			seg = seg[1:]
		}
		route = append(route, seg...)
	}
	return route, dist[ziel], true
}

type ManualRoute struct {
	Ok         bool         `json:"ok"`
	Mode       string       `json:"mode"`
	Points     [][2]float64 `json:"points"` // [lon,lat]
	DistanceKm float64      `json:"distance_km"`
	BearingDeg float64      `json:"bearing_deg"`
	ToName     string       `json:"to_name"`
	ToLat      float64      `json:"to_lat"`
	ToLng      float64      `json:"to_lng"`
	Note       string       `json:"note,omitempty"`
}

// manualRoute berechnet die Route von der aktuellen Position zum gewählten Ziel.
// mode "net" = entlang der Fahrrinne (Graph), sonst Luftlinie.
func manualRoute(n *NetworkData, curLat, curLng float64, toName, mode string) ManualRoute {
	r := ManualRoute{Mode: mode, ToName: toName}
	ls := landingsFromNet(n)
	var target *Landing
	for i := range ls {
		if ls[i].Name == toName {
			target = &ls[i]
			break
		}
	}
	if target == nil {
		r.Note = "Ziel unbekannt"
		return r
	}
	r.ToLat, r.ToLng = target.Lat, target.Lng
	cur := [2]float64{curLng, curLat}
	to := [2]float64{target.Lng, target.Lat}

	if mode == "net" {
		g := buildGraph(n)
		start := nearestLanding(ls, curLat, curLng)
		if start != nil {
			if route, km, ok := dijkstra(g, start.Name, toName); ok {
				pts := [][2]float64{cur, {start.Lng, start.Lat}}
				pts = append(pts, route...)
				r.Points = pts
				r.DistanceKm = haversineKm(curLat, curLng, start.Lat, start.Lng) + km
				r.BearingDeg = bearingDeg(curLat, curLng, start.Lat, start.Lng)
				r.Ok = true
				return r
			}
		}
		r.Note = "keine Fahrrinne gefunden — Luftlinie"
		r.Mode = "line"
	}
	r.Points = [][2]float64{cur, to}
	r.DistanceKm = haversineKm(curLat, curLng, target.Lat, target.Lng)
	r.BearingDeg = bearingDeg(curLat, curLng, target.Lat, target.Lng)
	r.Ok = true
	return r
}

// --- Persistenz (von KAI gecacht, offline nutzbar) ---

func networkPath() string {
	if p := os.Getenv("BORDWAECHTER_NETWORK"); p != "" {
		return p
	}
	if _, err := os.Stat("/etc/bordwaechter"); err == nil {
		return "/etc/bordwaechter/network.json"
	}
	return "network.json"
}

func saveNetwork(n *NetworkData) {
	data, err := json.Marshal(n)
	if err != nil {
		return
	}
	p := networkPath()
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	tmp := p + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		_ = os.Rename(tmp, p)
	}
}

func loadNetwork() *NetworkData {
	data, err := os.ReadFile(networkPath())
	if err != nil {
		return nil
	}
	var n NetworkData
	if json.Unmarshal(data, &n) != nil {
		return nil
	}
	return &n
}
