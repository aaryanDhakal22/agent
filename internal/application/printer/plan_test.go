package printer

import (
	"reflect"
	"testing"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
)

var (
	front = RosterEntry{ID: "p-front", Name: "Front", IsReceipt: true}
	pizza = RosterEntry{ID: "p-pizza", Name: "Pizza"}
	wings = RosterEntry{ID: "p-wings", Name: "Wings"}
)

func item(name, routing, printerID string) order.LineItem {
	return order.LineItem{Name: name, Quantity: 1, Routing: routing, PrinterID: printerID}
}

func names(items []order.LineItem) []string {
	out := []string{}
	for _, it := range items {
		out = append(out, it.Name)
	}
	return out
}

func TestPlan_EmptyRoster(t *testing.T) {
	tk, un := Plan(order.Snapshot{Items: []order.LineItem{item("A", "", "")}}, nil)
	if tk != nil || un != nil {
		t.Fatalf("want nothing, got %v %v", tk, un)
	}
}

func TestPlan_LegacyOrderPrintsOneFullReceipt(t *testing.T) {
	snap := order.Snapshot{Items: []order.LineItem{item("A", "", ""), item("B", "", "")}}
	tk, un := Plan(snap, []RosterEntry{pizza, front, wings})
	if len(tk) != 1 || tk[0].Kind != printer.TicketFull || tk[0].TargetID != front.ID || un != nil {
		t.Fatalf("legacy order: got %+v unrouted=%v", tk, un)
	}
}

func TestPlan_NoReceiptFlagUsesFirstPrinter(t *testing.T) {
	p, w := pizza, wings
	tk, _ := Plan(order.Snapshot{Items: []order.LineItem{item("A", "", "")}}, []RosterEntry{p, w})
	if tk[0].TargetID != p.ID {
		t.Fatalf("want first printer as receipt, got %s", tk[0].TargetID)
	}
}

func TestPlan_StationsPlusReceipt(t *testing.T) {
	snap := order.Snapshot{Items: []order.LineItem{
		item("Pepperoni", order.RoutingMatched, pizza.ID),
		item("Hot Wings", order.RoutingMatched, wings.ID),
		item("Cheese", order.RoutingMatched, pizza.ID),
	}}
	tk, un := Plan(snap, []RosterEntry{front, pizza, wings})
	if un != nil {
		t.Fatalf("unexpected unrouted %v", un)
	}
	if len(tk) != 3 {
		t.Fatalf("want 2 stations + receipt, got %d: %+v", len(tk), tk)
	}
	if tk[0].TargetID != pizza.ID || !reflect.DeepEqual(names(tk[0].Mine), []string{"Pepperoni", "Cheese"}) ||
		!reflect.DeepEqual(names(tk[0].Others), []string{"Hot Wings"}) {
		t.Fatalf("pizza ticket wrong: %+v", tk[0])
	}
	if tk[1].TargetID != wings.ID || !reflect.DeepEqual(names(tk[1].Mine), []string{"Hot Wings"}) ||
		!reflect.DeepEqual(names(tk[1].Others), []string{"Pepperoni", "Cheese"}) {
		t.Fatalf("wings ticket wrong: %+v", tk[1])
	}
	if tk[2].Kind != printer.TicketReceipt || tk[2].TargetID != front.ID {
		t.Fatalf("last ticket should be the receipt on Front: %+v", tk[2])
	}
}

func TestPlan_UnroutedGoesToEveryStation(t *testing.T) {
	snap := order.Snapshot{Items: []order.LineItem{
		item("Pepperoni", order.RoutingMatched, pizza.ID),
		item("Mystery", order.RoutingUnmatched, ""),
		item("Salad", order.RoutingUnassigned, ""),
		item("Ghost", order.RoutingMatched, "p-deleted"), // printer this agent doesn't have
	}}
	tk, un := Plan(snap, []RosterEntry{front, pizza, wings})
	if !reflect.DeepEqual(un, []string{"Mystery", "Salad", "Ghost"}) {
		t.Fatalf("unrouted = %v", un)
	}
	if len(tk) != 3 {
		t.Fatalf("want pizza + wings + receipt, got %+v", tk)
	}
	if !reflect.DeepEqual(names(tk[0].Mine), []string{"Pepperoni", "Mystery", "Salad", "Ghost"}) {
		t.Fatalf("pizza mine = %v", names(tk[0].Mine))
	}
	if !reflect.DeepEqual(names(tk[1].Mine), []string{"Mystery", "Salad", "Ghost"}) ||
		!reflect.DeepEqual(names(tk[1].Others), []string{"Pepperoni"}) {
		t.Fatalf("wings ticket = %+v", tk[1])
	}
}

