package receipt

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
)

// The kitchen ticket (the owner's design of 2026-11-11, station.go): the
// top four lines under the rail, the big number, items with "- " lines,
// "Also on this order:", the future band and the change bands as the only
// reverse print. Goldens: testdata/station_{counter,pickup_future,delivery,
// change,change_text,half_only}.golden (-update).

// printedAt pins the "Printed" time on every ticket the package's tests build.
var printedAt = time.Date(2026, 11, 11, 18, 31, 0, 0, time.UTC)

func TestMain(m *testing.M) {
	clock = func() time.Time { return printedAt }
	deliveryNow = func() time.Time { return printedAt } // the slips' Date / Time (softtouch.go)
	os.Exit(m.Run())
}

func kitchenLine(qty int, name, size, printerID string, kids ...order.LineItem) order.LineItem {
	return order.LineItem{Name: name, Size: size, Quantity: qty, Routing: order.RoutingMatched, PrinterID: printerID, Children: kids}
}

func kid(name string) order.LineItem { return order.LineItem{Name: name, Quantity: 1} }

func kitchenSnap(typ, source, name string, number int64, items ...order.LineItem) order.Snapshot {
	return order.Snapshot{ID: "o-k", SourceEventID: "V-77", KitchenNumber: &number, Source: source,
		PlacedAt: time.Date(2026, 11, 11, 18, 29, 0, 0, time.UTC), Customer: order.Customer{DisplayName: name},
		Fulfillment: order.Fulfillment{Type: typ}, Items: items}
}

func pizzaTicket(snap order.Snapshot) printer.Ticket {
	t := printer.Ticket{Kind: printer.TicketStation, TargetID: "p-pizza", Station: "Pizza"}
	for _, it := range snap.Items {
		if it.PrinterID == "p-pizza" {
			t.Mine = append(t.Mine, it)
		} else {
			t.Others = append(t.Others, it)
		}
	}
	return t
}

func counterOrder() order.Snapshot {
	pep := kitchenLine(2, "Pepperoni Pizza", "Large", "p-pizza", kid("Extra cheese"), kid("Well done"))
	knots := kitchenLine(1, "Garlic Knots", "Serving", "p-pizza", kid("Side of ranch"))
	fries := kitchenLine(1, "Fries", "", "p-fry")
	coke := kitchenLine(1, "2L Coke", "", "p-front")
	return kitchenSnap("dine_in", order.SourceCounter, "Ana", 12, pep, knots, fries, coke)
}

func TestKitchenTicket_Goldens(t *testing.T) {
	counter := counterOrder()

	// A pickup due later with half and half incl. ONLY.
	half := kitchenLine(1, "Cheese Pizza", "X-Large", "p-pizza",
		order.LineItem{Name: "Extra cheese", Quantity: 1},
		order.LineItem{Name: "ONLY Pepperoni (1st half)", Portion: order.PortionFirst, Mode: order.ModeOnly, Quantity: 1},
		order.LineItem{Name: "Hawaiian (2nd half)", Portion: order.PortionSecond, Mode: order.ModeSpecialty, Quantity: 1},
		order.LineItem{Name: "Mushrooms (2nd half)", Portion: order.PortionSecond, Mode: order.ModeAdd, Quantity: 1})
	future := kitchenSnap("pickup", "web", "Ada", 13, half, kitchenLine(1, "Garden salad", "", "p-cold"))
	ready, start := time.Date(2026, 11, 11, 18, 45, 0, 0, time.UTC), time.Date(2026, 11, 11, 18, 30, 0, 0, time.UTC)
	future.Fulfillment.PromisedAt, future.MakeAt = &ready, &start

	// A delivery from main (an online order, kitchen number 1104).
	wings := kitchenLine(2, "Buffalo Wings", "20 pc", "p-pizza", kid("Hot"), kid("Well done"))
	wings.Notes = "Ranch on the side"
	delivery := kitchenSnap("delivery", "online", "Ravi Patel", 1104, wings,
		kitchenLine(1, "Supreme Pizza", "X-Large", "p-oven"), kitchenLine(1, "2L Coke", "", "p-front"))
	delivery.Fulfillment.Address = &order.Address{Line1: "12 Main St"}

	// A change after SEND: added a pizza + a coke, removed the knots.
	change := counterOrder()
	added := []order.LineItem{kitchenLine(1, "Cheese Pizza", "Large", "p-pizza", kid("Mushrooms")), kitchenLine(1, "2L Coke", "", "p-pizza")}
	gone := []order.LineItem{kitchenLine(1, "Garlic Knots", "", "p-pizza", kid("Side of ranch"))}
	changeTk := printer.Ticket{Kind: printer.TicketStation, TargetID: "p-pizza", Station: "Pizza", Change: true, Mine: added, Removed: gone}
	noImages := order.DefaultReceiptPrefs()
	noImages.StrikeImages = false

	halfOnly := kitchenSnap("dine_in", order.SourceCounter, "Ada", 301, half)

	for _, c := range []struct {
		path string
		got  []byte
	}{
		{"testdata/station_counter.golden", BuildStation(counter, pizzaTicket(counter))},
		{"testdata/station_pickup_future.golden", BuildStation(future, pizzaTicket(future))},
		{"testdata/station_delivery.golden", BuildStation(delivery, pizzaTicket(delivery))},
		{"testdata/station_change.golden", BuildStation(change, changeTk)},
		{"testdata/station_change_text.golden", buildStation(change, changeTk, noImages)},
		{"testdata/station_half_only.golden", BuildStation(halfOnly, pizzaTicket(halfOnly))},
	} {
		golden(t, c.path, c.got)
	}
}

