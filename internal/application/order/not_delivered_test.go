package order

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/infra/database/repositories"

	"github.com/google/uuid"
)

// 2.2.1: "Not delivered" — retry (the came-back list) or cancel; the order
// leaves its driver's run and no driver report counts it.

func notDeliveredEvents(t *testing.T, repo *repositories.OrderRepository) []order.NotDeliveredEvent {
	t.Helper()
	evs, err := repo.ListUnsyncedEvents(context.Background(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	var out []order.NotDeliveredEvent
	for _, ev := range evs {
		if ev.Type != order.FloorEventNotDelivered {
			continue
		}
		var nd order.NotDeliveredEvent
		if err := json.Unmarshal(ev.Payload, &nd); err != nil {
			t.Fatal(err)
		}
		out = append(out, nd)
	}
	return out
}

func driverStatus(t *testing.T, svc *Service, id string) string {
	t.Helper()
	ds, err := svc.ListDrivers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range ds {
		if d.ID == id {
			return d.Status
		}
	}
	t.Fatalf("driver %s not found", id)
	return ""
}

func reportFor(t *testing.T, svc *Service, driverID string) *order.DriverReport {
	t.Helper()
	reps, err := svc.DriverReports(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := range reps {
		if reps[i].DriverID == driverID {
			return &reps[i]
		}
	}
	return nil
}

func TestNotDeliveredRetryReleaseAndDeliverLater(t *testing.T) {
	ctx := context.Background()
	svc, repo, events := newPoolService(t)
	ann, _ := svc.CreateDriver(ctx, "Ann", "")
	bob, _ := svc.CreateDriver(ctx, "Bob", "")
	svc.ClockIn(ctx, ann.ID)
	svc.ClockIn(ctx, bob.ID)
	a := readyDelivery(t, svc, 5*time.Minute)
	b := readyDelivery(t, svc, 5*time.Minute)
	if _, err := svc.AssignDriver(ctx, ann.ID, []string{a, b}); err != nil {
		t.Fatal(err)
	}
	drain(events)

	// One of two stops comes back: Ann stays out with the other.
	so, err := svc.NotDelivered(ctx, a, order.NotDeliveredRetry)
	if err != nil {
		t.Fatal(err)
	}
	st := so.Status
	if st.State != order.StateReady || !st.CameBack || st.DriverID != "" || st.DriverName != "" || st.PoolDriverID != "" ||
		st.OutAt != nil || st.NotDelivered == nil || st.NotDelivered.Action != "retry" || st.NotDelivered.DriverID != ann.ID ||
		st.NotDelivered.At.IsZero() {
		t.Fatalf("after retry: %+v (nd %+v)", st, st.NotDelivered)
	}
	if got := driverStatus(t, svc, ann.ID); got != order.DriverOut {
		t.Fatalf("Ann = %s, want out (b still on the run)", got)
	}
	if _, ok := eventOf(events, "order.updated"); !ok {
		t.Fatal("no order.updated")
	}
	if _, ok := eventOf(events, "drivers.updated"); !ok {
		t.Fatal("no drivers.updated")
	}
	nds := notDeliveredEvents(t, repo)
	if len(nds) != 1 || nds[0].OrderID != a || nds[0].Action != "retry" || nds[0].Status != order.StateReady ||
		nds[0].DriverID != ann.ID || nds[0].DriverName != "Ann" || nds[0].AgentStatus == nil || !nds[0].AgentStatus.CameBack {
		t.Fatalf("floor events: %+v", nds)
	}
	feed := feedOf(t, svc)
	last := feed[len(feed)-1]
	var fd order.OrderNotDeliveredData
	_ = json.Unmarshal(last.Data, &fd)
	if last.Type != order.FeedOrderNotDelivered || fd.OrderID != a || fd.ExtRef != nil || fd.Action != "retry" ||
		fd.DriverID != ann.ID || fd.DriverPosNumber != nil {
		t.Fatalf("feed: %s %s", last.Type, last.Data)
	}

	// Again on the same order: it is no longer out.
	if _, err := svc.NotDelivered(ctx, a, order.NotDeliveredCancel); !errors.Is(err, order.ErrNotOutOrDelivered) {
		t.Fatalf("second not-delivered: %v", err)
	}

	// Held: no pool, no send-out, no AUTO, no assign.
	if _, err := svc.Pool(ctx, []string{a}, bob.ID); !errors.Is(err, order.ErrCameBack) {
		t.Fatalf("pool: %v", err)
	}
	if _, err := svc.SendOut(ctx, bob.ID, SendOutRequest{Include: []string{a}, MarkReady: true}); !errors.Is(err, order.ErrCameBack) {
		t.Fatalf("send-out include: %v", err)
	}
	if _, err := svc.AssignDriver(ctx, bob.ID, []string{a}); !errors.Is(err, order.ErrCameBack) {
		t.Fatalf("assign: %v", err)
	}
	locate(t, svc, a, 40.001, -74.0, 120)
	plan, err := svc.PlanDeliveries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range plan.Runs {
		for _, id := range r.OrderIDs {
			if id == a {
				t.Fatal("AUTO planned a came-back order")
			}
		}
	}
	for _, u := range plan.Unassigned {
		if u.OrderID == a {
			t.Fatal("AUTO listed a came-back order")
		}
	}

	// Ann returns: b delivered; a counts on no report of hers.
	if out, err := svc.DriverReturned(ctx, ann.ID); err != nil || len(out) != 1 || out[0].Order.ID != b {
		t.Fatalf("returned: %v %v", out, err)
	}
	if rep := reportFor(t, svc, ann.ID); rep == nil || len(rep.Lines) != 1 || rep.Lines[0].OrderID != b {
		t.Fatalf("Ann's report: %+v", rep)
	}

	// Release: a normal unassigned ready delivery (the record stays).
	if _, err := svc.ReleaseCameBack(ctx, b); !errors.Is(err, order.ErrNotCameBack) {
		t.Fatalf("release a delivered order: %v", err)
	}
	rel, err := svc.ReleaseCameBack(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if rel.Status.CameBack || rel.Status.State != order.StateReady || rel.Status.NotDelivered == nil {
		t.Fatalf("released: %+v", rel.Status)
	}
	if _, err := svc.ReleaseCameBack(ctx, a); !errors.Is(err, order.ErrNotCameBack) {
		t.Fatalf("release twice: %v", err)
	}

	// Bob takes it later: it counts for Bob only.
	if _, err := svc.SendOut(ctx, bob.ID, SendOutRequest{Include: []string{a}}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.DriverReturned(ctx, bob.ID); err != nil {
		t.Fatal(err)
	}
	if rep := reportFor(t, svc, bob.ID); rep == nil || len(rep.Lines) != 1 || rep.Lines[0].OrderID != a {
		t.Fatalf("Bob's report: %+v", rep)
	}
	if rep := reportFor(t, svc, ann.ID); rep == nil || len(rep.Lines) != 1 {
		t.Fatalf("Ann's report after Bob: %+v", rep)
	}
	ds, _ := svc.ListDrivers(ctx)
	for _, d := range ds {
		if d.ID == ann.ID && d.Deliveries != 1 || d.ID == bob.ID && d.Deliveries != 1 {
			t.Fatalf("deliveries: %s %d", d.Name, d.Deliveries)
		}
	}
}

func TestNotDeliveredCancelAfterReturnAndLastStop(t *testing.T) {
	ctx := context.Background()
	svc, repo, _ := newPoolService(t)
	ann, _ := svc.CreateDriver(ctx, "Ann", "")
	svc.ClockIn(ctx, ann.ID)
	a := readyDelivery(t, svc, 5*time.Minute)
	if _, err := svc.AssignDriver(ctx, ann.ID, []string{a}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.DriverReturned(ctx, ann.ID); err != nil {
		t.Fatal(err)
	}
	// Delivered, the driver already back: the order just leaves that run.
	so, err := svc.NotDelivered(ctx, a, order.NotDeliveredCancel)
	if err != nil {
		t.Fatal(err)
	}
	if so.Status.State != order.StateCancelled || so.Status.CameBack || so.Status.CancelledAt == nil ||
		so.Status.DeliveredAt != nil || so.Status.DriverID != "" || so.Status.NotDelivered.Action != "cancel" {
		t.Fatalf("cancelled: %+v", so.Status)
	}
	if got := driverStatus(t, svc, ann.ID); got != order.DriverAvailable {
		t.Fatalf("Ann = %s", got)
	}
	if rep := reportFor(t, svc, ann.ID); rep == nil || len(rep.Lines) != 0 || rep.CashCents != 0 {
		t.Fatalf("Ann's report: %+v", rep) // still a slip (clocked in), no line
	}
	if nds := notDeliveredEvents(t, repo); len(nds) != 1 || nds[0].Status != order.StateCancelled || nds[0].Action != "cancel" {
		t.Fatalf("events: %+v", nds)
	}
	// History keeps it, cancelled.
	list, _ := svc.ListByBatch(ctx, 0, 0, 50)
	if len(list) != 1 || list[0].Status.State != order.StateCancelled {
		t.Fatalf("batch list: %+v", list)
	}

	// The only stop of a run still out: the driver is back in the queue.
	b := readyDelivery(t, svc, time.Minute)
	if _, err := svc.AssignDriver(ctx, ann.ID, []string{b}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.NotDelivered(ctx, b, order.NotDeliveredRetry); err != nil {
		t.Fatal(err)
	}
	if got := driverStatus(t, svc, ann.ID); got != order.DriverAvailable {
		t.Fatalf("Ann after her last stop came back = %s", got)
	}
	// Returned can't be pressed for a run that's gone.
	if _, err := svc.DriverReturned(ctx, ann.ID); !errors.Is(err, order.ErrInvalidState) {
		t.Fatalf("returned: %v", err)
	}

	// Close batch: the came-back order blocks a plain close and is cancelled
	// (not auto-delivered) by a forced one.
	if _, left, err := svc.CloseBatch(ctx, false); err == nil || len(left) != 1 || left[0].Order.ID != b {
		t.Fatalf("plain close: %v %v", left, err)
	}
	if _, _, err := svc.CloseBatch(ctx, true); err != nil {
		t.Fatal(err)
	}
	fresh := storedOf(t, svc, b)
	if fresh.Status.State != order.StateCancelled || fresh.Status.CameBack || fresh.Status.DeliveredAt != nil {
		t.Fatalf("after close: %+v", fresh.Status)
	}
	nds := notDeliveredEvents(t, repo)
	if l := nds[len(nds)-1]; l.OrderID != b || l.Action != "cancel" || !l.Auto || l.DriverID != ann.ID {
		t.Fatalf("close event: %+v", l)
	}
}

func TestNotDeliveredRefusals(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newPoolService(t)
	a := readyDelivery(t, svc, time.Minute)
	if _, err := svc.NotDelivered(ctx, a, "lost"); !errors.Is(err, ErrBadAction) {
		t.Fatalf("bad action: %v", err)
	}
	if _, err := svc.NotDelivered(ctx, a, order.NotDeliveredRetry); !errors.Is(err, order.ErrNotOutOrDelivered) {
		t.Fatalf("ready: %v", err)
	}
	if _, err := svc.NotDelivered(ctx, uuid.NewString(), order.NotDeliveredRetry); !errors.Is(err, order.ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	// A pickup is never out.
	s := order.Snapshot{ID: uuid.NewString(), TenantID: uuid.NewString(), LocationID: uuid.NewString(), Source: "online",
		SourceEventID: "2", PlacedAt: time.Now().UTC(), Fulfillment: order.Fulfillment{Type: "pickup"}}
	if err := svc.OnArrival(ctx, s); err != nil {
		t.Fatal(err)
	}
	p := 10
	svc.Accept(ctx, uuid.MustParse(s.ID), &p)
	svc.Ready(ctx, s.ID)
	if _, err := svc.NotDelivered(ctx, s.ID, order.NotDeliveredCancel); !errors.Is(err, order.ErrNotOutOrDelivered) {
		t.Fatalf("pickup: %v", err)
	}
	// A delivery of a closed batch is history.
	ann, _ := svc.CreateDriver(ctx, "Ann", "")
	svc.ClockIn(ctx, ann.ID)
	svc.AssignDriver(ctx, ann.ID, []string{a})
	svc.DriverReturned(ctx, ann.ID)
	if _, _, err := svc.CloseBatch(ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.NotDelivered(ctx, a, order.NotDeliveredCancel); !errors.Is(err, order.ErrNotOutOrDelivered) {
		t.Fatalf("closed batch: %v", err)
	}
}

// A POS delivery: the hub hears order.not_delivered with the check and the
// driver number; nothing reaches main; the POS still showing that driver is
// not mirrored back, a later assignment at the terminal is.
func TestNotDeliveredPOS(t *testing.T) {
	ctx := context.Background()
	svc, repo, _, _, _ := newIntegService(t)
	ann, _ := svc.CreateDriver(ctx, "Ann", "12")
	svc.ClockIn(ctx, ann.ID)
	e := extOrder("delivery", "a")
	num := "12"
	e.DriverPosNumber = &num
	res := mustUpsert(t, svc, "100", e)
	if so := storedOf(t, svc, res.OrderID); so.Status.State != order.StateOnDelivery {
		t.Fatalf("mirror: %+v", so.Status)
	}
	floorBefore, _ := repo.ListUnsyncedEvents(ctx, 1000)

	so, err := svc.NotDelivered(ctx, res.OrderID, order.NotDeliveredRetry)
	if err != nil {
		t.Fatal(err)
	}
	if !so.Status.CameBack || so.Status.State != order.StateReady {
		t.Fatalf("POS retry: %+v", so.Status)
	}
	if got := driverStatus(t, svc, ann.ID); got != order.DriverAvailable {
		t.Fatalf("Ann = %s", got)
	}
	feed := feedOf(t, svc)
	last := feed[len(feed)-1]
	var fd order.OrderNotDeliveredData
	_ = json.Unmarshal(last.Data, &fd)
	if last.Type != order.FeedOrderNotDelivered || fd.ExtRef == nil || *fd.ExtRef != "100" || fd.Source != order.SourcePOS ||
		fd.DriverPosNumber == nil || *fd.DriverPosNumber != "12" || fd.Action != "retry" || fd.DriverName != "Ann" {
		t.Fatalf("feed: %s", last.Data)
	}
	floorAfter, _ := repo.ListUnsyncedEvents(ctx, 1000)
	for _, ev := range floorAfter[len(floorBefore):] {
		if ev.Type == order.FloorEventNotDelivered || ev.OrderID == res.OrderID {
			t.Fatalf("a POS order reached main: %s %s", ev.Type, ev.Payload)
		}
	}

	// The POS still shows #12 (another change on the check): not sent again.
	e.Hash = "a2"
	mustUpsert(t, svc, "100", e)
	if so := storedOf(t, svc, res.OrderID); so.Status.State != order.StateReady || !so.Status.CameBack {
		t.Fatalf("stale #12 mirrored: %+v", so.Status)
	}
	// The hub cleared it; then the cashier assigns #12 again: mirrored.
	e.Hash, e.DriverPosNumber = "a3", nil
	mustUpsert(t, svc, "100", e)
	e.Hash, e.DriverPosNumber = "a4", &num
	mustUpsert(t, svc, "100", e)
	if so := storedOf(t, svc, res.OrderID); so.Status.State != order.StateOnDelivery || so.Status.CameBack || so.Status.DriverID != ann.ID {
		t.Fatalf("re-assigned at the terminal: %+v", so.Status)
	}

	// Cancel this time; a different number set at the POS before the clear
	// would mirror, but a cancelled order is finished.
	if _, err := svc.NotDelivered(ctx, res.OrderID, order.NotDeliveredCancel); err != nil {
		t.Fatal(err)
	}
	if so := storedOf(t, svc, res.OrderID); so.Status.State != order.StateCancelled {
		t.Fatalf("POS cancel: %+v", so.Status)
	}
}
