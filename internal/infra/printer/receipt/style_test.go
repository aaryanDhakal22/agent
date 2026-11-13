package receipt

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
)

// receipt_prefs.style_online / style_store (2.5.2): softtouch (the slip) or classic (the receipt the Pi
// printed before: buildFull, the money fixes in it), chosen by the order's source; goldens at 48 / 42 / 32.
func TestReceiptStyles(t *testing.T) {
	sc := deliveryStoreCtx()
	classicStore := order.DefaultReceiptPrefs()
	classicStore.StyleStore = order.StyleClassic
	classicOnline := order.DefaultReceiptPrefs()
	classicOnline.StyleOnline = order.StyleClassic
	for _, cols := range []int{48, 42, 32} {
		store := slipSnap("tip") // a store order (counter)
		online := slipSnap("cash")
		online.Source = order.SourceOnline
		rec := printer.Ticket{Kind: printer.TicketReceipt, Cols: cols}
		cop := printer.Ticket{Kind: printer.TicketDeliveryCopies, Cols: cols}
		isSlip := func(b []byte) bool { return bytes.Contains(b, []byte("Customer Copy")) }

		// The store order: classic with style_store, the slip with style_online only.
		got := Build(store, sc, rec, classicStore)
		if isSlip(got) || !bytes.Equal(got, buildFull(localize(scrubSnapshot(store), sc), scrubStore(sc), "", classicStore, layoutFor(cols))) {
			t.Errorf("%d: a store order with style_store classic is the classic receipt", cols)
		}
		if !isSlip(Build(store, sc, rec, classicOnline)) {
			t.Errorf("%d: style_online leaves a store order on the slip", cols)
		}
		// The online order: classic with style_online only.
		if isSlip(Build(online, sc, rec, classicOnline)) || !isSlip(Build(online, sc, rec, classicStore)) {
			t.Errorf("%d: an online order follows style_online", cols)
		}
		// Classic delivery copies: the classic receipt twice, the money fixes kept (the tip, the card).
		copies := Build(store, sc, cop, classicStore)
		txt := textOf(copies)
		if isSlip(copies) || strings.Count(string(copies), cmdCut) < 2 || !strings.Contains(txt, "Tip") ||
			!strings.Contains(txt, "1111") {
			t.Errorf("%d: classic copies\n%s", cols, txt)
		}
		// No Directions on any slip (the owner, 2.5.2: drivers go by the street): the classic layout never
		// printed the address's notes; keep it so.
		if store.Fulfillment.Address == nil || store.Fulfillment.Address.Notes == "" {
			t.Fatal("the fixture needs directions")
		}
		if strings.Contains(textOf(got)+txt, store.Fulfillment.Address.Notes) || strings.Contains(textOf(got)+txt, "Directions") {
			t.Errorf("%d: classic prints the directions", cols)
		}
		golden(t, fmt.Sprintf("testdata/slips/classic_receipt_%d.golden", cols), got)
		golden(t, fmt.Sprintf("testdata/slips/classic_copies_%d.golden", cols), copies)
	}
	// Validation + defaults.
	if d := order.DefaultReceiptPrefs(); d.StyleOnline != order.StyleSoftTouch || d.StyleStore != order.StyleSoftTouch {
		t.Error("defaults are softtouch")
	}
	bad := order.DefaultReceiptPrefs()
	bad.StyleStore = "fancy"
	if bad.Validate() == nil {
		t.Error("style must be softtouch or classic")
	}
}
