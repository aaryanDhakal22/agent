package dispatch

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"testing"
)

// plan runs the optimizer on a world and checks every rule of the result.
func plan(t *testing.T, in Input) Output {
	t.Helper()
	if in.Params == nil && testParams != nil {
		p := *testParams
		in.Params = &p
	}
	out := Plan(in)
	if err := checkPlan(in, out); err != nil {
		js, _ := json.Marshal(in)
		t.Fatalf("%v\n%s", err, js)
	}
	if out.Fallback {
		t.Fatal("fallback")
	}
	return out
}

func runOf(out Output, driver string) *Run {
	for i := range out.Runs {
		if out.Runs[i].DriverID == driver {
			return &out.Runs[i]
		}
	}
	return nil
}

func reasonOf(out Output, id string) string {
	for _, u := range out.Unassigned {
		if u.OrderID == id {
			return u.Reason
		}
	}
	return ""
}

func wantRun(t *testing.T, out Output, driver string, ids ...string) {
	t.Helper()
	r := runOf(out, driver)
	if r == nil {
		t.Fatalf("%s has no run; plan %+v", driver, out)
	}
	if !slices.Equal(r.OrderIDs, ids) {
		t.Fatalf("%s run = %v, want %v", driver, r.OrderIDs, ids)
	}
}

func wantReason(t *testing.T, out Output, id, reason string) {
	t.Helper()
	if got := reasonOf(out, id); got != reason {
		t.Fatalf("%s: reason %q, want %q (plan %+v)", id, got, reason, out)
	}
}

// testParams: DISPATCH_TEST_PARAMS (JSON over the defaults) runs the rule
// tests under candidate constants, so the simulator's tuning can gate on them.
var testParams = func() *Params {
	s := os.Getenv("DISPATCH_TEST_PARAMS")
	if s == "" {
		return nil
	}
	var p Params
	if err := json.Unmarshal([]byte(s), &p); err != nil {
		panic(err)
	}
	return &p
}()

func TestOneDriver(t *testing.T) {
	w := newWorld()
	w.order("east", 2000, 0, 30, -1)
	w.order("north", 0, 2000, 20, -1)
	w.order("west", -2000, 0, 10, -1)
	w.driver("d1")
	out := plan(t, w.fill())
	wantRun(t, out, "d1", "east") // the oldest; the others are other ways
	wantReason(t, out, "north", ReasonNoDriver)
	wantReason(t, out, "west", ReasonNoDriver)
}

func TestNoDrivers(t *testing.T) {
	w := newWorld()
	w.order("a", 1000, 0, 10, -1)
	w.order("b", 0, 1000, 5, -1)
	out := plan(t, w.fill())
	if len(out.Runs) != 0 || out.Cost != 0 {
		t.Fatalf("plan %+v", out)
	}
	wantReason(t, out, "a", ReasonNoDriver)
	wantReason(t, out, "b", ReasonNoDriver)
}

func TestNothingToPlan(t *testing.T) {
	w := newWorld()
	w.driver("d1")
	out := plan(t, w.fill())
	if len(out.Runs) != 0 || len(out.Unassigned) != 0 {
		t.Fatalf("plan %+v", out)
	}
	if js, _ := json.Marshal(out); !json.Valid(js) || string(js[:12]) != `{"runs":[],"` {
		t.Fatalf("empty lists must encode as []: %s", js)
	}
}

func TestOneStreetBundlesNearFirst(t *testing.T) {
	w := newWorld()
	// Placed farthest first, so stop order isn't just placed order.
	w.order("s2000", 2000, 0, 20, -1)
	w.order("s1200", 1200, 0, 15, -1)
	w.order("s400", 400, 0, 10, -1)
	w.order("s1600", 1600, 0, 8, -1)
	w.driver("d1")
	out := plan(t, w.fill())
	wantRun(t, out, "d1", "s400", "s1200", "s1600", "s2000")

	// A fifth on the street: still 4 stops at most, the oldest aboard.
	w.order("s800", 800, 0, 5, -1)
	out = plan(t, w.fill())
	r := runOf(out, "d1")
	if len(r.OrderIDs) != MaxStops || !slices.Contains(r.OrderIDs, "s2000") {
		t.Fatalf("run %v", r.OrderIDs)
	}
	for i := 1; i < len(r.OrderIDs); i++ {
		if w.pos[r.OrderIDs[i]][0] < w.pos[r.OrderIDs[i-1]][0] {
			t.Fatalf("not near-first: %v", r.OrderIDs)
		}
	}
	if len(out.Unassigned) != 1 || out.Unassigned[0].Reason != ReasonNoDriver {
		t.Fatalf("unassigned %v", out.Unassigned)
	}
}

