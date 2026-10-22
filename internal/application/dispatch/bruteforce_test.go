package dispatch

import (
	"math"
	"sort"
)

// An independent re-implementation of the model, straight from the Input
// (no shared code with the optimizer beyond Params/DefaultParams): the
// brute-force optimum, and an evaluator for a plan the optimizer emitted.
// Test instances carry a complete drive-time matrix.

type bf struct {
	in       Input
	p        Params
	target   float64 // minutes
	byID     map[string]*Order
	cands    []string // oldest first
	free     []string // drivers without a pool, queue order
	locked   []string // drivers with a pool, queue order
	pool     map[string][]string
	waitFrom float64 // seconds after now a left-over order could leave
}

func newBF(in Input) *bf {
	b := &bf{in: in, p: DefaultParams(), byID: map[string]*Order{}, pool: map[string][]string{}}
	if in.Params != nil {
		b.p = *in.Params
	}
	b.target = float64(in.TargetMinutes)
	if b.target <= 0 {
		b.target = 45
	}
	for i := range in.Orders {
		if _, dup := b.byID[in.Orders[i].ID]; !dup {
			b.byID[in.Orders[i].ID] = &in.Orders[i]
		}
	}
	avail := map[string]bool{}
	for _, d := range in.Drivers {
		avail[d.ID] = true
	}
	// An order's pool_driver_id wins; else the first driver listing it.
	pooled := map[string]bool{}
	for _, o := range in.Orders {
		if o.PoolDriverID != "" && !pooled[o.ID] {
			pooled[o.ID] = true
			if avail[o.PoolDriverID] {
				b.pool[o.PoolDriverID] = append(b.pool[o.PoolDriverID], o.ID)
			}
		}
	}
	for _, d := range in.Drivers {
		for _, id := range d.Pool {
			if _, known := b.byID[id]; known && !pooled[id] {
				pooled[id] = true
				b.pool[d.ID] = append(b.pool[d.ID], id)
			}
		}
	}
	for _, d := range in.Drivers {
		ok := true
		for _, id := range b.pool[d.ID] {
			ok = ok && b.routable(id)
		}
		switch {
		case len(b.pool[d.ID]) == 0:
			b.free = append(b.free, d.ID)
		case ok:
			b.locked = append(b.locked, d.ID)
		}
	}
	soon := float64(max(in.SoonReadyMinutes, 0)) * 60
	for _, o := range in.Orders {
		if pooled[o.ID] || !b.routable(o.ID) {
			continue
		}
		if o.Ready || (!o.ReadyAt.IsZero() && o.ReadyAt.Sub(in.Now).Seconds() <= soon) {
			b.cands = append(b.cands, o.ID)
		}
	}
	sort.Slice(b.cands, func(i, j int) bool {
		a, c := b.byID[b.cands[i]], b.byID[b.cands[j]]
		if !a.PlacedAt.Equal(c.PlacedAt) {
			return a.PlacedAt.Before(c.PlacedAt)
		}
		return a.ID < c.ID
	})
	if len(in.OutDrivers) > 0 {
		b.waitFrom = math.Inf(1)
		for _, d := range in.OutDrivers {
			b.waitFrom = math.Min(b.waitFrom, math.Max(0, d.ReturnAt.Sub(in.Now).Seconds()))
		}
	} else {
		b.waitFrom = b.p.TypicalRunMin * 60
	}
	return b
}

// routable: located, and the store legs are known or estimable.
func (b *bf) routable(id string) bool {
	if !b.byID[id].Located {
		return false
	}
	if b.in.Store != nil {
		return true
	}
	_, to := b.in.Times.Store.To[id]
	_, from := b.in.Times.Store.From[id]
	return to && from
}

func (b *bf) dur(from, to string) float64 {
	switch {
	case from == to:
		return 0
	case from == "":
		return float64(b.in.Times.Store.To[to])
	case to == "":
		return float64(b.in.Times.Store.From[from])
	}
	return float64(b.in.Times.Pairs[from][to])
}

