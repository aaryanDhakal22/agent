package dispatch

import (
	"math"
	"sort"
)

// The hold rule (2.2.1, Input.Hold): "far apart — send the first, hold the
// other for the returning driver". It is a run feasibility rule like the
// detour limit and the sector, so the exact search stays exact: a run whose
// later stop is far from the run being built is not a run, the far order
// then goes with another available driver if one is free, else it is left
// over and reported held for the out driver whose return still makes the
// target. Unlike the detour and sector rules it is not closed under subsets
// (B may only fit next to A and C); the solver only ever looks runs up.

// setHold reads Input.Hold: the rule, and for each node the out driver it
// may be held for (the earliest return; -1 when none, or when even he would
// get it to the door after placed + target).
func (m *model) setHold(in *Input, pts []LatLng) {
	m.pts = pts
	m.holdFor = make([]int, len(m.ids))
	for k := range m.holdFor {
		m.holdFor[k] = -1
	}
	h := in.Hold
	if h == nil {
		return
	}
	switch h.Rule {
	case HoldDetour, HoldDistance:
		m.holdRule = h.Rule
		m.hold = *h
	case HoldBundle:
	default:
		return
	}
	m.holdOn = true
	best := -1
	for i, d := range in.OutDrivers {
		if best < 0 || d.ReturnAt.Before(in.OutDrivers[best].ReturnAt) {
			best = i
		}
	}
	if best < 0 {
		return
	}
	back := math.Max(0, in.OutDrivers[best].ReturnAt.Sub(m.now).Seconds())
	for k := 1; k < len(m.ids); k++ {
		door := math.Max(back, m.ready[k]) + m.direct[k]
		if door-m.placed[k] <= m.target*60+1e-9 {
			m.holdFor[k] = best
		}
	}
}

// apartOK applies the hold rule to a run's node set: taken oldest-placed
// first, every later candidate must be nearby the stops kept so far, unless
// no returning driver could take it in time (then it rides along as before).
func (m *model) apartOK(nodes []int) bool {
	if m.holdRule == "" || len(nodes) < 2 {
		return true
	}
	var buf [maxPermStops + MaxStops]int
	sorted := buf[:len(nodes)]
	copy(sorted, nodes)
	sort.Slice(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if m.placed[a] != m.placed[b] {
			return m.placed[a] < m.placed[b]
		}
		return m.ids[a] < m.ids[b]
	})
	kept := make([]int, 1, len(sorted))
	kept[0] = sorted[0]
	for _, k := range sorted[1:] {
		pooled := k > m.nc
		if pooled || m.holdFor[k] < 0 || m.nearKept(kept, k) {
			kept = append(kept, k)
			continue
		}
		return false
	}
	return true
}

// nearKept is the nearby test of node c against the kept stops.
func (m *model) nearKept(kept []int, c int) bool {
	switch m.holdRule {
	case HoldDistance:
		for _, k := range kept {
			if haversine(m.pts[k], m.pts[c]) <= m.hold.DistanceM {
				return true
			}
		}
		return false
	case HoldDetour:
		route := m.nearestFirst(kept)
		best := math.Inf(1)
		prev := 0
		for i := 0; i <= len(route); i++ {
			next := 0
			if i < len(route) {
				next = int(route[i])
			}
			best = math.Min(best, m.mx.t(prev, c)+m.mx.t(c, next)-m.mx.t(prev, next))
			prev = next
		}
		// Whole minutes, as the nearby rule states it (70 s = "1 min").
		return math.Round(best/60) <= m.hold.DetourMinutes
	}
	return true
}

// heldFor is the Unassigned entry of a left-over candidate node held for a
// returning driver (nil when the rule is off or nobody makes the target).
func (m *model) heldFor(in *Input, k int) *HeldFor {
	if !m.holdOn || m.holdFor == nil || m.holdFor[k] < 0 {
		return nil
	}
	d := in.OutDrivers[m.holdFor[k]]
	return &HeldFor{DriverID: d.ID, Name: d.Name, ReturnAt: d.ReturnAt.UTC()}
}

// leftOver is the Unassigned entry of candidate node k left out of every run
// with reason (no_driver becomes held_for_returning_driver when a returning
// driver takes it in time).
func (m *model) leftOver(in *Input, k int, reason string) Unassigned {
	u := Unassigned{OrderID: m.ids[k], Reason: reason}
	if reason == ReasonNoDriver {
		if h := m.heldFor(in, k); h != nil {
			u.Reason, u.HeldFor = ReasonHeld, h
		}
	}
	return u
}