func TestKitchenTicket_Layout(t *testing.T) {
	snap := counterOrder()
	got := string(BuildStation(snap, pizzaTicket(snap)))
	// The top four lines: the type double-size, the print time, a blank line - then the number.
	top := cmdInit + cmdCenter + cmdBoldOn + cmdDoubleSz + "COUNTER\n" + cmdBoldOff + cmdSizeReset + cmdNormalSz +
		"Printed 6:31 PM\n\n" + cmdBoldOn + cmdHugeSz + "12\n"
	if !strings.HasPrefix(got, top) {
		t.Fatalf("top:\n%q", got[:min(len(got), len(top)+20)])
	}
	for _, want := range []string{
		"ANA\n",
		"2   Pepperoni Pizza\n    (Large)\n", H8 + "- Extra cheese\n", H8 + "- Well done\n",
		"1   Garlic Knots\n", H8 + "- Side of ranch\n",
		// ITEMS TO OTHER PRINTER (the owner, 2026-11-14, #2): one size smaller (font A 1x1), " - 1   Fries".
		strings.Repeat("-", Cols80) + "\n" + cmdSizeReset + cmdNormalSz + "ITEMS TO OTHER PRINTER\n - 1   Fries\n - 1   2L Coke\n" + cmdBaseSz,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q\n%q", want, got)
		}
	}
	// No reverse print, no prices, no station names, no rung-by on an order ticket.
	for _, gone := range []string{cmdInvertOn, "$", "Pizza:", "PIZZA STATION", "Rung", "Also on this order"} {
		if strings.Contains(got, gone) {
			t.Errorf("an order ticket carries %q", gone)
		}
	}
	// Nothing else on the order: the section still closes the ticket.
	one := pizzaTicket(snap)
	one.Others = nil
	if alone := string(BuildStation(snap, one)); !strings.Contains(alone, cmdNormalSz+"ITEMS TO OTHER PRINTER\n"+cmdBaseSz) ||
		strings.Contains(alone, "\n - ") || strings.Contains(alone, "nothing else") {
		t.Error("ITEMS TO OTHER PRINTER is always at the bottom, the heading alone when nothing else prints")
	}
}

func TestKitchenTicket_TypesAndNumber(t *testing.T) {
	for _, c := range []struct {
		typ, source, want string
	}{
		{"dine_in", order.SourceCounter, "COUNTER"}, {"pickup", "online", "PICKUP"},
		{"delivery", "web", "DELIVERY"}, {"dine_in", "online", "DINE IN"}, {"", "online", "ORDER"},
	} {
		if got := stationType(order.Snapshot{Source: c.source, Fulfillment: order.Fulfillment{Type: c.typ}}); got != c.want {
			t.Errorf("%s/%s: %q want %q", c.typ, c.source, got, c.want)
		}
	}
	if numberSize("1104", defaultLayout) != cmdHugeSz || numberSize("1234567890123", defaultLayout) != cmdQuadSz || numberSize(strings.Repeat("9", 17), defaultLayout) != cmdDoubleSz {
		t.Error("the number prints at the largest size that fits")
	}
	snap := counterOrder()
	snap.KitchenNumber = nil
	if !strings.Contains(string(BuildStation(snap, pizzaTicket(snap))), cmdHugeSz+"V-77\n") {
		t.Error("no kitchen number: the vendor's order id")
	}
}

