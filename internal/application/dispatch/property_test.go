package dispatch

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"reflect"
	"slices"
	"testing"
	"time"
)

// TestProperties checks the plan's rules on many random instances (sizes up
// to the 25-candidate cap and beyond, pools, cooking and unlocated orders,
// out drivers, duplicate addresses, tuning changes).
func TestProperties(t *testing.T) {
	n := 10000
	if testing.Short() {
		n = 1000
	}
	rng := rand.New(rand.NewPCG(42, 2026))
	var slowest time.Duration
	stats := map[string]int{}
	for it := range n {
		size := rng.IntN(15)
		if it%50 == 0 {
			size = 15 + rng.IntN(16) // up to 30: past the cap
		}
		in := randomInstance(rng, size, rng.IntN(6), 500+rng.Float64()*6000)
		if it%4 == 3 {
			p := DefaultParams()
			p.Lambda = rng.Float64() * 20
			p.M = rng.Float64() * 60
			p.W2 = rng.Float64() * 100
			in.Params = &p
		}
		in.BudgetMs = 200
		start := time.Now()
		out := Plan(in)
		took := time.Since(start)
		slowest = max(slowest, took)
		if err := checkPlan(in, out); err != nil {
			js, _ := json.Marshal(in)
			t.Fatalf("instance %d: %v\n%s", it, err, js)
		}
		again := Plan(in)
		out.SolveMs, again.SolveMs = 0, 0
		if !reflect.DeepEqual(out, again) {
			t.Fatalf("instance %d: not deterministic", it)
		}
		if out.Fallback {
			t.Fatalf("instance %d: hit the budget (%v)", it, took)
		}
		stats["runs"] += len(out.Runs)
		for _, r := range out.Runs {
			stats[fmt.Sprintf("stops=%d", len(r.OrderIDs))]++
		}
		for _, u := range out.Unassigned {
			stats[u.Reason]++
		}
	}
	t.Logf("%d instances, slowest %v, %v", n, slowest, stats)
}

