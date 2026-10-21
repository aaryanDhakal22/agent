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
		!reflect.DeepEqual(names(tk[0].Others), []string{"Soda"}) {
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

func TestPlan_PickupCashPrintsOnlyTheReceipt(t *testing.T) {
	items := []order.LineItem{item("Pepperoni", order.RoutingMatched, pizza.ID), item("Wings", order.RoutingMatched, wings.ID)}
	roster := []RosterEntry{front, pizza, wings}
	snap := func(kind string, pays ...order.PaymentSummary) order.Snapshot {
		return order.Snapshot{Items: items, Fulfillment: order.Fulfillment{Type: kind}, Money: order.Money{Total: 2500}, Payments: pays}
	}

	// Pickup with no payments (= cash) or a cash payment: just the receipt.
	for _, s := range []order.Snapshot{snap("pickup"), snap("Pickup", order.PaymentSummary{Method: "cash", AmountCents: 2500})} {
		tk, un := Plan(s, roster)
		if len(un) != 0 || len(tk) != 1 || tk[0].Kind != printer.TicketReceipt || tk[0].TargetID != front.ID {
			t.Fatalf("pickup cash: want only the receipt on Front, got %+v", tk)
		}
	}
	// Even on a single-printer store.
	if tk, _ := Plan(snap("pickup"), []RosterEntry{front}); len(tk) != 1 || tk[0].Kind != printer.TicketReceipt {
		t.Fatalf("single printer pickup cash: %+v", tk)
	}
	// Pickup paid by card, and cash delivery: kitchen tickets as usual.
	for _, s := range []order.Snapshot{snap("pickup", order.PaymentSummary{Method: "card", AmountCents: 2500}), snap("delivery")} {
		if tk, _ := Plan(s, roster); len(tk) != 3 {
			t.Fatalf("%s: want 2 stations + receipt, got %+v", s.Fulfillment.Type, tk)
		}
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
	if tk, _ := Plan(snap, roster); !reflect.DeepEqual(kinds(tk), []string{"receipt:p-front"}) {
		t.Fatalf("accept of pickup cash should be receipt only: %v", kinds(tk))
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
		!reflect.DeepEqual(names(tk[0].Mine), []string{"Pepperoni"}) || !reflect.DeepEqual(names(tk[0].Others), []string{"Soda"}) {
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
