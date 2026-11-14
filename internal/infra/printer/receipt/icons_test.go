package receipt

import (
	"bytes"
	"strings"
	"testing"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
)

// iconsAsText reads the type lines' pictures back as the fake printer does:
// "PICKUP [icon bag]".
func iconsAsText(b []byte) []byte {
	for _, w := range []string{"PICKUP", "COUNTER", "DELIVERY"} {
		name := "bag"
		if w == "DELIVERY" {
			name = "car"
		}
		b = bytes.ReplaceAll(b, []byte(typeLine(w, true)), []byte(w+" [icon "+name+"]\n"))
	}
	return b
}

// The owner, 2026-11-14 (#16): after Pickup / Counter one space and a
// line-art bag, after Delivery one space and a line-art car, as one GS v 0
// picture 24 dots tall: the word 12 dots a character, a blank 12-dot space,
// then the icon.
func TestTypeLineIcons(t *testing.T) {
	for _, c := range []struct {
		word string
		icon []string
	}{{"PICKUP", iconBag}, {"COUNTER", iconBag}, {"DELIVERY", iconCar}} {
		raw := typeLine(c.word, true)
		if !strings.HasPrefix(raw, "\x1dv0\x00") || strings.HasSuffix(raw, "\n") {
			t.Fatalf("%s: not a GS v 0 picture of its own: %q", c.word, raw[:8])
		}
		wb, rows := int(raw[4])|int(raw[5])<<8, int(raw[6])|int(raw[7])<<8
		x0 := (len(c.word) + 1) * 12
		if rows != 24 || wb != (x0+len(c.icon[0])+7)/8 || len(raw) != 8+wb*rows {
			t.Fatalf("%s: %d x %d bytes", c.word, wb, rows)
		}
		data := []byte(raw[8:])
		dot := func(x, y int) bool { return data[y*wb+x/8]&(0x80>>(x%8)) != 0 }
		for y, row := range c.icon {
			for x, ch := range row {
				if dot(x0+x, y) != (ch == '#') {
					t.Fatalf("%s: the icon's dot (%d,%d) differs", c.word, x, y)
				}
			}
			for x := x0 - 12; x < x0; x++ {
				if dot(x, y) {
					t.Fatalf("%s: the space before the icon has ink at (%d,%d)", c.word, x, y)
				}
			}
		}
		// The word is the strike font's (the print checks read it back): the first letter has ink.
		ink := false
		for y := 0; y < 24; y++ {
			for x := 0; x < 12; x++ {
				ink = ink || dot(x, y)
			}
		}
		if !ink {
			t.Errorf("%s: no word drawn", c.word)
		}
		// A printer without pictures (strike_images off): the word as text.
		if got := typeLine(c.word, false); got != c.word+"\n" {
			t.Errorf("%s without pictures = %q", c.word, got)
		}
	}
	if typeIcon("DINE IN") != nil || typeIcon("ORDER") != nil {
		t.Error("only pickup, counter and delivery carry an icon")
	}
	for _, art := range [][]string{iconBag, iconCar} {
		for _, r := range art {
			if len(r) != len(art[0]) || len(art) != 24 {
				t.Fatal("an icon is 24 rows of one width")
			}
		}
	}
}

// Where the owner sees it: the slips print the icon line right after the
// Order #, a delivery's two copies the car, a pickup's receipt the bag; the
// Pi's strike_images off prints the word alone.
func TestSlipsPrintTheTypeIcon(t *testing.T) {
	sc, prefs := deliveryStoreCtx(), order.DefaultReceiptPrefs()
	copies := Build(deliverySnap(true), sc, printer.Ticket{Kind: printer.TicketDeliveryCopies, Station: "Front"}, prefs)
	if n := bytes.Count(copies, []byte(typeLine("DELIVERY", true))); n != 2 {
		t.Errorf("the delivery copies carry %d car lines, want 2", n)
	}
	pickup := counterOrder()
	pickup.Fulfillment.Type = "pickup"
	rcpt := Build(pickup, sc, printer.Ticket{Kind: printer.TicketReceipt, Station: "Front"}, prefs)
	if !bytes.Contains(rcpt, []byte("Order #: 12\n"+gsNormal+cmdBoldOff+cmdFeed+typeLine("PICKUP", true))) {
		t.Errorf("the bag line right after the Order #:\n%q", rcpt)
	}
	prefs.StrikeImages = false
	if plain := Build(pickup, sc, printer.Ticket{Kind: printer.TicketReceipt, Station: "Front"}, prefs); bytes.Contains(plain, []byte("\x1dv0\x00\x0e")) ||
		!bytes.Contains(plain, []byte("\nPICKUP\n")) {
		t.Errorf("without pictures: the word alone")
	}
	// Kitchen tickets keep their own type line (no icon).
	if st := BuildStation(pickup, pizzaTicket(pickup)); bytes.Contains(st, []byte(typeLine("PICKUP", true))) {
		t.Error("a kitchen ticket carries the icon")
	}
}