func TestPlan_ReceiptOnlyRosterIsAlsoTheStation(t *testing.T) {
	snap := order.Snapshot{Items: []order.LineItem{item("Mystery", order.RoutingUnmatched, "")}}
	tk, _ := Plan(snap, []RosterEntry{front})
	if len(tk) != 2 || tk[0].Kind != printer.TicketStation || tk[0].TargetID != front.ID || tk[1].Kind != printer.TicketReceipt {
		t.Fatalf("single printer: want station + receipt on it, got %+v", tk)
	}
}

func TestPlan_ItemRoutedToReceiptPrinterIsReceiptOnly(t *testing.T) {
	snap := order.Snapshot{Items: []order.LineItem{
		item("Soda", order.RoutingMatched, front.ID),
		item("Pepperoni", order.RoutingMatched, pizza.ID),
	}}
	tk, un := Plan(snap, []RosterEntry{front, pizza})
	if len(un) != 0 || len(tk) != 2 {
		t.Fatalf("want pizza station + receipt only, got %+v (unrouted %v)", tk, un)
	}
	if tk[0].Kind != printer.TicketStation || tk[0].TargetID != pizza.ID ||
		!reflect.DeepEqual(names(tk[0].Mine), []string{"Pepperoni"}) ||
		len(tk[0].Others) != 0 { // 2.4.0: the soda isn't listed for the cooks
		t.Fatalf("pizza ticket = %+v", tk[0])
	}
	if tk[1].Kind != printer.TicketReceipt || tk[1].TargetID != front.ID {
		t.Fatalf("receipt = %+v", tk[1])
	}
}

func TestPlan_OnlyReceiptOnlyItemsPrintJustTheReceipt(t *testing.T) {
	snap := order.Snapshot{Items: []order.LineItem{
		item("Soda", order.RoutingMatched, front.ID),
		item("Brownie", order.RoutingMatched, front.ID),
	}}
	tk, _ := Plan(snap, []RosterEntry{front, pizza, wings})
	if len(tk) != 1 || tk[0].Kind != printer.TicketReceipt || tk[0].TargetID != front.ID {
		t.Fatalf("want just the receipt, got %+v", tk)
	}
}

func TestPlan_SolePrinterStillGetsStationTicketForItsItems(t *testing.T) {
	snap := order.Snapshot{Items: []order.LineItem{item("Soda", order.RoutingMatched, front.ID)}}
	tk, _ := Plan(snap, []RosterEntry{front})
	if len(tk) != 2 || tk[0].Kind != printer.TicketStation || tk[0].TargetID != front.ID || tk[1].Kind != printer.TicketReceipt {
		t.Fatalf("single printer is the kitchen too: want station + receipt, got %+v", tk)
	}
}

func TestPlan_PickupCashPrintsLikeAnyOrder(t *testing.T) {
	items := []order.LineItem{item("Pepperoni", order.RoutingMatched, pizza.ID), item("Wings", order.RoutingMatched, wings.ID)}
	roster := []RosterEntry{front, pizza, wings}
	snap := func(kind string, pays ...order.PaymentSummary) order.Snapshot {
		return order.Snapshot{Items: items, Fulfillment: order.Fulfillment{Type: kind}, Money: order.Money{Total: 2500}, Payments: pays}
	}
	// 2.5.7 (the owner, 2026-11-13): a pickup prints its station tickets
	// only, cash or card, whatever the switches say; a delivery its station
	// tickets + the receipt.
	for _, s := range []order.Snapshot{snap("pickup"), snap("Pickup", order.PaymentSummary{Method: "cash", AmountCents: 2500}),
		snap("pickup", order.PaymentSummary{Method: "card", AmountCents: 2500})} {
		if tk, _ := Plan(s, roster); !reflect.DeepEqual(kinds(tk), []string{"station:p-pizza", "station:p-wings"}) {
			t.Fatalf("%s: want the 2 stations only, got %v", s.Fulfillment.Type, kinds(tk))
		}
	}
	if tk, _ := Plan(snap("delivery"), roster); len(tk) != 3 {
		t.Fatalf("delivery: want 2 stations + receipt, got %v", kinds(tk))
	}
	off := Rules{Tickets: order.DefaultTicketToggles()}
	off.Tickets.OnlineCashPickup = false
	if tk, _ := PlanWith(snap("pickup"), roster, printer.ModeAccept, off); !reflect.DeepEqual(kinds(tk), []string{"station:p-pizza", "station:p-wings"}) {
		t.Fatalf("pickup cash, switch off: still the stations only, got %v", kinds(tk))
	}
	// A single-printer store: the receipt printer is the kitchen, so the
	// station ticket prints there, never the customer receipt.
	if tk, _ := PlanWith(snap("pickup"), []RosterEntry{front}, printer.ModeAccept, off); !reflect.DeepEqual(kinds(tk), []string{"station:p-front"}) {
		t.Fatalf("single printer pickup cash: %v", kinds(tk))
	}
}

