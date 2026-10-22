package dispatch

import (
	"math"
	"math/bits"
)

// maxPermStops bounds the exact stop-order search of a locked pool larger
// than MaxStops (staff can pool more by hand); beyond it the pool is driven
// nearest-first.
const maxPermStops = 7

// seqEval walks a stop sequence leaving the store at depart (seconds after
// now). It returns the summed per-stop late cost, the run minutes
// (out + services + back) and whether every stop meets the detour limit.
func (m *model) seqEval(stops []int16, depart float64) (late, minutes float64, detourOK bool) {
	t, prev := 0.0, 0
	detourOK = true
	for _, s16 := range stops {
		k := int(s16)
		t += m.mx.t(prev, k)
		if t > m.limit[k] {
			detourOK = false
		}
		late += m.p.late((depart+t-m.placed[k])/60, m.target)
		t += m.p.ServiceS
		prev = k
	}
	t += m.mx.t(prev, 0)
	return late, t / 60, detourOK
}

// setLimits precomputes each node's detour limit (in-car seconds).
func (m *model) setLimits() {
	m.limit = make([]float64, len(m.direct))
	for k, d := range m.direct {
		m.limit[k] = math.Max(d+m.p.DetourMinS, m.p.DetourFactor*d) + 1e-9
	}
}

// permSearch finds the cheapest stop order of a node set by DFS over
// permutations, pruning a prefix once it breaks the detour limit (when
// required) or already costs at least the best full order (every cost term
// is >= 0, so a prefix's cost never decreases).
type permSearch struct {
	m        *model
	nodes    []int
	depart   float64
	check    bool
	cur      [maxPermStops]int16
	best     [maxPermStops]int16
	bestCost float64
	bestMin  float64
	found    bool
}

func (ps *permSearch) run(nodes []int, depart float64, check bool) bool {
	ps.nodes, ps.depart, ps.check = nodes, depart, check
	ps.bestCost, ps.found = math.Inf(1), false
	ps.dfs(0, 0, 0, 0, 0)
	return ps.found
}

func (ps *permSearch) dfs(level int, used uint32, prev int, t, late float64) {
	m := ps.m
	if level == len(ps.nodes) {
		ret := t + m.mx.t(prev, 0)
		cost := late + m.p.Beta*ret/60
		if cost < ps.bestCost {
			ps.bestCost, ps.bestMin, ps.found = cost, ret/60, true
			ps.best = ps.cur
		}
		return
	}
	for i, k := range ps.nodes {
		if used&(1<<i) != 0 {
			continue
		}
		arr := t + m.mx.t(prev, k)
		if ps.check && arr > m.limit[k] {
			continue
		}
		l := late + m.p.late((ps.depart+arr-m.placed[k])/60, m.target)
		if l+m.p.Beta*arr/60 >= ps.bestCost {
			continue
		}
		ps.cur[level] = int16(k)
		ps.dfs(level+1, used|1<<i, k, arr+m.p.ServiceS, l)
	}
}

func (m *model) departOf(nodes []int) float64 {
	d := 0.0
	for _, k := range nodes {
		d = math.Max(d, m.ready[k])
	}
	return d
}

func (m *model) push(seq []int16) int32 {
	off := int32(len(m.arena))
	m.arena = append(m.arena, seq...)
	return off
}

// enumerate builds every feasible free run (subsets of <= MaxStops
// candidates, best order each) and every locked driver's options (its pool,
// alone or extended by candidates up to MaxStops).
func (m *model) enumerate() {
	ps := &permSearch{m: m}
	var nodes []int
	var mask uint32
	var rec func(from, room int, emit func())
	rec = func(from, room int, emit func()) {
		for c := from; c < m.nc; c++ {
			nodes = append(nodes, c+1)
			mask |= 1 << c
			emit()
			if room > 1 {
				rec(c+1, room-1, emit)
			}
			mask &^= 1 << c
			nodes = nodes[:len(nodes)-1]
		}
	}

	m.setLimits()
	m.enumerateFree()

	for li, ld := range m.locked {
		nodes, mask = append(nodes[:0], ld.pool...), 0
		ld.base = m.poolOnly(ps, ld.pool)
		ld.base.drv = int16(li)
		if room := m.p.MaxStops - len(ld.pool); room > 0 {
			rec(0, room, func() {
				if m.inSectorNodes(nodes) && ps.run(nodes, m.departOf(nodes), true) {
					m.runs = append(m.runs, runOpt{
						mask: mask, off: m.push(ps.best[:len(nodes)]), ns: uint8(len(nodes)), drv: int16(li),
						cost: ps.bestCost, minutes: ps.bestMin,
					})
				}
			})
		}
	}
}

