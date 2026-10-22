package dispatch

import (
	"cmp"
	"math"
	"math/bits"
	"slices"
	"sort"
	"time"
)

// solver picks the cheapest plan: each locked driver gets its pool alone or
// one of its pool extensions, at most F (free drivers) free runs, all runs
// disjoint, every other candidate left over. Two hard rules on top of the
// cost:
//   - anchor: when a free driver exists, the oldest candidate is placed;
//   - no idle driver: an order is only left over when every free driver
//     got a run.
//
// The balance term Lambda*(most stops - fewest stops) is not additive, so
// the search runs once per stop-count window [lo, hi]: only runs with lo..hi
// stops, balance charged as Lambda*(hi-lo). Every plan is searched in its
// own exact window, where that charge is exact, so the minimum over windows
// is the optimum (a plan found in a wider window is recorded at its true,
// lower cost). Inside a window the objective is additive:
//
//   - LP relaxation (set partitioning + "<= F free runs" + "one option per
//     locked driver", by simplex): once over every run (its duals bound
//     and order the windows), then per searched window. LP + a run's
//     reduced cost >= incumbent removes the run;
//   - branch & bound: take the oldest undecided candidate i; it is either
//     the oldest member of a run whose members are all undecided, or left
//     over (each plan is generated once). Two bounds, both valid at every
//     node, so the search is exact: rcBound (every plan costs LP + the
//     reduced costs of its columns; each undecided candidate carries at
//     least its cheapest rc share) and bound (the Lagrangian at the LP
//     duals with the run sizes kept exact — it sees what the LP can't:
//     25 orders don't split into 4-stop runs — and the no-idle rule);
//   - primal heuristics (heuristic.go): greedy, LP rounding, and a local
//     search polishing every incumbent;
//   - a memo prunes a partial plan reaching an already-seen state (decided
//     set, free runs used, locked drivers extended, anything left) at no
//     lower cost.
//
// Free drivers are interchangeable here; turn order is applied by emit.
type solver struct {
	m   *model
	F   int    // free drivers
	L   int    // locked drivers
	all uint32 // all candidate bits

	best     float64 // incumbent's true cost
	bestPick []int32 // incumbent's runs (indexes into m.runs)
	pick     []int32 // search state

	rootLP  float64   // root LP optimum (-Inf when not solved)
	rootRC  []float64 // per run: root LP reduced cost (0 when not solved)
	u, minU []float64 // multipliers in use; min(u_j, leftover_j)
	rc      []float64 // per run: cost - sum of u over its candidates

	// The window being searched.
	width               float64                 // Lambda*(hi-lo)
	baseC               []float64               // per locked driver: pool-alone cost (+Inf outside the window)
	byLow               [][]int32               // live runs by oldest member, ascending rc
	bySize              [MaxStops + 1][]int32   // live free runs by size, ascending rc
	sizeFrom            [][MaxStops + 1][]int32 // [i][size]: those whose oldest order is >= i
	diff                []float64               // leftover_j - u_j (+Inf: anchor)
	diffOrder           []int                   // candidates by ascending diff
	lockBy              [][MaxStops + 1][]int32 // per locked driver: live extensions by orders added, ascending rc
	diffs               []float64               // bound scratch
	hAny                []float64               // bound scratch: free runs, any count
	hFull               []float64               // bound scratch: free runs, all R used
	loc, nl             []float64               // bound scratch: locked additions
	seen                map[uint64]float64
	pi                  []float64        // the window LP's duals (nil: not solved)
	shareAll, shareLock [][]float64      // rcBound tables [i][j]
	index               map[uint64]int32 // run by candidates + driver (local search)

	nodes    int
	windows  int                  // windows searched
	trace    func(string, ...any) // test hook
	deadline time.Time
	aborted  bool
}

// window is one stop-count window.
type window struct {
	lo, hi int
	lb     float64 // root Lagrangian bound + Lambda*(hi-lo)
}

