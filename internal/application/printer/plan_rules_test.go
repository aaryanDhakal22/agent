package printer

import (
	"reflect"
	"testing"
	"time"

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
		{"card, switch on", ruleSnap("online", "pickup", card), Rules{Tickets: on}, full},
		{"card, switch off", ruleSnap("online", "pickup", card), Rules{Tickets: off}, rcpt},
		{"card delivery, switch on", ruleSnap("online", "delivery", card), Rules{Tickets: on}, full},
		{"card delivery, card off", ruleSnap("online", "delivery", card), Rules{Tickets: order.TicketToggles{OnlineCashDelivery: true}}, rcpt},
		{"cash delivery, switch on", ruleSnap("online", "delivery", nil), Rules{Tickets: on}, full},
		{"cash delivery, switch off", ruleSnap("online", "delivery", nil), Rules{Tickets: order.TicketToggles{OnlineCard: true}}, rcpt},
		// Cash pickup: tickets only with the switch AND the hub inserting live.
		{"cash pickup, switch on, insert not live", ruleSnap("online", "pickup", nil), Rules{Tickets: on, InsertLive: false}, rcpt},
		{"cash pickup, switch on, insert live", ruleSnap("online", "pickup", nil), Rules{Tickets: on, InsertLive: true}, full},
		{"cash pickup, switch off, insert live", ruleSnap("online", "pickup", nil),
			Rules{Tickets: order.TicketToggles{OnlineCard: true, OnlineCashDelivery: true}, InsertLive: true}, rcpt},
		{"cash pickup, switch off, insert not live", ruleSnap("online", "pickup", nil), Rules{Tickets: off}, rcpt},
		{"cash dine-in prints as before", ruleSnap("online", "dine_in", nil), Rules{Tickets: off}, full},
	} {
		tk, _ := PlanWith(tc.snap, roster, printer.ModeAccept, tc.rules)
		if got := kinds(tk); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestPlanWith_DefaultsAreThe207Rule(t *testing.T) {
	roster := []RosterEntry{front, pizza, wings}
	for _, snap := range []order.Snapshot{ruleSnap("online", "pickup", nil), ruleSnap("online", "pickup", card), ruleSnap("online", "delivery", nil)} {
		a, _ := PlanFor(snap, roster, printer.ModeAccept)
		b, _ := PlanWith(snap, roster, printer.ModeAccept, DefaultRules())
		if !reflect.DeepEqual(kinds(a), kinds(b)) {
			t.Fatalf("PlanFor != PlanWith(defaults): %v vs %v", kinds(a), kinds(b))
		}
	}
	if tk, _ := PlanFor(ruleSnap("online", "pickup", nil), roster, printer.ModeAccept); !reflect.DeepEqual(kinds(tk), rcpt) {
		t.Fatalf("default cash pickup = %v, want receipt only", kinds(tk))
	}
}

func TestPlanWith_POSOrders(t *testing.T) {
	roster := []RosterEntry{front, pizza, wings}
	all := Rules{Tickets: order.TicketToggles{PosDelivery: true, PosTakeout: true, PosCounter: true}, InsertLive: true}
	for _, tc := range []struct {
		name  string
		ftype string
		rules Rules
		want  []string
	}{
		{"delivery off", "delivery", DefaultRules(), nil},
		{"takeout off", "pickup", DefaultRules(), nil},
		{"counter off", "dine_in", DefaultRules(), nil},
		{"delivery on: stations, never a receipt", "delivery", Rules{Tickets: order.TicketToggles{PosDelivery: true}}, stns},
		{"takeout on", "pickup", Rules{Tickets: order.TicketToggles{PosTakeout: true}}, stns},
		{"counter on", "dine_in", Rules{Tickets: order.TicketToggles{PosCounter: true}}, stns},
		{"takeout switch doesn't print a delivery", "delivery", Rules{Tickets: order.TicketToggles{PosTakeout: true}}, nil},
		{"all on", "delivery", all, stns},
	} {
		snap := ruleSnap(order.SourcePOS, tc.ftype, nil)
		tk, _ := PlanWith(snap, roster, printer.ModeAccept, tc.rules)
		got := kinds(tk)
		if len(tc.want) == 0 && len(got) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
	// Reprint = the receipt, Resend = the stations, whatever the switches.
	snap := ruleSnap(order.SourcePOS, "delivery", nil)
	if tk, _ := PlanWith(snap, roster, printer.ModeReceipt, DefaultRules()); !reflect.DeepEqual(kinds(tk), rcpt) {
		t.Errorf("POS reprint = %v", kinds(tk))
	}
	if tk, _ := PlanWith(snap, roster, printer.ModeKitchen, DefaultRules()); !reflect.DeepEqual(kinds(tk), stns) {
		t.Errorf("POS resend = %v", kinds(tk))
	}
}

// A POS line without a Pi printer is not for this kitchen (no station, not
// reported unrouted); one naming a printer this Pi lacks goes everywhere.
func TestPlanWith_POSLinesWithoutPrinter(t *testing.T) {
	roster := []RosterEntry{front, pizza, wings}
	snap := order.Snapshot{Source: order.SourcePOS, Fulfillment: order.Fulfillment{Type: "delivery"}, Items: []order.LineItem{
		item("Pizza", order.RoutingMatched, pizza.ID), item("Coke", order.RoutingUnassigned, ""),
	}}
	tk, un := PlanWith(snap, roster, printer.ModeAccept, Rules{Tickets: order.TicketToggles{PosDelivery: true}})
	if !reflect.DeepEqual(kinds(tk), []string{"station:p-pizza"}) || un != nil {
		t.Fatalf("got %v unrouted=%v", kinds(tk), un)
	}
	if !reflect.DeepEqual(names(tk[0].Mine), []string{"Pizza"}) || !reflect.DeepEqual(names(tk[0].Others), []string{"Coke"}) {
		t.Fatalf("pizza ticket = %+v", tk[0])
	}
	snap.Items = []order.LineItem{item("Coke", order.RoutingUnassigned, "")}
	if tk, _ := PlanWith(snap, roster, printer.ModeAccept, Rules{Tickets: order.TicketToggles{PosDelivery: true}}); len(tk) != 0 {
		t.Fatalf("nothing for the kitchen, got %v", kinds(tk))
	}
	snap.Items = []order.LineItem{item("Calzone", order.RoutingMatched, "p-gone")}
	tk, un = PlanWith(snap, roster, printer.ModeAccept, Rules{Tickets: order.TicketToggles{PosDelivery: true}})
	if !reflect.DeepEqual(kinds(tk), stns) || !reflect.DeepEqual(un, []string{"Calzone"}) {
		t.Fatalf("unknown printer: %v unrouted=%v", kinds(tk), un)
	}
}

// A POS check built by the domain routes as the planner expects.
func TestPlanWith_POSSnapshotFromHub(t *testing.T) {
	e := order.ExternalOrder{CheckType: "pickup", Hash: "h", Items: []order.ExternalItem{
		{Name: "Cheese Pizza", Quantity: 1, PrinterID: pizza.ID}, {Name: "Soda", Quantity: 1},
	}}
	snap := e.Snapshot("id", "t", "l", "1234", time.Now())
	tk, _ := PlanWith(snap, []RosterEntry{front, pizza, wings}, printer.ModeAccept, Rules{Tickets: order.TicketToggles{PosTakeout: true}})
	if !reflect.DeepEqual(kinds(tk), []string{"station:p-pizza"}) {
		t.Fatalf("got %v", kinds(tk))
	}
}