func kinds(tk []printer.Ticket) []string {
	out := []string{}
	for _, t := range tk {
		out = append(out, string(t.Kind)+":"+t.TargetID)
	}
	return out
}

func TestPlanReceipt_OnlyTheCustomerReceipt(t *testing.T) {
	roster := []RosterEntry{pizza, front, wings}
	routed := order.Snapshot{Fulfillment: order.Fulfillment{Type: "delivery"}, Items: []order.LineItem{
		item("Pepperoni", order.RoutingMatched, pizza.ID),
		item("Mystery", order.RoutingUnmatched, ""),
	}}
	tk, un := PlanFor(routed, roster, printer.ModeReceipt)
	if un != nil || !reflect.DeepEqual(kinds(tk), []string{"receipt:p-front"}) {
		t.Fatalf("routed reprint: %v unrouted=%v", kinds(tk), un)
	}
	// Pickup cash: the same single receipt.
	pc := routed
	pc.Fulfillment.Type = "pickup"
	if tk, _ := PlanFor(pc, roster, printer.ModeReceipt); !reflect.DeepEqual(kinds(tk), []string{"receipt:p-front"}) {
		t.Fatalf("pickup cash reprint: %v", kinds(tk))
	}
	// Legacy (unrouted) order: its one full receipt.
	legacy := order.Snapshot{Items: []order.LineItem{item("Old", "", "")}}
	if tk, _ := PlanFor(legacy, roster, printer.ModeReceipt); !reflect.DeepEqual(kinds(tk), []string{"full:p-front"}) {
		t.Fatalf("legacy reprint: %v", kinds(tk))
	}
	// Single printer: still just the receipt.
	if tk, _ := PlanFor(routed, []RosterEntry{front}, printer.ModeReceipt); !reflect.DeepEqual(kinds(tk), []string{"receipt:p-front"}) {
		t.Fatalf("single printer reprint: %v", kinds(tk))
	}
	if tk, _ := PlanFor(routed, nil, printer.ModeReceipt); tk != nil {
		t.Fatalf("empty roster: %v", tk)
	}
}

func TestPlanKitchen_StationsWithoutTheReceipt(t *testing.T) {
	snap := order.Snapshot{Fulfillment: order.Fulfillment{Type: "delivery"}, Items: []order.LineItem{
		item("Pepperoni", order.RoutingMatched, pizza.ID),
		item("Hot Wings", order.RoutingMatched, wings.ID),
	}}
	tk, un := PlanFor(snap, []RosterEntry{front, pizza, wings}, printer.ModeKitchen)
	if un != nil || !reflect.DeepEqual(kinds(tk), []string{"station:p-pizza", "station:p-wings"}) {
		t.Fatalf("kitchen: %v unrouted=%v", kinds(tk), un)
	}
	if !reflect.DeepEqual(names(tk[0].Mine), []string{"Pepperoni"}) || !reflect.DeepEqual(names(tk[0].Others), []string{"Hot Wings"}) {
		t.Fatalf("pizza ticket = %+v", tk[0])
	}
}

