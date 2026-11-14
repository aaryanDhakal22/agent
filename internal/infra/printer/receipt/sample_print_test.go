package receipt

// Owner's sample tickets (2026-11-14): a made-up delivery order rendered with
// the Pi's real delivery slip (BuildDeliveryCopies, one copy) and the store's
// real settings, so the owner can check the layout on paper. Skipped unless
// RECEIPT_SAMPLE_DIR is set; quicc-ops scripts/receipt-sample.sh fills that
// folder (store_config + receipt prefs + logo, read-only from the Pi), runs
// this test and sends the .bin files to a printer through iris.
//
// Variants (each a <name>.bin in the folder), all the real builders' bytes in
// the owner's final design (2026-11-14: the stacked logo header, Order #100,
// DELIVERY in Go Bold + the car centred, the note between rules, the drink
// marks on a delivery):
//   - asis: the sample delivery as it prints (one copy)
//   - drinks_real: 2 x 8 Wings & Fries (a can each) + a 2 liter, the drinks
//     stamped by the Pi's real rule (application/terminal StampDrinksWith over
//     a build with a Beverages category and the wings' soda group)
//   - online_note: an online (vendor) delivery with an order note

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	terminalApp "quiccpos/agent/internal/application/terminal"
	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
	"quiccpos/agent/internal/domain/storeconfig"
)

