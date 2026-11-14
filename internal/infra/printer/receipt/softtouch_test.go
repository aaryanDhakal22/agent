package receipt

import (
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
)

// testLogo is a made-up logo: a dark ring with a bar, on transparency.
func testLogo() image.Image {
	img := image.NewNRGBA(image.Rect(0, 0, 160, 120))
	for y := 0; y < 120; y++ {
		for x := 0; x < 160; x++ {
			dx, dy := float64(x-80), float64(y-60)
			r := dx*dx/(70*70) + dy*dy/(52*52)
			switch {
			case r < 1 && r > 0.55, y > 52 && y < 68 && x > 20 && x < 140:
				img.Set(x, y, color.NRGBA{R: 160, G: 20, B: 20, A: 255})
			}
		}
	}
	return img
}

func slipSnap(kind string) order.Snapshot {
	d := deliverySnap(true)
	d.Source, d.SourceEventID = order.SourceCounter, "t-dev1-4f9c2a7b"
	d.SourceMeta = map[string]any{"terminal": map[string]any{"staff_name": "Cara S."}}
	at := time.Date(2026, 11, 12, 22, 40, 0, 0, time.UTC)
	switch kind {
	case "card": // a typed card, no tip yet: the signature block
		d.Money.Tip, d.Money.Total = 0, 2490
		d.Payments = []order.PaymentSummary{{Method: "card", Tender: "card", AmountCents: 2490, Last4: "5850", Brand: "visa",
			Entry: "chip", AuthCode: "188974", Ref: "sq-7Hq2KdYbWZ", ID: "0193a7c2-5b1e-7a40-9f3d-1428039aa01f", At: &at}}
	case "tip": // the call center's card with the driver's tip
		d.Money.Tip, d.Money.Total = 0, 2490
		d.TakenBy = "Call center - Cece"
		d.Payments = []order.PaymentSummary{{Method: "card", Tender: "manual_card", AmountCents: 2490, TipCents: 300, Last4: "1111",
			Entry: "keyed", Ref: "s9EkUvdx48VTZ3C3MZVZOXVpwpZZY", ID: "0193a7c2-5b1e-7a40-9f3d-1428039aa020", At: &at}}
	case "cash":
		d.Money.Tip, d.Money.Total = 0, 2490
		d.Payments = nil
	}
	return d
}