// poolOnly is a locked driver's run with nothing added: the best order
// meeting the detour limit if one exists, else simply the cheapest (staff
// built this pool; AUTO never breaks it up).
func (m *model) poolOnly(ps *permSearch, pool []int) runOpt {
	depart := m.departOf(pool)
	var seq []int16
	if len(pool) <= maxPermStops {
		if !ps.run(pool, depart, true) {
			ps.run(pool, depart, false)
		}
		seq = ps.best[:len(pool)]
	} else {
		seq = m.nearestFirst(pool)
	}
	late, minutes, _ := m.seqEval(seq, depart)
	return runOpt{off: m.push(seq), ns: uint8(len(seq)), cost: late + m.p.Beta*minutes, minutes: minutes}
}

// nearestFirst orders a big pool greedily: always the closest next stop
// (ties: the older order, i.e. pool order).
func (m *model) nearestFirst(pool []int) []int16 {
	left := append([]int(nil), pool...)
	seq := make([]int16, 0, len(pool))
	prev := 0
	for len(left) > 0 {
		bi := 0
		for i := range left {
			if m.mx.t(prev, left[i]) < m.mx.t(prev, left[bi]) {
				bi = i
			}
		}
		prev = left[bi]
		seq = append(seq, int16(prev))
		left = append(left[:bi], left[bi+1:]...)
	}
	return seq
}

// enumerateFree builds every feasible free run in one DFS over stop
// sequences (not subsets): a prefix that breaks the detour limit is cut once
// for all the sets containing it, and each sequence is scored against the
// best order found so far for its set (slots by combinatorial rank). A
// prefix's late cost is reused while the departure (latest ready time)
// doesn't move.
func (m *model) enumerateFree() {
	n, K := m.nc, m.p.MaxStops
	binom := &m.binom
	for a := 0; a <= 32; a++ {
		binom[a][0] = 1
		for b := 1; b <= K+1 && b <= a; b++ {
			binom[a][b] = binom[a-1][b-1] + binom[a-1][b]
		}
	}
	base := &m.slotBase
	for k := 1; k <= K; k++ {
		base[k+1] = base[k] + binom[n][k]
	}
	total := base[K+1]
	cost := make([]float64, total)
	mins := make([]float64, total)
	seqs := make([]int16, total*MaxStops)
	for i := range cost {
		cost[i] = math.Inf(1)
	}
	S, beta, target := m.p.ServiceS, m.p.Beta, m.target
	var seq [MaxStops]int16
	var arr [MaxStops]float64
	var dfs func(level int, used uint32, prev int, t, depart, prefixLate float64)
	dfs = func(level int, used uint32, prev int, t, depart, prefixLate float64) {
		row := m.mx.sec[prev*m.mx.n : prev*m.mx.n+m.mx.n]
		for c := 0; c < n; c++ {
			if used&(1<<c) != 0 {
				continue
			}
			k := c + 1
			a := t + row[k]
			if a > m.limit[k] {
				continue
			}
			seq[level], arr[level] = int16(k), a
			if level > 0 && !m.inSector(seq[:level+1]) {
				continue // a superset can't fit the sector either
			}
			d := math.Max(depart, m.ready[k])
			late := prefixLate
			if d != depart { // later departure: every stop shifts
				late = 0
				for i := 0; i < level; i++ {
					late += m.p.late((d+arr[i]-m.placed[seq[i]])/60, target)
				}
			}
			late += m.p.late((d+a-m.placed[k])/60, target)
			ret := a + S + m.mx.sec[k*m.mx.n]
			total := late + beta*ret/60
			mask := used | 1<<c
			slot := base[level+1]
			for b, t := mask, 1; b != 0; b &= b - 1 {
				slot += binom[bits.TrailingZeros32(b)][t]
				t++
			}
			if total < cost[slot] {
				cost[slot], mins[slot] = total, ret/60
				copy(seqs[slot*MaxStops:], seq[:level+1])
			}
			if level+1 < K {
				dfs(level+1, mask, k, a+S, d, late)
			}
		}
	}
	dfs(0, 0, 0, 0, 0, 0)
	// Emit in combination order (by size, then colex rank) for determinism.
	m.slotRun = make([]int32, total)
	for k := 1; k <= K; k++ {
		for r := 0; r < binom[n][k]; r++ {
			slot := base[k] + r
			m.slotRun[slot] = -1
			if math.IsInf(cost[slot], 1) {
				continue
			}
			m.slotRun[slot] = int32(len(m.runs))
			sq := seqs[slot*MaxStops : slot*MaxStops+k]
			var mask uint32
			for _, nd := range sq {
				mask |= 1 << (nd - 1)
			}
			m.runs = append(m.runs, runOpt{
				mask: mask, off: m.push(sq), ns: uint8(k), drv: -1,
				cost: cost[slot], minutes: mins[slot],
			})
		}
	}
}