func TestReceiptSamples(t *testing.T) {
	dir := os.Getenv("RECEIPT_SAMPLE_DIR")
	if dir == "" {
		t.Skip("RECEIPT_SAMPLE_DIR not set (scripts/receipt-sample.sh)")
	}
	var sc storeconfig.StoreContext
	raw, err := os.ReadFile(filepath.Join(dir, "sc.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &sc); err != nil {
		t.Fatal(err)
	}
	prefs := order.DefaultReceiptPrefs()
	if praw, err := os.ReadFile(filepath.Join(dir, "prefs.json")); err == nil {
		if prefs, err = order.DecodeReceiptPrefs(praw); err != nil {
			t.Fatal(err)
		}
	}
	if logo, err := os.ReadFile(filepath.Join(dir, "logo.bin")); err == nil {
		img, err := DecodeReceiptLogo(logo)
		if err != nil {
			t.Fatal(err)
		}
		SetReceiptLogo(img, "sample")
		defer SetReceiptLogo(nil, "")
	}
	cols := 42
	if c := os.Getenv("RECEIPT_SAMPLE_COLS"); c == "48" || c == "32" {
		cols = map[string]int{"48": 48, "32": 32}[c]
	}
	was := deliveryNow
	deliveryNow = time.Now
	defer func() { deliveryNow = was }()

	n := int64(100)
	s := order.Snapshot{ID: "o-sample", Source: "counter", SourceEventID: "SAMPLE-100", KitchenNumber: &n,
		PlacedAt: time.Now().UTC(), TakenBy: "Khawar",
		Customer: order.Customer{DisplayName: "Sample Customer", Phone: "4105550100"},
		Fulfillment: order.Fulfillment{Type: "delivery", Address: &order.Address{Line1: "123 Sample St", Line2: "Apt 2",
			City: "Towson", State: "MD", PostalCode: "21204", Notes: "Side door"}},
		Items: []order.LineItem{
			{Name: "16\" Cheese Pizza", Quantity: 1, UnitPrice: 1599, LineTotal: 1798, Children: []order.LineItem{
				{Name: "Pepperoni", Quantity: 1, UnitPrice: 199}}},
			{Name: "10 pc Wings", Quantity: 1, UnitPrice: 1299, LineTotal: 1299, Notes: "Extra crispy"},
			{Name: "2 Liter Soda", Quantity: 1, UnitPrice: 399, LineTotal: 399},
		},
		Money: order.Money{Subtotal: 3496, Tax: 210, Total: 4006,
			Fees: []order.Fee{{Type: "delivery", Label: "Delivery fee", Amount: 300}}},
		Notes: "Sample ticket - not a real order",
	}
	out := map[string][]byte{
		"asis": firstCopy(t, BuildDeliveryCopies(s, sc, prefs, printer.Paper{Cols: cols})),
	}
	// drinks_real (the owner 2026-11-14): the drinks_* samples' order, stamped by the real rule.
	yes := true
	combo := func(kind, sauce, dressing, can string) order.LineItem {
		return order.LineItem{Name: "8 Wings & Fries", MenuItemID: sampleWings, Quantity: 1, UnitPrice: 1299, LineTotal: 1299,
			Children: []order.LineItem{{Name: kind, Quantity: 1}, {Name: sauce, Quantity: 1}, {Name: dressing, Quantity: 1},
				{Name: can, Quantity: 1, ShowOnDriverTicket: &yes}}}
	}
	drinks := s
	drinks.Items = terminalApp.StampDrinksWith(sampleDrinkBuild(t), prefs.DrinkRule(), []order.LineItem{
		combo("Regular", "Hot Sauce", "Ranch", "W Can Coke"),
		combo("Boneless", "BBQ", "Blue Cheese", "W Can Sprite"),
		{Name: "2Lt Pepsi", MenuItemID: sample2Lt, Quantity: 1, UnitPrice: 365, LineTotal: 365},
	})
	drinks.Money = order.Money{Subtotal: 2963, Tax: 178, Total: 3441,
		Fees: []order.Fee{{Type: "delivery", Label: "Delivery fee", Amount: 300}}}
	out["drinks_real"] = firstCopy(t, BuildDeliveryCopies(drinks, sc, prefs, printer.Paper{Cols: cols}))
	if !bytes.Contains(out["drinks_real"], []byte(order.DrinkMark)) {
		t.Fatal("drinks_real: no drink marks")
	}
	// online_note: a vendor delivery (no kitchen number in the sample: its vendor id) with an order note.
	online := s
	online.Source, online.SourceEventID, online.TakenBy = order.SourceOnline, "V-5521", ""
	online.Notes = "Please call when outside, the doorbell is broken"
	out["online_note"] = firstCopy(t, BuildDeliveryCopies(online, sc, prefs, printer.Paper{Cols: cols}))
	for name, b := range out {
		if err := os.WriteFile(filepath.Join(dir, name+".bin"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// The sample build: a Beverages category with the 2 liter and the wings with
// Lombardi's soda group (its cans and one of its sauces).
const (
	sampleWings = "00000000-0000-0000-0000-00000000aa01"
	sample2Lt   = "00000000-0000-0000-0000-00000000aa02"
	sampleSoda  = "00000000-0000-0000-0000-00000000aa03"
)

func sampleDrinkBuild(t *testing.T) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"categories": []any{
			map[string]any{"name": "Party Wings", "items": []any{map[string]any{"id": sampleWings, "name": "8 Wings & Fries",
				"modifier_group_ids": []string{sampleSoda}}}},
			map[string]any{"name": "Beverages", "items": []any{map[string]any{"id": sample2Lt, "name": "2Lt Pepsi"}}},
		},
		"modifier_groups": []any{map[string]any{"id": sampleSoda, "name": "Whole Wings Soda (Counter)", "options": []any{
			map[string]any{"name": "W Can Coke"}, map[string]any{"name": "W Can Sprite"}, map[string]any{"name": "Extra BBq Sauce"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// firstCopy is the slip up to and including its first paper cut (the Merchant Copy).
func firstCopy(t *testing.T, slip []byte) []byte {
	t.Helper()
	cut := bytes.Index(slip, []byte("\x1dV"))
	if cut < 0 {
		t.Fatal("no cut")
	}
	end := cut + 3
	if slip[cut+2] == 'A' || slip[cut+2] == 'B' {
		end = cut + 4
	}
	return slip[:end]
}
