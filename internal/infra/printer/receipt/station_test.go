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
		"DELIVERY\n", cmdHugeSz + "1104\n", "ANA\n",
		// Printed on a stand-in: the reroute bands, English then Spanish, reverse + double size (2026-11-12).
		cmdInvertOn + pad("REROUTED FROM OVEN", defaultLayout.big()) + cmdInvertOff,
		cmdInvertOn + pad("REDIRIGIDO DE OVEN", defaultLayout.big()) + cmdInvertOff,
		"2  Pepperoni Pizza\n   (Large)\n", "   - Extra cheese\n", "   Note: well done\n",
		"1  Mystery\n", "** UNMATCHED **",
		"Also on this order:\n   1 Fries\n",
		"NOTE: Ring the bell twice\n", // the order's note, under the header (the owner, 2026-11-12)
	} {
		if !bytes.Contains(got, []byte(want)) {
			t.Errorf("station ticket missing %q", want)
		}
	}
	// The kitchen ticket is the type, the number, the name, the order's note
	// and the food. No reverse print but the two reroute bands, no prices.
	if n := bytes.Count(got, []byte(cmdInvertOn)); n != 2 {
		t.Errorf("reverse print %d times, want 2 (the reroute bands)", n)
	}
	ps, pt := stationFixture()
	pt.ReroutedFrom = ""
	if plain := BuildStation(ps, pt); bytes.Contains(plain, []byte(cmdInvertOn)) || bytes.Contains(plain, []byte("REROUTED")) {
		t.Errorf("a ticket on its own printer has no reroute band")
	}
	for _, gone := range []string{"Order #4417", "Order notes", "$", "#1104"} {
		if bytes.Contains(got, []byte(gone)) {
			t.Errorf("station ticket should not carry %q", gone)
		}
	}
	if n := bytes.Count(got, []byte("UNMATCHED")); n != 1 {
		t.Errorf("UNMATCHED appears %d times, want 1", n)
	}
}

