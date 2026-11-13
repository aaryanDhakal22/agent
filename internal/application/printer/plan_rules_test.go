package printer

import (
	"reflect"
	"testing"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
)

// 2.1.0 print rules: the six kitchen-ticket switches x the hub's live insert.

func ruleSnap(source, ftype string, payments []order.PaymentSummary) order.Snapshot {
	return order.Snapshot{Source: source, Fulfillment: order.Fulfillment{Type: ftype}, Money: order.Money{Total: 2000},
		Payments: payments, Items: []order.LineItem{item("Pizza", order.RoutingMatched, pizza.ID), item("Wings", order.RoutingMatched, wings.ID)}}
}

var (
	card = []order.PaymentSummary{{Method: "card", AmountCents: 2000}}
	full = []string{"station:p-pizza", "station:p-wings", "receipt:p-front"}
	rcpt = []string{"receipt:p-front"}
	stns = []string{"station:p-pizza", "station:p-wings"}
)

func TestPlanWith_OnlineMatrix(t *testing.T) {
	roster := []RosterEntry{front, pizza, wings}
	on := order.DefaultTicketToggles() // online switches on
	off := order.TicketToggles{}       // everything off
	for _, tc := range []struct {
		name  string
		snap  order.Snapshot
		rules Rules
		want  []string
	}{
		// 2.5.7: a pickup / dine-in prints its station tickets only, whatever the
		// switches say (its receipt prints at Cash Out on the terminal).
		{"card, switch on", ruleSnap("online", "pickup", card), Rules{Tickets: on}, stns},
		{"card, switch off", ruleSnap("online", "pickup", card), Rules{Tickets: off}, stns},
		{"card delivery, switch on", ruleSnap("online", "delivery", card), Rules{Tickets: on}, full},
		{"card delivery, card off", ruleSnap("online", "delivery", card), Rules{Tickets: order.TicketToggles{OnlineCashDelivery: true}}, rcpt},
		{"cash delivery, switch on", ruleSnap("online", "delivery", nil), Rules{Tickets: on}, full},
		{"cash delivery, switch off", ruleSnap("online", "delivery", nil), Rules{Tickets: order.TicketToggles{OnlineCard: true}}, rcpt},
		// Cash pickup: its switch alone (H3: no "insert live" rule any more).
		{"cash pickup, switch on", ruleSnap("online", "pickup", nil), Rules{Tickets: on}, stns},
		{"cash pickup, switch off", ruleSnap("online", "pickup", nil),
			Rules{Tickets: order.TicketToggles{OnlineCard: true, OnlineCashDelivery: true}}, stns},
		{"cash pickup, everything off", ruleSnap("online", "pickup", nil), Rules{Tickets: off}, stns},
		{"cash dine-in: stations only", ruleSnap("online", "dine_in", nil), Rules{Tickets: off}, stns},
		{"no type at all prints as before", ruleSnap("online", "", nil), Rules{Tickets: on}, full},
	} {
		tk, _ := PlanWith(tc.snap, roster, printer.ModeAccept, tc.rules)
		if got := kinds(tk); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestPlanWith_Defaults(t *testing.T) {
	roster := []RosterEntry{front, pizza, wings}
	for _, snap := range []order.Snapshot{ruleSnap("online", "pickup", nil), ruleSnap("online", "pickup", card), ruleSnap("online", "delivery", nil)} {
		a, _ := PlanFor(snap, roster, printer.ModeAccept)
		b, _ := PlanWith(snap, roster, printer.ModeAccept, DefaultRules())
		if !reflect.DeepEqual(kinds(a), kinds(b)) {
			t.Fatalf("PlanFor != PlanWith(defaults): %v vs %v", kinds(a), kinds(b))
		}
	}
	if tk, _ := PlanFor(ruleSnap("online", "pickup", nil), roster, printer.ModeAccept); !reflect.DeepEqual(kinds(tk), stns) {
		t.Fatalf("default cash pickup = %v, want the station tickets only (2.5.7)", kinds(tk))
	}
	if tk, _ := PlanFor(ruleSnap("online", "delivery", nil), roster, printer.ModeAccept); !reflect.DeepEqual(kinds(tk), full) {
		t.Fatalf("default cash delivery = %v, want the station tickets + the receipt", kinds(tk))
	}
}

// 2.5.0: a channel order (the store's website) always prints the station
// tickets + the receipt, whatever the online switches say; since 2.5.7 the
// receipt only when it is a delivery.
func TestPlanWith_ChannelOrders(t *testing.T) {
	roster := []RosterEntry{front, pizza, wings}
	off := Rules{Tickets: order.TicketToggles{}}
	for _, tc := range []struct {
		name  string
		snap  order.Snapshot
		rules Rules
		want  []string
	}{
		{"web card pickup, switches off", ruleSnap("web", "pickup", card), off, stns},
		{"web cash pickup, switches off", ruleSnap("web", "pickup", nil), off, stns},
		{"web cash pickup, defaults", ruleSnap("web", "pickup", nil), DefaultRules(), stns},
		{"web cash delivery, switches off", ruleSnap("web", "delivery", nil), off, full},
		{"web card delivery, switches off", ruleSnap("web", "delivery", card), off, full},
		{"operator cash pickup", ruleSnap("operator", "pickup", nil), off, stns},
		{"counter card pickup", ruleSnap("counter", "pickup", card), off, stns},
	} {
		tk, _ := PlanWith(tc.snap, roster, printer.ModeAccept, tc.rules)
		if got := kinds(tk); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
		// Kitchen-only accept (future order whose receipt printed early).
		tk, _ = PlanWith(tc.snap, roster, printer.ModeAcceptKitchen, tc.rules)
		if got := kinds(tk); !reflect.DeepEqual(got, stns) {
			t.Errorf("%s accept_kitchen: got %v, want %v", tc.name, got, stns)
		}
	}
	// The same cash delivery from the vendor follows its switch.
	if tk, _ := PlanWith(ruleSnap("online", "delivery", nil), roster, printer.ModeAccept, off); !reflect.DeepEqual(kinds(tk), rcpt) {
		t.Fatalf("online cash delivery = %v, want receipt only", kinds(tk))
	}
	// VOID (CANCEL ORDER): station tickets only, no receipt.
	tk, _ := PlanWith(ruleSnap("web", "delivery", card), roster, printer.ModeVoid, off)
	if got := kinds(tk); !reflect.DeepEqual(got, stns) {
		t.Fatalf("void = %v, want %v", got, stns)
	}
	for _, x := range tk {
		if !x.Void || x.Others != nil {
			t.Fatalf("void ticket %+v: want Void, no others", x)
		}
	}
}

func TestSnapshot_IsChannel(t *testing.T) {
	for src, want := range map[string]bool{"web": true, "operator": true, "counter": true, "Web": true,
		"online": false, "pos": false, "": false, " online ": false} {
		if got := (order.Snapshot{Source: src}).IsChannel(); got != want {
			t.Errorf("IsChannel(%q) = %v, want %v", src, got, want)
		}
	}
}