func TestPlanKitchen_PickupCashStillPrintsStations(t *testing.T) {
	snap := order.Snapshot{Fulfillment: order.Fulfillment{Type: "pickup"}, Money: order.Money{Total: 2500},
		Items: []order.LineItem{item("Pepperoni", order.RoutingMatched, pizza.ID), item("Hot Wings", order.RoutingMatched, wings.ID)}}
	roster := []RosterEntry{front, pizza, wings}
	off := Rules{Tickets: order.DefaultTicketToggles()}
	off.Tickets.OnlineCashPickup = false
	if tk, _ := PlanWith(snap, roster, printer.ModeAccept, off); !reflect.DeepEqual(kinds(tk), []string{"station:p-pizza", "station:p-wings"}) {
		t.Fatalf("accept of pickup cash with its switch off: the stations only (2.5.7): %v", kinds(tk))
	}
	if tk, _ := PlanFor(snap, roster, printer.ModeKitchen); !reflect.DeepEqual(kinds(tk), []string{"station:p-pizza", "station:p-wings"}) {
		t.Fatalf("kitchen resend of pickup cash: %v", kinds(tk))
	}
}

func TestPlanKitchen_ReceiptOnlyItemsExcluded(t *testing.T) {
	roster := []RosterEntry{front, pizza}
	snap := order.Snapshot{Items: []order.LineItem{
		item("Soda", order.RoutingMatched, front.ID),
		item("Pepperoni", order.RoutingMatched, pizza.ID),
	}}
	tk, _ := PlanFor(snap, roster, printer.ModeKitchen)
	if !reflect.DeepEqual(kinds(tk), []string{"station:p-pizza"}) ||
		!reflect.DeepEqual(names(tk[0].Mine), []string{"Pepperoni"}) || len(tk[0].Others) != 0 {
		t.Fatalf("kitchen: %+v", tk)
	}
	// Only receipt-only items: nothing for the kitchen.
	only := order.Snapshot{Items: []order.LineItem{item("Soda", order.RoutingMatched, front.ID)}}
	if tk, un := PlanFor(only, roster, printer.ModeKitchen); len(tk) != 0 || un != nil {
		t.Fatalf("receipt-only order: want no tickets, got %v %v", kinds(tk), un)
	}
}

func TestPlanKitchen_SinglePrinterIsTheStation(t *testing.T) {
	snap := order.Snapshot{Fulfillment: order.Fulfillment{Type: "pickup"}, Items: []order.LineItem{
		item("Soda", order.RoutingMatched, front.ID),
		item("Mystery", order.RoutingUnmatched, ""),
	}}
	tk, _ := PlanFor(snap, []RosterEntry{front}, printer.ModeKitchen)
	if !reflect.DeepEqual(kinds(tk), []string{"station:p-front"}) || !reflect.DeepEqual(names(tk[0].Mine), []string{"Soda", "Mystery"}) {
		t.Fatalf("single printer kitchen: %+v", tk)
	}
}

func TestPlanKitchen_UnroutedGoesToEveryStation(t *testing.T) {
	snap := order.Snapshot{Items: []order.LineItem{
		item("Pepperoni", order.RoutingMatched, pizza.ID),
		item("Mystery", order.RoutingUnmatched, ""),
	}}
	tk, un := PlanFor(snap, []RosterEntry{front, pizza, wings}, printer.ModeKitchen)
	if !reflect.DeepEqual(un, []string{"Mystery"}) || !reflect.DeepEqual(kinds(tk), []string{"station:p-pizza", "station:p-wings"}) {
		t.Fatalf("kitchen: %v unrouted=%v", kinds(tk), un)
	}
	if !reflect.DeepEqual(names(tk[0].Mine), []string{"Pepperoni", "Mystery"}) || !reflect.DeepEqual(names(tk[1].Mine), []string{"Mystery"}) {
		t.Fatalf("mine: pizza=%v wings=%v", names(tk[0].Mine), names(tk[1].Mine))
	}
	// A legacy order (no routing at all): every item to every station.
	legacy := order.Snapshot{Items: []order.LineItem{item("Old", "", "")}}
	if tk, _ := PlanFor(legacy, []RosterEntry{front, pizza, wings}, printer.ModeKitchen); !reflect.DeepEqual(kinds(tk), []string{"station:p-pizza", "station:p-wings"}) {
		t.Fatalf("legacy kitchen: %v", kinds(tk))
	}
}

