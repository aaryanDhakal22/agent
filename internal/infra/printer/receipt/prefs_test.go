package receipt

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
	"quiccpos/agent/internal/domain/storeconfig"
)

// The customer receipt with default prefs is pinned byte-for-byte: the
// receipt settings must not move a single byte until someone changes one.
func TestBuildReceipt_Golden(t *testing.T) {
	sc := previewStore()
	cases := map[string][]byte{
		"testdata/receipt_delivery.golden": Build(previewDelivery(), sc, printer.Ticket{Kind: printer.TicketReceipt}, order.DefaultReceiptPrefs()),
		"testdata/receipt_pickup.golden":   Build(previewPickup(), sc, printer.Ticket{Kind: printer.TicketReceipt, ReroutedFrom: "Pizza"}, order.DefaultReceiptPrefs()),
	}
	for path, got := range cases {
		if *update {
			if err := os.WriteFile(path, got, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%v (run with -update to create)", err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: receipt bytes changed (run with -update if intended)\ngot:  %q\nwant: %q", path, got, want)
		}
	}
}

func prefsWith(edit func(*order.ReceiptPrefs)) order.ReceiptPrefs {
	p := order.DefaultReceiptPrefs()
	edit(&p)
	return p
}

func strPtr(s string) *string { return &s }

func receiptTicket() printer.Ticket { return printer.Ticket{Kind: printer.TicketReceipt} }

func stationTicket() printer.Ticket {
	snap := previewDelivery()
	return printer.Ticket{Kind: printer.TicketStation, TargetID: "p-pizza", Station: "Pizza",
		Mine: snap.Items[:1], Others: snap.Items[1:]}
}

// Defaults print exactly what Build printed before the settings existed — the
// same bytes BuildWith/BuildStation (the default wrappers) produce.
func TestBuild_DefaultsMatchTheDefaultBuilders(t *testing.T) {
	sc := previewStore()
	def := order.DefaultReceiptPrefs()
	if !bytes.Equal(Build(previewDelivery(), sc, receiptTicket(), def), BuildWith(previewDelivery(), sc)) {
		t.Error("receipt: default prefs differ from BuildWith")
	}
	snap, tk := stationFixture()
	if !bytes.Equal(Build(snap, storeconfig.StoreContext{}, tk, def), BuildStation(snap, tk)) {
		t.Error("station: default prefs differ from BuildStation")
	}
	// A zero-valued size/copies falls back to the default too.
	zero := def
	zero.KitchenItemSize, zero.KitchenNumberSize, zero.FooterSize, zero.Copies = "", "", "", 0
	if !bytes.Equal(Build(snap, storeconfig.StoreContext{}, tk, zero), BuildStation(snap, tk)) {
		t.Error("station: empty sizes should print like the defaults")
	}
}

func TestBuild_KitchenItemSize(t *testing.T) {
	snap := previewDelivery()
	for size, want := range map[string]string{
		order.SizeNormal: cmdBoldOn + cmdBaseSz + "1 Pepperoni Pizza (Large)\n",
		order.SizeLarge:  cmdBoldOn + cmdDoubleSz + "1 Pepperoni Pizza\n  (Large)\n",
		// 16 characters a line at quad size.
		order.SizeXLarge: cmdBoldOn + cmdQuadSz + "1 Pepperoni\n  Pizza (Large)\n",
	} {
		got := string(Build(snap, previewStore(), stationTicket(), prefsWith(func(p *order.ReceiptPrefs) { p.KitchenItemSize = size })))
		if !strings.Contains(got, want) {
			t.Errorf("%s: kitchen item should print as %q\n%q", size, want, got)
		}
	}
}

func TestBuild_KitchenNumberSize(t *testing.T) {
	snap := previewDelivery()
	for size, cmd := range map[string]string{order.SizeLarge: cmdDoubleSz, order.SizeXLarge: cmdQuadSz, order.SizeHuge: cmdHugeSz} {
		got := Build(snap, previewStore(), stationTicket(), prefsWith(func(p *order.ReceiptPrefs) { p.KitchenNumberSize = size }))
		if !bytes.Contains(got, []byte(cmdBoldOn+cmd+"#1123\n")) {
			t.Errorf("%s: kitchen number should print with %q: %q", size, cmd, got)
		}
	}
	// The number size is a kitchen-ticket setting: the receipt keeps quad.
	got := Build(snap, previewStore(), receiptTicket(), prefsWith(func(p *order.ReceiptPrefs) { p.KitchenNumberSize = order.SizeHuge }))
	if !bytes.Contains(got, []byte(cmdQuadSz+"#1123\n")) || bytes.Contains(got, []byte(cmdHugeSz)) {
		t.Error("the customer receipt's number doesn't follow the kitchen number size")
	}
}

func TestBuild_FooterFallbackChain(t *testing.T) {
	snap := previewDelivery()
	withFooter := previewStore()
	withFooter.Location.Config = map[string]any{"receipt_footer": "THANKS FOR ORDERING"}
	cases := []struct {
		name   string
		sc     storeconfig.StoreContext
		footer *string
		want   string // "" = no footer line at all
	}{
		{"default, no location footer", previewStore(), nil, "ONLINE ORDER"},
		{"default, location footer", withFooter, nil, "THANKS FOR ORDERING"},
		{"setting wins", withFooter, strPtr("THANK YOU"), "THANK YOU"},
		{"empty = none", withFooter, strPtr(""), ""},
	}
	for _, c := range cases {
		got := string(Build(snap, c.sc, receiptTicket(), prefsWith(func(p *order.ReceiptPrefs) { p.FooterText = c.footer })))
		end := got[strings.LastIndex(got, "Order Total:"):]
		if c.want == "" {
			for _, f := range []string{"ONLINE ORDER", "THANKS FOR ORDERING", cmdQuadSz} {
				if strings.Contains(end, f) {
					t.Errorf("%s: no footer expected, found %q", c.name, f)
				}
			}
			continue
		}
		if !strings.Contains(end, cmdBoldOn+cmdQuadSz+cmdCenter+c.want+cmdBoldOff) {
			t.Errorf("%s: footer %q missing: %q", c.name, c.want, end)
		}
	}
}