func (b *bf) lateCost(x float64) float64 {
	c := b.p.W0 * x
	s := b.target - b.p.UrgentMin
	switch {
	case x > b.target:
		c += b.p.W1*b.p.UrgentMin + b.p.W2*(x-b.target)
	case x > s:
		c += b.p.W1 * (x - s)
	}
	return c
}

func (b *bf) placed(id string) float64 {
	return math.Min(0, b.byID[id].PlacedAt.Sub(b.in.Now).Seconds())
}

func (b *bf) readyIn(id string) float64 {
	o := b.byID[id]
	if o.Ready || o.ReadyAt.IsZero() {
		return 0
	}
	return math.Max(0, o.ReadyAt.Sub(b.in.Now).Seconds())
}

// sectorOK: some far stop's bearing starts a clockwise SectorDeg arc that
// holds every far stop (far = more than SectorMinDistM from the store).
func (b *bf) sectorOK(stops []string) bool {
	if b.in.Store == nil || b.p.SectorDeg >= 360 {
		return true
	}
	var bs []float64
	for _, id := range stops {
		o := b.byID[id]
		st := *b.in.Store
		if haversine(st, LatLng{o.Lat, o.Lng}) <= b.p.SectorMinDistM {
			continue
		}
		r := math.Pi / 180
		y := math.Sin((o.Lng-st.Lng)*r) * math.Cos(o.Lat*r)
		x := math.Cos(st.Lat*r)*math.Sin(o.Lat*r) - math.Sin(st.Lat*r)*math.Cos(o.Lat*r)*math.Cos((o.Lng-st.Lng)*r)
		bs = append(bs, math.Mod(math.Atan2(y, x)/r+360, 360))
	}
	if len(bs) < 2 {
		return true
	}
	for _, start := range bs {
		all := true
		for _, v := range bs {
			if math.Mod(v-start+360, 360) > b.p.SectorDeg+1e-9 {
				all = false
			}
		}
		if all {
			return true
		}
	}
	return false
}

// seq evaluates one stop order: cost, whether every stop meets the detour
// limit, and per stop the arrival (seconds after now).
func (b *bf) seq(stops []string) (cost float64, detourOK bool, eta []float64) {
	depart := 0.0
	for _, id := range stops {
		depart = math.Max(depart, b.readyIn(id))
	}
	t, prev, late := 0.0, "", 0.0
	detourOK = true
	for _, id := range stops {
		t += b.dur(prev, id)
		direct := b.dur("", id)
		if t > math.Max(direct+b.p.DetourMinS, b.p.DetourFactor*direct)+1e-9 {
			detourOK = false
		}
		late += b.lateCost((depart + t - b.placed(id)) / 60)
		eta = append(eta, depart+t)
		t += b.p.ServiceS
		prev = id
	}
	t += b.dur(prev, "")
	return late + b.p.Beta*t/60, detourOK, eta
}

// bestRun: the cheapest stop order of a set; with mustDetour, only orders
// meeting the detour limit count.
func (b *bf) bestRun(stops []string, mustDetour bool) (float64, bool) {
	best, found := math.Inf(1), false
	perm := append([]string(nil), stops...)
	var gen func(k int)
	gen = func(k int) {
		if k == len(perm) {
			c, ok, _ := b.seq(perm)
			if (ok || !mustDetour) && c < best {
				best, found = c, true
			}
			return
		}
		for i := k; i < len(perm); i++ {
			perm[k], perm[i] = perm[i], perm[k]
			gen(k + 1)
			perm[k], perm[i] = perm[i], perm[k]
		}
	}
	gen(0)
	return best, found
}

func (b *bf) leftoverCost(id string) float64 {
	direct := b.dur("", id)
	door := math.Max(b.waitFrom, b.readyIn(id)) + direct
	solo := direct + b.p.ServiceS + b.dur(id, "")
	return b.lateCost((door-b.placed(id))/60) + b.p.Beta*solo/60 + b.p.M
}