func TestOppositeDirectionsNeverBundled(t *testing.T) {
	w := newWorld()
	w.order("east", 3000, 0, 20, -1)
	w.order("west", -3000, 0, 10, -1)
	w.driver("d1")
	out := plan(t, w.fill())
	wantRun(t, out, "d1", "east")
	wantReason(t, out, "west", ReasonNoDriver)

	w.driver("d2")
	out = plan(t, w.fill())
	wantRun(t, out, "d1", "east")
	wantRun(t, out, "d2", "west")

	// Even when leaving it costs a lot (M huge), the detour limit holds.
	p := DefaultParams()
	p.M = 10000
	in := w.fill()
	in.Drivers = in.Drivers[:1]
	in.Params = &p
	out = plan(t, in)
	wantRun(t, out, "d1", "east")
}

func TestStopOnTheWayGoesFirst(t *testing.T) {
	w := newWorld()
	w.order("deep", 3000, 0, 20, -1) // older, farther
	w.order("near", 1000, 0, 5, -1)  // on the road to it
	w.driver("d1")
	out := plan(t, w.fill())
	wantRun(t, out, "d1", "near", "deep")
	if r := runOf(out, "d1"); !slices.Equal(r.SuggestedIDs, []string{"near", "deep"}) || r.Stops[0].ETA.After(r.Stops[1].ETA) {
		t.Fatalf("run %+v", r)
	}
}

func TestLateOrderGoesAlone(t *testing.T) {
	w := newWorld()
	w.order("late", 2000, 0, 50, -1) // already past the 45 min target
	w.order("fresh", 2200, 0, 1, 4)  // same way, but ready in 4 min
	w.driver("d1")
	out := plan(t, w.fill())
	wantRun(t, out, "d1", "late") // waiting 4 min for "fresh" would make it later
	wantReason(t, out, "fresh", ReasonNoDriver)
	if r := runOf(out, "d1"); !r.Stops[0].Late || !r.DepartAt.Equal(t0) {
		t.Fatalf("run %+v", r)
	}

	// Ready now, it rides along after the late one: costs it nothing.
	w.in.Orders[1].Ready = true
	out = plan(t, w.fill())
	wantRun(t, out, "d1", "late", "fresh")
}

func TestSixSameWayTwoDriversThreeAndThree(t *testing.T) {
	w := newWorld()
	for i := range 6 {
		w.order(fmt.Sprintf("c%d", i), 3000+float64(i)*15, 40, float64(20-i), -1)
	}
	w.driver("d1")
	w.driver("d2")
	out := plan(t, w.fill())
	if len(out.Runs) != 2 || len(out.Runs[0].OrderIDs) != 3 || len(out.Runs[1].OrderIDs) != 3 {
		t.Fatalf("want 3+3, got %+v", out.Runs)
	}
}

func TestLockedPoolsKeptAndExtended(t *testing.T) {
	w := newWorld()
	w.order("p1", 2000, 0, 30, -1)
	w.order("c1", 2400, 0, 10, -1) // same way as d1's pool
	w.order("c2", -2000, 0, 5, -1) // the other way
	w.driver("d1", "p1")
	w.driver("d2")
	out := plan(t, w.fill())
	wantRun(t, out, "d1", "p1", "c1")
	if r := runOf(out, "d1"); !slices.Equal(r.SuggestedIDs, []string{"c1"}) {
		t.Fatalf("suggested %v", r.SuggestedIDs)
	}
	wantRun(t, out, "d2", "c2")

	// A full pool is left exactly as is.
	w = newWorld()
	for i := range 4 {
		w.order(fmt.Sprintf("p%d", i), 1000+float64(i)*300, 0, 30, -1)
	}
	w.order("c", 2500, 0, 5, -1)
	w.driver("d1", "p0", "p1", "p2", "p3")
	out = plan(t, w.fill())
	wantRun(t, out, "d1", "p0", "p1", "p2", "p3")
	if len(runOf(out, "d1").SuggestedIDs) != 0 {
		t.Fatal("suggested on a full pool")
	}
	wantReason(t, out, "c", ReasonNoFeasibleRun)

	// A pool staff built against the detour limit is kept, never extended.
	w = newWorld()
	w.order("pe", 3000, 0, 30, -1)
	w.order("pw", -3000, 0, 30, -1)
	w.order("c", 3200, 0, 5, -1)
	w.driver("d1", "pe", "pw")
	out = plan(t, w.fill())
	if r := runOf(out, "d1"); len(r.OrderIDs) != 2 || len(r.SuggestedIDs) != 0 {
		t.Fatalf("run %+v", r)
	}
	wantReason(t, out, "c", ReasonNoFeasibleRun)

	// pool_driver_id alone (driver's pool list empty) locks it too.
	w = newWorld()
	w.order("p", 2000, 0, 30, -1).PoolDriverID = "d1"
	w.order("c", -2000, 0, 5, -1)
	w.driver("d1")
	w.driver("d2")
	out = plan(t, w.fill())
	wantRun(t, out, "d1", "p")
	wantRun(t, out, "d2", "c")
}