// maxSeen caps the memo (memory on the Pi); past it, new states are not
// remembered (still exact, just less pruning).
const maxSeen = 1 << 18

func newSolver(m *model, deadline time.Time) *solver {
	s := &solver{m: m, F: len(m.free), L: len(m.locked), deadline: deadline, best: math.Inf(1)}
	if m.nc > 0 {
		s.all = uint32(1)<<m.nc - 1
	}
	s.u = make([]float64, m.nc)
	s.minU = make([]float64, m.nc)
	s.rc = make([]float64, len(m.runs))
	s.rootRC = make([]float64, len(m.runs))
	s.baseC = make([]float64, s.L)
	s.diffs = make([]float64, 0, m.nc)
	s.hAny = make([]float64, m.nc+1)
	s.hFull = make([]float64, m.nc+1)
	s.loc = make([]float64, m.nc+1)
	s.nl = make([]float64, m.nc+1)
	return s
}

func (s *solver) solve() {
	s.greedy()
	m := s.m
	var present [256]bool
	for i := range m.runs {
		present[m.runs[i].ns] = true
	}
	for _, ld := range m.locked {
		present[ld.base.ns] = true
	}
	var vals []int
	for v, ok := range present {
		if ok {
			vals = append(vals, v)
		}
	}
	// The root LP over every run.
	every := make([]int32, len(m.runs))
	for ri := range every {
		every[ri] = int32(ri)
	}
	s.rootLP = math.Inf(-1)
	if pi, val, ok := s.solveLP(every, 0, math.MaxInt); ok {
		s.rootLP = val
		s.setMultipliers(pi, every)
		copy(s.rootRC, s.rc)
		for ri := range m.runs {
			s.rootRC[ri] -= pi[s.row(&m.runs[ri])]
		}
	} else {
		s.setMultipliers(nil, every)
	}
	var wins []window
	for a, lo := range vals {
		for _, hi := range vals[a:] {
			width := m.p.Lambda * float64(hi-lo)
			if s.rootLP+width >= s.best-1e-9 {
				continue
			}
			if lb := s.windowBound(lo, hi) + width; lb < s.best-1e-9 {
				wins = append(wins, window{lo, hi, lb})
			}
		}
	}
	sort.SliceStable(wins, func(a, b int) bool { return wins[a].lb < wins[b].lb })
	for _, w := range wins {
		if w.lb >= s.best-1e-9 || s.timeUp() {
			break
		}
		s.search(w)
	}
}

// row is the LP row of a run's driver: the free-run count or its locked driver.
func (s *solver) row(r *runOpt) int {
	if r.drv < 0 {
		return s.m.nc
	}
	return s.m.nc + 1 + int(r.drv)
}

func inWindow(ns uint8, lo, hi int) bool { return int(ns) >= lo && int(ns) <= hi }

// windowBound is the root Lagrangian bound restricted to a window's runs
// (the current multipliers are the root ones).
func (s *solver) windowBound(lo, hi int) float64 {
	m := s.m
	lb := 0.0
	for _, v := range s.minU {
		lb += v
	}
	// The F most negative free runs, by selection.
	top := make([]float64, 0, s.F+1)
	lock := make([]float64, s.L)
	for k, ld := range m.locked {
		lock[k] = math.Inf(1)
		if inWindow(ld.base.ns, lo, hi) {
			lock[k] = ld.base.cost
		}
	}
	for ri := range m.runs {
		r := &m.runs[ri]
		if !inWindow(r.ns, lo, hi) {
			continue
		}
		v := s.rc[ri]
		if r.drv >= 0 {
			lock[r.drv] = math.Min(lock[r.drv], v)
			continue
		}
		if v >= 0 || s.F == 0 || (len(top) == s.F && v >= top[s.F-1]) {
			continue
		}
		p, _ := slices.BinarySearch(top, v)
		top = slices.Insert(top, p, v)
		if len(top) > s.F {
			top = top[:s.F]
		}
	}
	for _, v := range top {
		lb += v
	}
	for _, v := range lock {
		lb += v
	}
	return lb
}

