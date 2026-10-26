package dispatch

import (
	"math"
	"math/bits"
	"sort"
	"time"
)

// emit turns the solver's best selection into Output runs (driver queue
// order) and the left-over candidates into Unassigned.
//
// Turn order: a locked driver keeps its own run; the free runs, sorted by
// their oldest order, go to the free drivers in queue order — the next
// driver gets the run with the oldest waiting delivery.
func (m *model) emit(in *Input, s *solver, out *Output) {
	byDriver := map[int]*runOpt{}
	var covered uint32
	var free []int32
	for _, ri := range s.bestPick {
		r := &m.runs[ri]
		covered |= r.mask
		if r.drv >= 0 {
			byDriver[m.locked[r.drv].drv] = r
		} else {
			free = append(free, ri)
		}
	}
	for _, ld := range m.locked {
		if _, ext := byDriver[ld.drv]; !ext {
			byDriver[ld.drv] = &ld.base
		}
	}
	sort.Slice(free, func(a, b int) bool {
		return bits.TrailingZeros32(m.runs[free[a]].mask) < bits.TrailingZeros32(m.runs[free[b]].mask)
	})
	for n, ri := range free {
		byDriver[m.free[n]] = &m.runs[ri]
	}
	for di := range in.Drivers {
		r, ok := byDriver[di]
		if !ok {
			continue
		}
		out.Runs = append(out.Runs, m.runOut(in.Drivers[di].ID, r, &out.Estimated))
	}
	for c := 0; c < m.nc; c++ {
		if covered&(1<<c) != 0 {
			continue
		}
		reason := ReasonNoDriver
		if s.F == 0 && !m.lockedCanTake(c) {
			reason = ReasonNoFeasibleRun
		}
		out.Unassigned = append(out.Unassigned, m.leftOver(in, c+1, reason))
	}
	out.Cost = s.best
	out.Fallback = s.aborted
}

func (m *model) runOut(driverID string, r *runOpt, estimated *bool) Run {
	seq := m.stops(r)
	nodes := make([]int, len(seq))
	for i, k := range seq {
		nodes[i] = int(k)
	}
	depart := m.departOf(nodes)
	at := func(sec float64) time.Time {
		return m.now.Add(time.Duration(math.Round(sec)) * time.Second)
	}
	run := Run{DriverID: driverID, OrderIDs: []string{}, SuggestedIDs: []string{}, Stops: []Stop{}, Cost: r.cost, DepartAt: at(depart)}
	t, drive, prev := 0.0, 0.0, 0
	for _, k := range nodes {
		leg := m.mx.t(prev, k)
		*estimated = *estimated || m.mx.isEst(prev, k)
		t += leg
		drive += leg
		x := depart + t - m.placed[k]
		run.OrderIDs = append(run.OrderIDs, m.ids[k])
		if k <= m.nc { // a candidate, i.e. not already in this driver's pool
			run.SuggestedIDs = append(run.SuggestedIDs, m.ids[k])
		}
		ptd := int(math.Round(x))
		run.Stops = append(run.Stops, Stop{
			OrderID: m.ids[k], ETA: at(depart + t),
			PlacedToDoorS: ptd, Late: float64(ptd) > m.target*60,
		})
		t += m.p.ServiceS
		prev = k
	}
	leg := m.mx.t(prev, 0)
	*estimated = *estimated || m.mx.isEst(prev, 0)
	drive += leg
	run.ReturnAt = at(depart + t + leg)
	run.DriveS = int(math.Round(drive))
	return run
}

// lockedCanTake reports whether some locked pool extension carries
// candidate c (with no free driver, the only way it could have gone).
func (m *model) lockedCanTake(c int) bool {
	for ri := range m.runs {
		if m.runs[ri].drv >= 0 && m.runs[ri].mask&(1<<c) != 0 {
			return true
		}
	}
	return false
}
