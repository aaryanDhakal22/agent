package dispatch

import (
	"slices"
	"testing"
	"time"
)

// 2.4.0 (owner): a delivery nobody marked READY is planned like a ready one
// once its estimated ready time has passed - by AUTO and by auto-assign -
// and one still cooking past the soon window waits.
func TestPastReadyTimeIsPlanned(t *testing.T) {
	w := newWorld()
	late := w.order("late", 2000, 0, 30, 1)  // cooking ...
	late.ReadyAt = t0.Add(-10 * time.Minute) // ... but its time passed 10 min ago
	w.order("later", -2000, 0, 5, 25)        // ready in 25 min (past the 5-min soon window)
	w.driver("d1")
	w.driver("d2")
	out := plan(t, w.fill())
	if r := runOf(out, "d1"); r == nil || !slices.Contains(r.OrderIDs, "late") {
		t.Fatalf("past-time order not planned: %+v", out)
	}
	wantReason(t, out, "later", ReasonNotReadySoon)
	if got := next(t, w.fill(), "d1"); !slices.Equal(got, []string{"late"}) {
		t.Fatalf("auto-assign: %v", got)
	}
}