// optimum enumerates every assignment of the candidates to a driver or
// "left over" (n <= 10, k <= 3) and returns the cheapest valid plan's cost.
func (b *bf) optimum() float64 {
	drivers := append(append([]string(nil), b.locked...), b.free...)
	k, n := len(drivers), len(b.cands)
	if k == 0 {
		return 0 // nothing to plan: no cost reported
	}
	nl := len(b.locked)
	// memo[d][mask] = run cost (+Inf infeasible), stops
	type runv struct {
		cost float64
		ns   int
	}
	memo := make([][]runv, k)
	for d := range drivers {
		memo[d] = make([]runv, 1<<n)
		for mask := range 1 << n {
			var set []string
			if d < nl {
				set = append(set, b.pool[drivers[d]]...)
			}
			for c := range n {
				if mask&(1<<c) != 0 {
					set = append(set, b.cands[c])
				}
			}
			switch {
			case mask == 0 && d >= nl:
				memo[d][mask] = runv{0, 0} // no run
			case mask == 0:
				c, ok := b.bestRun(set, true)
				if !ok {
					c, _ = b.bestRun(set, false)
				}
				memo[d][mask] = runv{c, len(set)}
			case len(set) > b.p.MaxStops || !b.sectorOK(set):
				memo[d][mask] = runv{math.Inf(1), 0}
			default:
				c, ok := b.bestRun(set, true)
				if !ok {
					c = math.Inf(1)
				}
				memo[d][mask] = runv{c, len(set)}
			}
		}
	}
	left := make([]float64, n)
	for c := range n {
		left[c] = b.leftoverCost(b.cands[c])
	}
	best := math.Inf(1)
	assign := make([]int, n) // -1 = left over
	masks := make([]int, k)
	var rec func(c int)
	rec = func(c int) {
		if c == n {
			cost, lo, hi, nr, anyLeft, idle := 0.0, math.MaxInt, 0, 0, false, false
			for d := range k {
				r := memo[d][masks[d]]
				if math.IsInf(r.cost, 1) {
					return
				}
				if d >= nl && masks[d] == 0 {
					idle = true
					continue
				}
				cost += r.cost
				lo, hi, nr = min(lo, r.ns), max(hi, r.ns), nr+1
			}
			for i := range n {
				if assign[i] < 0 {
					cost += left[i]
					anyLeft = true
				}
			}
			if anyLeft && idle {
				return // no idle driver while an order waits
			}
			if n > 0 && len(b.free) > 0 && assign[0] < 0 {
				return // anchor: the oldest goes when a free driver exists
			}
			if nr >= 2 {
				cost += b.p.Lambda * float64(hi-lo)
			}
			best = math.Min(best, cost)
			return
		}
		assign[c] = -1
		rec(c + 1)
		for d := range k {
			assign[c] = d
			masks[d] |= 1 << c
			rec(c + 1)
			masks[d] &^= 1 << c
		}
	}
	rec(0)
	return best
}

// planCost evaluates an emitted plan with this model: each run's own stop
// order, the balance term and the left-over candidates.
func (b *bf) planCost(out Output) float64 {
	inRun := map[string]bool{}
	cost, lo, hi, nr := 0.0, math.MaxInt, 0, 0
	for _, r := range out.Runs {
		c, _, _ := b.seq(r.OrderIDs)
		cost += c
		lo, hi, nr = min(lo, len(r.OrderIDs)), max(hi, len(r.OrderIDs)), nr+1
		for _, id := range r.OrderIDs {
			inRun[id] = true
		}
	}
	if nr >= 2 {
		cost += b.p.Lambda * float64(hi-lo)
	}
	for _, id := range b.cands {
		if !inRun[id] {
			cost += b.leftoverCost(id)
		}
	}
	return cost
}
