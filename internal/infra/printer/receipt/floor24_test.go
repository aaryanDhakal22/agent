package receipt

import (
	"strings"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
)

// 2.4.0 condensed receipt: one line per item priced with all its modifiers,
// only the first modifier (no price), notes kept, no Sum lines; the totals
// and payment stay. Expanded (the default) is unchanged.
func TestBuild_CondensedReceipt(t *testing.T) {
	snap := previewDelivery()
	got := string(Build(snap, previewStore(), receiptTicket(), prefsWith(func(p *order.ReceiptPrefs) { p.Format = order.FormatCondensed })))
	for _, want := range []string{
		itemLine(1, "Large Pepperoni Pizza", 2199), // 18.99 + 2.00 + 1.00
		"    - Extra cheese\n",
		itemLine(2, "10 pc Buffalo Wings", 2598),
		"    - Hot\n",
		"    Note: extra ranch please\n",
		"Subtotal:", "Total:", "PAID - CARD",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("condensed receipt lacks %q:\n%q", want, got)
		}
	}
	for _, not := range []string{"Garlic crust", "Well done", "Sum:"} {
		if strings.Contains(got, not) {
			t.Errorf("condensed receipt has %q:\n%q", not, got)
		}
	}
	expanded := string(Build(snap, previewStore(), receiptTicket(), order.DefaultReceiptPrefs()))
	if !strings.Contains(expanded, "Garlic crust") || !strings.Contains(expanded, "Sum:") {
		t.Errorf("expanded (default) receipt changed:\n%q", expanded)
	}
	// Condensed never touches a kitchen ticket.
	a := Build(snap, previewStore(), stationTicket(), order.DefaultReceiptPrefs())
	b := Build(snap, previewStore(), stationTicket(), prefsWith(func(p *order.ReceiptPrefs) { p.Format = order.FormatCondensed }))
	if string(a) != string(b) {
		t.Error("condensed changed the kitchen ticket")
	}
}

func TestReceiptPrefs_Format(t *testing.T) {
	if order.DefaultReceiptPrefs().Format != order.FormatExpanded {
		t.Fatal("default format must be expanded")
	}
	p, err := order.DecodeReceiptPrefs([]byte(`{}`))
	if err != nil || p.Format != order.FormatExpanded {
		t.Fatalf("decode {} = %+v %v", p, err)
	}
	p.Format = "tiny"
	if p.Validate() == nil {
		t.Fatal("bad format accepted")
	}
}

// 2.4.0: the order type prints over the number on both tickets - COUNTER
// for a POS dine-in check, DINE IN online, nothing when unknown.
func TestOrderTypeLine(t *testing.T) {
	for _, c := range []struct {
		typ, source, want string
	}{
		{"delivery", "", "DELIVERY"}, {"pickup", "", "PICKUP"}, {"Pickup", order.SourcePOS, "PICKUP"},
		{"dine_in", order.SourcePOS, "COUNTER"}, {"dine_in", "online", "DINE IN"}, {"", "", ""},
	} {
		if got := orderTypeLine(order.Snapshot{Source: c.source, Fulfillment: order.Fulfillment{Type: c.typ}}); got != c.want {
			t.Errorf("%s/%s = %q, want %q", c.typ, c.source, got, c.want)
		}
	}
	snap := previewDelivery()
	snap.Fulfillment.Type = ""
	got := string(Build(snap, previewStore(), stationTicket(), order.DefaultReceiptPrefs()))
	if strings.Contains(got, "DELIVERY") || strings.Contains(got, "PICKUP") {
		t.Errorf("unknown type printed a type line: %q", got)
	}
}

