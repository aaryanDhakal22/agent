package receipt

import (
	"bytes"
	"testing"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
)

// main's takeout_two_copies (Store › Settings › Receipts and printing, the
// owner 2026-11-13; contract agent §2.10ah): a pickup's customer receipt
// prints two copies (each cut) when on, one when off, and a counter order's
// too (the owner 2026-11-14); the receipt Copies still counts (max); a
// reprint, a delivery's Merchant + Customer Copy and a kitchen ticket don't
// change.
func TestTakeoutTwoCopies(t *testing.T) {
	sc := previewStore()
	on := prefsWith(func(p *order.ReceiptPrefs) { p.TakeoutTwoCopies = true })
	off := order.DefaultReceiptPrefs()
	pickup := previewPickup()
	pickup.Source = "web"
	for _, kind := range []printer.TicketKind{printer.TicketReceipt, printer.TicketFull} {
		tk := printer.Ticket{Kind: kind, Cols: 42}
		one := Build(pickup, sc, tk, off)
		two := Build(pickup, sc, tk, on)
		if !bytes.Equal(two, append(append([]byte{}, one...), one...)) {
			t.Errorf("%s: a pickup with takeout_two_copies on prints the whole receipt (with its cuts) twice", kind)
		}
		if c1, c2 := bytes.Count(one, []byte(cmdCut)), bytes.Count(two, []byte(cmdCut)); c1 == 0 || c2 != 2*c1 {
			t.Errorf("%s: cuts off %d, on %d (each copy cut)", kind, c1, c2)
		}
		// The receipt Copies setting still counts: max(copies, 2).
		both := Build(pickup, sc, tk, prefsWith(func(p *order.ReceiptPrefs) { p.TakeoutTwoCopies, p.Copies = true, 2 }))
		if !bytes.Equal(both, two) {
			t.Errorf("%s: copies 2 + takeout on = 2 copies, not 4", kind)
		}
	}

	// Every source's pickup: online (vendor), website, call center / terminal (counter).
	for _, src := range []string{"", order.SourceOnline, "web", order.SourceCounter} {
		s := previewPickup()
		s.Source = src
		if n := CustomerCopies(s, printer.Ticket{Kind: printer.TicketReceipt}, on); n != 2 {
			t.Errorf("source %q pickup: %d copies, want 2", src, n)
		}
		if n := CustomerCopies(s, printer.Ticket{Kind: printer.TicketReceipt}, off); n != 1 {
			t.Errorf("source %q pickup, setting off: %d copies, want 1", src, n)
		}
	}

	// A reprint keeps printing one.
	rp := printer.Ticket{Kind: printer.TicketReceipt, Reprint: true}
	if a, b := Build(pickup, sc, rp, off), Build(pickup, sc, rp, on); !bytes.Equal(a, b) {
		t.Error("a reprint prints one copy whatever takeout_two_copies says")
	}
	if n := CustomerCopies(pickup, rp, on); n != 1 {
		t.Errorf("reprint: %d copies, want 1", n)
	}

	// A counter order (the owner 2026-11-14: "all the counter orders ... always
	// prints like 2 receipts"): two too, a reprint one.
	for _, c := range []struct{ src, typ string }{{order.SourceCounter, "dine_in"}, {order.SourceCounter, "counter"}, {"pos", "dine_in"}, {"", "counter"}} {
		counter := previewPickup()
		counter.Source, counter.Fulfillment.Type = c.src, c.typ
		tk := printer.Ticket{Kind: printer.TicketReceipt}
		one, two := Build(counter, sc, tk, off), Build(counter, sc, tk, on)
		if !bytes.Equal(two, append(append([]byte{}, one...), one...)) {
			t.Errorf("counter order %+v: two copies when on", c)
		}
		if n := CustomerCopies(counter, printer.Ticket{Kind: printer.TicketReceipt, Reprint: true}, on); n != 1 {
			t.Errorf("counter order %+v reprint: %d copies, want 1", c, n)
		}
	}
	// An online dine-in order (not rung at the counter): unchanged.
	dine := previewPickup()
	dine.Source, dine.Fulfillment.Type = order.SourceOnline, "dine_in"
	if n := CustomerCopies(dine, printer.Ticket{Kind: printer.TicketReceipt}, on); n != 1 {
		t.Errorf("online dine-in: %d copies, want 1", n)
	}

	// A delivery: its Merchant + Customer Copy, unchanged (and its plain receipt too).
	d := deliverySnap(false)
	dc := printer.Ticket{Kind: printer.TicketDeliveryCopies}
	if a, b := Build(d, deliveryStoreCtx(), dc, off), Build(d, deliveryStoreCtx(), dc, on); !bytes.Equal(a, b) {
		t.Error("a delivery's copies don't change with takeout_two_copies")
	}
	if a, b := Build(previewDelivery(), sc, printer.Ticket{Kind: printer.TicketReceipt}, off), Build(previewDelivery(), sc, printer.Ticket{Kind: printer.TicketReceipt}, on); !bytes.Equal(a, b) {
		t.Error("a delivery's receipt doesn't double with takeout_two_copies")
	}

	// A kitchen ticket prints once.
	if a, b := Build(pickup, sc, stationTicket(), off), Build(pickup, sc, stationTicket(), on); !bytes.Equal(a, b) || bytes.Count(b, []byte(cmdCut)) != 1 {
		t.Error("a pickup's kitchen ticket prints once")
	}

	// A future pickup's receipt printed early is the customer's receipt too.
	if n := CustomerCopies(pickup, printer.Ticket{Kind: printer.TicketReceipt, Future: true}, on); n != 2 {
		t.Errorf("future pickup receipt: %d copies, want 2", n)
	}
}

// The setting is never stored with the tablet's receipt prefs nor sent back.
func TestTakeoutTwoCopiesNotInPrefsJSON(t *testing.T) {
	p, err := order.DecodeReceiptPrefs([]byte(`{"takeout_two_copies": true}`))
	if err != nil || p.TakeoutTwoCopies {
		t.Fatalf("decoded %+v, %v: the tablet can't set it", p, err)
	}
}
