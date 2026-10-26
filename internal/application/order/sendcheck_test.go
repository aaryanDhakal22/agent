package order

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
)

// 2.2.1: far apart — send the first, hold the other for the returning
// driver (send-check + exclude_order_ids), and the run order of drivers out.

// holdFloor: store at (40.0, -74.0). Ann is available with A (placed 20 min
// ago, ~1.1 km north) and B (10 min ago, ~2.2 km south) pooled — B adds
// ~13 min to her run. Abdul is out with X, back in a few minutes.
type holdFloor struct {
	svc        *Service
	ann, abdul string
	a, b, x    string
}

func newHoldFloor(t *testing.T) *holdFloor {
	t.Helper()
	ctx := context.Background()
	svc, _, _ := newPoolService(t)
	svc.SetGeo(GeoDeps{Floor: func() (*FloorStore, *FloorMap) { return &FloorStore{Lat: 40.0, Lng: -74.0}, nil }})
	f := &holdFloor{svc: svc}
	ann, _ := svc.CreateDriver(ctx, "Ann", "")
	abdul, _ := svc.CreateDriver(ctx, "Abdul Karim", "")
	f.ann, f.abdul = ann.ID, abdul.ID
	svc.ClockIn(ctx, f.abdul)
	svc.ClockIn(ctx, f.ann)
	f.x = readyDelivery(t, svc, 15*time.Minute)
	locate(t, svc, f.x, 40.003, -74.0, 60)
	if _, err := svc.AssignDriver(ctx, f.abdul, []string{f.x}); err != nil {
		t.Fatal(err)
	}
	f.a = readyDelivery(t, svc, 20*time.Minute)
	f.b = readyDelivery(t, svc, 10*time.Minute)
	locate(t, svc, f.a, 40.01, -74.0, 150)
	locate(t, svc, f.b, 39.98, -74.0, 300)
	if _, err := svc.Pool(ctx, []string{f.a, f.b}, f.ann); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestSendCheckHoldsTheFarOne(t *testing.T) {
	ctx := context.Background()
	f := newHoldFloor(t)
	res, err := f.svc.SendCheck(ctx, f.ann, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hold) != 1 || res.Hold[0].OrderID != f.b || res.Hold[0].Address != "1 Main St" ||
		res.Hold[0].HeldFor.DriverID != f.abdul || res.Hold[0].HeldFor.Name != "Abdul Karim" || res.Hold[0].HeldFor.ReturnAt.IsZero() {
		t.Fatalf("send-check: %+v", res)
	}
	// The same with the orders named.
	if res, _ := f.svc.SendCheck(ctx, f.ann, []string{f.a, f.b}); len(res.Hold) != 1 {
		t.Fatalf("named: %+v", res)
	}
	// A wide detour limit: nearby, nothing held.
	f.svc.UpdateDeliverySettings(ctx, DeliveryPatch{NearbyDetourMinutes: ptr(int64(30))})
	if res, _ := f.svc.SendCheck(ctx, f.ann, nil); len(res.Hold) != 0 {
		t.Fatalf("30 min detour: %+v", res)
	}
	// Distance rule, 1 mi: B is ~1.9 mi from A.
	f.svc.UpdateDeliverySettings(ctx, DeliveryPatch{NearbyRule: ptr(order.NearbyDistance)})
	if res, _ := f.svc.SendCheck(ctx, f.ann, nil); len(res.Hold) != 1 {
		t.Fatalf("distance: %+v", res)
	}
	// The returning driver too late for the target: send both.
	f.svc.UpdateDeliverySettings(ctx, DeliveryPatch{DeliveryTargetMinutes: ptr(int64(15))})
	if res, _ := f.svc.SendCheck(ctx, f.ann, nil); len(res.Hold) != 0 {
		t.Fatalf("too late: %+v", res)
	}
	// Rule off: never.
	f.svc.UpdateDeliverySettings(ctx, DeliveryPatch{NearbyRule: ptr(order.NearbyOff), DeliveryTargetMinutes: ptr(int64(45))})
	if res, _ := f.svc.SendCheck(ctx, f.ann, nil); res == nil || len(res.Hold) != 0 {
		t.Fatalf("off: %+v", res)
	}
	if _, err := f.svc.SendCheck(ctx, "no-such-driver", nil); err == nil {
		t.Fatal("unknown driver")
	}
	// The JSON shape the tablet reads.
	f.svc.UpdateDeliverySettings(ctx, DeliveryPatch{NearbyRule: ptr(order.NearbyDetour), NearbyDetourMinutes: ptr(int64(5))})
	res, _ = f.svc.SendCheck(ctx, f.ann, nil)
	js, _ := json.Marshal(res)
	var wire struct {
		Hold []struct {
			OrderID string `json:"order_id"`
			Address string `json:"address"`
			HeldFor struct {
				DriverID string    `json:"driver_id"`
				Name     string    `json:"name"`
				ReturnAt time.Time `json:"return_at"`
			} `json:"held_for"`
		} `json:"hold"`
	}
	if json.Unmarshal(js, &wire) != nil || len(wire.Hold) != 1 || wire.Hold[0].HeldFor.DriverID != f.abdul {
		t.Fatalf("wire: %s", js)
	}
}

func TestSendOutExcludeAndRunOrder(t *testing.T) {
	ctx := context.Background()
	f := newHoldFloor(t)
	sent, err := f.svc.SendOut(ctx, f.ann, SendOutRequest{Exclude: []string{f.b}})
	if err != nil {
		t.Fatal(err)
	}
	if len(sent) != 1 || sent[0].Order.ID != f.a || sent[0].Status.RunSeq == nil || *sent[0].Status.RunSeq != 1 {
		t.Fatalf("sent: %+v", sent)
	}
	if b := storedOf(t, f.svc, f.b); b.Status.PoolDriverID != "" || b.Status.State != order.StateReady || b.Status.CameBack {
		t.Fatalf("excluded: %+v", b.Status)
	}

	// Abdul comes back and takes B and a new C; his run's stop order is
	// what the tablet lists under him.
	f.svc.DriverReturned(ctx, f.abdul)
	c := readyDelivery(t, f.svc, time.Minute)
	locate(t, f.svc, c, 39.99, -74.0, 150)
	if _, err := f.svc.SendOut(ctx, f.abdul, SendOutRequest{Include: []string{f.b, c}}); err != nil {
		t.Fatal(err)
	}
	ds, _ := f.svc.ListDrivers(ctx)
	runs := map[string][]string{}
	for _, d := range ds {
		runs[d.ID] = d.RunOrderIDs
	}
	if !slices.Equal(runs[f.ann], []string{f.a}) || len(runs[f.abdul]) != 2 || !slices.Contains(runs[f.abdul], f.b) || !slices.Contains(runs[f.abdul], c) {
		t.Fatalf("runs: %v", runs)
	}
	for i, id := range runs[f.abdul] {
		if so := storedOf(t, f.svc, id); so.Status.RunSeq == nil || *so.Status.RunSeq != int64(i+1) {
			t.Fatalf("run_seq of stop %d: %+v", i, so.Status.RunSeq)
		}
	}
	// C (nearer) is first.
	if runs[f.abdul][0] != c {
		t.Fatalf("stop order %v", runs[f.abdul])
	}
	// One stop came back: the list loses it.
	if _, err := f.svc.NotDelivered(ctx, c, order.NotDeliveredRetry); err != nil {
		t.Fatal(err)
	}
	ds, _ = f.svc.ListDrivers(ctx)
	for _, d := range ds {
		if d.ID == f.abdul && !slices.Equal(d.RunOrderIDs, []string{f.b}) {
			t.Fatalf("after came back: %v", d.RunOrderIDs)
		}
		if d.Status != order.DriverOut && len(d.RunOrderIDs) != 0 {
			t.Fatalf("%s not out but has run ids", d.Name)
		}
	}
}
