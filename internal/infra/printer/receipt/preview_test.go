package receipt

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
	"quiccpos/agent/internal/domain/storeconfig"
)

// TestPreviewDump writes the exact printer bytes for a few realistic orders so
// receipt layout can be looked at without burning paper:
//
//	RECEIPT_PREVIEW_DIR=/tmp/rp go test ./internal/infra/printer/receipt -run TestPreviewDump
//	uv run --with pillow python scripts/escpos_preview.py /tmp/rp/*.bin
//
// Skipped unless RECEIPT_PREVIEW_DIR is set.
func TestPreviewDump(t *testing.T) {
	dir := os.Getenv("RECEIPT_PREVIEW_DIR")
	if dir == "" {
		t.Skip("set RECEIPT_PREVIEW_DIR to write preview .bin files")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sc := previewStore()
	delivery := previewDelivery()
	pickup := previewPickup()
	future := previewDelivery()
	promised := time.Date(2026, 10, 24, 19, 30, 0, 0, time.UTC)
	future.Fulfillment.PromisedAt = &promised
	k := int64(1124)
	future.KitchenNumber = &k

	// RECEIPT_PREVIEW_PREFS='{"kitchen_item_size":"xlarge",...}' previews
	// receipt settings (missing keys = defaults).
	prefs, err := order.DecodeReceiptPrefs([]byte(os.Getenv("RECEIPT_PREVIEW_PREFS")))
	if err != nil {
		t.Fatal(err)
	}
	build := func(snap order.Snapshot, tk printer.Ticket) []byte { return Build(snap, sc, tk, prefs) }
	write := func(name string, b []byte) {
		if err := os.WriteFile(filepath.Join(dir, name+".bin"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("1-receipt-delivery-card", build(delivery, printer.Ticket{Kind: printer.TicketReceipt}))
	write("2-receipt-pickup-cash", build(pickup, printer.Ticket{Kind: printer.TicketReceipt}))
	write("3-receipt-scheduled", build(future, printer.Ticket{Kind: printer.TicketReceipt}))
	write("4-station-pizza", build(delivery, printer.Ticket{
		Kind: printer.TicketStation, TargetID: "p-pizza", Station: "Pizza",
		Mine: delivery.Items[:1], Others: delivery.Items[1:],
	}))
	write("5-station-wings", build(delivery, printer.Ticket{
		Kind: printer.TicketStation, TargetID: "p-fry", Station: "Fry",
		Mine: delivery.Items[1:2], Others: []order.LineItem{delivery.Items[0], delivery.Items[2], delivery.Items[3]},
	}))
	write("6-station-cold", build(delivery, printer.Ticket{
		Kind: printer.TicketStation, TargetID: "p-cold", Station: "Cold",
		Mine: delivery.Items[2:], Others: delivery.Items[:2],
	}))
	write("7-station-rerouted", build(pickup, printer.Ticket{
		Kind: printer.TicketStation, TargetID: "p-pizza", Station: "Pizza", ReroutedFrom: "Pizza oven",
		Mine: pickup.Items, Others: nil,
	}))
	sched := future
	sched.Items = append([]order.LineItem{}, future.Items...)
	sched.Items = append(sched.Items, order.LineItem{Name: "Chef's Special Calzone", Quantity: 1, Routing: order.RoutingUnmatched,
		Notes: "no onions, customer is allergic"})
	write("8-station-scheduled-unmatched", build(sched, printer.Ticket{
		Kind: printer.TicketStation, TargetID: "p-pizza", Station: "Pizza",
		Mine: []order.LineItem{sched.Items[0], sched.Items[4]}, Others: sched.Items[1:4],
	}))
}

func previewStore() storeconfig.StoreContext {
	return storeconfig.StoreContext{
		Tenant: storeconfig.TenantContext{Name: "Lombardi's Pizza"},
		Location: storeconfig.LocationContext{
			Name: "Lombardi's Pizza", AddressLine1: "9 Main St", AddressLine2: "Baltimore, MD 21201",
			Timezone: "America/New_York",
		},
	}
}

func previewDelivery() order.Snapshot {
	k := int64(1123)
	pizza := order.LineItem{Name: "Pepperoni Pizza", Size: "Large", Quantity: 1, UnitPrice: 1899, LineTotal: 2199,
		Routing: order.RoutingMatched, PrinterID: "p-pizza",
		Children: []order.LineItem{{Name: "Extra cheese", Quantity: 1, UnitPrice: 200}, {Name: "Well done", Quantity: 1}, {Name: "Garlic crust", Quantity: 1, UnitPrice: 100}}}
	wings := order.LineItem{Name: "Buffalo Wings", Size: "10 pc", Quantity: 2, UnitPrice: 1299, LineTotal: 2598,
		Routing: order.RoutingMatched, PrinterID: "p-fry", Notes: "extra ranch please",
		Children: []order.LineItem{{Name: "Hot", Quantity: 1}}}
	salad := order.LineItem{Name: "Garden Salad", Quantity: 1, UnitPrice: 849, LineTotal: 849,
		Routing: order.RoutingMatched, PrinterID: "p-cold", Children: []order.LineItem{{Name: "Ranch dressing", Quantity: 1}}}
	soda := order.LineItem{Name: "Coca-Cola", Size: "2 Liter", Quantity: 1, UnitPrice: 349, LineTotal: 349, Routing: order.RoutingMatched, PrinterID: "p-cold"}
	return order.Snapshot{
		ID: "o-preview-1", SourceEventID: "934661791", KitchenNumber: &k,
		PlacedAt: time.Date(2026, 10, 21, 22, 41, 0, 0, time.UTC),
		Customer: order.Customer{DisplayName: "Maria Gonzalez", Phone: "4105551234"},
		Fulfillment: order.Fulfillment{Type: "delivery", Address: &order.Address{
			Line1: "1234 Harford Rd", Line2: "Apt 2B", City: "Baltimore", State: "MD", PostalCode: "21214"}},
		Notes: "Ring the bell, the dog barks but is friendly",
		Items: []order.LineItem{pizza, wings, salad, soda},
		Money: order.Money{
			Subtotal: 5995, Tax: 360, Tip: 600,
			Fees:      []order.Fee{{Type: "delivery", Label: "Delivery fee", Amount: 399}, {Type: "service", Label: "Service fee", Amount: 150}},
			Discounts: []order.Discount{{Type: "coupon", Label: "10% off (WELCOME10)", Amount: 600}},
			Total:     6904,
		},
		Payments: []order.PaymentSummary{{Method: "card", AmountCents: 6904, Last4: "4242", AuthCode: "A1B2C3"}},
	}
}

func previewPickup() order.Snapshot {
	k := int64(1101)
	return order.Snapshot{
		ID: "o-preview-2", SourceEventID: "934661802", KitchenNumber: &k,
		PlacedAt:    time.Date(2026, 10, 21, 23, 2, 0, 0, time.UTC),
		Customer:    order.Customer{DisplayName: "Sam Lee", Phone: "4435550199"},
		Fulfillment: order.Fulfillment{Type: "pickup"},
		Items: []order.LineItem{
			{Name: "Cheese Pizza", Size: "Medium", Quantity: 1, UnitPrice: 1299, LineTotal: 1299},
			{Name: "Garlic Knots", Size: "6 pc", Quantity: 2, UnitPrice: 499, LineTotal: 998},
		},
		Money: order.Money{Subtotal: 2297, Tax: 138, Total: 2435},
	}
}
