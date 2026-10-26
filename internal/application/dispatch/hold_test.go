package dispatch

import (
	"math/rand/v2"
	"testing"
	"time"
)

// Rule tests for the hold rule (2.2.1, Input.Hold): far apart — send the
// first, hold the other for the returning driver — and only when he still
// gets it to the door by placed + target.

// holdWorld: "first" 3000 m east (placed 20 min ago) and "far" 3000 m east +
// 2400 m north (10 min ago): same quadrant and within AUTO's detour limit,
// so AUTO alone bundles them, but "far" adds 10 min to the run (detour) and
// is ~1.5 mi from "first" (distance). One driver available; "out1" is on a
// run and back in returnIn minutes.
func holdWorld(rule string, returnIn float64) Input {
	w := newWorld()
	w.order("first", 3000, 0, 20, -1)
	w.order("far", 3000, 2400, 10, -1)
	w.driver("d1")
	in := w.fill()
	in.OutDrivers = []OutDriver{{ID: "out1", Name: "Abdul", ReturnAt: t0.Add(time.Duration(returnIn * float64(time.Minute)))}}
	in.Hold = &HoldRule{Rule: rule, DetourMinutes: 5, DistanceM: 1609.344}
	return in
}

func heldOf(t *testing.T, out Output, id string) *HeldFor {
	t.Helper()
	for _, u := range out.Unassigned {
		if u.OrderID == id {
			if u.Reason != ReasonHeld || u.HeldFor == nil {
				t.Fatalf("%s: %+v, want held_for_returning_driver", id, u)
			}
			return u.HeldFor
		}
	}
	t.Fatalf("%s not unassigned (plan %+v)", id, out)
	return nil
}

func TestHoldFarApartForReturningDriver(t *testing.T) {
	for _, rule := range []string{HoldDetour, HoldDistance} {
		out := plan(t, holdWorld(rule, 10))
		wantRun(t, out, "d1", "first")
		h := heldOf(t, out, "far")
		if h.DriverID != "out1" || h.Name != "Abdul" || !h.ReturnAt.Equal(t0.Add(10*time.Minute)) {
			t.Fatalf("%s: held_for %+v", rule, h)
		}
	}
}

func TestHoldSendsBothWhenTheReturningDriverIsTooLate(t *testing.T) {
	// Back in 30 min: "far" (placed 10 min ago, 675 s from the store) would
	// reach the door at ~51 min > 45 — bundled as today.
	for _, rule := range []string{HoldDetour, HoldDistance} {
		out := plan(t, holdWorld(rule, 30))
		wantRun(t, out, "d1", "first", "far")
	}
	// Nobody out: nothing to hold for.
	in := holdWorld(HoldDetour, 10)
	in.OutDrivers = nil
	wantRun(t, plan(t, in), "d1", "first", "far")
}

func TestHoldOffAndLimits(t *testing.T) {
	// off (and no rule at all): AUTO as before.
	wantRun(t, plan(t, holdWorld(HoldOff, 10)), "d1", "first", "far")
	in := holdWorld(HoldDetour, 10)
	in.Hold = nil
	wantRun(t, plan(t, in), "d1", "first", "far")
	// Nearby by the store's limits: a 10 min detour allowed, or 2 mi.
	in = holdWorld(HoldDetour, 10)
	in.Hold.DetourMinutes = 10
	wantRun(t, plan(t, in), "d1", "first", "far")
	in = holdWorld(HoldDistance, 10)
	in.Hold.DistanceM = 2 * 1609.344
	wantRun(t, plan(t, in), "d1", "first", "far")
	// bundle: AUTO's own bundling is the nearby test — nothing held.
	wantRun(t, plan(t, holdWorld(HoldBundle, 10)), "d1", "first", "far")
}

func TestHoldNeverWhileADriverIsFree(t *testing.T) {
	in := holdWorld(HoldDetour, 10)
	in.Drivers = append(in.Drivers, Driver{ID: "d2", Name: "D d2"})
	out := plan(t, in)
	wantRun(t, out, "d1", "first")
	wantRun(t, out, "d2", "far")
}

// The owner's example: two deliveries on opposite sides, one driver free,
// the other out. AUTO never bundles them; the one left is reported held for
// the returning driver (any rule but off) when he still makes the target.
func TestHoldOppositeSides(t *testing.T) {
	mk := func(rule string, returnIn float64) Input {
		w := newWorld()
		w.order("stevenson", 3000, 0, 20, -1)
		w.order("gleneagle", -3000, 0, 10, -1)
		w.driver("d1")
		in := w.fill()
		in.OutDrivers = []OutDriver{{ID: "out1", Name: "Abdul", ReturnAt: t0.Add(time.Duration(returnIn * float64(time.Minute)))}}
		in.Hold = &HoldRule{Rule: rule, DetourMinutes: 5, DistanceM: 1609.344}
		return in
	}
	for _, rule := range []string{HoldDetour, HoldDistance, HoldBundle} {
		out := plan(t, mk(rule, 8))
		wantRun(t, out, "d1", "stevenson")
		if h := heldOf(t, out, "gleneagle"); h.DriverID != "out1" {
			t.Fatalf("%s: %+v", rule, h)
		}
	}
	out := plan(t, mk(HoldOff, 8))
	wantReason(t, out, "gleneagle", ReasonNoDriver)
	out = plan(t, mk(HoldDetour, 40)) // back too late for the target
	wantReason(t, out, "gleneagle", ReasonNoDriver)
}

// A pooled order is staff's choice: never held, the run keeps it.
func TestHoldNeverTouchesAPool(t *testing.T) {
	w := newWorld()
	w.order("first", 3000, 0, 20, -1)
	w.order("far", 3000, 2400, 10, -1)
	w.driver("d1", "first", "far")
	in := w.fill()
	in.OutDrivers = []OutDriver{{ID: "out1", Name: "Abdul", ReturnAt: t0.Add(10 * time.Minute)}}
	in.Hold = &HoldRule{Rule: HoldDetour, DetourMinutes: 5}
	out := plan(t, in)
	if r := runOf(out, "d1"); r == nil || len(r.OrderIDs) != 2 {
		t.Fatalf("pool run %+v", out)
	}
}

// Random nights with the hold rule on: every plan still keeps every rule,
// and no run carries a far, holdable stop.
func TestHoldRandomPlansKeepTheRules(t *testing.T) {
	rng := rand.New(rand.NewPCG(221, 7))
	for i := range 60 {
		in := randomInstance(rng, 4+rng.IntN(9), 1+rng.IntN(3), 4000)
		in.OutDrivers = append(in.OutDrivers, OutDriver{ID: "out-x", Name: "X", ReturnAt: t0.Add(time.Duration(rng.IntN(30)) * time.Minute)})
		rules := []string{HoldDetour, HoldDistance, HoldBundle}
		in.Hold = &HoldRule{Rule: rules[i%3], DetourMinutes: float64(1 + rng.IntN(8)), DistanceM: 400 + rng.Float64()*2000}
		out := plan(t, in)
		m, _ := build(&in, DefaultParams().sane())
		idx := map[string]int{}
		for k, id := range m.ids {
			idx[id] = k
		}
		for _, r := range out.Runs {
			nodes := make([]int, 0, len(r.OrderIDs))
			for _, id := range r.OrderIDs {
				nodes = append(nodes, idx[id])
			}
			if !m.apartOK(nodes) {
				t.Fatalf("instance %d: run %s %v breaks the hold rule", i, r.DriverID, r.OrderIDs)
			}
		}
	}
}