func TestKitchenTicket_FutureBand(t *testing.T) {
	snap := counterOrder()
	ready, start := printedAt.Add(14*time.Minute), printedAt.Add(-1*time.Minute)
	snap.Fulfillment.PromisedAt, snap.MakeAt = &ready, &start
	got := string(BuildStation(snap, pizzaTicket(snap)))
	band := cmdInvertOn + pad("MAKE AT 6:30 \xfa READY 6:45", Cols80) + cmdInvertOff
	if strings.Count(got, cmdInvertOn) != 1 || !strings.Contains(got, band) {
		t.Fatalf("one reverse band:\n%q", got)
	}
	name, b, rule := strings.Index(got, "ANA\n"), strings.Index(got, band), strings.Index(got, strings.Repeat("-", Cols80))
	if !(name < b && b < rule) {
		t.Errorf("band between the name and the rule: %d %d %d", name, b, rule)
	}
	// Nothing to count back from: READY alone; another day: the day under it.
	snap.MakeAt = nil
	later := printedAt.Add(25 * time.Hour)
	snap.Fulfillment.PromisedAt = &later
	got = string(BuildStation(snap, pizzaTicket(snap)))
	if !strings.Contains(got, pad("READY 7:31", Cols80)) || !strings.Contains(got, pad("THU NOV 12", Cols80)) {
		t.Errorf("ready only, another day:\n%q", got)
	}
}

func TestKitchenTicket_Change(t *testing.T) {
	snap := counterOrder()
	added := []order.LineItem{kitchenLine(1, "Cheese Pizza", "Large", "p-pizza", kid("Mushrooms"))}
	gone := []order.LineItem{kitchenLine(1, "Garlic Knots", "", "p-pizza")}
	tk := printer.Ticket{Kind: printer.TicketStation, TargetID: "p-pizza", Change: true, Mine: added, Removed: gone}
	got := string(BuildStation(snap, tk))
	addBand := cmdDoubleSz + cmdInvertOn + pad("ADD / AGREGAR", defaultLayout.big()) + cmdInvertOff
	rmBand := cmdDoubleSz + cmdInvertOn + pad("REMOVE / QUITAR", defaultLayout.big()) + cmdInvertOff
	rule, a, item, r, void := strings.Index(got, strings.Repeat("-", Cols80)), strings.Index(got, addBand),
		strings.Index(got, "1   Cheese Pizza (Large)\n"), strings.Index(got, rmBand), strings.Index(got, "(VOID) \x1b*\x21")
	if rule < 0 || !(rule < a && a < item && item < r && r < void) {
		t.Fatalf("rule %d < ADD %d < item %d < REMOVE %d < (VOID) %d:\n%q", rule, a, item, r, void, got)
	}
	if !strings.HasPrefix(got, cmdInit+cmdCenter+cmdBoldOn+cmdDoubleSz+"COUNTER\n") || strings.Contains(got, otherPrintersHead) {
		t.Errorf("same top, no other items:\n%q", got)
	}
	if strings.Count(got, cmdInvertOn) != 2 {
		t.Errorf("reverse only for the two bands")
	}
	// The struck picture: 15 characters x 12 dots, 24 dots tall, a line through the middle.
	img := strikeImage("1   Garlic Knots")
	if !strings.Contains(got, "(VOID) "+img+"\n") || len(img) != 5+16*12*3 || img[3] != byte(16*12%256) || img[4] != byte(16*12/256) {
		t.Errorf("struck line: %d bytes", len(img))
	}
	data := []byte(img[5:])
	for x := 0; x < 16*12; x++ {
		if data[x*3+strikeTop/8]&(0x80>>(strikeTop%8)) == 0 {
			t.Fatalf("no strike at column %d", x)
		}
	}
	// Only removals (a VOID ticket): no ADD band.
	voidTk := printer.Ticket{Kind: printer.TicketStation, TargetID: "p-pizza", Void: true, Mine: gone}
	got = string(BuildStation(snap, voidTk))
	if strings.Contains(got, "ADD / AGREGAR") || !strings.Contains(got, "REMOVE / QUITAR") || !strings.Contains(got, "(VOID) ") {
		t.Errorf("void ticket:\n%q", got)
	}
	// A printer without pictures: (VOID) in reverse and the text.
	p := order.DefaultReceiptPrefs()
	p.StrikeImages = false
	got = string(buildStation(snap, tk, p))
	if !strings.Contains(got, cmdInvertOn+"(VOID)"+cmdInvertOff+" 1   Garlic Knots\n") || strings.Contains(got, "\x1b*") {
		t.Errorf("text fallback:\n%q", got)
	}
}

