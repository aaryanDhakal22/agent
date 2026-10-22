package dispatch

import (
	"cmp"
	"math"
	"math/bits"
	"slices"
)

// Primal heuristics: a good incumbent early is what lets the LP reduced
// costs and the node bounds prune. None of this affects exactness.

// greedy is the starting incumbent (and the plan when the budget runs out
// before anything better): locked drivers keep their pool as is; each free
// driver, in turn, takes among the runs holding the oldest unplaced
// candidate the one that saves the most against leaving its orders behind.
func (s *solver) greedy() { s.complete(nil) }

// complete extends a partial plan (disjoint runs) greedily as above and
// records it if it is valid and better.
func (s *solver) complete(picks []int32) {
	m := s.m
	var covered uint32
	used := 0
	for _, ri := range picks {
		covered |= m.runs[ri].mask
		if m.runs[ri].drv < 0 {
			used++
		}
	}
	for ; used < s.F; used++ {
		undec := s.all &^ covered
		if undec == 0 {
			break
		}
		i := bits.TrailingZeros32(undec)
		bi, bv := int32(-1), math.Inf(1)
		for ri := range m.runs {
			r := &m.runs[ri]
			if r.drv >= 0 || r.mask&(1<<i) == 0 || r.mask&covered != 0 {
				continue
			}
			v := r.cost
			for b := r.mask; b != 0; b &= b - 1 {
				v -= m.leftover[bits.TrailingZeros32(b)]
			}
			if v < bv {
				bi, bv = int32(ri), v
			}
		}
		picks = append(picks, bi) // the solo run always exists
		covered |= m.runs[bi].mask
	}
	if s.valid(picks) {
		s.record(s.improve(picks))
	}
}

// valid checks a heuristic plan against the rules the search enforces by
// construction: disjoint runs, <= F free runs, one option per locked
// driver, the anchor and the no-idle rule.
func (s *solver) valid(picks []int32) bool {
	var covered, lockUsed uint32
	used := 0
	for _, ri := range picks {
		r := &s.m.runs[ri]
		if r.mask&covered != 0 {
			return false
		}
		covered |= r.mask
		if r.drv < 0 {
			used++
		} else if lockUsed&(1<<r.drv) != 0 {
			return false
		} else {
			lockUsed |= 1 << r.drv
		}
	}
	if used > s.F || (s.F > 0 && s.all != 0 && covered&1 == 0) {
		return false
	}
	return covered == s.all || used == s.F
}

// round is the LP rounding heuristic: walk the LP's support from the
// largest value down, keeping each run that still fits the plan (disjoint,
// driver limits), then complete greedily. A good incumbent early is what
// lets reduced costs and bounds prune.
func (s *solver) round(live []int32, x []float64) {
	m := s.m
	order := make([]int, 0, 64)
	for k := range live {
		if x[k] > 1e-9 {
			order = append(order, k)
		}
	}
	slices.SortStableFunc(order, func(a, b int) int { return cmp.Compare(x[b], x[a]) })
	var picks []int32
	var covered, lockUsed uint32
	used := 0
	for _, k := range order {
		r := &m.runs[live[k]]
		if r.mask&covered != 0 || (r.drv < 0 && used == s.F) || (r.drv >= 0 && lockUsed&(1<<r.drv) != 0) {
			continue
		}
		picks = append(picks, live[k])
		covered |= r.mask
		if r.drv < 0 {
			used++
		} else {
			lockUsed |= 1 << r.drv
		}
	}
	s.complete(picks)
}

// runKey identifies a locked extension by its candidates and driver.
func runKey(mask uint32, drv int16) uint64 { return uint64(mask) | uint64(uint16(drv))<<32 }

// improve is a local search on a valid plan: re-split the orders of any
// two runs (incl. into one run, for a free driver), add a left-over order
// to a run, or swap one in for one of its stops — whatever lowers the true
// cost while keeping the plan valid — until nothing does.
func (s *solver) improve(picks []int32) []int32 {
	m := s.m
	if s.index == nil {
		s.index = map[uint64]int32{}
		for ri := range m.runs {
			if m.runs[ri].drv >= 0 {
				s.index[runKey(m.runs[ri].mask, m.runs[ri].drv)] = int32(ri)
			}
		}
	}
	picks = slices.Clone(picks)
	cur := s.planCost(picks)
	try := func(cand []int32) bool {
		if c := s.planCost(cand); c < cur-1e-9 && s.valid(cand) {
			cur = c
			picks = append(picks[:0], cand...)
			return true
		}
		return false
	}
	cand := make([]int32, 0, len(picks)+1)
	for rounds := 0; rounds < 50; rounds++ {
		improved := false
		// Re-split pairs of runs.
		for a := 0; a < len(picks) && !improved; a++ {
			for b := a + 1; b < len(picks) && !improved; b++ {
				ra, rb := &m.runs[picks[a]], &m.runs[picks[b]]
				U := ra.mask | rb.mask
				bestA, bestB, bestC := int32(-1), int32(-1), cur
				for S := U; ; S = (S - 1) & U { // every subset of U, U first
					ia, okA := s.lookup(S, ra.drv)
					ib, okB := s.lookup(U&^S, rb.drv)
					if okA && okB {
						cand = cand[:0]
						for k, ri := range picks {
							if k != a && k != b {
								cand = append(cand, ri)
							}
						}
						if ia >= 0 {
							cand = append(cand, ia)
						}
						if ib >= 0 {
							cand = append(cand, ib)
						}
						if c := s.planCost(cand); c < bestC-1e-9 && s.valid(cand) {
							bestA, bestB, bestC = ia, ib, c
						}
					}
					if S == 0 {
						break
					}
				}
				if bestC < cur-1e-9 {
					cand = cand[:0]
					for k, ri := range picks {
						if k != a && k != b {
							cand = append(cand, ri)
						}
					}
					if bestA >= 0 {
						cand = append(cand, bestA)
					}
					if bestB >= 0 {
						cand = append(cand, bestB)
					}
					improved = try(cand)
				}
			}
		}
		// Add / swap in a left-over order.
		var covered uint32
		for _, ri := range picks {
			covered |= m.runs[ri].mask
		}
		for left := s.all &^ covered; left != 0 && !improved; left &= left - 1 {
			j := uint32(1) << bits.TrailingZeros32(left)
			for a := 0; a < len(picks) && !improved; a++ {
				r := &m.runs[picks[a]]
				for out := r.mask | j; out != 0 && !improved; out &= out - 1 {
					x := uint32(1) << bits.TrailingZeros32(out)
					if x == j {
						x = 0 // pure add
					}
					if ri, ok := s.lookup(r.mask|j&^x, r.drv); ok && ri >= 0 {
						cand = append(cand[:0], picks...)
						cand[a] = ri
						improved = try(cand)
					}
				}
			}
		}
		if !improved {
			break
		}
	}
	return picks
}

// lookup finds the run carrying exactly mask for a driver kind; an empty
// mask is "no run" (-1, ok) — a free driver idles, a locked one drives its
// pool alone.
func (s *solver) lookup(mask uint32, drv int16) (int32, bool) {
	if mask == 0 {
		return -1, true
	}
	if drv < 0 {
		ri := s.m.freeRun(mask)
		return ri, ri >= 0
	}
	ri, ok := s.index[runKey(mask, drv)]
	return ri, ok
}
