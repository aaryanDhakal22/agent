package receipt

import (
	"strings"
	"testing"

	"quiccpos/agent/internal/domain/order"
)

// show_on_driver_ticket (the canonical-menu merge, 2026-11-12): a delivery's
// receipt leaves off a $0 child stamped false; kitchen tickets and pickup
// receipts never change. 2.5.7: an unstamped $0 child prints only when the
// driver hands it over (a hand-over word, not a NO): "W Can Pepsi" yes,
// "Well done" / "Hot" no.

func boolp(v bool) *bool { return &v }

// comboDelivery: a sub combo with a $0 drink (true), a $0 "Well Done"
// (false), a $0 "NO Onion" (false), a priced extra stamped false (still
// printed: only $0 children hide) and a pizza with an unstamped $0 note.
func comboDelivery() order.Snapshot {
	snap := previewDelivery()
	combo := order.LineItem{Name: "Cheesesteak Combo", Quantity: 1, UnitPrice: 1299, LineTotal: 1449,
		Routing: order.RoutingMatched, PrinterID: "p-fry",
		Children: []order.LineItem{
			{Name: "Well Done", Quantity: 1, ShowOnDriverTicket: boolp(false)},
			{Name: "NO Onion", Quantity: 1, Mode: order.ModeNo, ShowOnDriverTicket: boolp(false)},
			{Name: "W Can Pepsi", Quantity: 1, ShowOnDriverTicket: boolp(true)},
			{Name: "Extra Cheese", Quantity: 1, UnitPrice: 150, ShowOnDriverTicket: boolp(false)},
			{Name: "Slushie Cherry", Quantity: 1, ShowOnDriverTicket: boolp(true)},
		}}
	snap.Items = append([]order.LineItem{combo}, snap.Items...)
	return snap
}

func TestDriverTicket_DeliveryExpanded(t *testing.T) {
	got := string(Build(comboDelivery(), previewStore(), receiptTicket(), order.DefaultReceiptPrefs()))
	for _, want := range []string{"W Can Pepsi", "Slushie Cherry", "Extra Cheese"} {
		if !strings.Contains(got, want) {
			t.Errorf("delivery receipt lacks %q:\n%s", want, got)
		}
	}
	// "Well done" / "Hot": the unstamped $0 cooking choices of the other lines (2.5.7: off).
	for _, not := range []string{"Well Done", "NO Onion", "Well done", "    Hot"} {
		if strings.Contains(got, not) {
			t.Errorf("delivery receipt has the hidden %q:\n%s", not, got)
		}
	}
	// The item's sum is unchanged (hidden children are $0).
	if !strings.Contains(got, "Sum: $14.49") {
		t.Errorf("combo sum changed:\n%s", got)
	}
}

func TestDriverTicket_DeliveryCondensed(t *testing.T) {
	prefs := prefsWith(func(p *order.ReceiptPrefs) { p.Format = order.FormatCondensed })
	got := string(Build(comboDelivery(), previewStore(), receiptTicket(), prefs))
	// First modifier = the first one the driver may see (Well Done and NO
	// Onion skipped), then every stamped-true child; Extra Cheese is not the
	// first and not flagged: off as before.
	i, j := strings.Index(got, "    - W Can Pepsi\n"), strings.Index(got, "    - Slushie Cherry\n")
	if i < 0 || j < i {
		t.Fatalf("condensed delivery must list the drink then the slushie:\n%s", got)
	}
	for _, not := range []string{"Well Done", "NO Onion", "Extra Cheese"} {
		if strings.Contains(got, not) {
			t.Errorf("condensed delivery has %q:\n%s", not, got)
		}
	}
	// The combo line is still priced with all its children.
	if !strings.Contains(got, defaultLayout.itemLine(1, "Cheesesteak Combo", 1449)) {
		t.Errorf("combo line price changed:\n%s", got)
	}
}

func TestDriverTicket_PickupAndKitchenUnchanged(t *testing.T) {
	pickup := comboDelivery()
	pickup.Fulfillment = order.Fulfillment{Type: "pickup"}
	got := string(Build(pickup, previewStore(), receiptTicket(), order.DefaultReceiptPrefs()))
	for _, want := range []string{"Well Done", "NO Onion", "W Can Pepsi"} {
		if !strings.Contains(got, want) {
			t.Errorf("pickup receipt lacks %q:\n%s", want, got)
		}
	}
	// Condensed pickup: the first child only, as before.
	cond := string(Build(pickup, previewStore(), receiptTicket(), prefsWith(func(p *order.ReceiptPrefs) { p.Format = order.FormatCondensed })))
	if !strings.Contains(cond, "    - Well Done\n") || strings.Contains(cond, "W Can Pepsi") {
		t.Errorf("condensed pickup changed:\n%s", cond)
	}
	// The kitchen ticket of a delivery still has every child.
	snap := comboDelivery()
	st := stationTicket()
	st.Mine = snap.Items[:1]
	st.Others = snap.Items[1:]
	kt := strings.ToUpper(string(Build(snap, previewStore(), st, order.DefaultReceiptPrefs())))
	for _, want := range []string{"WELL DONE", "ONION", "PEPSI"} {
		if !strings.Contains(kt, want) {
			t.Errorf("kitchen ticket lacks %q:\n%s", want, kt)
		}
	}
}

// Unset stamps (vendor and older orders, 2.5.7): a $0 child prints when the
// driver hands it over (a hand-over word, not a NO), else it is left off; a
// stamped-true child always prints.
func TestDriverTicket_UnsetFollowsTheHandoverWords(t *testing.T) {
	strip := func(s order.Snapshot) order.Snapshot {
		items := make([]order.LineItem, len(s.Items))
		for i, it := range s.Items {
			kids := make([]order.LineItem, len(it.Children))
			for j, c := range it.Children {
				c.ShowOnDriverTicket = nil
				kids[j] = c
			}
			it.Children = kids
			items[i] = it
		}
		s.Items = items
		return s
	}
	got := string(Build(strip(comboDelivery()), previewStore(), receiptTicket(), order.DefaultReceiptPrefs()))
	for _, want := range []string{"W Can Pepsi", "Extra Cheese", "Slushie Cherry"} { // a drink by its name
		if !strings.Contains(got, want) {
			t.Errorf("unstamped delivery lacks %q:\n%s", want, got)
		}
	}
	for _, not := range []string{"Well Done", "NO Onion"} {
		if strings.Contains(got, not) {
			t.Errorf("unstamped delivery has %q:\n%s", not, got)
		}
	}
	// No hand-over words: only the priced child and the stamped ones.
	none := prefsWith(func(p *order.ReceiptPrefs) { p.HandoverChoiceWords = order.NewWordList() })
	if got := string(Build(strip(comboDelivery()), previewStore(), receiptTicket(), none)); strings.Contains(got, "W Can Pepsi") {
		t.Errorf("no words: the unstamped can is off:\n%s", got)
	}
	if got := string(Build(comboDelivery(), previewStore(), receiptTicket(), none)); !strings.Contains(got, "W Can Pepsi") {
		t.Errorf("no words: the stamped can still prints:\n%s", got)
	}
	marked := previewDelivery()
	for i := range marked.Items {
		for j := range marked.Items[i].Children {
			marked.Items[i].Children[j].ShowOnDriverTicket = boolp(true)
		}
	}
	all := string(Build(marked, previewStore(), receiptTicket(), order.DefaultReceiptPrefs()))
	for _, it := range marked.Items {
		for _, c := range it.Children {
			if !strings.Contains(all, c.Name) {
				t.Errorf("stamped true: %q missing:\n%s", c.Name, all)
			}
		}
	}
}
