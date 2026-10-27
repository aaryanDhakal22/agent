package order

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/infra/database/repositories"
)

// 2.2.1 simple auto-assign. The other tests run with it off (noAutoAssign)
// so their pools stay exactly as they build them.

func noAutoAssign(t *testing.T, repo *repositories.OrderRepository) {
	t.Helper()
	p := order.DefaultDeliveryPrefs
	p.AutoAssign = false
	if err := repo.SetDeliveryPrefs(context.Background(), p); err != nil {
		t.Fatal(err)
	}
}

// autoFloor: store at (40.0, -74.0), auto-assign on, Ann and Bob clocked in
// (Ann first in the queue).
type autoFloor struct {
	svc      *Service
	repo     *repositories.OrderRepository
	ann, bob string
}

func newAutoFloor(t *testing.T) *autoFloor {
	t.Helper()
	ctx := context.Background()
	svc, repo, _ := newPoolService(t)
	svc.SetGeo(GeoDeps{Floor: func() (*FloorStore, *FloorMap) { return &FloorStore{Lat: 40.0, Lng: -74.0}, nil }})
	f := &autoFloor{svc: svc, repo: repo}
	ann, _ := svc.CreateDriver(ctx, "Ann Lee", "")
	bob, _ := svc.CreateDriver(ctx, "Bob", "")
	f.ann, f.bob = ann.ID, bob.ID
	svc.ClockIn(ctx, f.ann)
	svc.ClockIn(ctx, f.bob)
	if _, err := svc.UpdateDeliverySettings(ctx, DeliveryPatch{AutoAssign: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	svc.WaitBackground()
	return f
}

// located: a ready delivery placed ago, located at (lat, -74.0).
func (f *autoFloor) located(t *testing.T, ago time.Duration, lat float64, storeS int64) string {
	t.Helper()
	id := readyDelivery(t, f.svc, ago) // Ready runs the check: not located yet, nothing to give
	locate(t, f.svc, id, lat, -74.0, storeS)
	return id
}

func (f *autoFloor) pool(t *testing.T, driver string) []string {
	t.Helper()
	var ids []string
	for _, so := range f.svc.poolOf(context.Background(), driver) {
		ids = append(ids, so.Order.ID)
	}
	slices.Sort(ids)
	return ids
}

func TestAutoAssignOneOrderToTheNextDriver(t *testing.T) {
	ctx := context.Background()
	f := newAutoFloor(t)
	a := f.located(t, 10*time.Minute, 40.01, 150)
	f.svc.AutoAssignTick(ctx)
	if got := f.pool(t, f.ann); !slices.Equal(got, []string{a}) {
		t.Fatalf("Ann's pool %v", got)
	}
	if got := f.pool(t, f.bob); len(got) != 0 {
		t.Fatalf("Bob's pool %v", got)
	}
	// Pooled, not sent.
	if so := storedOf(t, f.svc, a); so.Status.State != order.StateReady || so.Status.PoolDriverID != f.ann {
		t.Fatalf("a: %+v", so.Status)
	}
	// Ann's pool isn't empty: the next order waits (Bob is not next).
	b := f.located(t, 5*time.Minute, 39.98, 300)
	f.svc.AutoAssignTick(ctx)
	if got := f.pool(t, f.ann); !slices.Equal(got, []string{a}) {
		t.Fatalf("Ann's pool grew: %v", got)
	}
	if so := storedOf(t, f.svc, b); so.Status.PoolDriverID != "" {
		t.Fatalf("b pooled to %s", so.Status.PoolDriverID)
	}
}

func TestAutoAssignBundlesAtMostThree(t *testing.T) {
	ctx := context.Background()
	f := newAutoFloor(t)
	f.svc.UpdateDeliverySettings(ctx, DeliveryPatch{AutoAssign: ptr(false)})
	ids := []string{
		f.located(t, 12*time.Minute, 40.004, 60),
		f.located(t, 11*time.Minute, 40.008, 100),
		f.located(t, 10*time.Minute, 40.012, 140),
		f.located(t, 9*time.Minute, 40.016, 180),
	}
	f.svc.UpdateDeliverySettings(ctx, DeliveryPatch{AutoAssign: ptr(true)})
	f.svc.WaitBackground()
	got := f.pool(t, f.ann)
	if len(got) != 3 || !slices.Contains(got, ids[0]) {
		t.Fatalf("Ann's pool %v", got)
	}
	// Stop numbers follow the planner's driving order.
	for _, id := range got {
		if so := storedOf(t, f.svc, id); so.Status.PoolSeq == nil {
			t.Fatalf("no pool_seq on %s", id)
		}
	}
}

func TestAutoAssignHoldsTheFarOneAndSkipsCameBack(t *testing.T) {
	ctx := context.Background()
	f := newAutoFloor(t)
	f.svc.UpdateDeliverySettings(ctx, DeliveryPatch{AutoAssign: ptr(false)})
	// Bob goes out with X, back soon; Ann is next.
	x := f.located(t, 15*time.Minute, 40.003, 60)
	if _, err := f.svc.AssignDriver(ctx, f.bob, []string{x}); err != nil {
		t.Fatal(err)
	}
	a := f.located(t, 20*time.Minute, 40.01, 150)
	b := f.located(t, 10*time.Minute, 39.98, 300) // far from A: held for Bob
	// A came-back order is never auto-assigned.
	c := f.located(t, 30*time.Minute, 40.011, 150)
	if _, err := f.svc.AssignDriver(ctx, f.ann, []string{c}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.NotDelivered(ctx, c, order.NotDeliveredRetry); err != nil {
		t.Fatal(err)
	}
	f.svc.UpdateDeliverySettings(ctx, DeliveryPatch{AutoAssign: ptr(true)})
	f.svc.WaitBackground()
	if got := f.pool(t, f.ann); !slices.Equal(got, []string{a}) {
		t.Fatalf("Ann's pool %v (a=%s b=%s c=%s)", got, a, b, c)
	}
	// Released: Ann's pool isn't empty, so C waits for the next idle driver.
	if _, err := f.svc.ReleaseCameBack(ctx, c); err != nil {
		t.Fatal(err)
	}
	f.svc.WaitBackground()
	if so := storedOf(t, f.svc, c); so.Status.PoolDriverID != "" {
		t.Fatalf("c pooled to %s", so.Status.PoolDriverID)
	}
}

func TestAutoAssignOnReturnGoesToTheNextDriver(t *testing.T) {
	ctx := context.Background()
	f := newAutoFloor(t)
	f.svc.UpdateDeliverySettings(ctx, DeliveryPatch{AutoAssign: ptr(false)})
	// Ann out with X; Bob is next (available) — but Bob has a pool he built.
	x := f.located(t, 15*time.Minute, 40.003, 60)
	if _, err := f.svc.AssignDriver(ctx, f.ann, []string{x}); err != nil {
		t.Fatal(err)
	}
	p := f.located(t, 5*time.Minute, 40.02, 200)
	if _, err := f.svc.Pool(ctx, []string{p}, f.bob); err != nil {
		t.Fatal(err)
	}
	w := f.located(t, 8*time.Minute, 40.01, 150)
	f.svc.UpdateDeliverySettings(ctx, DeliveryPatch{AutoAssign: ptr(true)})
	f.svc.WaitBackground()
	if so := storedOf(t, f.svc, w); so.Status.PoolDriverID != "" {
		t.Fatal("assigned while the next driver's pool isn't empty")
	}
	// Bob leaves; Ann returns — she goes to the back, but she is the only
	// one available now, so she is next and gets W on the returned call.
	if _, err := f.svc.SendOut(ctx, f.bob, SendOutRequest{}); err != nil {
		t.Fatal(err)
	}
	f.svc.WaitBackground()
	if so := storedOf(t, f.svc, w); so.Status.PoolDriverID != "" {
		t.Fatal("assigned with nobody available")
	}
	_, assigned, err := f.svc.ReturnDriver(ctx, f.ann)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(assigned, []string{w}) || !slices.Equal(f.pool(t, f.ann), []string{w}) {
		t.Fatalf("returned assigned %v, pool %v", assigned, f.pool(t, f.ann))
	}
}

func TestAutoAssignNextDriverIsNotTheReturningOne(t *testing.T) {
	ctx := context.Background()
	f := newAutoFloor(t)
	f.svc.UpdateDeliverySettings(ctx, DeliveryPatch{AutoAssign: ptr(false)})
	x := f.located(t, 15*time.Minute, 40.003, 60)
	if _, err := f.svc.AssignDriver(ctx, f.ann, []string{x}); err != nil {
		t.Fatal(err)
	}
	w := f.located(t, 8*time.Minute, 40.01, 150)
	f.svc.UpdateDeliverySettings(ctx, DeliveryPatch{AutoAssign: ptr(true)})
	f.svc.WaitBackground()
	// Bob was idle and next: he got W already; Ann returns to the back.
	if got := f.pool(t, f.bob); !slices.Equal(got, []string{w}) {
		t.Fatalf("Bob's pool %v", got)
	}
	_, assigned, err := f.svc.ReturnDriver(ctx, f.ann)
	if err != nil || len(assigned) != 0 {
		t.Fatalf("returned: %v %v", assigned, err)
	}
	// Another order: Bob (next, pool not empty) blocks it — never Ann.
	y := f.located(t, 2*time.Minute, 40.02, 200)
	f.svc.AutoAssignTick(ctx)
	if so := storedOf(t, f.svc, y); so.Status.PoolDriverID != "" {
		t.Fatalf("y pooled to %s", so.Status.PoolDriverID)
	}
}

func TestAutoAssignOffAndPreviewPending(t *testing.T) {
	ctx := context.Background()
	f := newAutoFloor(t)
	f.svc.UpdateDeliverySettings(ctx, DeliveryPatch{AutoAssign: ptr(false)})
	a := f.located(t, 10*time.Minute, 40.01, 150)
	f.svc.AutoAssignTick(ctx)
	if got := f.pool(t, f.ann); len(got) != 0 {
		t.Fatalf("off: %v", got)
	}
	if fl, _ := f.svc.Floor(ctx); fl.AutoAssign {
		t.Fatal("floor auto_assign")
	}
	// An AUTO preview on a tablet: wait for it.
	if _, err := f.svc.PlanDeliveries(ctx); err != nil {
		t.Fatal(err)
	}
	f.svc.UpdateDeliverySettings(ctx, DeliveryPatch{AutoAssign: ptr(true)})
	f.svc.WaitBackground()
	f.svc.AutoAssignTick(ctx)
	if got := f.pool(t, f.ann); len(got) != 0 {
		t.Fatalf("assigned during an AUTO preview: %v", got)
	}
	_ = a
}

func TestAutoAssignedEvent(t *testing.T) {
	ctx := context.Background()
	svc, repo, events := newPoolService(t)
	svc.SetGeo(GeoDeps{Floor: func() (*FloorStore, *FloorMap) { return &FloorStore{Lat: 40.0, Lng: -74.0}, nil }})
	f := &autoFloor{svc: svc, repo: repo}
	ann, _ := svc.CreateDriver(ctx, "Juma Ali", "")
	f.ann = ann.ID
	svc.ClockIn(ctx, ann.ID)
	a := f.located(t, 10*time.Minute, 40.01, 150)
	drain(events)
	svc.UpdateDeliverySettings(ctx, DeliveryPatch{AutoAssign: ptr(true)})
	svc.WaitBackground()
	data, ok := eventOf(events, EventAutoAssigned)
	if !ok {
		t.Fatal("no deliveries.auto_assigned")
	}
	var ev AutoAssigned
	if json.Unmarshal(data, &ev) != nil || ev.DriverID != ann.ID || ev.DriverName != "Juma Ali" || !slices.Equal(ev.OrderIDs, []string{a}) {
		t.Fatalf("event %s", data)
	}
}
