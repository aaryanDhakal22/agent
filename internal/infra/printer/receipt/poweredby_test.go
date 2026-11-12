package receipt

import (
	"bytes"
	"image"
	"image/color"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
)

var gsv0 = []byte{0x1d, 0x76, 0x30, 0x00}

func TestPoweredBy_Footer(t *testing.T) {
	f := []byte(poweredByFooter(defaultLayout))
	if !bytes.HasPrefix(f, []byte(cmdFeed+cmdCenter+cmdFontBSz+"powered by"+cmdFeed)) {
		t.Fatalf("starts with the small, centred powered by: %q", f[:24])
	}
	at := bytes.Index(f, gsv0)
	if at < 0 {
		t.Fatal("no GS v 0 logo")
	}
	wb, h := int(f[at+4])|int(f[at+5])<<8, int(f[at+6])|int(f[at+7])<<8
	if wb != 72 {
		t.Errorf("576 dots = 72 bytes a row, got %d", wb)
	}
	if h < 40 || h >= 96 {
		t.Errorf("the 96-row logo without its blank rows, got %d rows", h)
	}
	if rest := f[at+8+wb*h:]; string(rest) != cmdBaseSz+cmdLeft {
		t.Errorf("base size + left after the logo, got %q", rest)
	}
	first, last := f[at+8:at+8+wb], f[at+8+wb*(h-1):at+8+wb*h]
	if bytes.Count(first, []byte{0}) == wb || bytes.Count(last, []byte{0}) == wb {
		t.Error("the first and last rows have ink")
	}
}

func TestPoweredBy_Raster(t *testing.T) {
	img := image.NewGray(image.Rect(0, 0, 10, 4)) // black on rows 1-2 only
	for x := 0; x < 10; x++ {
		for y := 0; y < 4; y++ {
			img.SetGray(x, y, color.Gray{Y: 255})
		}
	}
	img.SetGray(0, 1, color.Gray{})
	img.SetGray(9, 2, color.Gray{})
	got := rasterGSv0(img, img.Bounds())
	want := []byte{0x1d, 0x76, 0x30, 0x00, 2, 0, 2, 0, 0x80, 0x00, 0x00, 0x40}
	if !bytes.Equal(got, want) {
		t.Fatalf("raster = % x, want % x", got, want)
	}
	blank := image.NewGray(image.Rect(0, 0, 8, 2))
	for i := range blank.Pix {
		blank.Pix[i] = 255
	}
	if got := rasterGSv0(blank, blank.Bounds()); got != nil {
		t.Errorf("a blank image prints nothing, got % x", got)
	}
}

// Every customer receipt copy ends with it; the kitchen's and the staff's
// slips never have it.
func TestPoweredBy_OnlyCustomerReceipts(t *testing.T) {
	sc := previewStore()
	foot := []byte(poweredByFooter(defaultLayout))
	two := prefsWith(func(p *order.ReceiptPrefs) { p.Copies = 2 })
	for name, c := range map[string]struct {
		b    []byte
		want int
	}{
		"delivery":   {Build(previewDelivery(), sc, receiptTicket(), order.DefaultReceiptPrefs()), 1},
		"pickup":     {Build(previewPickup(), sc, printer.Ticket{Kind: printer.TicketReceipt, ReroutedFrom: "Pizza"}, order.DefaultReceiptPrefs()), 1},
		"two copies": {Build(previewPickup(), sc, receiptTicket(), two), 2},
		"no footer":  {Build(previewPickup(), sc, receiptTicket(), prefsWith(func(p *order.ReceiptPrefs) { p.ExtraPaper = false })), 1},
		"sample":     {BuildSample(sc, order.DefaultReceiptPrefs(), time.Date(2026, 11, 12, 18, 0, 0, 0, time.UTC), 0), 1},
		"station":    {Build(previewPickup(), sc, printer.Ticket{Kind: printer.TicketStation, Station: "Pizza", Mine: previewPickup().Items}, order.DefaultReceiptPrefs()), 0},
		"test slip":  {BuildTestSlip("Online", sc, "", time.Date(2026, 11, 12, 18, 0, 0, 0, time.UTC), 0), 0},
		"driver report": {func() []byte {
			rep, rsc := reportFixture()
			return BuildDriverReport(rep, rsc, "", printer.Paper{}, printed)
		}(), 0},
	} {
		if n := bytes.Count(c.b, foot); n != c.want {
			t.Errorf("%s: powered by %d times, want %d", name, n, c.want)
		}
	}
}
