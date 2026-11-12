package receipt

import (
	"strings"
	"testing"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
)

// An order the call center placed (contract §2.10ab) prints who took it on
// the kitchen tickets, the Pi's customer receipt and the delivery ticket;
// every other order prints as before (the goldens pin that).
func TestTakenByPrints(t *testing.T) {
	const by = "Call center - Elena V."
	pickup := counterOrder()
	pickup.Fulfillment.Type = "pickup"
	delivery := deliverySnap(true)
	sc := deliveryStoreCtx()
	prefs := order.DefaultReceiptPrefs()
	slips := func(takenBy string) map[string]string {
		p, d := pickup, delivery
		p.TakenBy, d.TakenBy = takenBy, takenBy
		return map[string]string{
			"kitchen ticket":      textOf(BuildStation(p, pizzaTicket(p))),
			"kitchen ticket 32":   textOf(Build(p, sc, func() printer.Ticket { t := pizzaTicket(p); t.Cols = 32; return t }(), prefs)),
			"change ticket":       textOf(Build(p, sc, printer.Ticket{Kind: printer.TicketStation, Station: "Pizza", Change: true, Mine: p.Items[:1]}, prefs)),
			"customer receipt":    textOf(Build(p, sc, printer.Ticket{Kind: printer.TicketReceipt, Station: "Front"}, prefs)),
			"customer receipt 32": textOf(Build(p, sc, printer.Ticket{Kind: printer.TicketReceipt, Station: "Front", Cols: 32}, prefs)),
			"delivery ticket":     textOf(Build(d, sc, printer.Ticket{Kind: printer.TicketDeliveryCopies, Station: "Front"}, prefs)),
			"delivery ticket 32":  textOf(Build(d, sc, printer.Ticket{Kind: printer.TicketDeliveryCopies, Station: "Front", Cols: 32}, prefs)),
			"delivery's receipt":  textOf(Build(d, sc, printer.Ticket{Kind: printer.TicketReceipt, Station: "Front"}, prefs)),
			"rerouted kitchen":    textOf(Build(p, sc, func() printer.Ticket { t := pizzaTicket(p); t.ReroutedFrom = "Pizza"; return t }(), prefs)),
			"legacy full receipt": textOf(Build(p, sc, printer.Ticket{Kind: printer.TicketFull, Station: "Front"}, prefs)),
		}
	}
	for name, text := range slips(by) {
		want := 1
		if strings.HasPrefix(name, "delivery ticket") {
			want = 2 // the Merchant Copy and the Customer Copy
		}
		if n := strings.Count(text, by); n != want {
			t.Errorf("%s: %q printed %d times, want %d\n%s", name, by, n, want, text)
		}
	}
	for name, text := range slips("") {
		if strings.Contains(text, "Call center") {
			t.Errorf("%s of a store order names the call center", name)
		}
	}
}

// A call-center delivery outside the store's area (acknowledged) says so on
// both delivery copies; nothing else prints the line.
func TestOutOfAreaPrintsOnTheDeliveryCopies(t *testing.T) {
	sc, prefs := deliveryStoreCtx(), order.DefaultReceiptPrefs()
	d := deliverySnap(true)
	copies := printer.Ticket{Kind: printer.TicketDeliveryCopies, Station: "Front"}
	if text := textOf(Build(d, sc, copies, prefs)); strings.Contains(text, "OUT OF AREA") {
		t.Fatal("an order in the area prints no OUT OF AREA")
	}
	d.OutOfArea, d.TakenBy = true, "Call center - Elena V."
	for _, cols := range []int{0, 42, 32} {
		copies.Cols = cols
		if text := textOf(Build(d, sc, copies, prefs)); strings.Count(text, "OUT OF AREA") != 2 {
			t.Errorf("%d columns: OUT OF AREA printed %d times, want on both copies\n%s", cols, strings.Count(text, "OUT OF AREA"), text)
		}
	}
}
