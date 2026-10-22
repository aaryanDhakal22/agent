package order

import (
	"context"
	"math"

	"quiccpos/agent/internal/domain/order"
)

// Local trip ordering: the fastest open path store -> every stop (no return,
// like main's OSRM trip with roundtrip=false) from the drive times main
// pre-sent, straight-line estimates where a time is missing. Used when main
// can't be reached (pool map) and for the delivery.run stop estimates.

const (
	// roadFactor turns a straight line into an approximate road distance;
	// estSpeedMps is an urban average driving speed (~25 km/h).
	roadFactor  = 1.3
	estSpeedMps = 6.94
	// bruteForceMax: up to this many located stops every order is tried.
	bruteForceMax = 7
)

type tripPoint struct {
	lat, lng float64
	ok       bool
}

func haversineM(a, b tripPoint) float64 {
	const r = 6371008.8
	rad := math.Pi / 180
	dLat, dLng := (b.lat-a.lat)*rad, (b.lng-a.lng)*rad
	h := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(a.lat*rad)*math.Cos(b.lat*rad)*math.Sin(dLng/2)*math.Sin(dLng/2)
	return 2 * r * math.Asin(math.Min(1, math.Sqrt(h)))
}

// localTrip orders the run's located stops fastest-first; unlocated ones go
// last in the given order. Always approximate, no polyline.
func (s *Service) localTrip(ctx context.Context, run []order.StoredOrder) Trip {
	var store tripPoint
	if st, _ := s.floorGeo(); st != nil {
		store = tripPoint{st.Lat, st.Lng, true}
	}
	var located, rest []order.StoredOrder
	for _, so := range run {
		if so.Geo.Located() {
			located = append(located, so)
		} else {
			rest = append(rest, so)
		}
	}
	ids := make([]string, 0, len(located))
	for _, so := range located {
		ids = append(ids, so.Order.ID)
	}
	pairs := map[string]map[string]int64{}
	if len(ids) > 1 {
		if p, err := s.repo.DriveTimes(ctx, ids); err == nil {
			pairs = p
		}
	}

	n := len(located)
	pts := make([]tripPoint, n)
	from0 := make([]float64, n) // store -> i
	leg := make([][]float64, n) // i -> j
	for i, so := range located {
		pts[i] = tripPoint{*so.Geo.Lat, *so.Geo.Lng, true}
	}
	for i, so := range located {
		if to, _, ok := storeTimes(so.Geo); ok {
			from0[i] = float64(to)
		} else if store.ok {
			from0[i] = haversineM(store, pts[i]) * roadFactor / estSpeedMps
		}
		leg[i] = make([]float64, n)
		for j := range located {
			if i == j {
				continue
			}
			if sec, ok := pairs[ids[i]][ids[j]]; ok {
				leg[i][j] = float64(sec)
			} else {
				leg[i][j] = haversineM(pts[i], pts[j]) * roadFactor / estSpeedMps
			}
		}
	}
	best := bestPath(from0, leg)

	t := Trip{OrderIDs: make([]string, 0, len(run)), Approximate: true}
	var dur, dist float64
	prev := -1
	for _, i := range best {
		t.OrderIDs = append(t.OrderIDs, ids[i])
		if prev < 0 {
			dur += from0[i]
			if store.ok {
				dist += haversineM(store, pts[i]) * roadFactor
			}
		} else {
			dur += leg[prev][i]
			dist += haversineM(pts[prev], pts[i]) * roadFactor
		}
		prev = i
	}
	for _, so := range rest {
		t.OrderIDs = append(t.OrderIDs, so.Order.ID)
	}
	t.DurationS, t.DistanceM = int64(math.Round(dur)), int64(math.Round(dist))
	return t
}

// bestPath returns the stop order with the least total time (store first, no
// return): every permutation up to bruteForceMax stops, nearest-neighbour
// beyond. Ties keep the lexically first permutation (deterministic).
func bestPath(from0 []float64, leg [][]float64) []int {
	n := len(from0)
	if n == 0 {
		return nil
	}
	if n > bruteForceMax {
		used := make([]bool, n)
		out := make([]int, 0, n)
		cur := -1
		for len(out) < n {
			bi, bc := -1, math.Inf(1)
			for j := 0; j < n; j++ {
				if used[j] {
					continue
				}
				c := from0[j]
				if cur >= 0 {
					c = leg[cur][j]
				}
				if c < bc {
					bi, bc = j, c
				}
			}
			used[bi] = true
			out = append(out, bi)
			cur = bi
		}
		return out
	}
	perm := make([]int, n)
	for i := range perm {
		perm[i] = i
	}
	best := append([]int(nil), perm...)
	bestCost := math.Inf(1)
	var rec func(k int, cost float64)
	used := make([]bool, n)
	cur := make([]int, 0, n)
	rec = func(k int, cost float64) {
		if cost >= bestCost {
			return
		}
		if k == n {
			bestCost = cost
			copy(best, cur)
			return
		}
		for j := 0; j < n; j++ {
			if used[j] {
				continue
			}
			c := from0[j]
			if k > 0 {
				c = leg[cur[k-1]][j]
			}
			used[j] = true
			cur = append(cur, j)
			rec(k+1, cost+c)
			cur = cur[:k]
			used[j] = false
		}
	}
	rec(0, 0)
	return best
}