func (s *solver) timeUp() bool {
	if !s.aborted && time.Now().After(s.deadline) {
		s.aborted = true
	}
	return s.aborted
}

// solveLP solves the LP relaxation over the given runs: rows = candidates
// (covered once, or left over), free-run count (<= F, slack), locked
// drivers (one option each; the pool alone only when its stop count is in
// [lo, hi]).
func (s *solver) solveLP(live []int32, lo, hi int) ([]float64, float64, bool) {
	pi, val, _, ok := s.lpSub(live, lo, hi, 0, 0, 0)
	return pi, val, ok
}

// lpSub is the LP of the subproblem left once the covered candidates,
// used free drivers and extended locked drivers are fixed (their rows get
// a zero right-hand side; runs touching them are skipped). Also returns the
// primal values of live (0 for skipped runs).
func (s *solver) lpSub(live []int32, lo, hi int, covered uint32, used int, lockUsed uint32) ([]float64, float64, []float64, bool) {
	m := s.m
	nc, L := m.nc, s.L
	rows := nc + 1 + L
	b := make([]float64, rows)
	cols := make([]lpCol, rows, rows+len(live))
	for j := range nc {
		if covered&(1<<j) == 0 {
			b[j] = 1
		}
		cols[j] = lpCol{rows: [6]int16{int16(j)}, n: 1, cost: m.leftover[j]}
	}
	b[nc] = float64(s.F - used)
	cols[nc] = lpCol{rows: [6]int16{int16(nc)}, n: 1}
	for k, ld := range m.locked {
		if lockUsed&(1<<k) == 0 {
			b[nc+1+k] = 1
		}
		c := ld.base.cost
		if !inWindow(ld.base.ns, lo, hi) {
			c = 1e7 // outside the window: only an extension fits
		}
		cols[nc+1+k] = lpCol{rows: [6]int16{int16(nc + 1 + k)}, n: 1, cost: c}
	}
	idx := make([]int, 0, len(live))
	for k, ri := range live {
		r := &m.runs[ri]
		if r.mask&covered != 0 || (r.drv < 0 && used >= s.F) || (r.drv >= 0 && lockUsed&(1<<r.drv) != 0) {
			continue
		}
		idx = append(idx, k)
		var c lpCol
		for bm := r.mask; bm != 0; bm &= bm - 1 {
			c.rows[c.n] = int16(bits.TrailingZeros32(bm))
			c.n++
		}
		c.rows[c.n] = int16(s.row(r))
		c.n++
		c.cost = r.cost
		cols = append(cols, c)
	}
	val, pi, xs, ok := solveLP(rows, b, cols)
	if !ok {
		return nil, 0, nil, false
	}
	x := make([]float64, len(live))
	for c, k := range idx {
		x[k] = xs[rows+c]
	}
	if covered == 0 {
		s.round(live, x)
	}
	return pi, val, x, true
}

func (s *solver) setMultipliers(pi []float64, runs []int32) {
	m := s.m
	if pi != nil {
		copy(s.u, pi[:m.nc])
	} else {
		copy(s.u, m.leftover)
		for _, ri := range runs {
			r := &m.runs[ri]
			sh := r.cost / float64(r.ns)
			for b := r.mask; b != 0; b &= b - 1 {
				j := bits.TrailingZeros32(b)
				s.u[j] = math.Min(s.u[j], sh)
			}
		}
	}
	for j := range s.u {
		s.minU[j] = math.Min(s.u[j], m.leftover[j])
	}
	for _, ri := range runs {
		r := &m.runs[ri]
		v := r.cost
		for b := r.mask; b != 0; b &= b - 1 {
			v -= s.u[bits.TrailingZeros32(b)]
		}
		s.rc[ri] = v
	}
}

