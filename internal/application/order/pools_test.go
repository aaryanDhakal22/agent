package order

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"quiccpos/agent/internal/application/dispatch"
	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
	"quiccpos/agent/internal/infra/database"
	"quiccpos/agent/internal/infra/database/repositories"
	"quiccpos/agent/internal/infra/ssebroker"
	"quiccpos/agent/internal/migrate"
	"quiccpos/agent/internal/observability"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
)

// Pools / AUTO / geo through the service with the real SQLite repo.

func newPoolService(t *testing.T) (*Service, *repositories.OrderRepository, chan ssebroker.Event) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agent.db")
	if err := migrate.Run(ctx, path); err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	repo := repositories.NewOrderRepository(db, zerolog.Nop())
	meters, _ := observability.NewMeters()
	broker := ssebroker.New()
	events, _ := broker.Subscribe()
	print := func(context.Context, order.Snapshot, printer.Mode) (printer.Outcome, error) {
		return printer.Outcome{Printed: []printer.TicketResult{{Kind: printer.TicketFull}}}, nil
	}
	svc := NewService(repo, repositories.NewSettingsRepository(db, zerolog.Nop()), print, nil, broker, zerolog.Nop(), meters)
	return svc, repo, events
}

// readyDelivery stores, accepts and readies a delivery placed `ago` ago.
func readyDelivery(t *testing.T, svc *Service, ago time.Duration) string {
	t.Helper()
	ctx := context.Background()
	s := order.Snapshot{ID: uuid.NewString(), TenantID: uuid.NewString(), LocationID: uuid.NewString(), Source: "online",
		SourceEventID: "1", PlacedAt: time.Now().Add(-ago).UTC(), Fulfillment: order.Fulfillment{Type: "delivery",
			Address: &order.Address{Line1: "1 Main St"}}}
	if err := svc.OnArrival(ctx, s); err != nil {
		t.Fatal(err)
	}
	p := 10
	if _, err := svc.Accept(ctx, uuid.MustParse(s.ID), &p); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Ready(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	return s.ID
}

func drain(ch chan ssebroker.Event) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

func eventOf(ch chan ssebroker.Event, typ string) (json.RawMessage, bool) {
	deadline := time.After(300 * time.Millisecond)
	for {
		select {
		case e := <-ch:
			if e.Type == typ {
				return e.Data, true
			}
		case <-deadline:
			return nil, false
		}
	}
}

func ptr[T any](v T) *T { return &v }

func locate(t *testing.T, svc *Service, id string, lat, lng float64, storeS int64, times ...order.GeoTime) {
	t.Helper()
	if _, err := svc.OnGeo(context.Background(), order.GeoUpdate{OrderID: id, State: order.GeoLocated, Lat: &lat, Lng: &lng,
		Quality: "exact", DurationS: storeS, StoreToS: &storeS, ToStoreS: &storeS, Times: times}); err != nil {
		t.Fatal(err)
	}
}

func TestOnGeoPublishesUpdated(t *testing.T) {
	svc, _, events := newPoolService(t)
	id := readyDelivery(t, svc, 5*time.Minute)
	drain(events)
	locate(t, svc, id, 40.66, -74.11, 300)
	data, ok := eventOf(events, "order.updated")
	if !ok {
		t.Fatal("no order.updated for order.geo")
	}
	var so order.StoredOrder
	json.Unmarshal(data, &so)
	if so.Geo == nil || so.Geo.State != order.GeoLocated || *so.Geo.Lat != 40.66 {
		t.Fatalf("SSE geo: %s", data)
	}
	if strings.Contains(string(data), "store_to_s") {
		t.Fatal("store_to_s must not be on the wire")
	}
	if _, err := svc.OnGeo(context.Background(), order.GeoUpdate{OrderID: id, State: "bogus"}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("bad state: %v", err)
	}
}

func TestGeoCommandReplies(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newPoolService(t)
	id := readyDelivery(t, svc, time.Minute)
	reply := func(payload string) string {
		t.Helper()
		r, err := svc.HandleGeoCommand(ctx, json.RawMessage(payload))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(r)
		return string(b)
	}
	if got := reply(`{"order_id":"` + id + `","state":"pending"}`); got != `{"ok":true}` {
		t.Fatalf("pending: %s", got)
	}
	if got := reply(`{"order_id":"` + uuid.NewString() + `","state":"located","lat":1,"lng":2}`); got != `{"code":"not_found","ok":false}` {
		t.Fatalf("unknown order: %s", got)
	}
	if got := reply(`{"state":"located"}`); got != `{"code":"bad_request","ok":false}` {
		t.Fatalf("no id: %s", got)
	}
}

func TestPoolAndSendOutPublish(t *testing.T) {
	ctx := context.Background()
	svc, _, events := newPoolService(t)
	d, _ := svc.CreateDriver(ctx, "Ann")
	svc.ClockIn(ctx, d.ID)
	id := readyDelivery(t, svc, 5*time.Minute)
	drain(events)

	if _, err := svc.Pool(ctx, []string{id}, d.ID); err != nil {
		t.Fatal(err)
	}
	data, ok := eventOf(events, "order.updated")
	if !ok || !strings.Contains(string(data), `"pool_driver_id":"`+d.ID+`"`) {
		t.Fatalf("pool SSE: %s", data)
	}
	sent, err := svc.SendOut(ctx, d.ID, SendOutRequest{})
	if err != nil || len(sent) != 1 || sent[0].Status.State != order.StateOnDelivery {
		t.Fatalf("send-out: %v %+v", err, sent)
	}
	if _, ok := eventOf(events, "drivers.dispatched"); !ok {
		t.Fatal("no drivers.dispatched (name flash)")
	}
}

func TestClockOutUnpoolsAndPublishes(t *testing.T) {
	ctx := context.Background()
	svc, _, events := newPoolService(t)
	d, _ := svc.CreateDriver(ctx, "Ann")
	svc.ClockIn(ctx, d.ID)
	id := readyDelivery(t, svc, time.Minute)
	svc.Pool(ctx, []string{id}, d.ID)
	drain(events)
	if _, err := svc.ClockOut(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	data, ok := eventOf(events, "order.updated")
	if !ok || strings.Contains(string(data), "pool_driver_id") {
		t.Fatalf("clock-out must publish the unpooled order: %s", data)
	}
}

func TestAutoPlanSimpleCaseAndConfirm(t *testing.T) {
	ctx := context.Background()
	svc, repo, _ := newPoolService(t)
	svc.SetGeo(GeoDeps{Floor: func() (*FloorStore, *FloorMap) {
		return &FloorStore{Lat: 40.66, Lng: -74.11, DeliveryRadiusM: 8047}, nil
	}})

	if _, err := svc.PlanDeliveries(ctx); !errors.Is(err, ErrNoDrivers) {
		t.Fatalf("no drivers: %v", err)
	}
	ann, _ := svc.CreateDriver(ctx, "Ann")
	bob, _ := svc.CreateDriver(ctx, "Bob")
	svc.ClockIn(ctx, ann.ID)
	svc.ClockIn(ctx, bob.ID)

	// Two orders north (near each other), one south, one never located.
	n1 := readyDelivery(t, svc, 20*time.Minute)
	n2 := readyDelivery(t, svc, 15*time.Minute)
	s1 := readyDelivery(t, svc, 10*time.Minute)
	lost := readyDelivery(t, svc, 8*time.Minute)
	locate(t, svc, n1, 40.690, -74.11, 420)
	locate(t, svc, n2, 40.695, -74.11, 450, order.GeoTime{Other: n1, ToS: ptr[int64](60), FromS: ptr[int64](60)})
	locate(t, svc, s1, 40.630, -74.11, 400,
		order.GeoTime{Other: n1, ToS: ptr[int64](800), FromS: ptr[int64](800)},
		order.GeoTime{Other: n2, ToS: ptr[int64](850), FromS: ptr[int64](850)})

	plan, err := svc.PlanDeliveries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if plan.PlanID == "" || len(plan.Runs) == 0 {
		t.Fatalf("plan: %+v", plan)
	}
	placed := map[string]string{}
	for _, r := range plan.Runs {
		for _, id := range r.SuggestedIDs {
			placed[id] = r.DriverID
		}
	}
	if placed[n1] == "" || placed[n1] != placed[n2] {
		t.Fatalf("the two northern orders should ride together: %+v", plan.Runs)
	}
	if plan.Runs[0].DriverID != ann.ID {
		t.Fatalf("the next driver (Ann) gets the oldest order's run: %+v", plan.Runs)
	}
	var lostReason string
	for _, u := range plan.Unassigned {
		if u.OrderID == lost {
			lostReason = u.Reason
		}
	}
	if lostReason != dispatch.ReasonNoLocation {
		t.Fatalf("unlocated order: %q (%+v)", lostReason, plan.Unassigned)
	}
	// Nothing changed on the floor.
	if so, _ := repo.GetByID(ctx, uuid.MustParse(n1)); so.Status.PoolDriverID != "" {
		t.Fatal("planning must not pool anything")
	}

	// n2 gets sent with Bob meanwhile -> skipped at confirm.
	svc.AssignDriver(ctx, bob.ID, []string{n2})
	res, err := svc.ConfirmPlan(ctx, plan.PlanID, nil)
	if err != nil {
		t.Fatal(err)
	}
	applied := map[string]bool{}
	for _, id := range res.Applied {
		applied[id] = true
	}
	if !applied[n1] || applied[n2] {
		t.Fatalf("confirm: %+v", res)
	}
	if so, _ := repo.GetByID(ctx, uuid.MustParse(n1)); so.Status.PoolDriverID != placed[n1] {
		t.Fatalf("n1 pooled to %q, want %q", so.Status.PoolDriverID, placed[n1])
	}
	if _, err := svc.ConfirmPlan(ctx, uuid.NewString(), nil); !errors.Is(err, order.ErrNotFound) {
		t.Fatalf("unknown plan: %v", err)
	}

	// Send Out with the plan id logs the suggestion in delivery.run.
	if _, err := svc.SendOut(ctx, placed[n1], SendOutRequest{PlanID: plan.PlanID}); err != nil {
		t.Fatal(err)
	}
	evs, _ := repo.ListUnsyncedEvents(ctx, 500)
	found := false
	for _, e := range evs {
		if e.Type != order.FloorEventDeliveryRun {
			continue
		}
		var run order.DeliveryRunEvent
		json.Unmarshal(e.Payload, &run)
		if run.PlanID == plan.PlanID {
			found = true
			if len(run.SuggestedIDs) == 0 || len(run.Stops) == 0 || run.Stops[0].ETA == nil {
				t.Fatalf("delivery.run: %s", e.Payload)
			}
		}
	}
	if !found {
		t.Fatal("no delivery.run with the plan id")
	}
}

func TestConfirmOnlyGivenIDs(t *testing.T) {
	ctx := context.Background()
	svc, repo, _ := newPoolService(t)
	svc.SetGeo(GeoDeps{Floor: func() (*FloorStore, *FloorMap) { return &FloorStore{Lat: 40.66, Lng: -74.11}, nil }})
	ann, _ := svc.CreateDriver(ctx, "Ann")
	svc.ClockIn(ctx, ann.ID)
	a := readyDelivery(t, svc, 20*time.Minute)
	b := readyDelivery(t, svc, 15*time.Minute)
	locate(t, svc, a, 40.690, -74.11, 420)
	locate(t, svc, b, 40.695, -74.11, 450, order.GeoTime{Other: a, ToS: ptr[int64](60), FromS: ptr[int64](60)})
	plan, err := svc.PlanDeliveries(ctx)
	if err != nil || len(plan.Runs) != 1 || len(plan.Runs[0].SuggestedIDs) != 2 {
		t.Fatalf("plan: %v %+v", err, plan)
	}
	res, err := svc.ConfirmPlan(ctx, plan.PlanID, []string{b})
	if err != nil || len(res.Applied) != 1 || res.Applied[0] != b || len(res.Skipped) != 0 {
		t.Fatalf("confirm only b: %v %+v", err, res)
	}
	if so, _ := repo.GetByID(ctx, uuid.MustParse(a)); so.Status.PoolDriverID != "" {
		t.Fatal("a was not chosen and must stay unassigned")
	}
}

func TestPoolTripLocalFallbackAndRemote(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newPoolService(t)
	svc.SetGeo(GeoDeps{Floor: func() (*FloorStore, *FloorMap) { return &FloorStore{Lat: 40.66, Lng: -74.11}, nil }})
	d, _ := svc.CreateDriver(ctx, "Ann")
	svc.ClockIn(ctx, d.ID)
	far := readyDelivery(t, svc, 5*time.Minute)
	near := readyDelivery(t, svc, 4*time.Minute)
	locate(t, svc, far, 40.70, -74.11, 900)
	locate(t, svc, near, 40.67, -74.11, 200, order.GeoTime{Other: far, ToS: ptr[int64](500), FromS: ptr[int64](500)})
	svc.Pool(ctx, []string{far, near}, d.ID)

	trip, err := svc.PoolTrip(ctx, d.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !trip.Approximate || trip.Polyline != "" || len(trip.OrderIDs) != 2 || trip.OrderIDs[0] != near || trip.DurationS != 700 {
		t.Fatalf("local trip: %+v", trip)
	}
	// main answers geo.trip.
	svc.SetGeo(GeoDeps{
		Floor: func() (*FloorStore, *FloorMap) { return nil, nil },
		Send: func(_ context.Context, typ string, _ any) (int, json.RawMessage, error) {
			if typ != MsgGeoTrip {
				t.Fatalf("type %s", typ)
			}
			return 200, json.RawMessage(`{"order_ids":["` + near + `","` + far + `"],"polyline":"xyz","distance_m":5000,"duration_s":650}`), nil
		},
		Linked: func() bool { return true },
	})
	trip, err = svc.PoolTrip(ctx, d.ID, []string{far, near})
	if err != nil || trip.Approximate || trip.Polyline != "xyz" || trip.DurationS != 650 {
		t.Fatalf("remote trip: %v %+v", err, trip)
	}
	if _, err := svc.PoolTrip(ctx, "nobody", nil); !errors.Is(err, order.ErrDriverNotFound) {
		t.Fatalf("unknown driver: %v", err)
	}
}

func TestPinForwardsToMain(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newPoolService(t)
	id := readyDelivery(t, svc, time.Minute)
	if err := svc.Pin(ctx, id, 40.6, -74.1); !errors.Is(err, ErrMainOffline) {
		t.Fatalf("not linked: %v", err)
	}
	var got map[string]any
	svc.SetGeo(GeoDeps{Linked: func() bool { return true }, Send: func(_ context.Context, typ string, p any) (int, json.RawMessage, error) {
		raw, _ := json.Marshal(p)
		json.Unmarshal(raw, &got)
		return 200, json.RawMessage(`{"ok":true}`), nil
	}})
	if err := svc.Pin(ctx, id, 40.6, -74.1); err != nil {
		t.Fatal(err)
	}
	if got["order_id"] != id || got["lat"] != 40.6 {
		t.Fatalf("geo.pin payload: %v", got)
	}
	if err := svc.Pin(ctx, id, 95, 0); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("range: %v", err)
	}
	if err := svc.Pin(ctx, uuid.NewString(), 40, -74); !errors.Is(err, order.ErrNotFound) {
		t.Fatalf("unknown order: %v", err)
	}
}

func TestDeliveryPrefsValidation(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newPoolService(t)
	for _, p := range []order.DeliveryPrefs{{SoonReadyMinutes: -1, DeliveryTargetMinutes: 45}, {SoonReadyMinutes: 31, DeliveryTargetMinutes: 45},
		{SoonReadyMinutes: 5, DeliveryTargetMinutes: 14}, {SoonReadyMinutes: 5, DeliveryTargetMinutes: 121}} {
		if err := svc.SetDeliveryPrefs(ctx, p); !errors.Is(err, ErrBadRequest) {
			t.Fatalf("%+v: %v", p, err)
		}
	}
	if err := svc.SetDeliveryPrefs(ctx, order.DeliveryPrefs{SoonReadyMinutes: 0, DeliveryTargetMinutes: 120}); err != nil {
		t.Fatal(err)
	}
	f, err := svc.Floor(ctx)
	if err != nil || f.SoonReadyMinutes != 0 || f.DeliveryTargetMinutes != 120 || f.Store != nil || f.Map != nil {
		t.Fatalf("floor: %v %+v", err, f)
	}
}
