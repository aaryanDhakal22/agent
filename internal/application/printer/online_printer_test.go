package printer

import (
	"fmt"
	"testing"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
)

// The Online printer prints only deliveries (agent 2.5.7, the owner
// 2026-11-13): Lombardi's receipt printer ("Online") gets, of an order's own
// paper, only a delivery's tickets, whatever the source: online vendor,
// website, call center, a terminal. A pickup / takeout / counter order's
// customer receipt prints at its Cash Out on the terminal's own printer.
// Every automatic print mode the Pi uses is checked for every source and
// type, cash and paid, with the switches on and off: the only tickets the
// receipt printer may get are a delivery's. Kitchen tickets are untouched
// (the stations still print). Reprint and a future order's PRINT NOW are a
// person's ask and are checked apart.
func TestOnlinePrinterPrintsOnlyDeliveries(t *testing.T) {
	online := RosterEntry{ID: "p-online", Name: "Online", IsReceipt: true}
	roster := []RosterEntry{{ID: "p-desi", Name: "Desi"}, online, pizza, {ID: "p-sub", Name: "Sub"}, wings}
	items := []order.LineItem{item("Chicken Tender", order.RoutingMatched, wings.ID), item("Cheese Pizza", order.RoutingMatched, pizza.ID)}
	card := []order.PaymentSummary{{Method: "card", AmountCents: 2500}}
	// Where each source's paper comes from (the app layer): main's orders are
	// placed on arrival (ModePlaced), a terminal's and the call center's SEND
	// print the counter print (ModePlaced since 2.5.7), a ringing order
	// accepted on the tablet / MAKE NOW (ModeAccept, ModeAcceptKitchen), a
	// future order's early print (ModeFutureEarly), the old call-center
	// receipt (ModeOrderReceipt), a delivery's UPDATED pair.
	auto := []printer.Mode{printer.ModePlaced, printer.ModeAccept, printer.ModeAcceptKitchen, printer.ModeFutureEarly,
		printer.ModeOrderReceipt, printer.ModeDeliveryUpdate, printer.ModeKitchen}
	rules := []Rules{DefaultRules(), {}, {Tickets: order.TicketToggles{FutureKitchen: true, FutureReceipt: true}}}
	for _, src := range []struct{ name, source, device string }{
		{"online", order.SourceOnline, ""}, {"website", "web", ""}, {"call center", order.SourceCounter, "callcenter"},
		{"counter", order.SourceCounter, ""},
	} {
		for _, typ := range []string{"pickup", "delivery", "dine_in"} {
			if typ == "dine_in" && src.source != order.SourceCounter {
				continue
			}
			for _, pays := range [][]order.PaymentSummary{nil, card} {
				snap := order.Snapshot{Source: src.source, DeviceKind: src.device, Fulfillment: order.Fulfillment{Type: typ},
					Money: order.Money{Total: 2500}, Payments: pays, Items: items}
				for _, mode := range auto {
					for ri, r := range rules {
						tk, _ := PlanWith(snap, roster, mode, r)
						name := fmt.Sprintf("%s %s paid=%v %s rules#%d", src.name, typ, pays != nil, mode, ri)
						onOnline := 0
						for _, x := range tk {
							if x.TargetID != online.ID {
								continue
							}
							onOnline++
							if typ != "delivery" {
								t.Errorf("%s: %s on the Online printer (only deliveries may print there)", name, x.Kind)
							} else if x.Kind == printer.TicketStation {
								t.Errorf("%s: a station ticket on the Online printer", name)
							}
						}
						// A delivery placed (main's, a terminal's, the call center's) gets its tickets there.
						if typ == "delivery" && mode == printer.ModePlaced && onOnline != 1 {
							t.Errorf("%s: the delivery ticket on the Online printer: %d", name, onOnline)
						}
						// The kitchen keeps its tickets.
						if mode == printer.ModePlaced || mode == printer.ModeKitchen {
							if got := kinds(tk); len(got) < 2 || got[0] != "station:p-pizza" || got[1] != "station:p-wings" {
								t.Errorf("%s: the station tickets: %v", name, got)
							}
						}
					}
				}
			}
		}
	}
	// A person's ask still reaches it: Reprint, a future pickup's PRINT NOW receipt.
	pick := order.Snapshot{Source: order.SourceCounter, Fulfillment: order.Fulfillment{Type: "pickup"}, Items: items}
	if tk, _ := PlanFor(pick, roster, printer.ModeReceipt); len(tk) != 1 || tk[0].TargetID != online.ID || !tk[0].Reprint {
		t.Errorf("a reprint of a pickup: %v", kinds(tk))
	}
	if tk, _ := PlanFor(pick, roster, printer.ModeFutureReceipt); len(tk) != 1 || tk[0].TargetID != online.ID || !tk[0].Future {
		t.Errorf("PRINT NOW receipt of a future pickup: %v", kinds(tk))
	}
}
