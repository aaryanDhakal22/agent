package dispatch

import (
	"slices"
	"testing"
	"time"
)

// Rule tests for the simple auto-assign (2.2.1, PlanNext): one driver, at
// most 3 stops, the hold rule respected, never a non-empty pool.

func next(t *testing.T, in Input, driver string) []string {
	t.Helper()
	if in.Params == nil && testParams != nil {
		p := *testParams
		in.Params = &p
	}
	return PlanNext(in, driver)
}

func TestNextOneOrder(t *testing.T) {
	w := newWorld()
	w.order("a", 2000, 0, 10, -1)
	w.driver("d1")
	if got := next(t, w.fill(), "d1"); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("got %v", got)
	}
}

func TestNextTwoBundled(t *testing.T) {
	w := newWorld()
	w.order("deep", 3000, 0, 20, -1)
	w.order("near", 1000, 0, 5, -1)
	w.driver("d1")
	w.driver("d2") // only d1 is planned for
	if got := next(t, w.fill(), "d1"); !slices.Equal(got, []string{"near", "deep"}) {
		t.Fatalf("got %v", got)
	}
}

func TestNextFarApartHoldsTheOther(t *testing.T) {
	in := holdWorld(HoldDetour, 10)
	if got := next(t, in, "d1"); !slices.Equal(got, []string{"first"}) {
		t.Fatalf("got %v", got)
	}
	// Returning driver too late for the target: both go.
	in = holdWorld(HoldDetour, 30)
	if got := next(t, in, "d1"); len(got) != 2 {
		t.Fatalf("got %v", got)
	}
}

func TestNextAtMostThree(t *testing.T) {
	w := newWorld()
	w.order("s400", 400, 0, 10, -1)
	w.order("s800", 800, 0, 9, -1)
	w.order("s1200", 1200, 0, 8, -1)
	w.order("s1600", 1600, 0, 7, -1)
	w.driver("d1")
	got := next(t, w.fill(), "d1")
	if len(got) != AutoAssignMaxStops || !slices.Contains(got, "s400") {
		t.Fatalf("got %v", got)
	}
	// Plan itself still allows 4.
	if r := runOf(Plan(w.fill()), "d1"); len(r.OrderIDs) != MaxStops {
		t.Fatalf("plan %v", r.OrderIDs)
	}
}

func TestNextNeverTouchesAPoolOrNothingToGive(t *testing.T) {
	w := newWorld()
	w.order("a", 2000, 0, 10, -1)
	w.order("b", 2100, 0, 5, -1)
	w.driver("d1", "a")
	if got := next(t, w.fill(), "d1"); got != nil {
		t.Fatalf("pooled driver got %v", got)
	}
	w = newWorld()
	w.order("cooking", 2000, 0, 10, 20) // ready in 20 min: not soon
	w.order("nowhere", 0, 0, 10, -1).Located = false
	w.driver("d1")
	if got := next(t, w.fill(), "d1"); got != nil {
		t.Fatalf("got %v", got)
	}
	if got := next(t, w.fill(), "nobody"); got != nil {
		t.Fatalf("unknown driver got %v", got)
	}
	// Soon ready (within soon_ready_minutes) counts.
	w = newWorld()
	w.order("soon", 2000, 0, 10, 3)
	w.driver("d1")
	in := w.fill()
	in.OutDrivers = []OutDriver{{ID: "o", ReturnAt: t0.Add(time.Hour)}}
	if got := next(t, in, "d1"); !slices.Equal(got, []string{"soon"}) {
		t.Fatalf("soon: %v", got)
	}
}
