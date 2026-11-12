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

// 00019: finishing orders, the update gate's open count, callout feed events.

var (
	expect = func(t *testing.T, ok bool, format string, a ...any) {
		t.Helper()
		if !ok {
			t.Fatalf(format, a...)
		}
	}
)

func TestFinishDueOnlinePickupsAndUpdateGate(t *testing.T) {
	ctx := context.Background()
	r := newFloorRepo(t)
	pick := accept(t, r, arrive(t, r, "pickup"))
	dlv := accept(t, r, arrive(t, r, "delivery"))
	for _, so := range []order.StoredOrder{pick, dlv} {
		if _, err := r.MarkReady(ctx, so.Order.ID, true); err != nil {
			t.Fatal(err)
		}
	}
	_, active, err := r.CountOpenBatchOrders(ctx)
	expect(t, err == nil && active == 2, "active before = %d %v", active, err)

	// Not due yet.
	now := time.Now()
	list, err := r.FinishDueOnlinePickups(ctx, now.Add(-30*time.Minute), now)
	expect(t, err == nil && len(list) == 0, "finished too early: %d %v", len(list), err)
	// Due: only the online pickup (never the delivery).
	list, err = r.FinishDueOnlinePickups(ctx, now.Add(time.Second), now)
	expect(t, err == nil && len(list) == 1 && list[0].Order.ID == pick.Order.ID && list[0].Status.FinishedBy == order.FinishedByAuto,
		"finished = %+v %v", list, err)
	_, active, _ = r.CountOpenBatchOrders(ctx)
	expect(t, active == 1, "active after the pickup = %d", active)

	// The driver brings the delivery back.
	d, _ := r.CreateDriver(ctx, "Abdul")
	clockIn(t, r, d.ID)
	if _, err := r.AssignDriver(ctx, d.ID, []string{dlv.Order.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.DriverReturned(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	total, active, _ := r.CountOpenBatchOrders(ctx)
	expect(t, total == 2 && active == 0, "a lull must read idle: total %d active %d", total, active)
}

func TestFeedReadyCarriesMoneyAndCalloutTest(t *testing.T) {
	ctx := context.Background()
	r := newFloorRepo(t)
	so := accept(t, r, arrive(t, r, "pickup")) // no payments: the whole total is cash due
	if _, err := r.MarkReady(ctx, so.Order.ID, true); err != nil {
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
	expect(t, ready.OrderID == so.Order.ID && ready.Source == "test" && !ready.Paid && ready.CashDueCents == so.Order.Money.Total &&
		ready.Number != nil && so.Status.KitchenNumber != nil && *ready.Number == *so.Status.KitchenNumber &&
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
