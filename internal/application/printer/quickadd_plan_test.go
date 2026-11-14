package printer

import (
	"reflect"
	"testing"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
)

// The owner, 2026-11-14 (#17): a Quicc Add the cashier sent nowhere (stored
// routing none) prints on no printer at all for a counter order (2.5.7: no
// receipt either) and on no other station's ITEMS TO OTHER PRINTER; one sent
// to a printer prints there alone, never on every station.
func TestPlan_QuiccAddPrintsOnlyWherePicked(t *testing.T) {
	roster := []RosterEntry{front, pizza, wings}
	add := func(routing, printerID string) order.LineItem {
		it := item("Pizza dough", routing, printerID)
		it.SKU = order.SKUCustom
		return it
	}
	counter := func(items ...order.LineItem) order.Snapshot {
		return order.Snapshot{Source: order.SourceCounter, Fulfillment: order.Fulfillment{Type: "counter"}, Items: items}
	}
	for _, mode := range []printer.Mode{printer.ModeAccept, printer.ModePlaced, printer.ModeKitchen} {
		if tk, un := PlanFor(counter(add(order.RoutingNone, "")), roster, mode); len(tk) != 0 || un != nil {
			t.Errorf("%s: a Quicc Add sent nowhere printed %+v (unrouted %v)", mode, tk, un)
		}
	}
	tk, _ := PlanFor(counter(item("Pepperoni", order.RoutingMatched, pizza.ID), add(order.RoutingNone, "")), roster, printer.ModePlaced)
	if len(tk) != 1 || len(tk[0].Others) != 0 {
		t.Fatalf("not under ITEMS TO OTHER PRINTER: %+v", tk)
	}
	tk, un := PlanFor(counter(add(order.RoutingMatched, wings.ID)), roster, printer.ModePlaced)
	if un != nil || len(tk) != 1 || tk[0].TargetID != wings.ID || !reflect.DeepEqual(names(tk[0].Mine), []string{"Pizza dough"}) {
		t.Fatalf("on the picked printer alone: %+v %v", tk, un)
	}
	// On the receipt printer (Front): a station ticket there with only the Quicc Add; a drink routed
	// there stays receipt-only.
	coke := item("Coke", order.RoutingMatched, front.ID)
	tk, _ = PlanFor(counter(add(order.RoutingMatched, front.ID), coke), roster, printer.ModePlaced)
	if len(tk) != 1 || tk[0].TargetID != front.ID || tk[0].Kind != printer.TicketStation || !reflect.DeepEqual(names(tk[0].Mine), []string{"Pizza dough"}) {
		t.Fatalf("a Quicc Add on the receipt printer: %+v", tk)
	}
	if tk, _ := PlanFor(counter(coke), roster, printer.ModePlaced); len(tk) != 0 {
		t.Fatalf("a drink on the receipt printer stays receipt-only: %+v", tk)
	}
}

// The owner 2026-11-14 (#17): a Quicc Add never prints unless a printer was chosen. A chosen printer this Pi
// doesn't know (deleted, another store's) must print nowhere, never fall back to every station; a priced
// note under it follows it. A menu item on an unknown printer still goes to every station as before.
func TestPlan_QuiccAddOnAnUnknownPrinterPrintsNowhere(t *testing.T) {
	roster := []RosterEntry{front, pizza, wings}
	dough := item("Pizza dough", order.RoutingMatched, "gone-printer")
	dough.SKU, dough.ID = order.SKUCustom, "l1"
	note := item("Extra flour", order.RoutingMatched, "gone-printer")
	note.SKU, note.ParentLineID = order.SKUNote, "l1"
	snap := order.Snapshot{Source: order.SourceCounter, Fulfillment: order.Fulfillment{Type: "counter"}, Items: []order.LineItem{dough, note}}
	for _, mode := range []printer.Mode{printer.ModeAccept, printer.ModePlaced, printer.ModeKitchen} {
		if tk, un := PlanFor(snap, roster, mode); len(tk) != 0 || un != nil {
			t.Errorf("%s: a Quicc Add on an unknown printer printed %+v (unrouted %v)", mode, tk, un)
		}
	}
	menu := item("Pepperoni", order.RoutingMatched, "gone-printer")
	if tk, un := PlanFor(order.Snapshot{Source: order.SourceCounter, Fulfillment: order.Fulfillment{Type: "counter"}, Items: []order.LineItem{menu}}, roster, printer.ModePlaced); len(tk) != 2 || len(un) != 1 {
		t.Fatalf("a menu item on an unknown printer still goes to every station: %+v %v", tk, un)
	}
}
