package receipt

import (
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
	"quiccpos/agent/internal/domain/storeconfig"
)

// BuildSample renders what a real order looks like with the given receipt
// settings — the customer receipt (every copy) then a kitchen ticket — for the
// tablet's PRINT SAMPLE. The order is made up (kitchen #1234, "Sample
// Customer") and says so in its notes.
func BuildSample(sc storeconfig.StoreContext, prefs order.ReceiptPrefs, at time.Time) []byte {
	snap := SampleOrder(at)
	out := Build(snap, sc, printer.Ticket{Kind: printer.TicketReceipt}, prefs)
	return append(out, Build(snap, sc, printer.Ticket{
		Kind: printer.TicketStation, TargetID: samplePrinter, Station: "Sample",
		Mine: snap.Items[:2], Others: snap.Items[2:],
	}, prefs)...)
}

const samplePrinter = "sample"

// SampleOrder is the made-up delivery BuildSample prints.
func SampleOrder(at time.Time) order.Snapshot {
	k := int64(1234)
	pizza := order.LineItem{Name: "Pepperoni Pizza", Size: "Large", Quantity: 1, UnitPrice: 1899, LineTotal: 2099,
		Routing: order.RoutingMatched, PrinterID: samplePrinter,
		Children: []order.LineItem{{Name: "Extra cheese", Quantity: 1, UnitPrice: 200}, {Name: "Well done", Quantity: 1}}}
	wings := order.LineItem{Name: "Buffalo Wings", Size: "10 pc", Quantity: 2, UnitPrice: 1299, LineTotal: 2598,
		Routing: order.RoutingMatched, PrinterID: samplePrinter, Notes: "extra ranch please",
		Children: []order.LineItem{{Name: "Hot", Quantity: 1}}}
	soda := order.LineItem{Name: "Coca-Cola", Size: "2 Liter", Quantity: 1, UnitPrice: 349, LineTotal: 349,
		Routing: order.RoutingMatched, PrinterID: "sample-other"}
	return order.Snapshot{
		ID: "00000000-0000-0000-0000-000000001234", SourceEventID: "SAMPLE", KitchenNumber: &k,
		PlacedAt: at,
		Customer: order.Customer{DisplayName: "Sample Customer", Phone: "4105550100"},
		Fulfillment: order.Fulfillment{Type: "delivery", Address: &order.Address{
			Line1: "123 Sample St", Line2: "Apt 4", City: "Baltimore", State: "MD", PostalCode: "21201"}},
		Notes: "SAMPLE - not a real order",
		Items: []order.LineItem{pizza, wings, soda},
		Money: order.Money{
			Subtotal: 5046, Tax: 303, Tip: 500,
			Fees:  []order.Fee{{Type: "delivery", Label: "Delivery fee", Amount: 399}},
			Total: 6248,
		},
		Payments: []order.PaymentSummary{{Method: "card", AmountCents: 6248, Last4: "4242", AuthCode: "SAMPLE"}},
	}
}