func TestSoonReadyWindow(t *testing.T) {
	w := newWorld()
	w.order("now", 1500, 0, 20, -1)
	w.order("in4", 1600, 0, 15, 4)
	w.order("in6", 1700, 0, 10, 6)
	w.order("cooking", 1800, 0, 10, 0) // not ready, no estimate
	w.in.Orders[3].Ready = false
	w.driver("d1")
	out := plan(t, w.fill())
	r := runOf(out, "d1")
	if !slices.Contains(r.OrderIDs, "now") || !slices.Contains(r.OrderIDs, "in4") || slices.Contains(r.OrderIDs, "in6") {
		t.Fatalf("run %v", r.OrderIDs)
	}
	if !r.DepartAt.Equal(t0.Add(4 * 60e9)) {
		t.Fatalf("departs %v, want when in4 is ready", r.DepartAt)
	}
	wantReason(t, out, "in6", ReasonNotReadySoon)
	wantReason(t, out, "cooking", ReasonNotReadySoon)
}

func TestNotLocatedSkipped(t *testing.T) {
	w := newWorld()
	w.order("ok", 1500, 0, 20, -1)
	w.order("lost", 1500, 100, 25, -1).Located = false
	w.driver("d1")
	out := plan(t, w.fill())
	wantRun(t, out, "d1", "ok")
	wantReason(t, out, "lost", ReasonNoLocation)

	// A pool holding an unlocated order: that driver gets no suggestion.
	w = newWorld()
	w.order("lost", 1500, 100, 25, -1).Located = false
	w.order("c", 1500, 0, 20, -1)
	w.driver("d1", "lost")
	out = plan(t, w.fill())
	if len(out.Runs) != 0 {
		t.Fatalf("runs %+v", out.Runs)
	}
	wantReason(t, out, "c", ReasonNoDriver)

	// No store pin and no road time from the store: can't be planned.
	w = newWorld()
	w.order("a", 1500, 0, 20, -1)
	w.order("b", 1600, 0, 10, -1)
	w.driver("d1")
	in := w.fill()
	in.Store = nil
	delete(in.Times.Store.To, "b")
	out = plan(t, in)
	wantRun(t, out, "d1", "a")
	wantReason(t, out, "b", ReasonNoLocation)
}

func TestMissingPairsEstimated(t *testing.T) {
	w := newWorld()
	w.order("a", 1500, 0, 20, -1)
	w.order("b", 2500, 0, 10, -1)
	w.driver("d1")
	out := plan(t, w.fill())
	if out.Estimated {
		t.Fatal("estimated with a full matrix")
	}
	wantRun(t, out, "d1", "a", "b")

	in := w.fill()
	delete(in.Times.Pairs, "a") // a -> b unknown
	delete(in.Times.Store.From, "b")
	out = plan(t, in)
	if !out.Estimated {
		t.Fatal("not flagged estimated")
	}
	wantRun(t, out, "d1", "a", "b")
	// The estimate learns this town's road factor from the known pairs:
	// Manhattan along one axis = straight line, 8 m/s → 125 s per km.
	if r := runOf(out, "d1"); r.DriveS < 2*312-10 || r.DriveS > 2*312+10 {
		t.Fatalf("drive %d s, want ~625", r.DriveS)
	}
}

func TestOverLimit(t *testing.T) {
	w := newWorld()
	for i := range 30 {
		w.order(fmt.Sprintf("o%02d", i), 1000+float64(i%5)*400, float64(i/5)*300, float64(60-i), -1)
	}
	w.driver("d1")
	w.driver("d2")
	w.driver("d3")
	out := plan(t, w.fill())
	for i := range 30 {
		id := fmt.Sprintf("o%02d", i)
		if over := reasonOf(out, id) == ReasonOverLimit; over != (i >= 25) {
			t.Fatalf("%s over_limit=%v", id, over)
		}
	}
}

func TestDuplicateAddressBundled(t *testing.T) {
	w := newWorld()
	w.order("apt1", 2000, 500, 20, -1)
	w.order("apt2", 2000, 500, 12, -1)
	w.driver("d1")
	w.driver("d2")
	out := plan(t, w.fill())
	wantRun(t, out, "d1", "apt1", "apt2") // one trip, older first
	if runOf(out, "d2") != nil {
		t.Fatal("second driver sent to the same door")
	}
}

