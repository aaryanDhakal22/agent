package receipt

import (
	"bytes"
	"unicode/utf8"
	"flag"
	"os"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
	"quiccpos/agent/internal/domain/storeconfig"
)

var update = flag.Bool("update", false, "rewrite golden files")

func stationFixture() (order.Snapshot, printer.Ticket) {
	kitchen := int64(1104)
	pep := order.LineItem{Name: "Pepperoni Pizza", Size: "Large", Quantity: 2, Routing: order.RoutingMatched, PrinterID: "p-pizza",
		Children: []order.LineItem{{Name: "Extra cheese", Quantity: 1}}, Notes: "well done"}
	myst := order.LineItem{Name: "Mystery", Quantity: 1, Routing: order.RoutingUnmatched}
	fries := order.LineItem{Name: "Fries", Quantity: 1, Routing: order.RoutingMatched, PrinterID: "p-fry"}
	snap := order.Snapshot{
		ID: "o1", SourceEventID: "4417", KitchenNumber: &kitchen,
		PlacedAt:    time.Date(2026, 10, 15, 18, 5, 0, 0, time.UTC),
		Customer:    order.Customer{DisplayName: "Ana"},
		Fulfillment: order.Fulfillment{Type: "delivery"},
		Items:       []order.LineItem{pep, myst, fries},
	}
	return snap, printer.Ticket{Kind: printer.TicketStation, TargetID: "p-pizza", Station: "Pizza",
		Mine: []order.LineItem{pep, myst}, Others: []order.LineItem{fries}, ReroutedFrom: "Oven"}
}

func TestBuildStation_Golden(t *testing.T) {
	got := BuildStation(stationFixture())
	path := "testdata/station.golden"
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create)", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("station ticket bytes changed (run with -update if intended)\ngot:  %q\nwant: %q", got, want)
	}
}

func TestBuildStation_Content(t *testing.T) {
	got := BuildStation(stationFixture())
	for _, want := range []string{
		" REROUTED from OVEN ", "1104\n", "Order #4417", " PIZZA ", "ANA",
		"2 x Pepperoni Pizza (Large)", "   + Extra cheese", "   Note: well done",
		"1 x Mystery", " ** UNMATCHED ** ",
		cmdSmallSz + "Also on this order:", "  1 Fries",
	} {
		if !bytes.Contains(got, []byte(want)) {
			t.Errorf("station ticket missing %q", want)
		}
	}
	// Prices/payment belong on the customer receipt only.
	if bytes.Contains(got, []byte("$")) {
		t.Errorf("station ticket should carry no prices")
	}
	// Only the unmatched item is flagged.
	if n := bytes.Count(got, []byte("UNMATCHED")); n != 1 {
		t.Errorf("UNMATCHED appears %d times, want 1", n)
	}
}

func TestBuildWith_PrintsSize(t *testing.T) {
	snap, _ := stationFixture()
	if !bytes.Contains(BuildWith(snap, storeconfigEmpty()), []byte("Pepperoni Pizza (Large)")) {
		t.Fatal("full receipt should print the size")
	}
}

func storeconfigEmpty() storeconfig.StoreContext { return storeconfig.StoreContext{} }

func TestBuildWith_NoKitchenNumberShowsVendorNumber(t *testing.T) {
	snap, _ := stationFixture()
	snap.KitchenNumber = nil
	got := BuildWith(snap, storeconfigEmpty())
	if !bytes.Contains(got, []byte("#4417")) || bytes.Contains(got, []byte("Order #4417")) {
		t.Fatal("without a kitchen number the header is the vendor #")
	}
}

func TestTicketsUseTheLocationTimezone(t *testing.T) {
	snap, tk := stationFixture() // placed 18:05 UTC
	sc := storeconfig.StoreContext{}
	sc.Location.Timezone = "America/New_York"
	for name, got := range map[string][]byte{"station": Build(snap, sc, tk), "receipt": BuildWith(snap, sc)} {
		if !bytes.Contains(got, []byte("2:05 PM")) || bytes.Contains(got, []byte("6:05 PM")) {
			t.Errorf("%s ticket should show New York time (2:05 PM)", name)
		}
	}
	sc.Location.Timezone = "Not/AZone"
	if !bytes.Contains(BuildWith(snap, sc), []byte("6:05 PM")) {
		t.Error("an unknown timezone leaves the time unchanged")
	}
}

func TestTruncRunesKeepsCharactersWhole(t *testing.T) {
	name := "Crème brûlée façon grand-mère extra long"
	got := truncRunes(name, 29)
	if !utf8.ValidString(got) || utf8.RuneCountInString(got) != 29 {
		t.Fatalf("got %q", got)
	}
	if truncRunes("Soda", 29) != "Soda" {
		t.Fatal("short names untouched")
	}
}
