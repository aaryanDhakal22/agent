package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/infra/database"
	"quiccpos/agent/internal/migrate"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
)

// 2.1.0: POS orders, the hub feed, driver numbers, future orders, close batch.

func posOrder(ftype, hash string) order.ExternalOrder {
	sent := time.Now().Add(-time.Minute).UTC()
	e := order.ExternalOrder{Source: order.ExtSourceSoftTouch, Hash: hash, DisplayNumber: 57, CheckType: ftype, SentAt: &sent,
		Customer: order.ExternalCustomer{DisplayName: "Pat Doe", Phone: "4105550100"},
		Items:    []order.ExternalItem{{Name: "14\" Cheese Pizza", Quantity: 1, UnitPriceCents: 1199, LineTotalCents: 1199}},
		Money:    order.ExternalMoney{SubtotalCents: 1199, TaxCents: 72, TotalCents: 1271}, CashDueCents: 1271}
	if ftype == "delivery" {
		e.Address = &order.Address{Line1: "931 Taylor Ave", City: "Towson", State: "MD", PostalCode: "21286"}
	}
	return e
}

func upsertPOS(t *testing.T, r *OrderRepository, ref string, e order.ExternalOrder) order.ExternalResult {
	t.Helper()
	res, err := r.UpsertExternal(context.Background(), ref, e, order.ExternalMeta{OrderID: uuid.Must(uuid.NewV7()).String(),
		TenantID: uuid.NewString(), LocationID: uuid.NewString(), PrepMinutes: 35, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func feedTypes(t *testing.T, r *OrderRepository) []string {
	t.Helper()
	evs, _, err := r.ListFeed(context.Background(), 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, e := range evs {
		out = append(out, e.Type)
	}
	return out
}

func TestUpsertExternalStoresLocalOnlyAccepted(t *testing.T) {
	r := newFloorRepo(t)
	e := posOrder("delivery", "h1")
	res := upsertPOS(t, r, "1276237", e)
	if !res.Created || res.Changed {
		t.Fatalf("first upsert = %+v", res)
	}
	so := res.Order
	st := so.Status
	if st.State != order.StateAccepted || st.KitchenNumber != nil || st.DisplayNumber == nil || *st.DisplayNumber != 57 ||
		!st.LocalOnly || st.ExtSource != "softtouch" || st.ExtRef != "1276237" || st.CashDueCents != 1271 {
		t.Fatalf("status = %+v", st)
	}
	if so.Order.Source != order.SourcePOS || so.Order.SourceEventID != "softtouch:1276237" || so.Order.CashDue() != 1271 {
		t.Fatalf("snapshot = %+v", so.Order)
	}
	if st.PrepMinutes == nil || *st.PrepMinutes != 35 || !st.EstimatedReadyAt.Equal(e.SentAt.Add(35*time.Minute).Truncate(time.Millisecond)) {
		t.Fatalf("prep = %v est = %v, want 35 from sent %v", st.PrepMinutes, st.EstimatedReadyAt, e.SentAt)
	}
	if res := upsertPOS(t, r, "1276237", e); res.Created || res.Changed || res.Order.Order.ID != so.Order.ID {
		t.Fatalf("same hash = %+v", res)
	}
	e2 := e
	e2.Hash, e2.Paid, e2.CashDueCents = "h2", true, 0
	e2.Items = append(e2.Items, order.ExternalItem{Name: "Wings", Quantity: 1, UnitPriceCents: 899, LineTotalCents: 899})
	res = upsertPOS(t, r, "1276237", e2)
	if res.Created || !res.Changed || res.AddressChanged || len(res.Order.Order.Items) != 2 || !res.Order.Status.Paid || res.Order.Order.CashDue() != 0 {
		t.Fatalf("changed = %+v", res)
	}
	e3 := e2
	e3.Hash, e3.Address = "h3", &order.Address{Line1: "1 Other St", City: "Towson"}
	if res = upsertPOS(t, r, "1276237", e3); !res.AddressChanged {
		t.Fatal("address change not reported")
	}
	// Never synced to main: no order.status (or anything naming it) in the outbox.
	for _, ev := range eventsOf(t, r) {
		if ev["order_id"] == so.Order.ID {
			t.Fatalf("POS order reached the main outbox: %v", ev)
		}
	}
}

func TestVoidAndFinishedPOSOrders(t *testing.T) {
	r := newFloorRepo(t)
	ctx := context.Background()
	res := upsertPOS(t, r, "900", posOrder("pickup", "a"))
	so, changed, err := r.VoidExternal(ctx, "softtouch", "900")
	if err != nil || !changed || so.Status.State != order.StateCancelled {
		t.Fatalf("void = %+v %v %v", so, changed, err)
	}
	if _, changed, _ = r.VoidExternal(ctx, "softtouch", "900"); changed {
		t.Fatal("second void changed something")
	}
	if _, _, err := r.VoidExternal(ctx, "softtouch", "nope"); !errors.Is(err, order.ErrNotFound) {
		t.Fatalf("unknown void = %v", err)
	}
	// A finished order still takes paid + money, never re-opens.
	e := posOrder("pickup", "b")
	e.Paid, e.Money.TotalCents = true, 999
	e.Items = nil
	got := upsertPOS(t, r, "900", e)
	if got.Created || !got.Changed || got.Order.Status.State != order.StateCancelled || !got.Order.Status.Paid ||
		got.Order.Order.Money.Total != 999 || len(got.Order.Order.Items) != 1 || got.Order.Order.ID != res.Order.Order.ID {
		t.Fatalf("finished upsert = %+v", got.Order)
	}
	if types := feedTypes(t, r); strings.Join(types, ",") != "order.cancelled" {
		t.Fatalf("feed = %v", types)
	}
}

func TestDriverPosNumberUnique(t *testing.T) {
	r := newFloorRepo(t)
	ctx := context.Background()
	a, err := r.CreateDriver(ctx, "Ann", "12")
	if err != nil || a.PosNumber != "12" {
		t.Fatalf("create = %+v %v", a, err)
	}
	if _, err := r.CreateDriver(ctx, "Bob", "12"); !errors.Is(err, order.ErrPosNumberTaken) {
		t.Fatalf("duplicate on create = %v", err)
	}
	b, _ := r.CreateDriver(ctx, "Bob", "")
	c, _ := r.CreateDriver(ctx, "Cat", "") // two drivers without a number are fine
	if err := r.SetDriverPosNumber(ctx, b.ID, "12"); !errors.Is(err, order.ErrPosNumberTaken) {
		t.Fatalf("duplicate on set = %v", err)
	}
	if err := r.SetDriverPosNumber(ctx, b.ID, "13"); err != nil {
		t.Fatal(err)
	}
	if err := r.SetDriverPosNumber(ctx, c.ID, ""); err != nil {
		t.Fatal(err)
	}
	if d, err := r.DriverByPosNumber(ctx, "13"); err != nil || d.ID != b.ID {
		t.Fatalf("by number = %+v %v", d, err)
	}
	// A removed driver's number is free again.
	if _, err := r.RemoveDriver(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CreateDriver(ctx, "Dee", "12"); err != nil {
		t.Fatalf("reuse after remove = %v", err)
	}
	if _, err := r.DriverByPosNumber(ctx, ""); !errors.Is(err, order.ErrDriverNotFound) {
		t.Fatalf("empty number = %v", err)
	}
	ds, _ := r.ListDrivers(ctx)
	got := map[string]string{}
	for _, d := range ds {
		got[d.Name] = d.PosNumber
	}
	if got["Bob"] != "13" || got["Dee"] != "12" || got["Cat"] != "" {
		t.Fatalf("roster numbers = %v", got)
	}
}

func TestFeedCursorAndPrune(t *testing.T) {
	r := newFloorRepo(t)
	ctx := context.Background()
	set := order.DefaultIntegrationSettings()
	for i := 0; i < 3; i++ {
		set.Times.DefaultPickupMinutes = int64(10 + i)
		if err := r.SetIntegrationSettings(ctx, set); err != nil {
			t.Fatal(err)
		}
	}
	evs, maxSeq, err := r.ListFeed(ctx, 0, 2)
	if err != nil || len(evs) != 2 || maxSeq != 3 || evs[0].Seq != 1 || evs[1].Seq != 2 || evs[0].Type != order.FeedSettingsChanged {
		t.Fatalf("page 1 = %+v max %d %v", evs, maxSeq, err)
	}
	var got order.IntegrationSettings
	if err := json.Unmarshal(evs[1].Data, &got); err != nil || got.Times.DefaultPickupMinutes != 11 {
		t.Fatalf("data = %s", evs[1].Data)
	}
	if evs, _, _ = r.ListFeed(ctx, 2, 10); len(evs) != 1 || evs[0].Seq != 3 {
		t.Fatalf("after 2 = %+v", evs)
	}
	if back, err := r.IntegrationSettings(ctx); err != nil || back.Times.DefaultPickupMinutes != 12 {
		t.Fatalf("stored = %+v %v", back, err)
	}
	// Kept 3 days; seq never reused after a prune.
	if _, err := r.db.ExecContext(ctx, `UPDATE integration_events SET created_at = ? WHERE seq <= 2`, fmtTS(time.Now().Add(-73*time.Hour))); err != nil {
		t.Fatal(err)
	}
	if _, err := r.TrimBatches(ctx, 3, time.Now().Add(-7*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if evs, _, _ = r.ListFeed(ctx, 0, 10); len(evs) != 1 || evs[0].Seq != 3 {
		t.Fatalf("after prune = %+v", evs)
	}
	if err := r.SetIntegrationSettings(ctx, set); err != nil {
		t.Fatal(err)
	}
	if _, maxSeq, _ = r.ListFeed(ctx, 0, 10); maxSeq != 4 {
		t.Fatalf("next seq = %d, want 4", maxSeq)
	}
}

func TestCloseBatchPOSAndScheduled(t *testing.T) {
	r := newFloorRepo(t)
	ctx := context.Background()
	cooking := upsertPOS(t, r, "1", posOrder("delivery", "a")).Order
	done := upsertPOS(t, r, "2", posOrder("pickup", "b")).Order
	if _, err := r.MarkReady(ctx, done.Order.ID); err != nil {
		t.Fatal(err)
	}
	future := arrive(t, r, "pickup")
	if _, err := r.Schedule(ctx, future.Order.ID, 20, time.Now().Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	old, _ := r.OpenBatch(ctx)

	// Neither the scheduled order nor the cooking POS check blocks a plain
	// close: the POS check is cancelled like at a forced one.
	if _, _, err := r.CloseBatch(ctx, false); err != nil {
		t.Fatal(err)
	}
	c := get(t, r, cooking.Order.ID)
	if c.Status.State != order.StateCancelled || c.Order.Customer.DisplayName != "" || c.Order.Customer.Phone != "" ||
		c.Order.Fulfillment.Address != nil || len(c.Order.Items) != 1 || c.Status.DisplayNumber == nil {
		t.Fatalf("cancelled POS = %+v", c)
	}
	d := get(t, r, done.Order.ID)
	if d.Status.State != order.StateReady || d.Order.Customer.DisplayName != "" || d.Order.Money.Total != 1271 {
		t.Fatalf("wiped POS = %+v", d)
	}
	f := get(t, r, future.Order.ID)
	nb, _ := r.OpenBatch(ctx)
	if f.Status.State != order.StateScheduled || f.Status.BatchID != nb.ID || nb.ID == old.ID {
		t.Fatalf("scheduled = %+v (batch %d)", f.Status, nb.ID)
	}
	types := feedTypes(t, r)
	if strings.Join(types, ",") != "order.ready,order.cancelled,batch.closed" {
		t.Fatalf("feed = %v", types)
	}
	// main hears about the scheduled online order, never about the POS ones.
	for _, ev := range eventsOf(t, r) {
		if ev["order_id"] == cooking.Order.ID || ev["order_id"] == done.Order.ID {
			t.Fatalf("POS in outbox: %v", ev)
		}
	}
}

func TestScheduleAndRaiseDue(t *testing.T) {
	r := newFloorRepo(t)
	ctx := context.Background()
	so := arrive(t, r, "delivery")
	ring := time.Now().Add(time.Hour)
	got, err := r.Schedule(ctx, so.Order.ID, 25, ring)
	if err != nil || got.Status.State != order.StateScheduled || *got.Status.ScheduledLeadMinutes != 25 || !got.Status.RingAt.Equal(ring.Truncate(time.Millisecond)) {
		t.Fatalf("schedule = %+v %v", got, err)
	}
	if ev := lastStatusEvent(t, r, so.Order.ID); ev["status"] != "scheduled" {
		t.Fatalf("event = %v", ev)
	}
	// Not in Deliveries / pools.
	open, _ := r.ListOpenDeliveries(ctx)
	if len(open) != 0 {
		t.Fatalf("open deliveries = %d", len(open))
	}
	if out, _ := r.RaiseDue(ctx, ring.Add(-time.Second)); len(out) != 0 {
		t.Fatal("raised early")
	}
	out, err := r.RaiseDue(ctx, ring.Add(time.Second))
	if err != nil || len(out) != 1 || out[0].Status.State != order.StateArrived || !out[0].Status.Rerung {
		t.Fatalf("raise = %+v %v", out, err)
	}
	// Escalation counts from the ring, not the first arrival.
	if od, _ := r.ListOverdueArrived(ctx, ring.Add(time.Second).Add(-2*time.Minute)); len(od) != 0 {
		t.Fatal("escalated at once")
	}
	if od, _ := r.ListOverdueArrived(ctx, ring.Add(2*time.Minute)); len(od) != 1 {
		t.Fatal("no escalation 2 min after the ring")
	}
	if _, err := r.Schedule(ctx, uuid.NewString(), 10, ring); !errors.Is(err, order.ErrNotFound) {
		t.Fatalf("missing = %v", err)
	}
}

// delivery.run reaches main with POS stops anonymous.
func TestDeliveryRunAnonymousPOSStops(t *testing.T) {
	r := newFloorRepo(t)
	ctx := context.Background()
	d, _ := r.CreateDriver(ctx, "Ann", "12")
	clockIn(t, r, d.ID)
	pos := upsertPOS(t, r, "777", posOrder("delivery", "a")).Order
	online := accept(t, r, arriveDelivery(t, r, "Erik Davis", 0, 2000, true))
	if _, err := r.MarkReady(ctx, online.Order.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.SendOut(ctx, d.ID, []string{pos.Order.ID, online.Order.ID}, true, order.RunMeta{SuggestedIDs: []string{pos.Order.ID}}); err != nil {
		t.Fatal(err)
	}
	var run map[string]any
	for _, ev := range eventsOf(t, r) {
		if ev["type"] == "delivery.run" {
			run = ev
		}
	}
	raw, _ := json.Marshal(run)
	if run == nil || strings.Contains(string(raw), pos.Order.ID) || strings.Contains(string(raw), "Taylor") || strings.Contains(string(raw), "Pat") {
		t.Fatalf("run leaks the POS order: %s", raw)
	}
	opaque := order.PosOpaqueID(pos.Status.BatchID, "777")
	if !strings.HasPrefix(opaque, "pos:") || len(opaque) != 16 || !strings.Contains(string(raw), `"opaque_id":"`+opaque+`"`) ||
		!strings.Contains(string(raw), `"source":"pos"`) || !strings.Contains(string(raw), online.Order.ID) {
		t.Fatalf("run = %s", raw)
	}
	// The hub's run.sent names both, the POS one by its check.
	evs, _, _ := r.ListFeed(ctx, 0, 100)
	var rs order.RunSentData
	for _, e := range evs {
		if e.Type == order.FeedRunSent {
			_ = json.Unmarshal(e.Data, &rs)
		}
	}
	if rs.Driver.PosNumber == nil || *rs.Driver.PosNumber != "12" || len(rs.Orders) != 2 || rs.RunID != run["event_id"] {
		t.Fatalf("run.sent = %+v", rs)
	}
	for _, o := range rs.Orders {
		if (o.OrderID == pos.Order.ID) != (o.ExtRef != nil && *o.ExtRef == "777") {
			t.Fatalf("run.sent order = %+v", o)
		}
	}
}

// 00014 rebuilds orders: an upgraded DB keeps its orders (and pools).
func TestMigrationTo00014KeepsOrders(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agent.db")
	if err := migrate.RunTo(ctx, path, 13); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO orders (id, tenant_id, location_id, source, source_event_id, snapshot, state, batch_id, pool_seq)
		VALUES ('0198b4a6-0000-7000-8000-000000000001', 't', 'l', 'online', '1', '{"id":"0198b4a6-0000-7000-8000-000000000001","fulfillment":{"type":"delivery"}}', 'ready', 1, 2)`); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	if err := migrate.Run(ctx, path); err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := NewOrderRepository(db, zerolog.Nop())
	so := get(t, r, "0198b4a6-0000-7000-8000-000000000001")
	if so.Status.State != order.StateReady || so.Status.PoolSeq == nil || *so.Status.PoolSeq != 2 || so.Status.LocalOnly {
		t.Fatalf("migrated = %+v", so.Status)
	}
	if set, err := r.IntegrationSettings(ctx); err != nil || set != order.DefaultIntegrationSettings() {
		t.Fatalf("settings = %+v %v", set, err)
	}
}

// Leftovers: an unfinished online order blocks a plain close, a POS check
// never does (it is still cancelled or finished at the forced close).
func TestCloseBatchLeftoversSkipPOS(t *testing.T) {
	r := newFloorRepo(t)
	ctx := context.Background()
	cooking := upsertPOS(t, r, "1", posOrder("pickup", "a")).Order
	online := accept(t, r, arrive(t, r, "pickup"))
	_, _, err := r.CloseBatch(ctx, false)
	var lo *order.LeftoversError
	if !errors.As(err, &lo) || len(lo.Orders) != 1 || lo.Orders[0].Order.ID != online.Order.ID {
		t.Fatalf("leftovers = %v", err)
	}
	if got := get(t, r, cooking.Order.ID); got.Status.State != order.StateAccepted {
		t.Fatalf("a refused close changed the POS check: %s", got.Status.State)
	}
	if _, _, err := r.CloseBatch(ctx, true); err != nil {
		t.Fatal(err)
	}
	if got := get(t, r, cooking.Order.ID); got.Status.State != order.StateCancelled {
		t.Fatalf("POS check after the close = %s", got.Status.State)
	}
	if got := get(t, r, online.Order.ID); got.Status.State != order.StateReady {
		t.Fatalf("online order after the close = %s", got.Status.State)
	}
}

func arriveAt(t *testing.T, r *OrderRepository, fulfillment string, promised time.Time) order.StoredOrder {
	t.Helper()
	p := promised.UTC()
	s := order.Snapshot{ID: uuid.NewString(), TenantID: uuid.NewString(), LocationID: uuid.NewString(),
		Source: "test", SourceEventID: uuid.NewString()[:6], PlacedAt: time.Now().UTC(),
		Fulfillment: order.Fulfillment{Type: fulfillment, PromisedAt: &p}}
	if _, err := r.UpsertArrived(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	return get(t, r, s.ID)
}

// A future order not accepted yet (never scheduled, or rung again) whose
// promised time is ahead moves into the next batch at Close batch, even a
// forced one; a past-due one is cancelled as before.
func TestCloseBatchMovesWaitingFutureOrders(t *testing.T) {
	r := newFloorRepo(t)
	ctx := context.Background()
	now := time.Now()
	fresh := arriveAt(t, r, "pickup", now.Add(3*time.Hour))
	if !fresh.Status.Future {
		t.Fatal("not a future order")
	}
	rerung := arriveAt(t, r, "delivery", now.Add(30*time.Minute))
	if _, err := r.Schedule(ctx, rerung.Order.ID, 40, now.Add(-10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	pastDue := arriveAt(t, r, "pickup", now.Add(-5*time.Minute))
	if _, err := r.Schedule(ctx, pastDue.Order.ID, 20, now.Add(-25*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if raised, err := r.RaiseDue(ctx, now); err != nil || len(raised) != 2 {
		t.Fatalf("raise = %d %v", len(raised), err)
	}
	soon := arrive(t, r, "pickup") // an ordinary order nobody accepted
	old, _ := r.OpenBatch(ctx)

	// Only the past-due and the ordinary order are leftovers.
	_, _, err := r.CloseBatch(ctx, false)
	var lo *order.LeftoversError
	if !errors.As(err, &lo) || len(lo.Orders) != 2 {
		t.Fatalf("leftovers = %v", err)
	}
	for _, so := range lo.Orders {
		if so.Order.ID != pastDue.Order.ID && so.Order.ID != soon.Order.ID {
			t.Fatalf("leftover %s", so.Order.ID)
		}
	}
	if _, _, err := r.CloseBatch(ctx, true); err != nil {
		t.Fatal(err)
	}
	nb, _ := r.OpenBatch(ctx)
	if nb.ID == old.ID {
		t.Fatal("no new batch")
	}
	for _, id := range []string{fresh.Order.ID, rerung.Order.ID} {
		got := get(t, r, id)
		if got.Status.State != order.StateArrived || got.Status.BatchID != nb.ID {
			t.Fatalf("waiting future order %s = %s in batch %d (new %d)", id, got.Status.State, got.Status.BatchID, nb.ID)
		}
	}
	if got := get(t, r, rerung.Order.ID); !got.Status.Rerung {
		t.Fatal("lost rerung")
	}
	for _, id := range []string{pastDue.Order.ID, soon.Order.ID} {
		if got := get(t, r, id); got.Status.State != order.StateCancelled || got.Status.BatchID != old.ID {
			t.Fatalf("order %s = %s in batch %d", id, got.Status.State, got.Status.BatchID)
		}
	}
}

// AUTO's suggested_ids reach main with POS orders by their opaque id — sent
// on this run or not — never their local id.
func TestDeliveryRunSuggestedPOSOpaque(t *testing.T) {
	r := newFloorRepo(t)
	ctx := context.Background()
	d, _ := r.CreateDriver(ctx, "Ann", "")
	clockIn(t, r, d.ID)
	sent := upsertPOS(t, r, "801", posOrder("delivery", "a")).Order
	left := upsertPOS(t, r, "802", posOrder("delivery", "b")).Order // suggested, not sent
	online := accept(t, r, arriveDelivery(t, r, "Erik Davis", 0, 2000, true))
	if _, err := r.MarkReady(ctx, online.Order.ID); err != nil {
		t.Fatal(err)
	}
	suggested := []string{sent.Order.ID, left.Order.ID, online.Order.ID, uuid.NewString()}
	if _, _, err := r.SendOut(ctx, d.ID, []string{sent.Order.ID, online.Order.ID}, true, order.RunMeta{SuggestedIDs: suggested}); err != nil {
		t.Fatal(err)
	}
	if suggested[0] != sent.Order.ID || suggested[1] != left.Order.ID {
		t.Fatal("the caller's suggestions were rewritten in place")
	}
	var run order.DeliveryRunEvent
	for _, ev := range eventsOf(t, r) {
		if ev["type"] == "delivery.run" {
			raw, _ := json.Marshal(ev)
			_ = json.Unmarshal(raw, &run)
		}
	}
	want := []string{order.PosOpaqueID(sent.Status.BatchID, "801"), order.PosOpaqueID(left.Status.BatchID, "802"), online.Order.ID}
	if strings.Join(run.SuggestedIDs, ",") != strings.Join(want, ",") {
		t.Fatalf("suggested_ids = %v, want %v", run.SuggestedIDs, want)
	}
}