// SoftTouch's slip (the owner, 2026-11-12): goldens at 48 / 42 / 32 columns for the delivery copies and
// the pickup receipt, card with no tip (the signature block), card with a tip, cash; with and without the
// store's logo.
func TestSoftTouchSlipGoldens(t *testing.T) {
	prevNow := deliveryNow
	deliveryNow = func() time.Time { return time.Date(2026, 11, 12, 22, 41, 0, 0, time.UTC) }
	defer func() { deliveryNow = prevNow }()
	defer SetReceiptLogo(nil, "")
	sc, prefs := deliveryStoreCtx(), order.DefaultReceiptPrefs()
	for _, logo := range []bool{false, true} {
		if logo {
			SetReceiptLogo(testLogo(), `"logo-1"`)
		} else {
			SetReceiptLogo(nil, "")
		}
		for _, kind := range []string{"card", "tip", "cash"} {
			for _, cols := range []int{48, 42, 32} {
				for _, slip := range []string{"copies", "pickup"} {
					s := slipSnap(kind)
					tk := printer.Ticket{Kind: printer.TicketDeliveryCopies, Cols: cols}
					if slip == "pickup" {
						s.Fulfillment = order.Fulfillment{Type: "pickup"}
						tk.Kind = printer.TicketReceipt
					}
					name := fmt.Sprintf("slip_%s_%s_%d", slip, kind, cols)
					if logo {
						name += "_logo"
					}
					got := Build(s, sc, tk, prefs)
					golden(t, filepath.Join("testdata", "slips", name+".golden"), got)
					txt := textOf(got)
					copies := 1
					if slip == "copies" {
						copies = 2
					}
					sig := strings.Count(txt, "I agree to pay")
					if want := map[bool]int{true: copies, false: 0}[kind == "card"]; sig != want {
						t.Errorf("%s: signature block %d times, want %d\n%s", name, sig, want, txt)
					}
					hasStoreText := strings.Contains(txt, "931 Taylor Ave")
					if hasStoreText == logo {
						t.Errorf("%s: with a logo the store's details are in the raster, without it text (text: %v)", name, hasStoreText)
					}
					for _, want := range []string{"Date: 11/12/26", "Order #1104", "Phone      : (410) 555-0100", "Customer Copy", "Powered By"} {
						if !strings.Contains(txt, want) {
							t.Errorf("%s: no %q\n%s", name, want, txt)
						}
					}
					if kind == "cash" && !strings.Contains(txt, "Collect Cash $24.90") {
						t.Errorf("%s: no Collect Cash\n%s", name, txt)
					}
					if kind != "cash" && !strings.Contains(txt, "Payment Date Time:") {
						t.Errorf("%s: no payment block\n%s", name, txt)
					}
					if slip == "pickup" && strings.Contains(txt, "Street") {
						t.Errorf("%s: a pickup prints Phone only", name)
					}
				}
			}
		}
	}
	// The terminal's header: raster bytes + an ETag with a logo, nothing without.
	SetReceiptLogo(nil, "")
	if b, _ := HeaderRaster(sc, 48); b != nil {
		t.Fatal("no logo: no header")
	}
	SetReceiptLogo(testLogo(), `"logo-1"`)
	b48, e48 := HeaderRaster(sc, 48)
	b32, e32 := HeaderRaster(sc, 32)
	if len(b48) == 0 || e48 == "" || e48 == e32 || len(b32) >= len(b48) || !strings.Contains(string(b48), "\x1dv0") {
		t.Fatalf("header: %d bytes %s / %d bytes %s", len(b48), e48, len(b32), e32)
	}
	if w := int(b48[7]) | int(b48[8])<<8; w != 576/8 { // cmdCenter (3 bytes) + GS v 0 m, then xL xH
		t.Fatalf("48 columns = 576 dots: %d bytes a row", w)
	}
}

// SLIPS_DIR=dir go test -run TestDumpSlips: the owner's examples (text + the header as a PNG).
func TestDumpSlips(t *testing.T) {
	dir := os.Getenv("SLIPS_DIR")
	if dir == "" {
		t.Skip()
	}
	_ = os.MkdirAll(dir, 0o755)
	sc, prefs := deliveryStoreCtx(), order.DefaultReceiptPrefs()
	defer SetReceiptLogo(nil, "")
	write := func(name string, b []byte) {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	SetReceiptLogo(testLogo(), `"logo-1"`)
	cp := Build(slipSnap("tip"), sc, printer.Ticket{Kind: printer.TicketDeliveryCopies, Cols: 42}, prefs)
	txt := textOf(cp)
	write("1-delivery-copies-with-logo-42.txt", []byte("[header raster: the logo centred, the phone under it, see 1-header-42.png]\n"+txt))
	write("1-delivery-copies-with-logo-42.bin", cp)
	lay := layoutFor(42)
	img := headerImage(sc, lay)
	if f, err := os.Create(filepath.Join(dir, "1-header-42.png")); err == nil {
		_ = png.Encode(f, img)
		_ = f.Close()
	}
	SetReceiptLogo(nil, "")
	pk := slipSnap("cash")
	pk.Fulfillment = order.Fulfillment{Type: "pickup"}
	b := Build(pk, sc, printer.Ticket{Kind: printer.TicketReceipt, Cols: 42}, prefs)
	write("2-pickup-receipt-no-logo-42.txt", []byte(textOf(b)))
	write("2-pickup-receipt-no-logo-42.bin", b)
	c := slipSnap("card")
	c.Fulfillment = order.Fulfillment{Type: "pickup"}
	b = Build(c, sc, printer.Ticket{Kind: printer.TicketReceipt, Cols: 48}, prefs)
	write("3-card-no-tip-signature-48.txt", []byte(textOf(b)))
	write("3-card-no-tip-signature-48.bin", b)
}