func TestBuildUnassignedReport(t *testing.T) {
	at := time.Date(2026, 10, 31, 22, 41, 0, 0, time.UTC)
	placed := at.Add(-40 * time.Minute)
	n := int64(1123)
	rep := order.UnassignedReport{BatchID: 9, BatchOpenedAt: at.Add(-8 * time.Hour), Lines: []order.UnassignedLine{{
		DriverReportLine: order.DriverReportLine{KitchenNumber: &n, Street: "4108 Old Court Rd Apt 2B", Cash: true,
			CashCents: 3340, SlipTotalCents: 3840, OrderTipCents: 500, DeliveredAt: &at},
		PlacedAt: &placed, MarkedAt: at, MarkedBy: order.MarkedBy{Kind: order.ByTablet, Name: "Counter"},
		FromDriver: "Abdul", Available: []string{"Mike", "Sam"},
	}}, CashCents: 3340, TipCents: 500}
	got := string(BuildUnassignedReport(rep, previewStore(), order.SizeNormal, "", at))
	for _, want := range []string{"UNASSIGNED", "Batch #9", "#1123 4108 Old Court Rd Apt 2B", "CASH $38.40", "Tip: $5.00",
		"Marked by tablet Counter", "Taken off Abdul", "In store then: Mike, Sam", "Accepted --", "WHO TOOK THESE?"} {
		if !strings.Contains(got, want) {
			t.Errorf("slip lacks %q:\n%q", want, got)
		}
	}
}

// 2.4.0 (owner): the customer receipt starts with the store, then the
// number, then the type; the kitchen ticket starts with the type, then the
// number, then the name.
func TestHeaderOrder(t *testing.T) {
	snap := previewDelivery()
	rc := string(Build(snap, previewStore(), receiptTicket(), order.DefaultReceiptPrefs()))
	store, num, typ := strings.Index(rc, "Lombardi's Pizza"), strings.Index(rc, "#1123\n"), strings.Index(rc, "DELIVERY\n")
	if store < 0 || num < 0 || typ < 0 || !(store < num && num < typ) {
		t.Errorf("receipt header order store %d, number %d, type %d:\n%q", store, num, typ, rc)
	}
	if !strings.Contains(rc, cmdQuadSz+"#1123\n"+cmdDoubleSz+"DELIVERY\n") {
		t.Errorf("receipt sizes: %q", rc)
	}
	kt := string(Build(snap, previewStore(), stationTicket(), order.DefaultReceiptPrefs()))
	typ, num, name := strings.Index(kt, "DELIVERY\n"), strings.Index(kt, "#1123\n"), strings.Index(kt, "GONZALEZ, MARIA")
	if !(typ >= 0 && typ < num && num < name) {
		t.Errorf("kitchen header order type %d, number %d, name %d:\n%q", typ, num, name, kt)
	}
}

// 2.4.0: a future order's early kitchen ticket says DO NOT MAKE on top and
// at the bottom, and when it is due on the second line; its early receipt
// has FUTURE on the very top.
func TestFutureEarlyTickets(t *testing.T) {
	at := time.Date(2026, 10, 31, 18, 30, 0, 0, time.UTC)
	kt := string(BuildFutureSample(previewStore(), order.DefaultReceiptPrefs(), at, true))
	if strings.Count(kt, "--- DO NOT MAKE ---") != 2 {
		t.Fatalf("DO NOT MAKE twice: %q", kt)
	}
	first, when, num := strings.Index(kt, "DO NOT MAKE"), strings.Index(kt, "DELIVERY AT "), strings.Index(kt, "#1234")
	if !(first < when && when < num) || strings.Contains(kt, " FUTURE ") {
		t.Fatalf("order: %q", kt)
	}
	rc := string(BuildFutureSample(previewStore(), order.DefaultReceiptPrefs(), at, false))
	fut, store := strings.Index(rc, "FUTURE"), strings.Index(rc, "Lombardi")
	if fut < 0 || store < 0 || fut > store || !strings.HasPrefix(rc, cmdInit+cmdBaseSz+cmdCenter) {
		t.Fatalf("receipt: %q", rc)
	}
	// A pickup says PICKUP AT, a later day shows the day.
	snap := SampleOrder(at)
	snap.Fulfillment.Type = "pickup"
	due := at.Add(26 * time.Hour)
	snap.Fulfillment.PromisedAt = &due
	got := string(Build(snap, previewStore(), printer.Ticket{Kind: printer.TicketStation, TargetID: "sample", Mine: snap.Items, Future: true}, order.DefaultReceiptPrefs()))
	if !strings.Contains(got, "PICKUP AT ") || !strings.Contains(got, strings.ToUpper(due.In(time.Local).Format("Mon Jan 2"))[:3]) {
		t.Fatalf("pickup: %q", got)
	}
}