// search runs the branch & bound of one window.
func (s *solver) search(w window) {
	m := s.m
	s.windows++
	s.width = m.p.Lambda * float64(w.hi-w.lo)
	// Live runs: in the window and not ruled out by the root LP.
	var live []int32
	for ri := range m.runs {
		if inWindow(m.runs[ri].ns, w.lo, w.hi) && s.rootLP+s.rootRC[ri]+s.width < s.best-1e-9 {
			live = append(live, int32(ri))
		}
	}
	// The window's own LP: tighter (it knows which stop counts are out).
	if pi, val, ok := s.solveLP(live, w.lo, w.hi); ok {
		if s.trace != nil {
			s.trace("window [%d,%d] lb=%.2f lp=%.2f+%.0f live=%d best=%.2f", w.lo, w.hi, w.lb, val, s.width, len(live), s.best)
		}
		if val+s.width >= s.best-1e-9 {
			return
		}
		s.setMultipliers(pi, live)
		s.pi = pi
		kept := live[:0]
		for _, ri := range live {
			if val+s.rc[ri]-pi[s.row(&m.runs[ri])]+s.width < s.best-1e-9 {
				kept = append(kept, ri)
			}
		}
		live = kept
	} else {
		s.setMultipliers(nil, live)
		s.pi = nil
	}
	s.buildLists(w, live)
	s.buildShares(live)
	if s.trace != nil {
		n0 := s.nodes
		defer func() { s.trace("  nodes=%d best=%.2f", s.nodes-n0, s.best) }()
	}
	s.seen = make(map[uint64]float64)
	s.pick = s.pick[:0]
	s.rec(0, 0, 0, 0, false)
}

// buildLists prepares the search's run lists for a window.
func (s *solver) buildLists(w window, live []int32) {
	m := s.m
	nc := m.nc
	s.byLow = make([][]int32, nc)
	for sz := range s.bySize {
		s.bySize[sz] = s.bySize[sz][:0]
	}
	s.lockBy = make([][MaxStops + 1][]int32, s.L)
	for k, ld := range m.locked {
		s.baseC[k] = math.Inf(1)
		if inWindow(ld.base.ns, w.lo, w.hi) {
			s.baseC[k] = ld.base.cost
		}
	}
	for _, ri := range live {
		r := &m.runs[ri]
		low := bits.TrailingZeros32(r.mask)
		s.byLow[low] = append(s.byLow[low], ri)
		add := bits.OnesCount32(r.mask)
		if r.drv < 0 {
			s.bySize[add] = append(s.bySize[add], ri)
		} else {
			s.lockBy[r.drv][add] = append(s.lockBy[r.drv][add], ri)
		}
	}
	s.sortLists()
	s.sizeFrom = make([][MaxStops + 1][]int32, nc)
	for sz := 1; sz <= MaxStops; sz++ {
		for _, ri := range s.bySize[sz] {
			low := bits.TrailingZeros32(m.runs[ri].mask)
			for i := 0; i <= low; i++ {
				s.sizeFrom[i][sz] = append(s.sizeFrom[i][sz], ri)
			}
		}
	}
	s.diff = make([]float64, nc)
	s.diffOrder = make([]int, nc)
	for j := range nc {
		s.diff[j] = m.leftover[j] - s.u[j]
		if j == 0 && s.F > 0 {
			s.diff[j] = math.Inf(1) // anchor: the oldest is never left over
		}
		s.diffOrder[j] = j
	}
	slices.SortStableFunc(s.diffOrder, func(a, b int) int { return cmp.Compare(s.diff[a], s.diff[b]) })
}

// sortLists orders the run lists by the current reduced costs.
func (s *solver) sortLists() {
	byRC := func(a, b int32) int {
		if c := cmp.Compare(s.rc[a], s.rc[b]); c != 0 {
			return c
		}
		return cmp.Compare(a, b)
	}
	for _, l := range s.byLow {
		slices.SortFunc(l, byRC)
	}
	for _, l := range s.bySize {
		slices.SortFunc(l, byRC)
	}
	for k := range s.lockBy {
		for _, l := range s.lockBy[k] {
			slices.SortFunc(l, byRC)
		}
	}
}

