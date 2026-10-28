package repositories

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"

	"github.com/google/uuid"
)

// 00019: terminal changes to POS checks, finishing orders, the update gate's
// open count, callout feed events.

func kitchenCheck(ftype, hash string, lines ...order.ExternalItem) order.ExternalOrder {
	e := posOrder(ftype, hash)
	if len(lines) == 0 {
		lines = []order.ExternalItem{{Ref: "1", Name: "14\" Cheese Pizza", Quantity: 1, UnitPriceCents: 1199, LineTotalCents: 1199, Kitchen: true}}
	}
	e.Items = lines
	return e
}

var (
	pizza  = order.ExternalItem{Ref: "1", Name: "14\" Cheese Pizza", Quantity: 1, UnitPriceCents: 1199, LineTotalCents: 1199, Kitchen: true}
	knots  = order.ExternalItem{Ref: "2", Name: "Garlic Knots", Quantity: 1, UnitPriceCents: 499, LineTotalCents: 499, Kitchen: true}
	cola   = order.ExternalItem{Ref: "3", Name: "Coke", Quantity: 1, UnitPriceCents: 199, LineTotalCents: 199}
	wings  = order.ExternalItem{Ref: "4", Name: "Wings", Quantity: 1, UnitPriceCents: 899, LineTotalCents: 899, Kitchen: true}
	expect = func(t *testing.T, ok bool, format string, a ...any) {
		t.Helper()
		if !ok {
			t.Fatalf(format, a...)
		}
	}
)

func TestPOSChangeKitchenAddRingsAndMarksAdded(t *testing.T) {
	r := newFloorRepo(t)
	upsertPOS(t, r, "900", kitchenCheck("pickup", "h1", pizza))
	res := upsertPOS(t, r, "900", kitchenCheck("pickup", "h2", pizza, knots))
	c := res.Change
	expect(t, c != nil && c.Kind == order.ChangeCooking && c.Text == "#57 changed: +1 Garlic Knots", "change = %+v", c)
	items := res.Order.Order.Items
	expect(t, len(items) == 2 && !items[0].Added && items[1].Added && items[1].PosRef == "2", "marks = %+v", items)
	expect(t, res.Order.Status.PosChange != nil && res.Order.Status.PosChange.ID == c.ID, "stored change = %+v", res.Order.Status.PosChange)

	// A void before OK merges into the same bar; the voided line stays
	// struck through and goes to the void ticket.
	res = upsertPOS(t, r, "900", kitchenCheck("pickup", "h3", knots))
	c2 := res.Change
	expect(t, c2 != nil && c2.ID != c.ID && c2.Text == "#57 changed: +1 Garlic Knots, −1 14\" Cheese Pizza", "merged = %+v", c2)
	so := res.Order
	expect(t, len(so.Order.Items) == 1 && so.Order.Items[0].Added, "items = %+v", so.Order.Items)
	expect(t, len(so.Order.RemovedItems) == 1 && so.Order.RemovedItems[0].PosRef == "1", "removed = %+v", so.Order.RemovedItems)
	expect(t, len(res.VoidPrint) == 1 && res.VoidPrint[0].Name == pizza.Name, "void print = %+v", res.VoidPrint)

	// OK on an older change id keeps ringing; the current one clears it.
	ctx := context.Background()
	if _, acked, err := r.AckPosChange(ctx, so.Order.ID, c.ID); err != nil || acked {
		t.Fatalf("stale ack = %v %v", acked, err)
	}
	got, acked, err := r.AckPosChange(ctx, so.Order.ID, c2.ID)
	expect(t, err == nil && acked && got.Status.PosChange != nil && !got.Status.PosChange.Pending(), "ack = %v %v %+v", acked, err, got)
	// A change after OK starts a new bar (not merged into the old one).
	res = upsertPOS(t, r, "900", kitchenCheck("pickup", "h4", knots, wings))
	expect(t, res.Change != nil && res.Change.Pending() && res.Change.Text == "#57 changed: +1 Wings", "after OK = %+v", res.Change)
	// The marks stay after OK.
	expect(t, got.Order.Items[0].Added && len(got.Order.RemovedItems) == 1, "marks after ack = %+v", got.Order)
}

