package repositories

import (
	"context"
	"errors"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"

	"github.com/google/uuid"
)

// arriveDelivery stores a delivery order with a customer, address, tip and a
// cash or card payment.
func arriveDelivery(t *testing.T, r *OrderRepository, name string, tip, total int64, cash bool) order.StoredOrder {
	t.Helper()
	pay := order.PaymentSummary{Method: "card", AmountCents: total, Last4: "4242"}
	if cash {
		pay = order.PaymentSummary{Method: "cash", AmountCents: total}
	}
	s := order.Snapshot{ID: uuid.NewString(), TenantID: uuid.NewString(), LocationID: uuid.NewString(),
		Source: "test", SourceEventID: uuid.NewString()[:6], PlacedAt: time.Now().UTC(),
		Customer:    order.Customer{DisplayName: name},
		Fulfillment: order.Fulfillment{Type: "delivery", Address: &order.Address{Line1: "12 Main St", Line2: "Apt 3"}},
		Money:       order.Money{Tip: tip, Total: total},
		Payments:    []order.PaymentSummary{pay}}
	if _, err := r.UpsertArrived(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	so, err := r.GetByID(context.Background(), uuid.MustParse(s.ID))
	if err != nil {
		t.Fatal(err)
	}
	return *so
}

func deliver(t *testing.T, r *OrderRepository, driverID string, orders ...order.StoredOrder) {
	t.Helper()
	ctx := context.Background()
	ids := []string{}
	for _, so := range orders {
		accept(t, r, so)
		if _, err := r.MarkReady(ctx, so.Order.ID); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, so.Order.ID)
	}
	if _, err := r.AssignDriver(ctx, driverID, ids); err != nil {
		t.Fatal(err)
	}
	if _, err := r.DriverReturned(ctx, driverID); err != nil {
		t.Fatal(err)
	}
}

func TestDriverReports(t *testing.T) {
	r := newFloorRepo(t)
	ctx := context.Background()
	abdul, _ := r.CreateDriver(ctx, "Abdul")
	carlos, _ := r.CreateDriver(ctx, "Carlos")
	deep, _ := r.CreateDriver(ctx, "Deep")
	clockIn(t, r, abdul.ID, carlos.ID, deep.ID)

	deliver(t, r, abdul.ID,
		arriveDelivery(t, r, "Erik Davis", 200, 2249, true),
		arriveDelivery(t, r, "Ana Perez", 300, 1850, true))
	deliver(t, r, abdul.ID, arriveDelivery(t, r, "Lee Min Ho", 400, 3000, false))
	deliver(t, r, carlos.ID, arriveDelivery(t, r, "Cher", 0, 1500, false))
	arrive(t, r, "pickup") // not a delivery: never on a report

	reps, err := r.DriverReports(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(reps) != 3 {
		t.Fatalf("reports = %d, want 3 (Deep clocked in, no runs)", len(reps))
	}
	if reps[0].DriverName != "Abdul" || reps[1].DriverName != "Carlos" || reps[2].DriverName != "Deep" {
		t.Fatalf("order = %s,%s,%s", reps[0].DriverName, reps[1].DriverName, reps[2].DriverName)
	}
	a := reps[0]
	a.Finish(600)
	if a.Deliveries != 3 || a.CashCents != 4099 || a.TipCents != 900 || a.TipAfterTaxCents != 846 || a.OwedCents != 3253 {
		t.Errorf("abdul = %+v", a)
	}
	if l := a.Lines[0]; l.Customer != "Erik D." || l.Address != "12 Main St, Apt 3" || !l.Cash || l.KitchenNumber == nil {
		t.Errorf("line = %+v", l)
	}
	if l := a.Lines[2]; l.Customer != "Lee H." || l.Cash || l.CashCents != 0 || l.TipCents != 400 {
		t.Errorf("card line = %+v", l)
	}
	c := reps[1]
	c.Finish(600)
	if c.Lines[0].Customer != "Cher" || c.CashCents != 0 || c.OwedCents != 0 {
		t.Errorf("carlos = %+v", c)
	}
	if len(reps[2].Lines) != 0 {
		t.Errorf("deep lines = %v", reps[2].Lines)
	}

	// Close: the closed batch keeps its reports; the new batch has only the
	// drivers still clocked in.
	b, _ := r.OpenBatch(ctx)
	if _, _, err := r.CloseBatch(ctx, true); err != nil {
		t.Fatal(err)
	}
	reps, err = r.DriverReports(ctx, b.ID)
	if err != nil || len(reps) != 3 || reps[0].BatchClosedAt == nil {
		t.Fatalf("closed batch reports = %v, %v", reps, err)
	}
	if _, err := r.DriverReports(ctx, 999); !errors.Is(err, order.ErrNotFound) {
		t.Errorf("unknown batch: %v", err)
	}
}

func TestUndoReturn(t *testing.T) {
	r := newFloorRepo(t)
	ctx := context.Background()
	abdul, _ := r.CreateDriver(ctx, "Abdul")
	carlos, _ := r.CreateDriver(ctx, "Carlos")
	clockIn(t, r, abdul.ID, carlos.ID)
	deliver(t, r, abdul.ID, arriveDelivery(t, r, "Erik Davis", 200, 2249, true))

	// Carlos never returned: not the latest return.
	if _, err := r.UndoReturn(ctx, carlos.ID, time.Now()); !errors.Is(err, order.ErrInvalidState) {
		t.Fatalf("undo carlos: %v", err)
	}
	// Too late.
	if _, err := r.UndoReturn(ctx, abdul.ID, time.Now().Add(11*time.Minute)); !errors.Is(err, order.ErrInvalidState) {
		t.Fatalf("undo late: %v", err)
	}
	d, err := r.UndoReturn(ctx, abdul.ID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != order.DriverOut {
		t.Fatalf("status = %s", d.Status)
	}
	ds, _ := r.ListDrivers(ctx)
	if ds[0].ID != abdul.ID {
		t.Errorf("abdul should be back at the head of the queue: %v", ds)
	}
	// Only once.
	if _, err := r.UndoReturn(ctx, abdul.ID, time.Now()); !errors.Is(err, order.ErrInvalidState) {
		t.Fatalf("second undo: %v", err)
	}
	// Returning again works (no orders on the run) and the order stays delivered.
	if out, err := r.DriverReturned(ctx, abdul.ID); err != nil || len(out) != 0 {
		t.Fatalf("returned again: %v %v", out, err)
	}
	reps, _ := r.DriverReports(ctx, 0)
	if reps[0].DriverName != "Abdul" || len(reps[0].Lines) != 1 {
		t.Errorf("report after undo = %+v", reps[0])
	}
}

func TestFloorPrefs(t *testing.T) {
	r := newFloorRepo(t)
	ctx := context.Background()
	p, err := r.FloorPrefs(ctx)
	if err != nil || p != order.DefaultFloorPrefs {
		t.Fatalf("defaults = %+v %v", p, err)
	}
	if err := r.SetFloorPrefs(ctx, order.FloorPrefs{GreenTillMinutes: 25, TipTaxBps: 650}); err != nil {
		t.Fatal(err)
	}
	if p, _ := r.FloorPrefs(ctx); p.GreenTillMinutes != 25 || p.TipTaxBps != 650 {
		t.Fatalf("prefs = %+v", p)
	}
}