func (s *solver) tick() bool {
	s.nodes++
	if s.nodes&255 == 0 {
		s.timeUp()
	}
	return s.aborted
}

// planCost is the true objective of a plan given by its runs (locked
// drivers not in it drive their pool alone; uncovered candidates are left
// over).
func (s *solver) planCost(picks []int32) float64 {
	m := s.m
	cost, covered, lockUsed := 0.0, uint32(0), uint32(0)
	lo, hi, nr := math.MaxInt, 0, 0
	add := func(r *runOpt) {
		cost += r.cost
		lo, hi, nr = min(lo, int(r.ns)), max(hi, int(r.ns)), nr+1
	}
	for _, ri := range picks {
		r := &m.runs[ri]
		add(r)
		covered |= r.mask
		if r.drv >= 0 {
			lockUsed |= 1 << r.drv
		}
	}
	for k := range m.locked {
		if lockUsed&(1<<k) == 0 {
			add(&m.locked[k].base)
		}
	}
	for b := s.all &^ covered; b != 0; b &= b - 1 {
		cost += m.leftover[bits.TrailingZeros32(b)]
	}
	if nr >= 2 {
		cost += m.p.Lambda * float64(hi-lo)
	}
	return cost
}

func (s *solver) record(picks []int32) {
	if c := s.planCost(picks); c < s.best-1e-9 {
		s.best = c
		s.bestPick = append(s.bestPick[:0], picks...)
		if s.trace != nil {
			s.trace("    found %.2f at node %d", c, s.nodes)
		}
	}
}

// rec decides the oldest undecided candidate; cost is the additive part
// so far (runs chosen + candidates left over).
func (s *solver) rec(dec uint32, cost float64, used int, lockUsed uint32, left bool) {
	if s.tick() {
		return
	}
	m := s.m
	if dec == s.all {
		if left && used < s.F {
			return // an order left behind while a driver idles
		}
		if s.planCost(s.pick) < s.best-1e-9 {
			s.record(s.improve(s.pick)) // polish it: a better incumbent prunes more
		}
		return
	}
	undec := s.all &^ dec
	R := s.F - used
	i := bits.TrailingZeros32(undec)
	// Two valid bounds, the cheap reduced-cost one first.
	if cost+s.width+s.rcBound(undec, i, R, lockUsed) >= s.best-1e-9 ||
		cost+s.width+s.bound(dec, undec, R, lockUsed, left) >= s.best-1e-9 {
		return
	}
	if s.L <= 22 { // the key has room for 22 locked drivers
		key := uint64(dec) | uint64(used)<<32 | b2u(left)<<40 | uint64(lockUsed)<<41
		if prev, ok := s.seen[key]; ok && prev <= cost+1e-9 {
			return
		} else if ok || len(s.seen) < maxSeen {
			s.seen[key] = cost
		}
	}
	for _, ri := range s.byLow[i] {
		r := &m.runs[ri]
		if r.mask&dec != 0 {
			continue
		}
		nu, nl := used, lockUsed
		if r.drv < 0 {
			if used == s.F {
				continue
			}
			nu++
		} else {
			if lockUsed&(1<<r.drv) != 0 {
				continue
			}
			nl |= 1 << r.drv
		}
		s.pick = append(s.pick, ri)
		s.rec(dec|r.mask, cost+r.cost, nu, nl, left)
		s.pick = s.pick[:len(s.pick)-1]
		if s.aborted {
			return
		}
	}
	// Leave i for later: never the oldest candidate while a free driver
	// exists (anchor), and only if the others can still give every
	// remaining free driver a run.
	if i == 0 && s.F > 0 {
		return
	}
	if bits.OnesCount32(undec)-1 < R {
		return
	}
	s.rec(dec|1<<i, cost+m.leftover[i], used, lockUsed, true)
}