// TestPlanVoid (00019): station tickets for the voided lines only, flagged
// Void, no receipt, no "other printers" list.
func TestPlanVoid(t *testing.T) {
	snap := order.Snapshot{Source: order.SourceCounter, Fulfillment: order.Fulfillment{Type: "pickup"},
		Items: []order.LineItem{item("Garlic Knots", order.RoutingMatched, "p-pizza"), item("Wings", order.RoutingMatched, "p-wings")}}
	tickets, _ := PlanWith(snap, []RosterEntry{front, pizza, wings}, printer.ModeVoid, DefaultRules())
	if len(tickets) != 2 {
		t.Fatalf("tickets = %+v", tickets)
	}
	for _, tk := range tickets {
		if !tk.Void || tk.Kind != printer.TicketStation || len(tk.Others) != 0 || len(tk.Mine) != 1 {
			t.Fatalf("ticket = %+v", tk)
		}
	}
}

// 2.4.0 (owner): "items to other printers" lists only food another kitchen
// station makes - the cans, desserts and chips that go to the receipt
// printer alone are left off; an unrouted item (sent to every station)
// still shows on the others.
func TestPlan_OthersListOnlyKitchenFood(t *testing.T) {
	roster := []RosterEntry{front, pizza, wings}
	snap := order.Snapshot{Items: []order.LineItem{
		item("Pepperoni", order.RoutingMatched, pizza.ID),
		item("Buffalo Wings", order.RoutingMatched, wings.ID),
		item("Can Coke", order.RoutingMatched, front.ID),
		item("Cannoli", order.RoutingMatched, front.ID),
	}}
	tk, _ := PlanFor(snap, roster, printer.ModeKitchen)
	if !reflect.DeepEqual(kinds(tk), []string{"station:p-pizza", "station:p-wings"}) {
		t.Fatalf("got %v", kinds(tk))
	}
	if !reflect.DeepEqual(names(tk[0].Others), []string{"Buffalo Wings"}) || !reflect.DeepEqual(names(tk[1].Others), []string{"Pepperoni"}) {
		t.Fatalf("others = %v / %v", names(tk[0].Others), names(tk[1].Others))
	}
	snap.Items = append(snap.Items, item("Mystery", order.RoutingUnmatched, ""))
	tk, _ = PlanFor(snap, roster, printer.ModeKitchen)
	if !reflect.DeepEqual(names(tk[0].Others), []string{"Buffalo Wings"}) || !reflect.DeepEqual(names(tk[0].Mine), []string{"Pepperoni", "Mystery"}) {
		t.Fatalf("unrouted: mine %v others %v", names(tk[0].Mine), names(tk[0].Others))
	}
}

// 2.4.0: a future order's early tickets follow the two switches, every one
// marked Future; the CONFIRM after an early receipt prints kitchen only.
func TestPlan_FutureEarlyAndAcceptKitchen(t *testing.T) {
	roster := []RosterEntry{front, pizza}
	snap := order.Snapshot{Items: []order.LineItem{item("Pepperoni", order.RoutingMatched, pizza.ID), item("Soda", order.RoutingMatched, front.ID)}}
	if tk, _ := PlanWith(snap, roster, printer.ModeFutureEarly, Rules{}); len(tk) != 0 {
		t.Fatalf("switches off: %v", kinds(tk))
	}
	tk, _ := PlanWith(snap, roster, printer.ModeFutureEarly, Rules{Tickets: order.TicketToggles{FutureKitchen: true, FutureReceipt: true}})
	if !reflect.DeepEqual(kinds(tk), []string{"station:p-pizza", "receipt:p-front"}) || !tk[0].Future || !tk[1].Future {
		t.Fatalf("both: %+v", tk)
	}
	tk, _ = PlanWith(snap, roster, printer.ModeFutureEarly, Rules{Tickets: order.TicketToggles{FutureReceipt: true}})
	if !reflect.DeepEqual(kinds(tk), []string{"receipt:p-front"}) {
		t.Fatalf("receipt only: %v", kinds(tk))
	}
	tk, _ = PlanWith(snap, roster, printer.ModeAcceptKitchen, Rules{Tickets: order.DefaultTicketToggles()})
	if !reflect.DeepEqual(kinds(tk), []string{"station:p-pizza"}) {
		t.Fatalf("accept kitchen: %v", kinds(tk))
	}
}

