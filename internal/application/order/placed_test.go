package order

import (
	"context"
	"errors"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"

	"github.com/google/uuid"
)

// §2.10aa: every order from main is placed on arrival like a called-in
// order - no ring, prep = the store's default for its type, the placed print
// (stations + a delivery's copies), never reverted when nothing prints.

func onlineSnap(typ string) order.Snapshot {
	s := order.Snapshot{ID: uuid.NewString(), TenantID: uuid.NewString(), LocationID: uuid.NewString(), Source: "online",
		SourceEventID: uuid.NewString()[:6], PlacedAt: time.Now().UTC(), Fulfillment: order.Fulfillment{Type: typ},
		Money: order.Money{Total: 2000}, Items: []order.LineItem{{ID: uuid.NewString(), Name: "Wings", Quantity: 1, UnitPrice: 2000,
			LineTotal: 2000, Routing: order.RoutingMatched}}}
	if typ == "delivery" {
		s.Fulfillment.Address = &order.Address{Line1: "1 Test St", City: "Towson"}
	}
	return s
}

func TestOnlineOrdersArePlacedOnArrival(t *testing.T) {
	ctx := context.Background()
	svc, _, events, prints, _ := newIntegService(t)
	svc.legacyArrival = false
	// The terminal's default (main's default_pickup_minutes / the Pi's delivery minutes).
	svc.SetPrepSource(func(f string) int64 {
		if f == "delivery" {
			return 40
		}
		return 15
	})
	svc.WaitBackground()
	drain(events)
	for _, typ := range []string{"pickup", "delivery"} {
		s := onlineSnap(typ)
		if err := svc.OnArrival(ctx, s); err != nil {
			t.Fatal(err)
		}
		so := storedOf(t, svc, s.ID)
		want := int64(15)
		if typ == "delivery" {
			want = 40
		}
		if so.Status.State != order.StateAccepted || so.Status.PrepMinutes == nil || *so.Status.PrepMinutes != want ||
			!so.Status.PlacedDirect || !so.Status.AutoAccepted || so.Status.KitchenNumber == nil || so.Status.EstimatedReadyAt == nil {
			t.Fatalf("%s placed: %+v", typ, so.Status)
		}
		p := prints.all()
		if last := p[len(p)-1]; last.OrderID != s.ID || last.Mode != printer.ModePlaced {
			t.Fatalf("%s print: %+v", typ, last)
		}
	}
	if _, ok := eventOf(events, "order.arrived"); ok {
		t.Fatal("a placed order rang")
	}
	// Nothing to escalate: no order waits.
	if n, _ := svc.EscalateOverdue(ctx, time.Now().Add(time.Hour), 2*time.Minute); n != 0 {
		t.Fatalf("escalated %d", n)
	}
}

func TestPlacedOrderStaysPlacedWhenNothingPrints(t *testing.T) {
	ctx := context.Background()
	svc, repo, events, _, _ := newIntegService(t)
	svc.legacyArrival = false
	svc.print = func(context.Context, order.Snapshot, printer.Mode) (printer.Outcome, error) {
		return printer.Outcome{}, errors.New("all printers down")
	}
	drain(events)
	s := onlineSnap("delivery")
	if err := svc.OnArrival(ctx, s); err != nil {
		t.Fatal(err)
	}
	so := storedOf(t, svc, s.ID)
	if so.Status.State != order.StateAccepted {
		t.Fatalf("reverted: %s", so.Status.State)
	}
	if _, ok := eventOf(events, "order.arrived"); ok {
		t.Fatal("rang")
	}
	// main hears it accepted all the same.
	evs, _ := repo.ListUnsyncedEvents(ctx, 100)
	var accepted bool
	for _, e := range evs {
		if e.OrderID == s.ID && e.Type == order.FloorEventOrderStatus {
			accepted = true
		}
	}
	if !accepted {
		t.Fatal("no accepted event for main")
	}
}

// An order an older Pi left arrived is placed at start (no ring).
func TestPlaceArrivedAtStart(t *testing.T) {
	ctx := context.Background()
	svc, _, events, prints, _ := newIntegService(t) // legacy: arrivals wait
	s := onlineSnap("pickup")
	if err := svc.OnArrival(ctx, s); err != nil {
		t.Fatal(err)
	}
	if so := storedOf(t, svc, s.ID); so.Status.State != order.StateArrived {
		t.Fatalf("setup: %s", so.Status.State)
	}
	svc.legacyArrival = false
	drain(events)
	svc.SetPrepSource(func(string) int64 { return 25 })
	svc.WaitBackground()
	so := storedOf(t, svc, s.ID)
	if so.Status.State != order.StateAccepted || *so.Status.PrepMinutes != 25 {
		t.Fatalf("placed at start: %+v", so.Status)
	}
	if p := prints.all(); len(p) != 1 || p[0].Mode != printer.ModePlaced {
		t.Fatalf("prints %+v", p)
	}
}
