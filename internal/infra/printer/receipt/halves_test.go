package receipt

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
)

// Half and half (TERMINAL-HALVES.md): the kitchen ticket's WHOLE: / 1ST
// HALF: / 2ND HALF: lines, the receipt's compact list and the condensed
// receipt's summary. Goldens: testdata/half_*.golden (-update).

func mod(name, portion, mode string, price int64) order.LineItem {
	return order.LineItem{Name: name, Portion: portion, Mode: mode, Quantity: 1, UnitPrice: price}
}

func halfPizza(name string, unit int64, kids ...order.LineItem) order.LineItem {
	return order.LineItem{Name: name, Size: "16 X-Large", Quantity: 1, UnitPrice: unit, Routing: order.RoutingMatched, PrinterID: "p-pizza",
		Children: kids}
}

// The format's cases as main sends them (names keep their readable text):
// the condensed receipt's summary and the kitchen ticket's lines under the item.
var halfCases = []struct {
	slug    string
	item    order.LineItem
	summary string
	lines   string
}{
	{"a_add_on_a_half", halfPizza("Hawaiian", 1999, mod("Pepperoni (1st half)", "first", "add", 225)),
		"HAWAIIAN + " + halfSign + " PEPPERONI",
		"   1ST HALF:\n   - Pepperoni\n   2ND HALF:\n   - nothing added\n"},
	{"b_only_on_a_half", halfPizza("Cheese Pizza", 1999, mod("Hawaiian (2nd half)", "second", "specialty", 0),
		mod("ONLY Pepperoni (1st half)", "first", "only", 225)),
		halfSign + " PEPPERONI | " + halfSign + " HAWAIIAN",
		"   1ST HALF:\n   - Pepperoni ONLY\n   2ND HALF:\n   - Hawaiian\n"},
	{"c_two_specialties", halfPizza("Cheese Pizza", 2899, mod("Supreme (1st half)", "first", "specialty", 0),
		mod("Veggie (2nd half)", "second", "specialty", 0), mod("Extra Cheese", "", "add", 200)),
		halfSign + " SUPREME | " + halfSign + " VEGGIE",
		"   WHOLE:\n   - Extra Cheese\n   1ST HALF:\n   - Supreme\n   2ND HALF:\n   - Veggie\n"},
	{"d_no_on_a_half", halfPizza("Hawaiian", 1999, mod("NO Pineapple (2nd half)", "second", "no", 0)),
		"HAWAIIAN (" + halfSign + " NO PINEAPPLE)",
		"   1ST HALF:\n   - nothing added\n   2ND HALF:\n   - NO Pineapple\n"},
	{"e_both_halves_added", halfPizza("Cheese Pizza", 1299, mod("Mushrooms (1st half)", "first", "add", 225),
		mod("Sausage (2nd half)", "second", "add", 225), mod("Onion (2nd half)", "second", "add", 175)),
		"CHEESE PIZZA " + halfSign + " MUSHROOMS | " + halfSign + " SAUSAGE, ONION",
		"   1ST HALF:\n   - Mushrooms\n   2ND HALF:\n   - Sausage\n   - Onion\n"},
	{"f_specialty_with_adds_and_whole_no", halfPizza("Cheese Pizza", 1999, mod("Hawaiian (2nd half)", "second", "specialty", 0),
		mod("Onion (2nd half)", "second", "add", 175), mod("Pepperoni (1st half)", "first", "add", 225), mod("NO Oregano", "", "no", 0)),
		halfSign + " CHEESE PIZZA + PEPPERONI | " + halfSign + " HAWAIIAN + ONION (NO OREGANO)",
		"   WHOLE:\n   - NO Oregano\n   1ST HALF:\n   - Pepperoni\n   2ND HALF:\n   - Hawaiian\n   - Onion\n"},
}

func halfSnap(items ...order.LineItem) order.Snapshot {
	k := int64(1107)
	return order.Snapshot{ID: "o-half", SourceEventID: "9001", KitchenNumber: &k, Source: "counter",
		PlacedAt: time.Date(2026, 11, 11, 18, 5, 0, 0, time.UTC), Customer: order.Customer{DisplayName: "Ada Lovelace"},
		Fulfillment: order.Fulfillment{Type: "pickup"}, Items: items,
		Money: order.Money{Subtotal: 2224, Tax: 133, Total: 2357}}
}