// buildShares prepares rcBound: with the window LP's duals pi every column
// (run, left-over order, idle-driver slack, pool alone) has a reduced cost
// rc >= 0, and any plan costs exactly pi·b + sum of its columns' rc. Each
// undecided candidate will be covered by some column, so it carries at
// least the cheapest rc-per-order of a column holding it (a run whose
// oldest order is >= i, or being left over).
func (s *solver) buildShares(live []int32) {
	m := s.m
	nc := m.nc
	if s.pi == nil {
		s.shareAll, s.shareLock = nil, nil
		return
	}
	inf := math.Inf(1)
	mk := func() [][]float64 {
		t := make([][]float64, nc+1)
		for i := range t {
			t[i] = make([]float64, nc)
			for j := range t[i] {
				t[i][j] = inf
			}
		}
		return t
	}
	all, lock := mk(), mk()
	for _, ri := range live {
		r := &m.runs[ri]
		sh := (s.rc[ri] - s.pi[s.row(r)]) / float64(bits.OnesCount32(r.mask))
		low := bits.TrailingZeros32(r.mask)
		for b := r.mask; b != 0; b &= b - 1 {
			j := bits.TrailingZeros32(b)
			all[low][j] = math.Min(all[low][j], sh)
			if r.drv >= 0 {
				lock[low][j] = math.Min(lock[low][j], sh)
			}
		}
	}
	for i := nc - 1; i >= 0; i-- {
		for j := range nc {
			all[i][j] = math.Min(all[i][j], all[i+1][j])
			lock[i][j] = math.Min(lock[i][j], lock[i+1][j])
		}
	}
	for i := range nc {
		for j := range nc {
			lr := m.leftover[j] - s.pi[j]
			if j == 0 && s.F > 0 {
				lr = inf // anchor
			}
			all[i][j] = math.Min(all[i][j], lr) + s.pi[j]
			lock[i][j] = math.Min(lock[i][j], lr) + s.pi[j]
		}
	}
	s.shareAll, s.shareLock = all, lock
}

// rcBound is the reduced-cost bound (see buildShares) on finishing from a
// node whose additive cost so far is accounted for by the caller:
//
//	sum_U (pi_j + share_j) + R*pi_F + sum_unextended pi_k + (idle slack >= R-|U|)*(-pi_F)
func (s *solver) rcBound(undec uint32, i, R int, lockUsed uint32) float64 {
	if s.pi == nil {
		return math.Inf(-1)
	}
	m := s.m
	t := s.shareAll[i]
	if R == 0 {
		t = s.shareLock[i]
	}
	lb, U := 0.0, 0
	for b := undec; b != 0; b &= b - 1 {
		lb += t[bits.TrailingZeros32(b)]
		U++
	}
	piF := s.pi[m.nc]
	lb += float64(R) * piF
	if R > U {
		lb -= float64(R-U) * piF
	}
	for k := range s.L {
		if lockUsed&(1<<k) == 0 {
			lb += s.pi[m.nc+1+k]
		}
	}
	return lb
}

func b2u(b bool) uint64 {
	if b {
		return 1
	}
	return 0
}

