package order

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"quiccpos/agent/internal/application/devlog"
	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
	"quiccpos/agent/internal/observability"
	agentrpc "quiccpos/agent/internal/rpc"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
)

// 2.2.0: the nearby rule, the delivery radius over location.radius, the
// auto_accepted mark and the developer-mode events.

// nearbyFloor: store at (40.0, -74.0); Juma (available, next) has A in his
// pool; B is ~140 m from A (1 min detour), C is far, D has no location.
type nearbyFloor struct {
	svc        *Service
	juma       string
	a, b, c, d string
}

func newNearbyFloor(t *testing.T) *nearbyFloor {
	t.Helper()
	ctx := context.Background()
	svc, _, _ := newPoolService(t)
	svc.SetGeo(GeoDeps{Floor: func() (*FloorStore, *FloorMap) { return &FloorStore{Lat: 40.0, Lng: -74.0}, nil }})
	f := &nearbyFloor{svc: svc}
	drv, err := svc.CreateDriver(ctx, "Juma Ali", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ClockIn(ctx, drv.ID); err != nil {
		t.Fatal(err)
	}
	f.juma = drv.ID
	f.a = readyDelivery(t, svc, 20*time.Minute) // #1100
	f.b = readyDelivery(t, svc, 15*time.Minute) // #1101
	f.c = readyDelivery(t, svc, 10*time.Minute) // #1102
	f.d = readyDelivery(t, svc, 5*time.Minute)  // #1103, never located
	locate(t, svc, f.a, 40.010, -74.000, 300)
	locate(t, svc, f.b, 40.011, -74.001, 310, order.GeoTime{Other: f.a, ToS: ptr[int64](60), FromS: ptr[int64](60)})
	locate(t, svc, f.c, 40.100, -74.200, 1500)
	if _, err := svc.Pool(ctx, []string{f.a}, f.juma); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *nearbyFloor) rule(t *testing.T, p DeliveryPatch) {
	t.Helper()
	if _, err := f.svc.UpdateDeliverySettings(context.Background(), p); err != nil {
		t.Fatal(err)
	}
}

func TestNextCheckDetour(t *testing.T) {
	ctx := context.Background()
	f := newNearbyFloor(t)
	got, err := f.svc.NextCheck(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.DriverID == nil || *got.DriverID != f.juma || got.Rule != order.NearbyDetour ||
		strings.Join(got.NearbyOrderIDs, ",") != f.b {
		t.Fatalf("got %+v (want only %s)", got, f.b)
	}
	if got.Reason != "#1101 adds 1 min to Juma's run." {
		t.Fatalf("reason %q", got.Reason)
	}
	// A tighter limit than the detour: nothing.
	f.rule(t, DeliveryPatch{NearbyDetourMinutes: ptr[int64](1)})
	if got, _ := f.svc.NextCheck(ctx, ""); len(got.NearbyOrderIDs) != 1 {
		t.Fatalf("1 min limit: %+v", got)
	}
	// C costs far more than 30 min: never nearby; D has no location.
	f.rule(t, DeliveryPatch{NearbyDetourMinutes: ptr[int64](30)})
	if got, _ := f.svc.NextCheck(ctx, ""); strings.Join(got.NearbyOrderIDs, ",") != f.b {
		t.Fatalf("30 min limit: %+v", got)
	}
}

func TestNextCheckDistance(t *testing.T) {
	ctx := context.Background()
	f := newNearbyFloor(t)
	f.rule(t, DeliveryPatch{NearbyRule: ptr(order.NearbyDistance), NearbyDistanceMi: ptr(0.2)})
	got, err := f.svc.NextCheck(ctx, f.juma)
	if err != nil || strings.Join(got.NearbyOrderIDs, ",") != f.b || got.Rule != order.NearbyDistance {
		t.Fatalf("%v %+v", err, got)
	}
	if got.Reason != "#1101 is 0.1 mi from #1100." {
		t.Fatalf("reason %q", got.Reason)
	}
	f.rule(t, DeliveryPatch{NearbyDistanceMi: ptr(0.1)}) // B is ~0.09 mi away
	if got, _ := f.svc.NextCheck(ctx, ""); len(got.NearbyOrderIDs) != 1 {
		t.Fatalf("0.1 mi: %+v", got)
	}
	f.rule(t, DeliveryPatch{NearbyDistanceMi: ptr(10.0)}) // C ~11.5 mi: still out
	if got, _ := f.svc.NextCheck(ctx, ""); strings.Join(got.NearbyOrderIDs, ",") != f.b {
		t.Fatalf("10 mi: %+v", got)
	}
}

func TestNextCheckBundle(t *testing.T) {
	ctx := context.Background()
	f := newNearbyFloor(t)
	f.rule(t, DeliveryPatch{NearbyRule: ptr(order.NearbyBundle)})
	got, err := f.svc.NextCheck(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.NearbyOrderIDs) == 0 || got.NearbyOrderIDs[0] != f.b {
		t.Fatalf("bundle: %+v", got)
	}
	for _, id := range got.NearbyOrderIDs {
		if id == f.d || id == f.a {
			t.Fatalf("bundle listed %s: %+v", id, got)
		}
	}
	if !strings.HasPrefix(got.Reason, "AUTO would add #1101 to Juma's run") {
		t.Fatalf("reason %q", got.Reason)
	}
}

func TestNextCheckEmptyCases(t *testing.T) {
	ctx := context.Background()
	f := newNearbyFloor(t)

	f.rule(t, DeliveryPatch{NearbyRule: ptr(order.NearbyOff)})
	if got, _ := f.svc.NextCheck(ctx, ""); len(got.NearbyOrderIDs) != 0 || got.Rule != "off" || got.DriverID == nil || got.Reason != "" {
		t.Fatalf("off: %+v", got)
	}
	f.rule(t, DeliveryPatch{NearbyRule: ptr(order.NearbyDetour)})

	// Empty pool: a second driver, next only after Juma — asked by id.
	d2, _ := f.svc.CreateDriver(ctx, "Sam", "")
	f.svc.ClockIn(ctx, d2.ID)
	if got, _ := f.svc.NextCheck(ctx, d2.ID); len(got.NearbyOrderIDs) != 0 || got.DriverID == nil || *got.DriverID != d2.ID {
		t.Fatalf("empty pool: %+v", got)
	}
	// An unknown driver: no driver.
	if got, _ := f.svc.NextCheck(ctx, uuid.NewString()); got.DriverID != nil || len(got.NearbyOrderIDs) != 0 {
		t.Fatalf("unknown driver: %+v", got)
	}

	// A pool whose only stop has no location: nothing to measure against.
	if _, err := f.svc.Pool(ctx, []string{f.a}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Pool(ctx, []string{f.d}, f.juma); err != nil {
		t.Fatal(err)
	}
	for _, rule := range []string{order.NearbyDetour, order.NearbyDistance} {
		f.rule(t, DeliveryPatch{NearbyRule: ptr(rule)})
		if got, _ := f.svc.NextCheck(ctx, f.juma); len(got.NearbyOrderIDs) != 0 {
			t.Fatalf("%s with an unlocated pool: %+v", rule, got)
		}
	}

	// Nobody available: driver_id null.
	if _, err := f.svc.Pool(ctx, []string{f.d}, ""); err != nil {
		t.Fatal(err)
	}
	f.svc.ClockOut(ctx, f.juma)
	f.svc.ClockOut(ctx, d2.ID)
	got, err := f.svc.NextCheck(ctx, "")
	if err != nil || got.DriverID != nil || len(got.NearbyOrderIDs) != 0 {
		t.Fatalf("no drivers: %v %+v", err, got)
	}
	b, _ := json.Marshal(got)
	if !strings.Contains(string(b), `"driver_id":null`) || !strings.Contains(string(b), `"nearby_order_ids":[]`) {
		t.Fatalf("wire: %s", b)
	}
}

// exchanger plays main's /store/v1/agent/rpc for the registry.
type exchanger struct {
	mu     sync.Mutex
	got    []agentrpc.Envelope
	status int
	reply  string
	err    error
}

func (e *exchanger) PostRPC(context.Context, []byte) error { return nil }
func (e *exchanger) ExchangeRPC(_ context.Context, body []byte) (int, []byte, error) {
	var env agentrpc.Envelope
	_ = json.Unmarshal(body, &env)
	e.mu.Lock()
	e.got = append(e.got, env)
	e.mu.Unlock()
	if e.err != nil {
		return 0, nil, e.err
	}
	if e.status != 200 {
		return e.status, []byte(`{}`), nil
	}
	out, _ := json.Marshal(agentrpc.Envelope{Type: env.Type + ".reply", ID: env.ID, Payload: json.RawMessage(e.reply)})
	return 200, out, nil
}

func TestDeliveryRadiusRoundTrip(t *testing.T) {
	ctx := context.Background()
	svc, _, events := newPoolService(t)
	meters, _ := observability.NewMeters()
	ex := &exchanger{status: 200, reply: `{"radius_m":6000}`}
	reg := agentrpc.NewRegistry(ex, zerolog.Nop(), meters)
	radius := int64(8047)
	svc.SetGeo(GeoDeps{Send: reg.Send, Linked: func() bool { return true },
		Radius: func() (int64, bool) { return radius, true }, SetRadius: func(m int64) { radius = m }})

	drain(events)
	out, err := svc.UpdateDeliverySettings(ctx, DeliveryPatch{DeliveryRadiusM: ptr[int64](6000), SoonReadyMinutes: ptr[int64](7)})
	if err != nil || out.DeliveryRadiusM == nil || *out.DeliveryRadiusM != 6000 || out.SoonReadyMinutes != 7 || radius != 6000 {
		t.Fatalf("%v %+v radius %d", err, out, radius)
	}
	if len(ex.got) != 1 || ex.got[0].Type != MsgLocationRadius || string(ex.got[0].Payload) != `{"radius_m":6000}` || ex.got[0].ID == "" {
		t.Fatalf("sent %+v", ex.got)
	}
	if _, ok := eventOf(events, "floor.updated"); !ok {
		t.Fatal("no floor.updated")
	}

	// main offline / not answering: nothing changes, 503.
	ex.err = errors.New("dial tcp: refused")
	if _, err := svc.UpdateDeliverySettings(ctx, DeliveryPatch{DeliveryRadiusM: ptr[int64](7000), SoonReadyMinutes: ptr[int64](9)}); !errors.Is(err, ErrMainOffline) {
		t.Fatalf("offline: %v", err)
	}
	if p := svc.DeliveryPrefs(ctx); p.SoonReadyMinutes != 7 || radius != 6000 {
		t.Fatalf("an offline radius changed things: %+v %d", p, radius)
	}
	// An older main (202 = unknown type): 501.
	ex.err, ex.status = nil, 202
	if _, err := svc.UpdateDeliverySettings(ctx, DeliveryPatch{DeliveryRadiusM: ptr[int64](7000)}); !errors.Is(err, ErrMainUnsupported) {
		t.Fatalf("unsupported: %v", err)
	}
	// Not linked: offline without asking.
	n := len(ex.got)
	svc.SetGeo(GeoDeps{Send: reg.Send, Linked: func() bool { return false }})
	if _, err := svc.UpdateDeliverySettings(ctx, DeliveryPatch{DeliveryRadiusM: ptr[int64](7000)}); !errors.Is(err, ErrMainOffline) || len(ex.got) != n {
		t.Fatalf("unlinked: %v (%d sends)", err, len(ex.got)-n)
	}
}

// auto_accepted: set by auto-accept (with an order.updated before the print,
// never order.arrived), not by staff, cleared when nothing printed.
func TestAutoAcceptedMark(t *testing.T) {
	ctx := context.Background()
	svc, repo, events := newPoolService(t)
	if err := svc.SetAutoAccept(ctx, true); err != nil {
		t.Fatal(err)
	}
	snap := func() order.Snapshot {
		return order.Snapshot{ID: uuid.NewString(), TenantID: uuid.NewString(), LocationID: uuid.NewString(), Source: "online",
			SourceEventID: "aa", Fulfillment: order.Fulfillment{Type: "pickup"}}
	}
	drain(events)
	s := snap()
	if err := svc.OnArrival(ctx, s); err != nil {
		t.Fatal(err)
	}
	got, _ := repo.GetByID(ctx, uuid.MustParse(s.ID))
	if got.Status.State != order.StateAccepted || !got.Status.AutoAccepted {
		t.Fatalf("auto: %+v", got.Status)
	}
	sawArrived, updates := false, 0
	for {
		select {
		case e := <-events:
			switch e.Type {
			case "order.arrived":
				sawArrived = true
			case "order.updated":
				if !strings.Contains(string(e.Data), `"auto_accepted":true`) {
					t.Fatalf("order.updated without auto_accepted: %s", e.Data)
				}
				updates++
			}
			continue
		default:
		}
		break
	}
	if sawArrived || updates < 1 {
		t.Fatalf("arrived %v, updates %d", sawArrived, updates)
	}

	// Staff accept: not marked (and omitted on the wire).
	svc.SetAutoAccept(ctx, false)
	s2 := snap()
	svc.OnArrival(ctx, s2)
	so, err := svc.Accept(ctx, uuid.MustParse(s2.ID), ptr(15))
	if err != nil || so.Status.AutoAccepted {
		t.Fatalf("manual: %v %+v", err, so)
	}
	if b, _ := json.Marshal(so); strings.Contains(string(b), "auto_accepted") {
		t.Fatalf("auto_accepted on a staff accept: %s", b)
	}

	// Nothing printed: reverted to arrived, mark cleared, and it rings.
	svc.print = func(context.Context, order.Snapshot, printer.Mode) (printer.Outcome, error) {
		return printer.Outcome{}, errors.New("all printers down")
	}
	svc.SetAutoAccept(ctx, true)
	drain(events)
	s3 := snap()
	svc.OnArrival(ctx, s3)
	back, _ := repo.GetByID(ctx, uuid.MustParse(s3.ID))
	if back.Status.State != order.StateArrived || back.Status.AutoAccepted {
		t.Fatalf("reverted: %+v", back.Status)
	}
	if _, ok := eventOf(events, "order.arrived"); !ok {
		t.Fatal("a failed auto-accept didn't ring")
	}
}

// The developer-mode events of an order's life carry numbers, never the
// customer.
func TestDevEventsNoPII(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newPoolService(t)
	ring := devlog.New(0)
	svc.SetDevLog(ring)
	svc.logger = zerolog.New(&strings.Builder{}).Hook(ring.Hook())
	svc.print = func(context.Context, order.Snapshot, printer.Mode) (printer.Outcome, error) {
		return printer.Outcome{
			Printed: []printer.TicketResult{{Kind: printer.TicketFull, Station: "Online", PrintedOn: "Online"},
				{Kind: printer.TicketFull, Station: "Pizza", PrintedOn: "Online", ReroutedFrom: "Pizza"}},
			Failed: []printer.TicketResult{{Kind: printer.TicketFull, Station: "Wings"}}}, nil
	}
	s := order.Snapshot{ID: uuid.NewString(), TenantID: uuid.NewString(), LocationID: uuid.NewString(), Source: "online",
		SourceEventID: "pii", PlacedAt: time.Now().UTC(),
		Customer:    order.Customer{DisplayName: "Erika Mustermann", Phone: "+14105550123"},
		Fulfillment: order.Fulfillment{Type: "delivery", Address: &order.Address{Line1: "931 Taylor Ave", City: "Towson"}}}
	if err := svc.OnArrival(ctx, s); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Accept(ctx, uuid.MustParse(s.ID), ptr(20)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Ready(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	drv, _ := svc.CreateDriver(ctx, "Juma", "")
	svc.ClockIn(ctx, drv.ID)
	if _, err := svc.AssignDriver(ctx, drv.ID, []string{s.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.DriverReturned(ctx, drv.ID); err != nil {
		t.Fatal(err)
	}
	svc.EscalateOverdue(ctx, time.Now().Add(time.Hour), time.Minute)
	events, _ := ring.Since(0, 0)
	var all []string
	for _, e := range events {
		all = append(all, e.Kind+"|"+e.Level+"|"+e.Text)
	}
	text := strings.Join(all, "\n")
	for _, pii := range []string{"Erika", "Mustermann", "5550123", "Taylor", "Towson"} {
		if strings.Contains(text, pii) {
			t.Fatalf("%q in the events:\n%s", pii, text)
		}
	}
	for _, want := range []string{"arrived (delivery)", "#1100 accepted (prep 20 min)", "#1100 printed on Online",
		"#1100 full ticket for Wings NOT printed", "#1100 Pizza ticket rerouted to Online", "#1100 ready",
		"Out on a run: #1100", "Delivered: #1100"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
	}
}