func TestBuildStation_NoPrices(t *testing.T) {
	snap, tk := stationFixture()
	tk.Mine[0].Children = []order.LineItem{{Name: "Extra cheese", Quantity: 1, UnitPrice: 200}, {Name: "Ranch", Quantity: 2}}
	got := string(BuildStation(snap, tk))
	if !strings.Contains(got, "   - Extra cheese\n") || !strings.Contains(got, "   - (x2) Ranch\n") || strings.Contains(got, "$") {
		t.Fatalf("modifiers without prices: %q", got)
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
	if st := Build(snap, sc, tk, order.DefaultReceiptPrefs()); !bytes.Contains(st, []byte("READY 7:30")) || !bytes.Contains(st, []byte("Printed 1:31 PM")) {
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
	// A word longer than the line is split (it used to overflow and the
	// printer broke it mid-word without the indent).
	got = wordWrap("1 Supercalifragilisticexpialidocious Pizza", 16, "  ")
	want = []string{"1", "  Supercalifragi", "  listicexpialid", "  ocious Pizza"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("long word: got %q want %q", got, want)
	}
	for _, l := range got {
		if utf8.RuneCountInString(l) > 16 {
			t.Fatalf("line too long: %q", l)
		}
	}
	// The first line keeps its lead ("  - " of a note).
	if got := wordWrap("  - Note: extra crispy please", 16, "    "); got[0] != "  - Note: extra" {
		t.Fatalf("lead lost: %q", got)
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
		{Name: "Turkey Club", Size: "Serving", Quantity: 1, Routing: order.RoutingMatched, PrinterID: "p1"},
		{Name: "Wings", Size: "Serving (6 Piece)", Quantity: 1, Routing: order.RoutingMatched, PrinterID: "p2"},
	}}
	out := string(BuildStation(snap, printer.Ticket{
		Kind: printer.TicketStation, TargetID: "p1", Station: "Kitchen", Mine: snap.Items[:1], Others: snap.Items[1:],
	}))
	if !strings.Contains(out, "1  Turkey Club\n") || strings.Contains(strings.ToLower(out), "serving") {
		t.Errorf("single-size item should print without a size:\n%q", out)
	}
	if !strings.Contains(out, "1 Wings (6 Piece)") {
		t.Errorf("a fixed portion keeps its size:\n%q", out)
	}
}

func TestBuildWith_PaymentWording(t *testing.T) {
	sc := storeconfig.StoreContext{}
	for _, tc := range []struct {
		name     string
		payments []order.PaymentSummary
		want     []string
		not      []string
	}{
		{"cash (no payments)", nil, []string{"CASH - $42.97", "Collect Cash: $42.97"}, []string{"PAID"}},
		{"card", []order.PaymentSummary{{Method: "card", AmountCents: 4297, Last4: "4242"}}, []string{"PAID - CARD", "Card: ****4242", "Order Total:$42.97"}, []string{"Collect Cash"}},
		{"third party", []order.PaymentSummary{{Method: "other", AmountCents: 4297}}, []string{"PAID", "Order Total:$42.97"}, []string{"Collect Cash", "CASH -"}},
		{"part-paid", []order.PaymentSummary{{Method: "card", AmountCents: 2297}, {Method: "cash", AmountCents: 2000}}, []string{"CASH - $20.00", "Collect Cash: $20.00"}, nil},
	} {
		snap := receiptFixture()
		snap.Money = order.Money{Subtotal: 4297, Total: 4297}
		snap.Payments = tc.payments
		out := string(BuildWith(snap, sc))
		for _, w := range tc.want {
			if !strings.Contains(out, w) {
				t.Errorf("%s: missing %q", tc.name, w)
			}
		}
		for _, n := range tc.not {
			if strings.Contains(out, n) {
				t.Errorf("%s: should not print %q", tc.name, n)
			}
		}
	}
}

// TestBuildStation_Void (00019): a void ticket is a change ticket with only
// REMOVE / QUITAR: every line "(VOID)" and struck through; nothing else.
func TestBuildStation_Void(t *testing.T) {
	snap, tk := stationFixture()
	tk.Void, tk.Others, tk.ReroutedFrom = true, nil, ""
	tk.Mine = tk.Mine[:1]
	out := string(BuildStation(snap, tk))
	for _, want := range []string{"REMOVE / QUITAR", "(VOID) " + strikeImage("2  Pepperoni Pizza (Large)") + "\n",
		"          " + strikeImage("- Extra cheese") + "\n"} {
		if !strings.Contains(out, want) {
			t.Fatalf("void ticket lacks %q:\n%q", want, out)
		}
	}
	if strings.Contains(out, "Also on this order") || strings.Contains(out, "ADD / AGREGAR") {
		t.Fatal("void ticket lists other items or adds")
	}
}

// TestBuildStation_VoidGolden pins the VOID slip a cancelled website order
// prints on its stations (2.5.0 CANCEL ORDER) byte for byte.
func TestBuildStation_VoidGolden(t *testing.T) {
	snap, tk := stationFixture()
	tk.Void, tk.Others, tk.ReroutedFrom = true, nil, ""
	golden(t, "testdata/station_void.golden", BuildStation(snap, tk))
}

// TestBuildStation_AddOn (terminal §2.10d, an older plan's ADD-ON ticket):
// the added lines under ADD / AGREGAR, nothing else.
func TestBuildStation_AddOn(t *testing.T) {
	snap, tk := stationFixture()
	tk.AddOn, tk.Others, tk.ReroutedFrom = true, nil, ""
	tk.Mine = tk.Mine[:1]
	out := string(BuildStation(snap, tk))
	if !strings.Contains(out, "ADD / AGREGAR") || !strings.Contains(out, "2  Pepperoni Pizza\n") {
		t.Fatalf("add-on ticket:\n%q", out)
	}
	if strings.Contains(out, "Also on this order") || strings.Contains(out, "VOID") {
		t.Fatal("add-on ticket lists other items or says VOID")
	}
}

// The reroute banner (the owner, 2026-11-12): English then Spanish, ALL
// CAPS, each line a reverse double-size band like ADD / AGREGAR; a long
// printer name wraps into more bands, never past the band of the printer it
// printed on (its double-width line: 24 / 21 / 16), a space either side of
// the text.
func TestRerouteBands(t *testing.T) {
	if got := RerouteLines("pizza", 0); strings.Join(got, "|") != "REROUTED FROM PIZZA|REDIRIGIDO DE PIZZA" {
		t.Fatalf("lines = %q", got)
	}
	for _, c := range []struct {
		cols int
		want string
	}{
		{Cols80, "REROUTED FROM ONLINE|RECEIPT PRINTER|REDIRIGIDO DE ONLINE|RECEIPT PRINTER"},
		{Cols72, "REROUTED FROM|ONLINE RECEIPT|PRINTER|REDIRIGIDO DE|ONLINE RECEIPT|PRINTER"},
		{Cols58, "REROUTED FROM|ONLINE RECEIPT|PRINTER|REDIRIGIDO DE|ONLINE RECEIPT|PRINTER"},
	} {
		long := RerouteLines("Online receipt printer", c.cols)
		if strings.Join(long, "|") != c.want {
			t.Fatalf("%d: long = %q", c.cols, long)
		}
		text := layoutFor(c.cols).big() - 2
		for _, l := range long {
			if len([]rune(l)) > text {
				t.Fatalf("%d: band text %q wider than %d", c.cols, l, text)
			}
		}
	}
	var buf bytes.Buffer
	writeRerouted(func(s string) { buf.WriteString(s) }, func() { buf.WriteString(cmdFeed) }, "Wings", layoutFor(Cols72))
	want := cmdCenter + cmdBoldOn + cmdDoubleSz + cmdInvertOn + " REROUTED FROM WINGS " + cmdInvertOff + cmdSizeReset + cmdBaseSz + cmdBoldOff + "\n" +
		cmdCenter + cmdBoldOn + cmdDoubleSz + cmdInvertOn + " REDIRIGIDO DE WINGS " + cmdInvertOff + cmdSizeReset + cmdBaseSz + cmdBoldOff + "\n" +
		cmdLeft + "\n"
	if buf.String() != want {
		t.Fatalf("bytes\n got %q\nwant %q", buf.String(), want)
	}
	// Every slip that can print on a stand-in carries the bands.
	snap, tk := stationFixture()
	tk.Kind = printer.TicketReceipt
	for name, b := range map[string][]byte{
		"receipt":    Build(snap, storeconfig.StoreContext{}, tk, order.DefaultReceiptPrefs()),
		"station":    BuildStation(stationFixture()),
		"unassigned": BuildUnassignedReport(order.UnassignedReport{}, storeconfig.StoreContext{}, "", printer.Paper{ReroutedFrom: "Oven"}, time.Now()),
		"unsettled":  BuildUnsettledList(order.UnsettledList{}, storeconfig.StoreContext{}, "", printer.Paper{ReroutedFrom: "Oven"}, time.Now()),
		"unpaid":     BuildUnpaidAtClose(order.UnpaidAtClose{}, storeconfig.StoreContext{}, "", printer.Paper{ReroutedFrom: "Oven"}, time.Now()),
		"driver":     BuildDriverReport(order.DriverReport{}, storeconfig.StoreContext{}, "", printer.Paper{ReroutedFrom: "Oven"}, time.Now()),
	} {
		for _, l := range []string{"REROUTED FROM OVEN", "REDIRIGIDO DE OVEN"} {
			if !bytes.Contains(b, []byte(cmdInvertOn+pad(l, defaultLayout.big())+cmdInvertOff)) {
				t.Errorf("%s: no %q band", name, l)
			}
		}
	}
}