// bound is a lower bound on the additive cost of deciding the undecided
// candidates with R free runs left: the Lagrangian of "each candidate
// decided once" at the multipliers u, with the counts kept exact. For any
// completion that leaves L candidates over,
//
//	cost = sum_U u_j + sum_left (leftover_j - u_j) + sum_runs (cost_r - sum_{j in r} u_j)
//
// and the runs' sizes (free runs' orders + locked additions) add up to
// exactly |U| - L, with all R free drivers used once anything is left over.
// So: the L smallest (leftover_j - u_j), plus the cheapest choice of
// distinct available runs — by size, up to R free ones, one option per
// unextended locked driver — whose sizes total |U| - L; minimised over L.
// This sees what an LP can't: 25 orders don't split into 4-stop runs.
func (s *solver) bound(dec, undec uint32, R int, lockUsed uint32, left bool) float64 {
	m := s.m
	U := bits.OnesCount32(undec)
	sumU := 0.0
	s.diffs = s.diffs[:0]
	for b := undec; b != 0; b &= b - 1 {
		sumU += s.u[bits.TrailingZeros32(b)]
	}
	for _, j := range s.diffOrder { // ascending leftover_j - u_j
		if undec&(1<<j) != 0 {
			s.diffs = append(s.diffs, s.diff[j])
		}
	}
	// The R cheapest available free runs of each size (runs whose oldest
	// order is before the oldest undecided one are all blocked).
	i0 := bits.TrailingZeros32(undec)
	var vals [MaxStops + 1][]float64
	var buf [(MaxStops + 1) * 33]float64
	for sz := 1; sz <= MaxStops; sz++ {
		v := buf[sz*33 : sz*33]
		if sz <= U {
			lim := min(R, U/sz) // more never fit
			for _, ri := range s.sizeFrom[i0][sz] {
				if len(v) >= lim {
					break
				}
				if m.runs[ri].mask&dec == 0 {
					v = append(v, s.rc[ri])
				}
			}
		}
		vals[sz] = v
	}
	inf := math.Inf(1)
	hAny, hFull := s.hAny[:U+1], s.hFull[:U+1]
	for i := range hAny {
		hAny[i], hFull[i] = inf, inf
	}
	// Every count vector (n1..n4), sum <= R: prefix sums per size.
	var pre [MaxStops + 1][34]float64
	for sz := 1; sz <= MaxStops; sz++ {
		for n, v := range vals[sz] {
			pre[sz][n+1] = pre[sz][n] + v
		}
	}
	for n1 := 0; n1 <= len(vals[1]); n1++ {
		for n2 := 0; n2 <= len(vals[2]) && n1+n2 <= R; n2++ {
			s2 := n1 + 2*n2
			if s2 > U {
				break
			}
			for n3 := 0; n3 <= len(vals[3]) && n1+n2+n3 <= R; n3++ {
				s3 := s2 + 3*n3
				if s3 > U {
					break
				}
				c3 := pre[1][n1] + pre[2][n2] + pre[3][n3]
				for n4 := 0; n4 <= len(vals[4]) && n1+n2+n3+n4 <= R; n4++ {
					size := s3 + 4*n4
					if size > U {
						break
					}
					c := c3 + pre[4][n4]
					if c < hAny[size] {
						hAny[size] = c
					}
					if n1+n2+n3+n4 == R && c < hFull[size] {
						hFull[size] = c
					}
				}
			}
		}
	}
	// Locked drivers not extended yet: their pool alone, or the cheapest
	// available extension adding a orders.
	loc := s.loc[:U+1]
	for i := range loc {
		loc[i] = inf
	}
	loc[0] = 0
	for k := range s.L {
		if lockUsed&(1<<k) != 0 {
			continue
		}
		var opt [MaxStops + 1]float64
		opt[0] = s.baseC[k]
		for a := 1; a <= MaxStops; a++ {
			opt[a] = inf
			for _, ri := range s.lockBy[k][a] {
				if m.runs[ri].mask&dec == 0 {
					opt[a] = s.rc[ri]
					break
				}
			}
		}
		nl := s.nl[:U+1]
		for i := range nl {
			nl[i] = inf
		}
		for A, c := range loc {
			if math.IsInf(c, 1) {
				continue
			}
			for a, o := range opt {
				if A+a <= U && c+o < nl[A+a] {
					nl[A+a] = c + o
				}
			}
		}
		copy(loc, nl)
	}
	best, prefix := inf, 0.0
	for L := 0; L <= U; L++ {
		if L > 0 {
			prefix += s.diffs[L-1]
		}
		h := hFull
		if L == 0 && !left {
			h = hAny
		}
		for A, c := range loc[:U-L+1] {
			if v := prefix + c + h[U-L-A]; v < best {
				best = v
			}
		}
	}
	return sumU + best
}
