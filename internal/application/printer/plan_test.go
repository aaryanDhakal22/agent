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

func TestPlan_ItemRoutedToReceiptPrinter(t *testing.T) {
	snap := order.Snapshot{Items: []order.LineItem{
		item("Soda", order.RoutingMatched, front.ID),
		item("Pepperoni", order.RoutingMatched, pizza.ID),
	}}
	tk, _ := Plan(snap, []RosterEntry{front, pizza})
	if len(tk) != 3 || tk[0].TargetID != front.ID || tk[0].Kind != printer.TicketStation {
		t.Fatalf("item on the receipt printer should still get a station ticket there: %+v", tk)
	}
}