func TestNextDriverGetsOldest(t *testing.T) {
	w := newWorld()
	w.order("new", 2000, 0, 5, -1)
	w.order("old", -2000, 0, 25, -1)
	w.driver("next")
	w.driver("after")
	out := plan(t, w.fill())
	wantRun(t, out, "next", "old")
	wantRun(t, out, "after", "new")
	if out.Runs[0].DriverID != "next" {
		t.Fatal("runs not in queue order")
	}

	// A pooled driver ahead in the queue keeps its pool; the free ones
	// still take turns oldest-first.
	w = newWorld()
	w.order("p", 0, 2500, 40, -1)
	w.order("new", 2000, 0, 5, -1)
	w.order("old", -2000, 0, 25, -1)
	w.driver("pooled", "p")
	w.driver("next")
	w.driver("after")
	out = plan(t, w.fill())
	wantRun(t, out, "pooled", "p")
	wantRun(t, out, "next", "old")
	wantRun(t, out, "after", "new")
}

func TestOutDriversShortenTheWait(t *testing.T) {
	// One driver, two orders in opposite directions: the one left over
	// is costed by when the out driver comes back.
	w := newWorld()
	w.order("a", 2000, 0, 30, -1)
	w.order("b", -2000, 0, 25, -1)
	w.driver("d1")
	soon := w.fill()
	soon.OutDrivers = []OutDriver{{ID: "x", ReturnAt: t0.Add(2 * 60e9)}}
	late := w.fill()
	late.OutDrivers = []OutDriver{{ID: "x", ReturnAt: t0.Add(40 * 60e9)}}
	a, b := plan(t, soon), plan(t, late)
	if a.Cost >= b.Cost {
		t.Fatalf("a quick return should make waiting cheaper: %.1f vs %.1f", a.Cost, b.Cost)
	}
}

func TestParamsJSON(t *testing.T) {
	var in Input
	if err := json.Unmarshal([]byte(`{"params":{"lambda":0,"m":12.5}}`), &in); err != nil {
		t.Fatal(err)
	}
	want := DefaultParams()
	want.Lambda, want.M = 0, 12.5
	if in.Params == nil || *in.Params != want {
		t.Fatalf("params %+v", in.Params)
	}
}

func TestBudgetFallbackStillValid(t *testing.T) {
	in := denseInstance(3, 25, 6)
	in.BudgetMs = 1
	out := Plan(in)
	if err := checkPlan(in, out); err != nil {
		t.Fatal(err)
	}
	if !out.Fallback {
		t.Skip("solved inside 1 ms: nothing to check here")
	}
	if len(out.Runs) == 0 {
		t.Fatal("fallback plan is empty")
	}
}

func TestDeterministicAcrossInputOrder(t *testing.T) {
	w := newWorld()
	for i := range 12 {
		w.order(fmt.Sprintf("o%02d", i), float64(i%4)*700-1000, float64(i/4)*600-500, 30, -1) // equal placed times: ties by id
	}
	w.driver("d1")
	w.driver("d2")
	in := w.fill()
	a := Plan(in)
	slices.Reverse(in.Orders)
	b := Plan(in)
	a.SolveMs, b.SolveMs = 0, 0
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Fatalf("order of input changed the plan:\n%s\n%s", ja, jb)
	}
}

func TestSectorRule(t *testing.T) {
	// East (bearing 90) and south-southwest (~185): the detour limit
	// allows the bundle, the quadrant rule doesn't.
	w := newWorld()
	w.order("east", 600, 0, 20, -1)
	w.order("south", -100, -1200, 15, -1)
	w.driver("d1")
	in := w.fill()
	out := plan(t, in)
	wantRun(t, out, "d1", "east")
	wantReason(t, out, "south", ReasonNoDriver)

	p := DefaultParams()
	p.SectorDeg = 360 // rule off: they ride together
	in.Params = &p
	out = plan(t, in)
	if r := runOf(out, "d1"); len(r.OrderIDs) != 2 {
		t.Fatalf("run %v", r.OrderIDs)
	}

	// A stop within 500 m of the store is exempt.
	w = newWorld()
	w.order("east", 300, 0, 20, -1)
	w.order("south", -100, -1200, 15, -1)
	w.driver("d1")
	out = plan(t, w.fill())
	if r := runOf(out, "d1"); len(r.OrderIDs) != 2 {
		t.Fatalf("run %v", r.OrderIDs)
	}

	// Wraparound: 350 and 20 degrees are 30 apart.
	w = newWorld()
	w.order("nnw", -350, 2000, 20, -1)
	w.order("nne", 700, 2000, 15, -1)
	w.driver("d1")
	out = plan(t, w.fill())
	if r := runOf(out, "d1"); len(r.OrderIDs) != 2 {
		t.Fatalf("run %v", r.OrderIDs)
	}
}
