package dispatch

import (
	"math"
	"sort"
)

// matrix is the dense drive-time table over the planning nodes: node 0 is
// the store, the rest are orders (candidates first, then locked pool
// orders). Missing road times are estimated from straight-line distance.
type matrix struct {
	n   int
	sec []float64 // sec[a*n+b] = seconds a -> b
	est []bool    // est[a*n+b] = sec is a straight-line estimate
}

func (m *matrix) t(a, b int) float64  { return m.sec[a*m.n+b] }
func (m *matrix) isEst(a, b int) bool { return m.est[a*m.n+b] }

const earthRadiusM = 6371008.8

func haversine(a, b LatLng) float64 {
	rad := math.Pi / 180
	dLat := (b.Lat - a.Lat) * rad
	dLng := (b.Lng - a.Lng) * rad
	s := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(a.Lat*rad)*math.Cos(b.Lat*rad)*math.Sin(dLng/2)*math.Sin(dLng/2)
	return 2 * earthRadiusM * math.Asin(math.Min(1, math.Sqrt(s)))
}

// buildMatrix fills the table for nodes (ids[0] is "" = the store). A
// missing pair costs haversine * secPerM, where secPerM is the median of
// (road seconds / straight meters) over this request's known pairs
// (>= 100 m apart), clamped to a sane band around the default.
func buildMatrix(ids []string, pts []LatLng, store *LatLng, times *Times, p *Params) *matrix {
	n := len(ids)
	m := &matrix{n: n, sec: make([]float64, n*n), est: make([]bool, n*n)}
	known := make([]bool, n*n)
	pt := func(i int) (LatLng, bool) {
		if i == 0 {
			if store == nil {
				return LatLng{}, false
			}
			return *store, true
		}
		return pts[i], true
	}
	lookup := func(a, b int) (float64, bool) {
		var v int
		var found bool
		switch {
		case a == 0:
			v, found = times.Store.To[ids[b]]
		case b == 0:
			v, found = times.Store.From[ids[a]]
		default:
			v, found = times.Pairs[ids[a]][ids[b]]
		}
		if !found || v < 0 {
			return 0, false
		}
		return float64(v), true
	}
	var ratios []float64
	for a := 0; a < n; a++ {
		for b := 0; b < n; b++ {
			if a == b {
				known[a*n+b] = true
				continue
			}
			v, found := lookup(a, b)
			if !found {
				continue
			}
			m.sec[a*n+b], known[a*n+b] = v, true
			pa, okA := pt(a)
			pb, okB := pt(b)
			if okA && okB {
				if d := haversine(pa, pb); d >= 100 {
					ratios = append(ratios, v/d)
				}
			}
		}
	}
	def := p.RoadFactor / p.SpeedMps
	secPerM := def
	if len(ratios) > 0 {
		sort.Float64s(ratios)
		mid := len(ratios) / 2
		secPerM = ratios[mid]
		if len(ratios)%2 == 0 {
			secPerM = (ratios[mid-1] + ratios[mid]) / 2
		}
		secPerM = math.Max(def/3, math.Min(def*3, secPerM))
	}
	for a := 0; a < n; a++ {
		for b := 0; b < n; b++ {
			if known[a*n+b] {
				continue
			}
			pa, okA := pt(a)
			pb, okB := pt(b)
			if !okA || !okB { // no store pin: Plan only routes orders whose store legs are known
				continue
			}
			m.sec[a*n+b] = math.Round(haversine(pa, pb) * secPerM)
			m.est[a*n+b] = true
		}
	}
	return m
}
