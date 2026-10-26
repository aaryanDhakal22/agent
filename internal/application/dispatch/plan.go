package dispatch

import (
	"math"
	"sort"
	"time"
)

// model is one request turned into dense arrays. Times are seconds
// relative to Now.
type model struct {
	p      Params
	target float64 // minutes
	now    time.Time
	mx     *matrix
	ids    []string  // node ids; ids[0] = "" (store)
	placed []float64 // per node, <= 0 (an order placed "in the future" counts as now)
	ready  []float64 // per node, >= 0 (0 = ready now / unknown)
	direct []float64 // per node, store -> node
	limit  []float64 // per node, detour limit (seconds in the car)
	// bearing per node (degrees from the store), NaN when the sector rule
	// doesn't apply to it (within SectorMinDistM, or no store pin / rule off).
	bearing []float64
	nc      int // candidates are nodes 1..nc (bit i = node i+1), oldest first

	// The hold rule (hold.go): pts per node, holdOn = any rule but off,
	// holdRule = detour | distance when it also limits runs, holdFor = the
	// out driver a node may be held for (-1 none).
	pts      []LatLng
	hold     HoldRule
	holdOn   bool
	holdRule string
	holdFor  []int

	locked []*lockedDriver
	free   []int // indexes into Input.Drivers of the drivers without a pool, queue order

	leftover []float64 // per candidate: the cost of leaving it for later
	// runs are every feasible run carrying >= 1 candidate: free runs
	// (drv -1) and locked drivers' pool extensions (drv = locked index).
	runs  []runOpt
	arena []int16 // stop sequences of every runOpt
	// Free runs by candidate set: slotRun[slotBase[k] + colex rank] (-1 =
	// infeasible).
	binom    [33][MaxStops + 2]int
	slotBase [MaxStops + 2]int
	slotRun  []int32
}

type lockedDriver struct {
	drv  int    // index into Input.Drivers
	pool []int  // nodes of its pooled orders
	base runOpt // the pool alone
}

// runOpt is one feasible run with its best stop order.
type runOpt struct {
	mask    uint32 // candidates it carries (pool orders aren't candidates)
	off     int32  // stops = arena[off : off+ns]
	ns      uint8
	drv     int16 // -1 = any free driver, else the locked driver it extends
	cost    float64
	minutes float64
}

func (m *model) stops(r *runOpt) []int16 { return m.arena[r.off : r.off+int32(r.ns)] }

// Plan suggests one run per available driver. See the package doc and
// Params for the model; it is exact unless the time budget is hit
// (Output.Fallback).
func Plan(in Input) Output {
	start := time.Now()
	p := DefaultParams()
	if in.Params != nil {
		p = *in.Params
	}
	p = p.sane()
	budget := time.Duration(in.BudgetMs) * time.Millisecond
	if in.BudgetMs <= 0 {
		budget = 200 * time.Millisecond
	}
	m, out := build(&in, p)
	switch {
	case len(m.locked)+len(m.free) == 0:
		for c := 0; c < m.nc; c++ {
			out.Unassigned = append(out.Unassigned, m.leftOver(&in, c+1, ReasonNoDriver))
		}
	case m.nc > 0 || len(m.locked) > 0:
		s := newSolver(m, start.Add(budget))
		s.solve()
		m.emit(&in, s, &out)
	}
	sortUnassigned(&in, out.Unassigned)
	out.SolveMs = time.Since(start).Milliseconds()
	return out
}