// The choices' columns under a large (double width) item (2.5.7; the
// 2026-11-14 quantity gap moved the name to column 8): H8 = two past the
// item's name (column 10), H10 = a half's topping nested under its heading
// (column 12). The names stayed: the tests read the same.
const (
	H8  = "          "
	H10 = "            "
)

func TestKitchenTicket_Halves(t *testing.T) {
	snap := kitchenSnap("dine_in", order.SourceCounter, "Ada", 13, kitchenLine(1, "Cheese Pizza", "X-Large", "p-pizza",
		order.LineItem{Name: "Extra cheese", Quantity: 1},
		order.LineItem{Name: "ONLY Pepperoni (1st half)", Portion: order.PortionFirst, Mode: order.ModeOnly, Quantity: 1},
		order.LineItem{Name: "NO Onion (2nd half)", Portion: order.PortionSecond, Mode: order.ModeNo, Quantity: 1}))
	got := string(BuildStation(snap, pizzaTicket(snap)))
	want := "1   Cheese Pizza\n    (X-Large)\n" + cmdSizeReset + cmdBaseSz + cmdBoldOff +
		H8 + "WHOLE:\n" + H10 + "- Extra cheese\n" + H8 + "1ST HALF:\n" + H10 + "- Pepperoni ONLY\n" + H8 + "2ND HALF:\n" + H10 + "- NO Onion\n"
	if !strings.Contains(got, want) {
		t.Fatalf("halves:\n%q", got)
	}
	if strings.Contains(got, cmdInvertOn) || strings.Contains(got, "\xab") || strings.Contains(got, "HALF & HALF") {
		t.Error("no reverse print, no ½ summary, no banner")
	}
}

// Old tickets (online orders from main) print in the same format.
func TestKitchenTicket_OnlineOrderSameFormat(t *testing.T) {
	snap := counterOrder()
	snap.Source, snap.Fulfillment.Type = "online", "delivery"
	n := int64(1104)
	snap.KitchenNumber = &n
	got := BuildStation(snap, pizzaTicket(snap))
	if !bytes.HasPrefix(got, []byte(cmdInit+cmdCenter+cmdBoldOn+cmdDoubleSz+"DELIVERY\n")) ||
		!bytes.Contains(got, []byte(cmdHugeSz+"1104\n")) || !bytes.Contains(got, []byte(otherPrintersHead)) {
		t.Fatalf("online delivery:\n%q", got)
	}
}

// A pickup moved to a delivery after SEND (terminal §2.10d `type`): the new type on top, a NOW
// DELIVERY band and "(WAS PICKUP)"; with no line changed, the station's lines plain under it.
func TestKitchenTicket_TypeChange(t *testing.T) {
	snap := kitchenSnap("delivery", order.SourceCounter, "Ada", 14, kitchenLine(1, "Cheese Pizza", "Large", "p-pizza"))
	tk := printer.Ticket{Kind: printer.TicketStation, TargetID: "p-pizza", Change: true, TypeOnly: true, WasType: "pickup", Mine: snap.Items}
	got := string(BuildStation(snap, tk))
	band := cmdDoubleSz + cmdInvertOn + pad("NOW DELIVERY", defaultLayout.big()) + cmdInvertOff
	top, b, was, item := strings.Index(got, "DELIVERY\n"), strings.Index(got, band), strings.Index(got, "(WAS PICKUP)"), strings.Index(got, "1   Cheese Pizza (Large)\n")
	if top < 0 || !(top < b && b < was && was < item) {
		t.Fatalf("top %d < band %d < was %d < item %d:\n%q", top, b, was, item, got)
	}
	if strings.Contains(got, "ADD / AGREGAR") || strings.Contains(got, otherPrintersHead) {
		t.Errorf("a type-only ticket has no ADD band and no other items:\n%q", got)
	}
	// With lines added too: the band first, then ADD.
	tk = printer.Ticket{Kind: printer.TicketStation, TargetID: "p-pizza", Change: true, WasType: "pickup", Mine: snap.Items}
	got = string(BuildStation(snap, tk))
	if b, a := strings.Index(got, band), strings.Index(got, "ADD / AGREGAR"); b < 0 || a < b {
		t.Errorf("band %d before ADD %d:\n%q", b, a, got)
	}
}