// 2.4.3 PRINT NOW: the early tickets staff picked, whatever the switches.
func TestPlan_FuturePrintNow(t *testing.T) {
	roster := []RosterEntry{front, pizza}
	snap := order.Snapshot{Items: []order.LineItem{item("Pepperoni", order.RoutingMatched, pizza.ID), item("Soda", order.RoutingMatched, front.ID)}}
	for mode, want := range map[printer.Mode][]string{
		printer.ModeFutureKitchen: {"station:p-pizza"},
		printer.ModeFutureReceipt: {"receipt:p-front"},
		printer.ModeFutureBoth:    {"station:p-pizza", "receipt:p-front"},
	} {
		tk, _ := PlanWith(snap, roster, mode, Rules{})
		if !reflect.DeepEqual(kinds(tk), want) {
			t.Fatalf("%s: %v", mode, kinds(tk))
		}
		for _, x := range tk {
			if !x.Future {
				t.Fatalf("%s: not marked future", mode)
			}
		}
	}
}

func TestPlanAddOn(t *testing.T) {
	snap := order.Snapshot{Source: order.SourceCounter, Fulfillment: order.Fulfillment{Type: "dine_in"},
		Items: []order.LineItem{item("Garlic Knots", order.RoutingMatched, "p-pizza")}}
	tickets, _ := PlanWith(snap, []RosterEntry{front, pizza, wings}, printer.ModeAddOn, DefaultRules())
	if len(tickets) != 1 || !tickets[0].AddOn || tickets[0].Void || tickets[0].Kind != printer.TicketStation ||
		tickets[0].TargetID != "p-pizza" || len(tickets[0].Others) != 0 {
		t.Fatalf("tickets = %+v", tickets)
	}
}

// ModeChange: added (Items) and removed (RemovedItems) lines merged into one
// change ticket per station, roster order; a station with only removals
// still gets one; nothing else on them.
func TestPlanChange(t *testing.T) {
	snap := order.Snapshot{Source: order.SourceCounter, Fulfillment: order.Fulfillment{Type: "dine_in"},
		Items:        []order.LineItem{item("Cheese Pizza", order.RoutingMatched, "p-pizza")},
		RemovedItems: []order.LineItem{item("Garlic Knots", order.RoutingMatched, "p-pizza"), item("Hot Wings", order.RoutingMatched, "p-wings")}}
	tickets, unrouted := PlanWith(snap, []RosterEntry{front, pizza, wings}, printer.ModeChange, DefaultRules())
	if len(tickets) != 2 || len(unrouted) != 0 {
		t.Fatalf("tickets = %+v unrouted %v", tickets, unrouted)
	}
	p, w := tickets[0], tickets[1]
	if p.TargetID != "p-pizza" || !p.Change || len(p.Mine) != 1 || p.Mine[0].Name != "Cheese Pizza" || len(p.Removed) != 1 ||
		p.Removed[0].Name != "Garlic Knots" || len(p.Others) != 0 || p.Kind != printer.TicketStation {
		t.Fatalf("pizza: %+v", p)
	}
	if w.TargetID != "p-wings" || !w.Change || len(w.Mine) != 0 || len(w.Removed) != 1 || w.Removed[0].Name != "Hot Wings" {
		t.Fatalf("wings: %+v", w)
	}
	if tk, _ := PlanWith(order.Snapshot{Source: order.SourceCounter}, []RosterEntry{front, pizza}, printer.ModeChange, DefaultRules()); len(tk) != 0 {
		t.Fatalf("nothing changed: %+v", tk)
	}
}

// A type change with no line changed: every station with lines of the order, the station's lines,
// TypeOnly + WasType; a change with lines carries WasType too.
func TestPlanTypeChange(t *testing.T) {
	snap := order.Snapshot{Source: order.SourceCounter, Fulfillment: order.Fulfillment{Type: "delivery"}, TypeChangedFrom: "pickup",
		Items: []order.LineItem{item("Cheese Pizza", order.RoutingMatched, "p-pizza"), item("Hot Wings", order.RoutingMatched, "p-wings")}}
	tickets, _ := PlanWith(snap, []RosterEntry{front, pizza, wings}, printer.ModeTypeChange, DefaultRules())
	if len(tickets) != 2 {
		t.Fatalf("tickets = %+v", tickets)
	}
	for _, tk := range tickets {
		if !tk.Change || !tk.TypeOnly || tk.WasType != "pickup" || len(tk.Mine) != 1 || len(tk.Others) != 0 || tk.Kind != printer.TicketStation {
			t.Fatalf("ticket: %+v", tk)
		}
	}
	snap.RemovedItems = []order.LineItem{item("Garlic Knots", order.RoutingMatched, "p-pizza")}
	tickets, _ = PlanWith(snap, []RosterEntry{front, pizza, wings}, printer.ModeChange, DefaultRules())
	if len(tickets) == 0 || tickets[0].WasType != "pickup" || tickets[0].TypeOnly {
		t.Fatalf("change with lines: %+v", tickets)
	}
}