// freeRun is the free run carrying exactly mask, or -1 if none is feasible.
func (m *model) freeRun(mask uint32) int32 {
	k := bits.OnesCount32(mask)
	if k == 0 || k > m.p.MaxStops || m.slotRun == nil {
		return -1
	}
	slot := m.slotBase[k]
	for b, t := mask, 1; b != 0; b &= b - 1 {
		slot += m.binom[bits.TrailingZeros32(b)][t]
		t++
	}
	return m.slotRun[slot]
}

// setBearings computes each order's bearing from the store for the sector
// rule (NaN = exempt).
func (m *model) setBearings(pts []LatLng, store *LatLng) {
	m.bearing = make([]float64, len(pts))
	for k := range pts {
		m.bearing[k] = math.NaN()
		if k == 0 || store == nil || m.p.SectorDeg >= 360 || haversine(*store, pts[k]) <= m.p.SectorMinDistM {
			continue
		}
		rad := math.Pi / 180
		p1, p2 := store.Lat*rad, pts[k].Lat*rad
		dl := (pts[k].Lng - store.Lng) * rad
		y := math.Sin(dl) * math.Cos(p2)
		x := math.Cos(p1)*math.Sin(p2) - math.Sin(p1)*math.Cos(p2)*math.Cos(dl)
		b := math.Atan2(y, x) / rad
		if b < 0 {
			b += 360
		}
		m.bearing[k] = b
	}
}

// inSector reports whether the bearings of these nodes (NaN = exempt) fit
// in one SectorDeg-wide sector: 360 minus the largest gap between
// neighbouring bearings around the circle is the narrowest arc holding them.
func (m *model) inSector(nodes []int16) bool {
	var a [maxPermStops + MaxStops]float64
	n := 0
	for _, k := range nodes {
		if b := m.bearing[k]; !math.IsNaN(b) {
			a[n] = b
			n++
		}
	}
	if n < 2 {
		return true
	}
	v := a[:n]
	for i := 1; i < n; i++ { // insertion sort, n <= 4
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
	gap := v[0] + 360 - v[n-1]
	for i := 1; i < n; i++ {
		gap = math.Max(gap, v[i]-v[i-1])
	}
	return 360-gap <= m.p.SectorDeg+1e-9
}

func (m *model) inSectorNodes(nodes []int) bool {
	var buf [maxPermStops + MaxStops]int16
	for i, k := range nodes {
		buf[i] = int16(k)
	}
	return m.inSector(buf[:len(nodes)])
}