func golden(t *testing.T, path string, got []byte) {
	t.Helper()
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
		t.Fatalf("%s changed (run with -update if intended)\ngot:  %q\nwant: %q", path, got, want)
	}
}

func TestHalves_StationGoldens(t *testing.T) {
	for _, c := range halfCases {
		t.Run(c.slug, func(t *testing.T) {
			if !c.item.HasHalves() {
				t.Fatal("fixture has no halves")
			}
			if got := halfText(c.item); got != c.summary {
				t.Errorf("summary\n got %q\nwant %q", got, c.summary)
			}
			snap := halfSnap(c.item)
			got := BuildStation(snap, printer.Ticket{Kind: printer.TicketStation, TargetID: "p-pizza", Station: "Pizza", Mine: snap.Items})
			head, lines := cmdBoldOn+cmdDoubleSz+"1  "+strings.ToUpper(c.item.Name), "\n"+cmdSizeReset+cmdBaseSz+cmdBoldOff+c.lines
			if !bytes.Contains(got, []byte(head)) || !bytes.Contains(got, []byte(lines)) {
				t.Errorf("ticket lacks\n%q\n%q\n%q", head, lines, got)
			}
			for _, gone := range []string{"LEFT", "RIGHT", "left half", "right half", "$", cmdInvertOn, "HALF & HALF", "\xab"} {
				if bytes.Contains(got, []byte(gone)) {
					t.Errorf("ticket carries %q", gone)
				}
			}
			golden(t, "testdata/half_station_"+c.slug+".golden", got)
		})
	}
}

// The receipt keeps each modifier with its price, grouped whole → 1st → 2nd;
// the condensed receipt shows the summary.
func TestHalves_Receipt(t *testing.T) {
	f := halfCases[5].item
	snap := halfSnap(f, order.LineItem{Name: "Soda", Quantity: 1, UnitPrice: 199, Routing: order.RoutingMatched, PrinterID: "p-pizza"})
	sc := previewStore()
	got := Build(snap, sc, printer.Ticket{Kind: printer.TicketReceipt}, order.DefaultReceiptPrefs())
	want := []string{modifierLine("NO Oregano", 0), modifierLine("1st: Pepperoni", 225), modifierLine("2nd: Hawaiian", 0), modifierLine("2nd: Onion", 175)}
	at := -1
	for _, w := range want {
		i := bytes.Index(got, []byte(w))
		if i < at {
			t.Fatalf("receipt lines out of order or missing at %q\n%q", w, got)
		}
		at = i
	}
	if !bytes.Contains(got, []byte("Sum: "+dollars(1999+225+175))) {
		t.Errorf("the half line's sum changed")
	}
	golden(t, "testdata/half_receipt.golden", got)

	prefs := order.DefaultReceiptPrefs()
	prefs.Format = order.FormatCondensed
	got = Build(snap, sc, printer.Ticket{Kind: printer.TicketReceipt}, prefs)
	if !bytes.Contains(got, []byte("    - \xab CHEESE PIZZA + PEPPERONI |")) {
		t.Errorf("condensed receipt lacks the summary\n%q", got)
	}
	golden(t, "testdata/half_receipt_condensed.golden", got)
}

// A line without portions (older main, a vendor order) prints exactly as
// before; mode alone (main's "add" / "no") changes nothing.
func TestHalves_NoPortionsPrintAsBefore(t *testing.T) {
	snap, tk := stationFixture()
	before := BuildStation(snap, tk)
	for i := range snap.Items {
		for j := range snap.Items[i].Children {
			snap.Items[i].Children[j].Mode = order.ModeAdd
		}
	}
	tk.Mine = snap.Items[:2]
	if got := BuildStation(snap, tk); !bytes.Equal(got, before) {
		t.Fatalf("a mode without a portion changed the ticket")
	}
}
