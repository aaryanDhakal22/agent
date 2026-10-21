package receipt

import (
	"bytes"
	"flag"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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
		Notes:       "Ring the bell twice",
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
		" REROUTED from OVEN ", cmdQuadSz + "#1104\n", "ANA\n",
		"2 Pepperoni Pizza\n", "  (Large)\n", "  - Extra cheese\n", "  - Note: well done",
		"1 Mystery", " ** UNMATCHED ** ",
		cmdNormalSz + cmdBoldOn + "ITEMS TO OTHER PRINTERS", "1 Fries",
	} {
		if !bytes.Contains(got, []byte(want)) {
			t.Errorf("station ticket missing %q", want)
		}
	}
	// The kitchen ticket is just the number, the name and the food; the
	// order's notes are on the customer receipt only.
	for _, gone := range []string{"Order #4417", "Delivery", " PIZZA ", "Ring the bell", "Order notes"} {
		if bytes.Contains(got, []byte(gone)) {
			t.Errorf("station ticket should not carry %q", gone)
		}
	}
	// Free modifiers carry no price.
	if bytes.Contains(got, []byte("$")) {
		t.Errorf("station ticket printed a price for a free modifier")
	}
	if n := bytes.Count(got, []byte("UNMATCHED")); n != 1 {
		t.Errorf("UNMATCHED appears %d times, want 1", n)
	}
}

func TestBuildStation_PaidModifierShowsPrice(t *testing.T) {
	snap, tk := stationFixture()
	tk.Mine[0].Children = []order.LineItem{{Name: "Extra cheese", Quantity: 1, UnitPrice: 200}, {Name: "Well done", Quantity: 1}}
	got := string(BuildStation(snap, tk))
	if !strings.Contains(got, "  - Extra cheese") || !strings.Contains(got, "$2.00") {
		t.Fatalf("a paid modifier shows its price: %q", got)
	}
	if strings.Contains(got, "$0.00") {
		t.Fatal("a free modifier shows no price")
	}
}

func TestBuildStation_NoOtherItems(t *testing.T) {
	snap, tk := stationFixture()
	tk.Others = nil
	if bytes.Contains(BuildStation(snap, tk), []byte("ITEMS TO OTHER PRINTERS")) {
		t.Fatal("no heading when every item is on this ticket")
	}
}

func receiptFixture() order.Snapshot {
	snap, _ := stationFixture()
	snap.Customer = order.Customer{DisplayName: "Maria Ana Gonzalez", Phone: "4105551234"}
	snap.Fulfillment.Address = &order.Address{Line1: "1234 Harford Rd", City: "Baltimore", PostalCode: "21214"}
	snap.Items[0].UnitPrice = 1899
	snap.Items[0].Children = []order.LineItem{
		{Name: "Extra cheese", Quantity: 1, UnitPrice: 200},
		{Name: "Well done", Quantity: 1},
		{Name: "Side of Fries", Quantity: 1, UnitPrice: 299},
	}
	return snap
}

func TestBuildWith_LooksLikeTheV1Receipt(t *testing.T) {
	got := string(BuildWith(receiptFixture(), storeconfigEmpty()))
	for _, want := range []string{
		cmdQuadSz + "#1104\n",            // the kitchen number in the vendor number's place
		" GONZALEZ, MARIA ANA ",          // LAST, FIRST
		"2   Large",                      // qty + size on the price line …
		"    Pepperoni Pizza\n",          // … the item name on its own line
		"     Well done\n",               // free modifier: no price
		"Extra cheese",                   // paid modifier keeps its price
		cmdBoldOn + "     Side of Fries", // sides in bold
		"Sum: $42.97",                    // 2 x $18.99 + modifiers, as v1 adds it
		"Apt: \n",                        // the Apt line is always there
		"ONLINE ORDER",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("receipt missing %q", want)
		}
	}
	if strings.Contains(got, "Well done                    $0.00") || strings.Contains(got, "Order #4417") {
		t.Error("no $0.00 modifiers and no vendor order number on the receipt")
	}
}

func TestBuildWith_ItemWithoutSize(t *testing.T) {
	got := string(BuildWith(receiptFixture(), storeconfigEmpty()))
	if !strings.Contains(got, "1   Mystery") {
		t.Fatalf("an item with no size prints its name on the price line: %q", got)
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
	got := BuildWith(snap, sc)
	if !bytes.Contains(got, []byte("2:05 PM")) || bytes.Contains(got, []byte("6:05 PM")) {
		t.Error("receipt should show New York time (2:05 PM)")
	}
	promised := time.Date(2026, 10, 15, 23, 30, 0, 0, time.UTC)
	snap.Fulfillment.PromisedAt = &promised
	if st := Build(snap, sc, tk, order.DefaultReceiptPrefs()); !bytes.Contains(st, []byte("FUTURE Oct 15  7:30 PM")) {
		t.Errorf("a scheduled order's kitchen ticket says when it's for, in New York time: %q", st)
	}
	sc.Location.Timezone = "Not/AZone"
	if !bytes.Contains(BuildWith(stationFixtureSnap(), sc), []byte("6:05 PM")) {
		t.Error("an unknown timezone leaves the time unchanged")
	}
}

func stationFixtureSnap() order.Snapshot { s, _ := stationFixture(); return s }

func TestWordWrap(t *testing.T) {
	got := wordWrap("1 x Pepperoni Pizza (Large)", 24, "    ")
	want := []string{"1 x Pepperoni Pizza", "    (Large)"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q want %q", got, want)
	}
	if got := wordWrap("short", 24, "  "); len(got) != 1 || got[0] != "short" {
		t.Fatalf("got %q", got)
	}
	for _, l := range wordWrap("Order notes: Ring the bell, the dog barks but is friendly and loves pizza", 41, "  ") {
		if utf8.RuneCountInString(l) > 41 {
			t.Fatalf("line too long: %q", l)
		}
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

func TestDisplaySize(t *testing.T) {
	for in, want := range map[string]string{
		"":                  "",
		"Serving":           "",
		" serving ":         "",
		"Serving (2 Piece)": "2 Piece",
		"Large":             "Large",
		"14 Large":          "14 Large",
		"Servings":          "Servings",
	} {
		if got := displaySize(in); got != want {
			t.Errorf("displaySize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildStation_HidesServingSize(t *testing.T) {
	snap := order.Snapshot{ID: "o1", Items: []order.LineItem{
		{Name: "Turkey Club Sandwich", Size: "Serving", Quantity: 1, Routing: order.RoutingMatched, PrinterID: "p1"},
		{Name: "Wings", Size: "Serving (6 Piece)", Quantity: 1, Routing: order.RoutingMatched, PrinterID: "p2"},
	}}
	out := string(BuildStation(snap, printer.Ticket{
		Kind: printer.TicketStation, TargetID: "p1", Station: "Kitchen", Mine: snap.Items[:1], Others: snap.Items[1:],
	}))
	if !strings.Contains(out, "1 Turkey Club Sandwich\n") || strings.Contains(out, "Serving") {
		t.Errorf("single-size item should print without a size:\n%q", out)
	}
	if !strings.Contains(out, "1 Wings (6 Piece)") {
		t.Errorf("a fixed portion keeps its size:\n%q", out)
	}
}
