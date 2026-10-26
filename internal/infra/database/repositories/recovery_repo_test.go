package repositories

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
)

// TestSwapRestoresIdenticalFloor plays the SD-card swap at the repo level: a
// mid-shift agent A reports its state; a blank agent B restores from what
// main would hand it (A's state + every order with A's status) and must end
// up with the same floor — orders, kitchen counter, drivers, queue, shifts.
func TestSwapRestoresIdenticalFloor(t *testing.T) {
	ctx := context.Background()
	a := newFloorRepo(t)

	pickup := accept(t, a, arrive(t, a, "pickup"))
	if _, err := a.MarkReady(ctx, pickup.Order.ID); err != nil {
		t.Fatal(err)
	}
	del := accept(t, a, arrive(t, a, "delivery"))
	if _, err := a.MarkReady(ctx, del.Order.ID); err != nil {
		t.Fatal(err)
	}
	waiting := arrive(t, a, "pickup")
	cooking := accept(t, a, arrive(t, a, "delivery"))

	d1, err := a.CreateDriver(ctx, "Abdul", "")
	if err != nil {
		t.Fatal(err)
	}
	d2, err := a.CreateDriver(ctx, "Juma", "")
	if err != nil {
		t.Fatal(err)
	}
	clockIn(t, a, d1.ID, d2.ID)
	if _, err := a.AssignDriver(ctx, d1.ID, []string{del.Order.ID}); err != nil {
		t.Fatal(err)
	}
	// 2.2.1: a delivery that came back with Juma waits in the came-back list.
	back := accept(t, a, arrive(t, a, "delivery"))
	if _, err := a.MarkReady(ctx, back.Order.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.AssignDriver(ctx, d2.ID, []string{back.Order.ID}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.NotDelivered(ctx, back.Order.ID, order.NotDeliveredRetry); err != nil {
		t.Fatal(err)
	}
	if err := a.SetFloorPrefs(ctx, order.FloorPrefs{GreenTillMinutes: 7, TipTaxBps: 1500}); err != nil {
		t.Fatal(err)
	}
	footer := "THANK YOU"
	rp := order.DefaultReceiptPrefs()
	rp.KitchenItemSize, rp.Copies, rp.ShowPhone, rp.FooterText = order.SizeXLarge, 2, false, &footer
	if err := a.SetReceiptPrefs(ctx, rp); err != nil {
		t.Fatal(err)
	}

	// What main stores: A's agent.state + each order's status event.
	stA, err := a.AgentState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(stA)
	var stWire order.AgentState
	if err := json.Unmarshal(raw, &stWire); err != nil {
		t.Fatal(err)
	}
	ordersA, err := a.ListByBatch(ctx, stA.Batch.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	data := order.RestoreData{State: &stWire}
	for _, so := range ordersA {
		st := so.Status
		b, _ := json.Marshal(st)
		var wire order.Status
		_ = json.Unmarshal(b, &wire)
		data.Orders = append(data.Orders, order.RestoreOrder{Order: so.Order, AgentStatus: &wire})
	}

	b := newFloorRepo(t)
	res, err := b.Restore(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	if res.Orders != 5 || res.Waiting != 1 || res.Drivers != 2 || res.Shifts != 2 || res.BatchID != stA.Batch.ID {
		t.Fatalf("restore result %+v", res)
	}

	stB, err := b.AgentState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ja, _ := json.Marshal(stA)
	jb, _ := json.Marshal(stB)
	if string(ja) != string(jb) {
		t.Fatalf("state differs\nA %s\nB %s", ja, jb)
	}
	if stWire.ReceiptPrefs == nil {
		t.Fatal("agent.state carries receipt_prefs")
	}
	if got, _ := b.ReceiptPrefs(ctx); got.Copies != 2 || got.ShowPhone || got.FooterText == nil || *got.FooterText != footer {
		t.Fatalf("receipt prefs not restored: %+v", got)
	}
	ordersB, err := b.ListByBatch(ctx, stA.Batch.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(ordersB) != len(ordersA) {
		t.Fatalf("orders: A %d B %d", len(ordersA), len(ordersB))
	}
	byID := map[string]order.StoredOrder{}
	for _, so := range ordersB {
		byID[so.Order.ID] = so
	}
	for _, so := range ordersA {
		got, ok := byID[so.Order.ID]
		if !ok {
			t.Fatalf("order %s missing on B", so.Order.ID)
		}
		x, _ := json.Marshal(so.Status)
		y, _ := json.Marshal(got.Status)
		if string(x) != string(y) {
			t.Fatalf("order %s status differs\nA %s\nB %s", so.Order.ID, x, y)
		}
	}
	if byID[waiting.Order.ID].Status.State != order.StateArrived {
		t.Fatal("the waiting order must still ring on B")
	}
	if byID[cooking.Order.ID].Status.State != order.StateAccepted {
		t.Fatal("the cooking order must stay accepted on B")
	}
	if st := byID[back.Order.ID].Status; !st.CameBack || st.NotDelivered == nil || st.NotDelivered.DriverID != d2.ID {
		t.Fatalf("the came-back order on B: %+v", st)
	}

	// The next kitchen number continues, never repeats.
	fsA, _ := a.FloorSettings(ctx)
	fsB, _ := b.FloorSettings(ctx)
	if fsA.KitchenNext != fsB.KitchenNext {
		t.Fatalf("kitchen next A %d B %d", fsA.KitchenNext, fsB.KitchenNext)
	}
	next := accept(t, b, byID[waiting.Order.ID])
	if next.Status.KitchenNumber == nil || *next.Status.KitchenNumber != fsA.KitchenNext {
		t.Fatalf("B's next kitchen number %v, want %d", next.Status.KitchenNumber, fsA.KitchenNext)
	}

	// A restore never runs over local data.
	if _, err := b.Restore(ctx, data); !errors.Is(err, order.ErrNotEmpty) {
		t.Fatalf("second restore: %v, want ErrNotEmpty", err)
	}
}

// TestRestoreWithoutState: an old agent that never reported agent.state —
// statuses come from main's mirror, drivers from main's roster, and the
// kitchen counter still skips past every number already handed out.
func TestRestoreWithoutState(t *testing.T) {
	ctx := context.Background()
	b := newFloorRepo(t)
	k := int64(1107)
	now := time.Now().UTC()
	data := order.RestoreData{
		Orders: []order.RestoreOrder{
			{Order: order.Snapshot{ID: "0190f0e8-0000-7000-8000-000000000001", TenantID: "t", LocationID: "l", Source: "online",
				SourceEventID: "e1", Status: "accepted", KitchenNumber: &k, ReceivedAt: now}},
			{Order: order.Snapshot{ID: "0190f0e8-0000-7000-8000-000000000002", TenantID: "t", LocationID: "l", Source: "online",
				SourceEventID: "e2", Status: "received", ReceivedAt: now}},
		},
		Drivers: []order.MirrorDriver{{ID: "d1", Name: "Carlos", Position: 1, Status: "available"}},
	}
	res, err := b.Restore(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	if res.Orders != 2 || res.Waiting != 1 || res.Drivers != 1 || res.KitchenNext != 1108 {
		t.Fatalf("restore result %+v", res)
	}
	ds, err := b.ListDrivers(ctx)
	if err != nil || len(ds) != 1 || ds[0].Name != "Carlos" {
		t.Fatalf("drivers %+v %v", ds, err)
	}
}