// build classifies the orders, builds the matrix and enumerates the runs.
// Orders it rejects go straight to out.Unassigned.
func build(in *Input, p Params) (*model, Output) {
	out := Output{Runs: []Run{}, Unassigned: []Unassigned{}}
	target := float64(in.TargetMinutes)
	if target <= 0 {
		target = 45
	}
	soon := float64(max(in.SoonReadyMinutes, 0)) * 60
	now := in.Now

	// First occurrence of an id wins; later duplicates are ignored.
	byID := map[string]int{}
	for i, o := range in.Orders {
		if o.ID == "" {
			continue
		}
		if _, dup := byID[o.ID]; !dup {
			byID[o.ID] = i
		}
	}
	// Pools: an order's pool_driver_id wins; else the first available
	// driver listing it. A pool on a driver who isn't available is ignored
	// (not ours to plan).
	drvIdx := map[string]int{}
	for i, d := range in.Drivers {
		if _, dup := drvIdx[d.ID]; !dup && d.ID != "" {
			drvIdx[d.ID] = i
		}
	}
	poolOf := map[string]int{} // order id -> driver index
	for id, oi := range byID {
		if pd := in.Orders[oi].PoolDriverID; pd != "" {
			if di, ok := drvIdx[pd]; ok {
				poolOf[id] = di
			} else {
				poolOf[id] = -1 // someone else's pool
			}
		}
	}
	for di, d := range in.Drivers {
		if drvIdx[d.ID] != di {
			continue
		}
		for _, id := range d.Pool {
			if _, known := byID[id]; !known {
				continue
			}
			if _, taken := poolOf[id]; !taken {
				poolOf[id] = di
			}
		}
	}

	rel := func(t time.Time) float64 { return t.Sub(now).Seconds() }
	readyRel := func(o *Order) float64 {
		if o.Ready || o.ReadyAt.IsZero() {
			return 0
		}
		return math.Max(0, rel(o.ReadyAt))
	}

	// routable: located, and its store legs are known or estimable (a
	// missing store leg needs the store pin).
	routable := func(o *Order) bool {
		if !o.Located {
			return false
		}
		if in.Store != nil {
			return true
		}
		to, okTo := in.Times.Store.To[o.ID]
		from, okFrom := in.Times.Store.From[o.ID]
		return okTo && okFrom && to >= 0 && from >= 0
	}

	// Candidates: unpooled, routable, ready or ready soon.
	var cands []int
	for id, oi := range byID {
		if _, pooled := poolOf[id]; pooled {
			continue
		}
		o := &in.Orders[oi]
		switch {
		case !routable(o):
			out.Unassigned = append(out.Unassigned, Unassigned{OrderID: id, Reason: ReasonNoLocation})
		case !o.Ready && (o.ReadyAt.IsZero() || rel(o.ReadyAt) > soon):
			out.Unassigned = append(out.Unassigned, Unassigned{OrderID: id, Reason: ReasonNotReadySoon})
		default:
			cands = append(cands, oi)
		}
	}
	older := func(a, b int) bool {
		oa, ob := &in.Orders[a], &in.Orders[b]
		if !oa.PlacedAt.Equal(ob.PlacedAt) {
			return oa.PlacedAt.Before(ob.PlacedAt)
		}
		return oa.ID < ob.ID
	}
	sort.Slice(cands, func(i, j int) bool { return older(cands[i], cands[j]) })
	if len(cands) > p.MaxCandidates {
		for _, oi := range cands[p.MaxCandidates:] {
			out.Unassigned = append(out.Unassigned, Unassigned{OrderID: in.Orders[oi].ID, Reason: ReasonOverLimit})
		}
		cands = cands[:p.MaxCandidates]
	}

	// Drivers: pooled ones are locked to a superset of their pool; a pool
	// holding an order we can't route is left exactly as is
	// (the driver gets no suggestion).
	pools := make([][]int, len(in.Drivers)) // order indexes, oldest first
	for id, di := range poolOf {
		if di >= 0 {
			pools[di] = append(pools[di], byID[id])
		}
	}
	m := &model{p: p, target: target, now: now}
	nodeOrders := []int{-1} // node -> order index (store = -1)
	nodeOrders = append(nodeOrders, cands...)
	m.nc = len(cands)
	var lockedNodes [][]int
	var lockedDrv []int
	for di, d := range in.Drivers {
		if drvIdx[d.ID] != di {
			continue
		}
		pool := pools[di]
		if len(pool) == 0 {
			m.free = append(m.free, di)
			continue
		}
		ok := true
		for _, oi := range pool {
			ok = ok && routable(&in.Orders[oi])
		}
		if !ok {
			continue
		}
		sort.Slice(pool, func(i, j int) bool { return older(pool[i], pool[j]) })
		var nodes []int
		for _, oi := range pool {
			nodes = append(nodes, len(nodeOrders))
			nodeOrders = append(nodeOrders, oi)
		}
		lockedNodes = append(lockedNodes, nodes)
		lockedDrv = append(lockedDrv, di)
	}

	n := len(nodeOrders)
	m.ids = make([]string, n)
	pts := make([]LatLng, n)
	m.placed = make([]float64, n)
	m.ready = make([]float64, n)
	for k := 1; k < n; k++ {
		o := &in.Orders[nodeOrders[k]]
		m.ids[k] = o.ID
		pts[k] = LatLng{o.Lat, o.Lng}
		m.placed[k] = math.Min(0, rel(o.PlacedAt))
		m.ready[k] = readyRel(o)
	}
	m.mx = buildMatrix(m.ids, pts, in.Store, &in.Times, &p)
	m.setBearings(pts, in.Store)

	for li, nodes := range lockedNodes {
		m.locked = append(m.locked, &lockedDriver{drv: lockedDrv[li], pool: nodes})
	}

	m.direct = make([]float64, n)
	for k := 1; k < n; k++ {
		m.direct[k] = m.mx.t(0, k)
	}
	m.leftover = make([]float64, m.nc)
	wait := p.TypicalRunMin * 60
	if len(in.OutDrivers) > 0 {
		wait = math.Inf(1)
		for _, d := range in.OutDrivers {
			wait = math.Min(wait, math.Max(0, rel(d.ReturnAt)))
		}
	}
	for c := 0; c < m.nc; c++ {
		k := c + 1
		door := math.Max(wait, m.ready[k]) + m.direct[k]
		solo := m.direct[k] + p.ServiceS + m.mx.t(k, 0)
		m.leftover[c] = p.late((door-m.placed[k])/60, target) + p.Beta*solo/60 + p.M
	}
	m.setHold(in, pts)
	if len(m.free)+len(m.locked) > 0 {
		m.enumerate()
	}
	return m, out
}

func sortUnassigned(in *Input, u []Unassigned) {
	placed := map[string]time.Time{}
	for _, o := range in.Orders {
		if _, dup := placed[o.ID]; !dup {
			placed[o.ID] = o.PlacedAt
		}
	}
	sort.SliceStable(u, func(i, j int) bool {
		a, b := placed[u[i].OrderID], placed[u[j].OrderID]
		if !a.Equal(b) {
			return a.Before(b)
		}
		return u[i].OrderID < u[j].OrderID
	})
}