// "No kitchen ticket" (main's item flag, routing none; the owner 2026-11-12):
// no station ticket of its own, never in "Also on this order", not unrouted;
// still on the customer receipt. An order of only such lines prints just the
// receipt (and nothing on a kitchen resend).
func TestPlan_NoKitchenTicket(t *testing.T) {
	snap := order.Snapshot{Items: []order.LineItem{
		item("Pepperoni", order.RoutingMatched, pizza.ID),
		item("Coke", order.RoutingNone, ""),
		item("Ranch", order.RoutingMatched, wings.ID),
		item("Cannoli", order.RoutingNone, ""),
	}}
	tk, un := Plan(snap, []RosterEntry{front, pizza, wings})
	if un != nil || len(tk) != 3 {
		t.Fatalf("want 2 stations + receipt, no unrouted: %+v %v", tk, un)
	}
	if !reflect.DeepEqual(names(tk[0].Mine), []string{"Pepperoni"}) || !reflect.DeepEqual(names(tk[0].Others), []string{"Ranch"}) {
		t.Fatalf("pizza ticket: %+v", tk[0])
	}
	if !reflect.DeepEqual(names(tk[1].Mine), []string{"Ranch"}) || !reflect.DeepEqual(names(tk[1].Others), []string{"Pepperoni"}) {
		t.Fatalf("wings ticket: %+v", tk[1])
	}
	if tk[2].Kind != printer.TicketReceipt {
		t.Fatalf("receipt: %+v", tk[2])
	}
	only := order.Snapshot{Items: []order.LineItem{item("Coke", order.RoutingNone, "")}}
	tk, un = Plan(only, []RosterEntry{front, pizza, wings})
	if un != nil || len(tk) != 1 || tk[0].Kind != printer.TicketReceipt {
		t.Fatalf("only no-kitchen lines: just the receipt: %+v %v", tk, un)
	}
	if tk, _ := PlanFor(only, []RosterEntry{front, pizza, wings}, printer.ModeKitchen); len(tk) != 0 {
		t.Fatalf("kitchen resend: nothing: %+v", tk)
	}
	// The sole printer (the receipt printer is the kitchen too): still no station ticket for them.
	tk, _ = Plan(only, []RosterEntry{front})
	if len(tk) != 1 || tk[0].Kind != printer.TicketReceipt {
		t.Fatalf("sole printer: %+v", tk)
	}
}

// §2.10aa: an online order placed on arrival prints like a SEND - the
// stations (whatever the online ticket switches say), and for a delivery the
// Merchant + Customer Copy on the receipt printer; a pickup no receipt.
func TestPlanPlaced_StationsAndDeliveryCopies(t *testing.T) {
	roster := []RosterEntry{front, pizza, wings}
	off := Rules{Tickets: order.DefaultTicketToggles()}
	off.Tickets.OnlineCashPickup, off.Tickets.OnlineCashDelivery, off.Tickets.OnlineCard = false, false, false
	items := []order.LineItem{item("Pepperoni", order.RoutingMatched, pizza.ID), item("Hot Wings", order.RoutingMatched, wings.ID)}
	deliv := order.Snapshot{Source: "online", Fulfillment: order.Fulfillment{Type: "delivery"}, Money: order.Money{Total: 2500}, Items: items}
	if tk, _ := PlanWith(deliv, roster, printer.ModePlaced, off); !reflect.DeepEqual(kinds(tk),
		[]string{"station:p-pizza", "station:p-wings", "delivery_copies:p-front"}) {
		t.Fatalf("placed delivery: %v", kinds(tk))
	}
	pick := deliv
	pick.Fulfillment.Type = "pickup"
	if tk, _ := PlanWith(pick, roster, printer.ModePlaced, off); !reflect.DeepEqual(kinds(tk), []string{"station:p-pizza", "station:p-wings"}) {
		t.Fatalf("placed pickup: %v", kinds(tk))
	}
}

