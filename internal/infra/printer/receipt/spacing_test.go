package receipt

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
	"quiccpos/agent/internal/domain/storeconfig"
)

// Line spacing (receipt settings `spacing`): extra dots (ESC J n) between an
// item and its first modifier, between modifiers, and after an item with no
// modifiers. 0 (the default) prints the same bytes as before.

var spaced = prefsWith(func(p *order.ReceiptPrefs) {
	p.Spacing = order.LineSpacing{ItemMod: 8, ModMod: 4, ItemItem: 12}
})

func spacingStation() (order.Snapshot, printer.Ticket) {
	k := int64(1104)
	pep := order.LineItem{Name: "Pepperoni Pizza", Quantity: 1, Routing: order.RoutingMatched, PrinterID: "p-pizza",
		Children: []order.LineItem{{Name: "Extra cheese", Quantity: 1}, {Name: "Onions", Quantity: 1}}}
	plain := order.LineItem{Name: "Garlic Knots", Quantity: 1, Routing: order.RoutingMatched, PrinterID: "p-pizza"}
	last := order.LineItem{Name: "Cheese Pizza", Quantity: 1, Routing: order.RoutingMatched, PrinterID: "p-pizza",
		Children: []order.LineItem{{Name: "Well done", Quantity: 1}}}
	snap := order.Snapshot{ID: "o1", SourceEventID: "1", KitchenNumber: &k, Customer: order.Customer{DisplayName: "Ana"},
		Fulfillment: order.Fulfillment{Type: "pickup"}, Items: []order.LineItem{pep, plain, last}}
	return snap, printer.Ticket{Kind: printer.TicketStation, TargetID: "p-pizza", Station: "Pizza", Mine: snap.Items}
}

func TestSpacing_Goldens(t *testing.T) {
	snap, tk := spacingStation()
	cases := map[string][]byte{
		"testdata/station_spacing.golden":          Build(snap, storeconfig.StoreContext{}, tk, spaced),
		"testdata/receipt_delivery_spacing.golden": Build(previewDelivery(), previewStore(), receiptTicket(), spaced),
	}
	for path, got := range cases {
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
			t.Errorf("%s: bytes changed (run with -update if intended)\ngot:  %q", path, got)
		}
	}
}

func TestSpacing_WhereTheFeedsGo(t *testing.T) {
	snap, tk := spacingStation()
	s := string(Build(snap, storeconfig.StoreContext{}, tk, spaced))
	im, mm, ii := feedDots(8), feedDots(4), feedDots(12)
	for _, want := range []string{
		"1 Pepperoni Pizza\n" + cmdBaseSz + cmdBoldOff + im + "  - Extra cheese\n" + mm + "  - Onions\n",
		"1 Garlic Knots\n" + cmdBaseSz + cmdBoldOff + ii + cmdBoldOn, // no modifiers → item→item
		"1 Cheese Pizza\n" + cmdBaseSz + cmdBoldOff + im + "  - Well done\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in %q", want, s)
		}
	}
	// After an item WITH modifiers there's no item→item feed.
	if strings.Contains(s, "  - Onions\n"+ii) {
		t.Error("item→item applies only after an item with no modifiers")
	}
	if feedDots(0) != "" || feedDots(8) != "\x1b\x4a\x08" {
		t.Error("feedDots")
	}
	// Defaults: not a byte added (the default goldens stay as they are).
	if strings.Contains(string(Build(snap, storeconfig.StoreContext{}, tk, order.DefaultReceiptPrefs())), "\x1b\x4a") {
		t.Error("default spacing must add nothing")
	}
}

// PRINT SAMPLE prints with the saved prefs, so it shows the spacing too.
func TestSpacing_Sample(t *testing.T) {
	if !strings.Contains(string(BuildSample(previewStore(), spaced, printed)), "\x1b\x4a") {
		t.Error("the sample must reflect the spacing")
	}
	if strings.Contains(string(BuildSample(previewStore(), order.DefaultReceiptPrefs(), printed)), "\x1b\x4a") {
		t.Error("the default sample has no extra spacing")
	}
}
