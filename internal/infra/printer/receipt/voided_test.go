package receipt

import (
	"bytes"
	"strings"
	"testing"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
)

// A voided / cancelled order's receipt (a reprint, the owner 2026-11-12:
// "removed / void items are stylized with strike through"): the banner, and
// every item / modifier / note line as a struck GS v 0 picture instead of its
// text, in SoftTouch's slip like every receipt (2026-11-12); strike_images
// off = the text with a reversed VOID tag over each item. Money lines stay text.
func TestVoidedReceipt_StruckThrough(t *testing.T) {
	f := halfCases[5].item
	f.Notes = "well done"
	snap := halfSnap(f, order.LineItem{Name: "Soda", Quantity: 1, UnitPrice: 199, Routing: order.RoutingMatched, PrinterID: "p-pizza"})
	snap.Voided = "VOID"
	sc := previewStore()
	prefs := order.DefaultReceiptPrefs()
	prefs.Format = order.FormatCondensed // a voided receipt is always expanded
	got := Build(snap, sc, printer.Ticket{Kind: printer.TicketReceipt}, prefs)
	if !bytes.Contains(got, []byte(cmdInvertOn+"  VOID  "+cmdInvertOff)) || !bytes.Contains(got, []byte("Nothing on this order is charged")) {
		t.Fatalf("no VOID banner\n%q", got)
	}
	for _, text := range []string{"Soda", "Cheese Pizza", "NO Oregano", "1st: Pepperoni", "Note: well done"} {
		if bytes.Contains(got, []byte(text)) {
			t.Errorf("%q printed as text, not struck", text)
		}
	}
	// SoftTouch's slip (the owner, 2026-11-12): the item line, its 2 priced choices (a $0 one doesn't
	// print on a customer slip, 2.5.2), the note, Soda = 5 struck pictures (GS v 0 m=0) besides the
	// powered-by logo.
	if n := bytes.Count(got, []byte("\x1dv0\x00")) - 1; n != 5 {
		t.Errorf("%d struck pictures, want 5\n%q", n, got)
	}
	if !bytes.Contains(got, []byte("VOID - NOT CHARGED")) || bytes.Contains(got, []byte("Collect Cash")) {
		t.Errorf("a voided slip says NOT CHARGED, never Collect Cash")
	}
	if !bytes.Contains(got, []byte("Total:")) {
		t.Errorf("the totals went")
	}
	golden(t, "testdata/receipt_voided.golden", got)

	prefs.StrikeImages = false
	got = Build(snap, sc, printer.Ticket{Kind: printer.TicketReceipt}, prefs)
	if bytes.Count(got, []byte("\x1dv0\x00")) != 1 || bytes.Count(got, []byte(cmdInvertOn+" VOID "+cmdInvertOff)) != 2 ||
		!bytes.Contains(got, []byte("SODA")) {
		t.Errorf("fallback: no pictures, a VOID tag per item, the text\n%q", got)
	}
	snap.Voided = "CANCELLED"
	got = Build(snap, sc, printer.Ticket{Kind: printer.TicketReceipt}, order.DefaultReceiptPrefs())
	if !bytes.Contains(got, []byte("  CANCELLED  ")) {
		t.Errorf("a cancelled online order says CANCELLED")
	}
}

// strikeRaster: 12 dots a character, 24 rows, the line through the text only
// (an indent stays clear), m = 2 when tall.
func TestStrikeRaster(t *testing.T) {
	r := strikeRaster("  AB", true)
	head := "\x1dv0\x02" + string([]byte{6, 0, 24, 0}) // 48 dots = 6 bytes
	if !strings.HasPrefix(r, head) || len(r) != len(head)+6*24 {
		t.Fatalf("header / size %q", r[:12])
	}
	data := []byte(r[len(head):])
	row := func(y int) []byte { return data[y*6 : y*6+6] }
	for y := strikeTop; y <= strikeBottom; y++ {
		if !bytes.Equal(row(y), []byte{0, 0, 0, 0xff, 0xff, 0xff}) {
			t.Errorf("row %d % x: the line covers AB, not the indent", y, row(y))
		}
	}
	if strings.HasPrefix(strikeRaster("A", false), "\x1dv0\x02") {
		t.Error("not tall = m 0")
	}
	if blank := strikeRaster("   ", false); strings.IndexByte(blank[8:], 0xff) >= 0 {
		t.Error("spaces only: nothing drawn")
	}
}