// ModeOrderReceipt (contract 2.10ab: a call-center order has no terminal
// printer): a delivery's Merchant + Customer Copy on the receipt printer;
// since 2.5.7 nothing for a pickup (its receipt prints at Cash Out on a
// terminal); never a station ticket.
func TestPlan_OrderReceipt(t *testing.T) {
	roster := []RosterEntry{front, pizza, wings}
	snap := order.Snapshot{Fulfillment: order.Fulfillment{Type: "pickup"}, Items: []order.LineItem{
		item("Pepperoni", order.RoutingMatched, pizza.ID), item("Ranch", order.RoutingMatched, wings.ID)}}
	tk, un := PlanFor(snap, roster, printer.ModeOrderReceipt)
	if un != nil || len(tk) != 0 {
		t.Fatalf("a pickup: nothing on the receipt printer: %+v %v", tk, un)
	}
	snap.Fulfillment.Type = "delivery"
	tk, _ = PlanFor(snap, roster, printer.ModeOrderReceipt)
	if len(tk) != 1 || tk[0].Kind != printer.TicketDeliveryCopies || tk[0].TargetID != front.ID {
		t.Fatalf("a delivery: the delivery ticket: %+v", tk)
	}
	legacy := order.Snapshot{Fulfillment: order.Fulfillment{Type: "pickup"}, Items: []order.LineItem{item("Pepperoni", "", "")}}
	if tk, _ = PlanFor(legacy, roster, printer.ModeOrderReceipt); len(tk) != 0 {
		t.Fatalf("an unrouted pickup: nothing: %+v", tk)
	}
	untyped := order.Snapshot{Items: []order.LineItem{item("Pepperoni", "", "")}}
	if tk, _ = PlanFor(untyped, roster, printer.ModeOrderReceipt); len(tk) != 1 || tk[0].Kind != printer.TicketFull {
		t.Fatalf("an order with no type: the full receipt as before: %+v", tk)
	}
	if tk, _ = PlanFor(snap, nil, printer.ModeOrderReceipt); len(tk) != 0 {
		t.Fatalf("no printers: nothing: %+v", tk)
	}
}

// Every ModeReceipt caller is a Reprint (the receipt says so); ModeDeliveryUpdate
// prints a delivery's copies again marked updated, and nothing for a pickup.
func TestPlan_ReprintAndDeliveryUpdate(t *testing.T) {
	roster := []RosterEntry{pizza, front, wings}
	d := order.Snapshot{Fulfillment: order.Fulfillment{Type: "delivery"}, Items: []order.LineItem{item("Pepperoni", order.RoutingMatched, pizza.ID)}}
	tk, _ := PlanFor(d, roster, printer.ModeReceipt)
	if len(tk) != 1 || !tk[0].Reprint || tk[0].Kind != printer.TicketReceipt {
		t.Fatalf("reprint: %+v", tk)
	}
	if tk, _ := PlanFor(order.Snapshot{Items: []order.LineItem{item("Old", "", "")}}, roster, printer.ModeReceipt); len(tk) != 1 || !tk[0].Reprint {
		t.Fatalf("legacy reprint: %+v", tk)
	}
	tk, _ = PlanFor(d, roster, printer.ModeDeliveryUpdate)
	if len(tk) != 1 || tk[0].Kind != printer.TicketDeliveryCopies || !tk[0].Updated || tk[0].TargetID != front.ID {
		t.Fatalf("delivery update: %+v", tk)
	}
	if tk, _ := PlanFor(d, roster, printer.ModePlaced); tk[len(tk)-1].Updated || tk[len(tk)-1].Reprint {
		t.Fatalf("the placement's copies are neither updated nor a reprint: %+v", tk)
	}
	p := d
	p.Fulfillment.Type = "pickup"
	if tk, _ := PlanFor(p, roster, printer.ModeDeliveryUpdate); len(tk) != 0 {
		t.Fatalf("a pickup: %+v", tk)
	}
}