func TestPOSChangeDrinksOnlyIsQuiet(t *testing.T) {
	r := newFloorRepo(t)
	upsertPOS(t, r, "901", kitchenCheck("pickup", "h1", pizza))
	res := upsertPOS(t, r, "901", kitchenCheck("pickup", "h2", pizza, cola))
	expect(t, res.Change == nil && res.Order.Status.PosChange == nil, "a drink rang: %+v", res.Change)
	expect(t, res.Order.Order.Items[1].Added, "drink not marked added")
	res = upsertPOS(t, r, "901", kitchenCheck("pickup", "h3", pizza))
	expect(t, res.Change == nil && len(res.VoidPrint) == 0 && len(res.Order.Order.RemovedItems) == 1, "drink void = %+v", res)
}

func TestPOSChangeReadyGoesBackToCooking(t *testing.T) {
	ctx := context.Background()
	r := newFloorRepo(t)
	// A ready pickup.
	so := upsertPOS(t, r, "902", kitchenCheck("pickup", "h1", pizza)).Order
	if _, err := r.MarkReady(ctx, so.Order.ID); err != nil {
		t.Fatal(err)
	}
	res := upsertPOS(t, r, "902", kitchenCheck("pickup", "h2", pizza, knots))
	st := res.Order.Status
	expect(t, res.Reopened && st.State == order.StateAccepted && st.ReadyAt == nil, "pickup not back to cooking: %+v", st)
	expect(t, res.Change != nil && res.Change.Kind == order.ChangeReopened, "change = %+v", res.Change)

	// A ready delivery in a pool leaves the pool.
	d, _ := r.CreateDriver(ctx, "Abdul", "")
	clockIn(t, r, d.ID)
	del := upsertPOS(t, r, "903", kitchenCheck("delivery", "h1", pizza)).Order
	if _, err := r.MarkReady(ctx, del.Order.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.PoolOrders(ctx, []string{del.Order.ID}, d.ID); err != nil {
		t.Fatal(err)
	}
	res = upsertPOS(t, r, "903", kitchenCheck("delivery", "h2", pizza, wings))
	st = res.Order.Status
	expect(t, res.Reopened && st.State == order.StateAccepted && st.PoolDriverID == "", "delivery = %+v", st)

	// Ready + only a removal: stays ready, still rings.
	so2 := upsertPOS(t, r, "904", kitchenCheck("pickup", "h1", pizza, knots)).Order
	if _, err := r.MarkReady(ctx, so2.Order.ID); err != nil {
		t.Fatal(err)
	}
	res = upsertPOS(t, r, "904", kitchenCheck("pickup", "h2", pizza))
	expect(t, !res.Reopened && res.Order.Status.State == order.StateReady && res.Change != nil && res.Change.Kind == order.ChangeReady,
		"ready removal = %+v / %+v", res.Order.Status, res.Change)
}

func TestPOSChangeOutDeliveryAlertsOnly(t *testing.T) {
	ctx := context.Background()
	r := newFloorRepo(t)
	d, _ := r.CreateDriver(ctx, "Abdul", "")
	clockIn(t, r, d.ID)
	so := upsertPOS(t, r, "905", kitchenCheck("delivery", "h1", pizza)).Order
	if _, err := r.MarkReady(ctx, so.Order.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.AssignDriver(ctx, d.ID, []string{so.Order.ID}); err != nil {
		t.Fatal(err)
	}
	res := upsertPOS(t, r, "905", kitchenCheck("delivery", "h2", pizza, knots))
	expect(t, res.Order.Status.State == order.StateOnDelivery && !res.Reopened, "state = %s", res.Order.Status.State)
	expect(t, res.Change != nil && res.Change.Kind == order.ChangeOut && res.Change.Text == "Abdul already left with #57 — +1 Garlic Knots",
		"change = %+v", res.Change)
	expect(t, len(res.VoidPrint) == 0, "void print while out")
}

func TestPOSHiddenCheckShownLaterAndStaysShown(t *testing.T) {
	r := newFloorRepo(t)
	small := kitchenCheck("dine_in", "h1", cola)
	small.BoardHidden = true
	res := upsertPOS(t, r, "906", small)
	expect(t, res.Order.Status.BoardHidden, "not hidden")
	// A hidden check changing rings nobody.
	grown := kitchenCheck("dine_in", "h2", cola, pizza)
	res = upsertPOS(t, r, "906", grown)
	expect(t, res.Shown && !res.Order.Status.BoardHidden && res.Change == nil, "shown = %+v", res)
	expect(t, !res.Order.Order.Items[1].Added, "a newly shown check carries ADDED marks")
	// Once shown it stays on Kitchen even when the hub says hidden again.
	back := kitchenCheck("dine_in", "h3", cola)
	back.BoardHidden = true
	res = upsertPOS(t, r, "906", back)
	expect(t, !res.Order.Status.BoardHidden && !res.Shown, "hidden again: %+v", res.Order.Status)
	expect(t, res.Change != nil && len(res.Order.Order.RemovedItems) == 1, "removal on a shown check: %+v", res.Change)
}

func TestPOSClosedFinishes(t *testing.T) {
	ctx := context.Background()
	r := newFloorRepo(t)
	// Ready, then closed at the POS.
	so := upsertPOS(t, r, "910", kitchenCheck("pickup", "h1")).Order
	if _, err := r.MarkReady(ctx, so.Order.ID); err != nil {
		t.Fatal(err)
	}
	e := kitchenCheck("pickup", "h2")
	e.Closed, e.Paid = true, true
	res := upsertPOS(t, r, "910", e)
	st := res.Order.Status
	expect(t, res.Finished && st.FinishedAt != nil && st.FinishedBy == order.FinishedByPOS && st.PosClosed && st.State == order.StateReady, "ready+closed = %+v", st)

	// Closed while cooking: stays on Kitchen, finished when marked ready.
	so = upsertPOS(t, r, "911", kitchenCheck("dine_in", "h1")).Order
	e = kitchenCheck("dine_in", "h2")
	e.Closed = true
	res = upsertPOS(t, r, "911", e)
	expect(t, !res.Finished && res.Order.Status.State == order.StateAccepted && res.Order.Status.PosClosed, "cooking+closed = %+v", res.Order.Status)
	got, err := r.MarkReady(ctx, so.Order.ID)
	expect(t, err == nil && got.Status.FinishedAt != nil && got.Status.FinishedBy == order.FinishedByPOS, "ready after close = %+v %v", got, err)

	// Never on Kitchen (hidden) and closed: ready + finished at once, and no
	// order.ready on the feed (nothing to call out).
	h := kitchenCheck("dine_in", "h1", cola)
	h.BoardHidden, h.Closed = true, true
	res = upsertPOS(t, r, "912", h)
	expect(t, res.Created && res.Finished && res.Order.Status.State == order.StateReady && res.Order.Status.FinishedAt != nil, "hidden closed = %+v", res.Order.Status)

	// A delivery is never finished by the POS (the driver returns it).
	dl := kitchenCheck("delivery", "h1")
	dl.Closed = true
	res = upsertPOS(t, r, "913", dl)
	expect(t, !res.Finished && res.Order.Status.FinishedAt == nil, "delivery finished")

	n := 0
	for _, typ := range feedTypes(t, r) {
		if typ == order.FeedOrderReady {
			n++
		}
	}
	expect(t, n == 2, "order.ready events = %d, want 2 (910, 911)", n)
}

func TestFinishDueOnlinePickupsAndUpdateGate(t *testing.T) {
	ctx := context.Background()
	r := newFloorRepo(t)
	pick := accept(t, r, arrive(t, r, "pickup"))
	dlv := accept(t, r, arrive(t, r, "delivery"))
	for _, so := range []order.StoredOrder{pick, dlv} {
		if _, err := r.MarkReady(ctx, so.Order.ID); err != nil {
			t.Fatal(err)
		}
	}
	// A POS takeout that is ready but still open at the POS.
	posSo := upsertPOS(t, r, "920", kitchenCheck("pickup", "h1")).Order
	if _, err := r.MarkReady(ctx, posSo.Order.ID); err != nil {
		t.Fatal(err)
	}
	_, active, err := r.CountOpenBatchOrders(ctx)
	expect(t, err == nil && active == 3, "active before = %d %v", active, err)

	// Not due yet.
	now := time.Now()
	list, err := r.FinishDueOnlinePickups(ctx, now.Add(-30*time.Minute), now)
	expect(t, err == nil && len(list) == 0, "finished too early: %d %v", len(list), err)
	// Due: only the online pickup (never the delivery, never a POS check).
	list, err = r.FinishDueOnlinePickups(ctx, now.Add(time.Second), now)
	expect(t, err == nil && len(list) == 1 && list[0].Order.ID == pick.Order.ID && list[0].Status.FinishedBy == order.FinishedByAuto,
		"finished = %+v %v", list, err)
	_, active, _ = r.CountOpenBatchOrders(ctx)
	expect(t, active == 2, "active after the pickup = %d", active)

	// The POS closes the takeout; the driver brings the delivery back.
	e := kitchenCheck("pickup", "h2")
	e.Closed, e.Paid = true, true
	upsertPOS(t, r, "920", e)
	d, _ := r.CreateDriver(ctx, "Abdul", "")
	clockIn(t, r, d.ID)
	if _, err := r.AssignDriver(ctx, d.ID, []string{dlv.Order.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.DriverReturned(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	total, active, _ := r.CountOpenBatchOrders(ctx)
	expect(t, total == 3 && active == 0, "a lull must read idle: total %d active %d", total, active)
}

func TestFeedReadyCarriesMoneyAndCalloutTest(t *testing.T) {
	ctx := context.Background()
	r := newFloorRepo(t)
	e := kitchenCheck("pickup", "h1")
	e.Paid, e.CashDueCents = true, 0
	so := upsertPOS(t, r, "930", e).Order
	if _, err := r.MarkReady(ctx, so.Order.ID); err != nil {
		t.Fatal(err)
	}
	if err := r.InsertCalloutTest(ctx, order.CalloutTestData{ID: uuid.NewString(), Kind: order.CalloutTest, Number: 102, Voice: "kokoro:af_heart"}); err != nil {
		t.Fatal(err)
	}
	evs, _, err := r.ListFeed(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var ready order.OrderReadyData
	var test order.CalloutTestData
	for _, ev := range evs {
		switch ev.Type {
		case order.FeedOrderReady:
			_ = json.Unmarshal(ev.Data, &ready)
		case order.FeedCalloutTest:
			_ = json.Unmarshal(ev.Data, &test)
		}
	}
	expect(t, ready.ExtRef != nil && *ready.ExtRef == "930" && ready.Paid && ready.CashDueCents == 0 && ready.Number != nil && *ready.Number == 57 &&
		ready.FulfillmentType == "pickup", "order.ready = %+v", ready)
	expect(t, test.Number == 102 && test.Kind == "test" && test.Voice == "kokoro:af_heart", "callout.test = %+v", test)
}

func TestCalloutAndFinishingSettingsRoundTrip(t *testing.T) {
	ctx := context.Background()
	r := newFloorRepo(t)
	set, err := r.IntegrationSettings(ctx)
	expect(t, err == nil && set.Finishing.OnlinePickupMinutes == 30 && reflect.DeepEqual(set.Callout, order.DefaultCallout()), "defaults = %+v %v", set, err)
	set.Finishing.OnlinePickupMinutes = 45
	set.Callout.Voice, set.Callout.MusicDip, set.Callout.Enabled, set.Callout.Repeat = "kokoro:bf_emma", 50, false, 3
	if err := r.SetIntegrationSettings(ctx, set); err != nil {
		t.Fatal(err)
	}
	got, _ := r.IntegrationSettings(ctx)
	expect(t, reflect.DeepEqual(got, set), "round trip = %+v", got)
	evs, _, _ := r.ListFeed(ctx, 0, 10)
	last := evs[len(evs)-1]
	expect(t, last.Type == order.FeedSettingsChanged && strings.Contains(string(last.Data), `"callout":{"enabled":false,"voice":"kokoro:bf_emma"`) &&
		strings.Contains(string(last.Data), `"repeat":3,"voice_lines":{},"guest_chance":0,"guest_voices":""}`) &&
		strings.Contains(string(last.Data), `"finishing":{"online_pickup_minutes":45}`), "settings.changed = %s", last.Data)
}