// checkPlan verifies every rule of an emitted plan against its input.
func checkPlan(in Input, out Output) error {
	b := newBF(in)
	p := b.p
	known := map[string]*Order{}
	for i := range in.Orders {
		if _, dup := known[in.Orders[i].ID]; !dup {
			known[in.Orders[i].ID] = &in.Orders[i]
		}
	}
	seen := map[string]string{}
	mark := func(id, where string) error {
		if _, ok := known[id]; !ok {
			return fmt.Errorf("%s: unknown order %q", where, id)
		}
		if prev, dup := seen[id]; dup {
			return fmt.Errorf("order %s twice (%s, %s)", id, prev, where)
		}
		seen[id] = where
		return nil
	}
	isFree := map[string]bool{}
	for _, d := range b.free {
		isFree[d] = true
	}
	queue := map[string]int{}
	for i, d := range in.Drivers {
		queue[d.ID] = i
	}
	runOf := map[string]*Run{}
	lastQ := -1
	for ri := range out.Runs {
		r := &out.Runs[ri]
		where := "run " + r.DriverID
		q, ok := queue[r.DriverID]
		if !ok {
			return fmt.Errorf("%s: not an available driver", where)
		}
		if q <= lastQ {
			return fmt.Errorf("runs not in queue order")
		}
		lastQ = q
		if runOf[r.DriverID] != nil {
			return fmt.Errorf("%s: two runs", where)
		}
		runOf[r.DriverID] = r
		pool := b.pool[r.DriverID]
		if len(r.OrderIDs) == 0 {
			return fmt.Errorf("%s: empty", where)
		}
		if len(r.OrderIDs) > p.MaxStops && len(r.OrderIDs) != len(pool) {
			return fmt.Errorf("%s: %d stops", where, len(r.OrderIDs))
		}
		for _, id := range r.OrderIDs {
			if err := mark(id, where); err != nil {
				return err
			}
			if !known[id].Located {
				return fmt.Errorf("%s: unlocated order %s", where, id)
			}
		}
		for _, id := range pool {
			if !slices.Contains(r.OrderIDs, id) {
				return fmt.Errorf("%s: dropped pooled order %s", where, id)
			}
		}
		var sugg []string
		for _, id := range r.OrderIDs {
			if !slices.Contains(pool, id) {
				sugg = append(sugg, id)
				if !slices.Contains(b.cands, id) {
					return fmt.Errorf("%s: %s is not a candidate", where, id)
				}
			}
		}
		if !slices.Equal(sugg, r.SuggestedIDs) && !(len(sugg) == 0 && len(r.SuggestedIDs) == 0) {
			return fmt.Errorf("%s: suggested %v, want %v", where, r.SuggestedIDs, sugg)
		}
		if len(r.Stops) != len(r.OrderIDs) {
			return fmt.Errorf("%s: stops", where)
		}
		if out.Estimated { // the evaluator only knows road times
			continue
		}
		cost, detourOK, eta := b.seq(r.OrderIDs)
		if len(sugg) > 0 && !detourOK {
			return fmt.Errorf("%s: a stop breaks the detour limit", where)
		}
		if len(sugg) > 0 && !b.sectorOK(r.OrderIDs) {
			return fmt.Errorf("%s: stops outside one %v-degree sector", where, p.SectorDeg)
		}
		if !near(cost, r.Cost) {
			return fmt.Errorf("%s: cost %.6f, recomputed %.6f", where, r.Cost, cost)
		}
		if len(r.Stops) != len(r.OrderIDs) {
			return fmt.Errorf("%s: stops", where)
		}
		prev := r.DepartAt
		for i, st := range r.Stops {
			if st.OrderID != r.OrderIDs[i] {
				return fmt.Errorf("%s: stop order", where)
			}
			want := in.Now.Add(time.Duration(math.Round(eta[i])) * time.Second)
			if st.ETA.Sub(want).Abs() > time.Second || st.ETA.Before(prev) {
				return fmt.Errorf("%s: eta %v, want %v", where, st.ETA, want)
			}
			prev = st.ETA
			ptd := st.ETA.Sub(known[st.OrderID].PlacedAt).Seconds()
			if math.Abs(ptd-float64(st.PlacedToDoorS)) > 1.5 || st.Late != (st.PlacedToDoorS > int(b.target*60)) {
				return fmt.Errorf("%s: placed-to-door %d / late %v", where, st.PlacedToDoorS, st.Late)
			}
		}
		if r.ReturnAt.Before(prev) || r.DriveS < 0 {
			return fmt.Errorf("%s: return", where)
		}
	}
	for _, r := range out.Runs {
		if r.ReturnAt.Before(r.DepartAt) || (len(r.Stops) > 0 && r.Stops[0].ETA.Before(r.DepartAt)) {
			return fmt.Errorf("run %s: times", r.DriverID)
		}
	}
	// Every routable pooled driver gets a run.
	for _, d := range b.locked {
		if runOf[d] == nil {
			return fmt.Errorf("pooled driver %s has no run", d)
		}
	}
	// Turn order: the free drivers with runs are the first ones in the
	// queue, and their runs' oldest orders go oldest-first.
	var freeRuns []*Run
	for _, d := range b.free {
		if r := runOf[d]; r != nil {
			freeRuns = append(freeRuns, r)
		} else {
			break
		}
	}
	nFree := 0
	for _, d := range b.free {
		if runOf[d] != nil {
			nFree++
		}
	}
	if nFree != len(freeRuns) {
		return fmt.Errorf("a later driver got a run while an earlier one has none")
	}
	oldest := func(r *Run) *Order {
		var o *Order
		for _, id := range r.OrderIDs {
			c := known[id]
			if o == nil || c.PlacedAt.Before(o.PlacedAt) || (c.PlacedAt.Equal(o.PlacedAt) && c.ID < o.ID) {
				o = c
			}
		}
		return o
	}
	for i := 1; i < len(freeRuns); i++ {
		a, c := oldest(freeRuns[i-1]), oldest(freeRuns[i])
		if c.PlacedAt.Before(a.PlacedAt) || (c.PlacedAt.Equal(a.PlacedAt) && c.ID < a.ID) {
			return fmt.Errorf("turn order: %s's run is older than %s's", freeRuns[i].DriverID, freeRuns[i-1].DriverID)
		}
	}
	// Unassigned: reasons, and never a placed order.
	for _, u := range out.Unassigned {
		if err := mark(u.OrderID, "unassigned "+u.Reason); err != nil {
			return err
		}
		o := known[u.OrderID]
		switch u.Reason {
		case ReasonNoLocation:
			if b.routable(o.ID) {
				return fmt.Errorf("%s: no_location but routable", o.ID)
			}
		case ReasonNotReadySoon, ReasonOverLimit, ReasonNoDriver, ReasonNoFeasibleRun:
		default:
			return fmt.Errorf("%s: reason %q", o.ID, u.Reason)
		}
	}
	// Every unpooled order is accounted for; pooled ones only in their run.
	owner := map[string]string{}
	for d, ids := range b.pool {
		for _, id := range ids {
			owner[id] = d
		}
	}
	for id, o := range known {
		_, placed := seen[id]
		d, pooled := owner[id]
		switch {
		case !pooled && o.PoolDriverID == "" && !placed:
			return fmt.Errorf("order %s missing from the plan", id)
		case pooled && slices.Contains(b.locked, d) && seen[id] != "run "+d:
			return fmt.Errorf("pooled order %s not in its driver's run", id)
		case pooled && !slices.Contains(b.locked, d) && placed:
			return fmt.Errorf("order %s of an unplannable pool was placed", id)
		}
	}
	// Anchor + no idle driver.
	if len(b.free) > 0 && len(b.cands) > 0 && !inRunOf(seen, b.cands[0]) {
		return fmt.Errorf("oldest candidate %s left while a driver is free", b.cands[0])
	}
	anyLeft := false
	for _, u := range out.Unassigned {
		if u.Reason == ReasonNoDriver || u.Reason == ReasonNoFeasibleRun {
			anyLeft = true
		}
	}
	if anyLeft && len(b.free) > 0 && nFree < len(b.free) {
		return fmt.Errorf("an order waits while a driver idles")
	}
	if len(in.Drivers) == 0 && len(out.Runs) > 0 {
		return fmt.Errorf("runs without drivers")
	}
	if out.SolveMs > int64(max(in.BudgetMs, 200))+50 {
		return fmt.Errorf("solve took %d ms", out.SolveMs)
	}
	return nil
}

func inRunOf(seen map[string]string, id string) bool {
	w, ok := seen[id]
	return ok && len(w) > 4 && w[:4] == "run "
}
