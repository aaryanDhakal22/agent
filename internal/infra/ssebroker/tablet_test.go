package ssebroker

import (
	"encoding/json"
	"testing"

	"quiccpos/agent/internal/domain/order"
)

// Quibase gets only the orders it shows (2.5.4): a counter terminal's soda is
// sent to the terminals and the Pi page, never to a tablet; an edit that
// hides an order it had shown sends the tablets floor.updated (they refetch);
// a run of hidden orders flashes nothing on a tablet.
func TestTabletFilter(t *testing.T) {
	b := New()
	b.SetTabletFilter(func(so order.StoredOrder) bool { return so.ShownOnTablet(500) })
	tab, offTab := b.SubscribeTablet()
	defer offTab()
	term, offTerm := b.Subscribe()
	defer offTerm()

	soda := order.LineItem{Name: "Can Coke", Routing: order.RoutingNone}
	pizza := order.LineItem{Name: "Pizza", Routing: order.RoutingMatched, PrinterID: "p"}
	counter := func(id string, total int64, items ...order.LineItem) order.StoredOrder {
		return order.StoredOrder{Order: order.Snapshot{ID: id, Source: order.SourceCounter, Money: order.Money{Total: total}, Items: items}}
	}
	types := func(ch chan Event) []string {
		var out []string
		for {
			select {
			case e := <-ch:
				out = append(out, e.Type)
			default:
				return out
			}
		}
	}
	same := func(name string, got []string, want ...string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s: got %v, want %v", name, got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("%s: got %v, want %v", name, got, want)
			}
		}
	}

	b.PublishUpdated(counter("a", 135, soda))
	same("soda: tablet", types(tab))
	same("soda: terminal", types(term), "order.updated")

	b.PublishUpdated(counter("b", 1377, pizza))
	same("pizza: tablet", types(tab), "order.updated")
	same("pizza: terminal", types(term), "order.updated")

	// The pizza taken off at a terminal: only the soda is left.
	b.PublishUpdated(counter("b", 135, soda))
	same("hidden after shown: tablet", types(tab), "floor.updated")
	same("hidden after shown: terminal", types(term), "order.updated")
	b.PublishUpdated(counter("b", 135, soda))
	same("still hidden: tablet", types(tab))

	// Added back: the tablet gets it again.
	b.PublishUpdated(counter("b", 1646, pizza, soda))
	same("shown again: tablet", types(tab), "order.updated")
	types(term)

	// Online orders always show.
	b.PublishUpdated(order.StoredOrder{Order: order.Snapshot{ID: "c", Source: order.SourceOnline, Money: order.Money{Total: 100}, Items: []order.LineItem{soda}}})
	same("online soda: tablet", types(tab), "order.updated")
	types(term)

	// A run: the tablet's frame lists only what it shows.
	b.PublishDispatched(order.Driver{ID: "d1", Name: "Ali"}, []order.StoredOrder{counter("x", 900, soda), counter("y", 1377, pizza)})
	e := <-tab
	var got struct {
		Orders []order.StoredOrder `json:"orders"`
	}
	if err := json.Unmarshal(e.Data, &got); err != nil || len(got.Orders) != 1 || got.Orders[0].Order.ID != "y" {
		t.Fatalf("dispatched to the tablet: %s", e.Data)
	}
	if e := <-term; !json.Valid(e.Data) || len(types(term)) != 0 {
		t.Fatal("the terminal gets one full dispatched frame")
	}
	b.PublishDispatched(order.Driver{ID: "d2"}, []order.StoredOrder{counter("z", 900, soda)})
	same("hidden run: tablet", types(tab))
	same("hidden run: terminal", types(term), "drivers.dispatched")

	// A delivery always shows (the owner, 2026-11-13), a soda one too.
	d := counter("dl", 535, soda)
	d.Order.Fulfillment.Type = "delivery"
	b.PublishUpdated(d)
	same("soda delivery: tablet", types(tab), "order.updated")
	same("soda delivery: terminal", types(term), "order.updated")
}

func TestNoFilterEveryOrderShows(t *testing.T) {
	b := New()
	tab, off := b.SubscribeTablet()
	defer off()
	b.PublishUpdated(order.StoredOrder{Order: order.Snapshot{ID: "a", Source: order.SourceCounter, Money: order.Money{Total: 1}}})
	if e := <-tab; e.Type != "order.updated" {
		t.Fatalf("got %s", e.Type)
	}
}