func TestBuild_FooterSize(t *testing.T) {
	for size, cmd := range map[string]string{order.SizeNormal: cmdBaseSz, order.SizeLarge: cmdDoubleSz, order.SizeXLarge: cmdQuadSz} {
		got := Build(previewDelivery(), previewStore(), receiptTicket(), prefsWith(func(p *order.ReceiptPrefs) { p.FooterSize = size }))
		if !bytes.Contains(got, []byte(cmdBoldOn+cmd+cmdCenter+"ONLINE ORDER")) {
			t.Errorf("%s: footer should print with %q", size, cmd)
		}
	}
}

func TestBuild_Copies(t *testing.T) {
	one := Build(previewDelivery(), previewStore(), receiptTicket(), order.DefaultReceiptPrefs())
	two := Build(previewDelivery(), previewStore(), receiptTicket(), prefsWith(func(p *order.ReceiptPrefs) { p.Copies = 2 }))
	if !bytes.Equal(two, append(append([]byte{}, one...), one...)) {
		t.Fatal("copies=2 prints the whole receipt (with its cuts) twice")
	}
	if n := bytes.Count(two, []byte(cmdCut)); n != 4 {
		t.Errorf("two copies with extra paper = 4 cuts, got %d", n)
	}
	// Kitchen tickets never multiply.
	st := Build(previewDelivery(), previewStore(), stationTicket(), prefsWith(func(p *order.ReceiptPrefs) { p.Copies = 2 }))
	if bytes.Count(st, []byte(cmdCut)) != 1 {
		t.Error("a kitchen ticket prints once whatever the copies setting")
	}
}

func TestBuild_HideStoreAddressAndPhone(t *testing.T) {
	got := string(Build(previewDelivery(), previewStore(), receiptTicket(), order.DefaultReceiptPrefs()))
	for _, want := range []string{"Lombardi's Pizza\n", "9 Main St\n", "Baltimore, MD 21201\n", "Phone: 410-555-1234"} {
		if !strings.Contains(got, want) {
			t.Errorf("default receipt missing %q", want)
		}
	}
	got = string(Build(previewDelivery(), previewStore(), receiptTicket(), prefsWith(func(p *order.ReceiptPrefs) {
		p.ShowStoreAddress, p.ShowPhone = false, false
	})))
	for _, gone := range []string{"Lombardi's Pizza", "9 Main St", "Baltimore, MD 21201", "Phone:", "410-555-1234"} {
		if strings.Contains(got, gone) {
			t.Errorf("hidden line still printed: %q", gone)
		}
	}
	// The rest of the receipt is untouched.
	for _, want := range []string{"GONZALEZ, MARIA", "St: 1234 Harford Rd", "Notes: Ring the bell", "Total:", "ONLINE ORDER"} {
		if !strings.Contains(got, want) {
			t.Errorf("receipt lost %q", want)
		}
	}
}

func TestBuild_NoExtraPaper(t *testing.T) {
	got := Build(previewDelivery(), previewStore(), receiptTicket(), prefsWith(func(p *order.ReceiptPrefs) { p.ExtraPaper = false }))
	if n := bytes.Count(got, []byte(cmdCut)); n != 1 {
		t.Fatalf("one cut without extra paper, got %d", n)
	}
	if !bytes.HasSuffix(got, []byte("ONLINE ORDER"+cmdBoldOff+cmdBaseSz+"\n\n\n"+cmdCut)) {
		t.Errorf("footer, 3 line feeds, one cut: %q", got[len(got)-40:])
	}
	def := Build(previewDelivery(), previewStore(), receiptTicket(), order.DefaultReceiptPrefs())
	if !bytes.HasSuffix(def, []byte(cmdCut+"\n\n\n\n\n"+cmdCut)) {
		t.Error("default ending is cut, feed, cut")
	}
}

func TestBuildSample(t *testing.T) {
	at := time.Date(2026, 10, 21, 22, 0, 0, 0, time.UTC)
	p := prefsWith(func(p *order.ReceiptPrefs) { p.Copies = 2; p.FooterText = strPtr("THANK YOU") })
	got := string(BuildSample(previewStore(), p, at))
	if n := strings.Count(got, "THANK YOU"); n != 2 {
		t.Errorf("footer on both receipt copies, got %d", n)
	}
	if n := strings.Count(got, " CUSTOMER, SAMPLE "); n != 2 {
		t.Errorf("two receipt copies, got %d", n)
	}
	for _, want := range []string{"#1234\n", "CUSTOMER, SAMPLE\n", "  - Note: extra ranch please", "ITEMS TO OTHER PRINTERS", "SAMPLE - not a real order", "PAID - CARD"} {
		if !strings.Contains(got, want) {
			t.Errorf("sample missing %q", want)
		}
	}
	if strings.Contains(got, "UNMATCHED") {
		t.Error("the sample kitchen ticket has no unmatched items")
	}
	// 2 receipt copies x 2 cuts + the kitchen ticket's cut.
	if n := strings.Count(got, cmdCut); n != 5 {
		t.Errorf("cuts: got %d want 5", n)
	}
}
