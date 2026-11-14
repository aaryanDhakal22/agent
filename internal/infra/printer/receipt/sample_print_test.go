package receipt

// Owner's sample tickets (2026-11-14): a made-up delivery order rendered with
// the Pi's real delivery slip (BuildDeliveryCopies, one copy) and the store's
// real settings, then one adjustment per variant, so the owner can compare
// layouts on paper before one becomes the real layout. Skipped unless
// RECEIPT_SAMPLE_DIR is set; quicc-ops scripts/receipt-sample.sh fills that
// folder (store_config + receipt prefs + logo, read-only from the Pi), runs
// this test and sends the .bin files to a printer through iris.
//
// Variants (each a <name>.bin in the folder):
//   - asis: the slip as it prints today
//   - typeface: DELIVERY in Go Bold + the car, one size larger, centred (a picture)
//   - plain: DELIVERY in the printer's own font, one size larger (GS ! 0x11), centred, no icon
//   - kitchen: Order # and DELIVERY printed the kitchen ticket's way (font A, bold, GS ! size), centred, no icon
//   - final: the owner's pick (2026-11-14): typeface + "Order # 100" (no colon), left as today
//   - final_stacked: final with the header stacked: the logo centred at half the paper's width, the phone under it

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/draw"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
	"quiccpos/agent/internal/domain/storeconfig"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

func TestReceiptSamples(t *testing.T) {
	dir := os.Getenv("RECEIPT_SAMPLE_DIR")
	if dir == "" {
		t.Skip("RECEIPT_SAMPLE_DIR not set (scripts/receipt-sample.sh)")
	}
	var sc storeconfig.StoreContext
	raw, err := os.ReadFile(filepath.Join(dir, "sc.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &sc); err != nil {
		t.Fatal(err)
	}
	prefs := order.DefaultReceiptPrefs()
	if praw, err := os.ReadFile(filepath.Join(dir, "prefs.json")); err == nil {
		if prefs, err = order.DecodeReceiptPrefs(praw); err != nil {
			t.Fatal(err)
		}
	}
	if logo, err := os.ReadFile(filepath.Join(dir, "logo.bin")); err == nil {
		img, err := DecodeReceiptLogo(logo)
		if err != nil {
			t.Fatal(err)
		}
		SetReceiptLogo(img, "sample")
		defer SetReceiptLogo(nil, "")
	}
	cols := 42
	if c := os.Getenv("RECEIPT_SAMPLE_COLS"); c == "48" || c == "32" {
		cols = map[string]int{"48": 48, "32": 32}[c]
	}
	was := deliveryNow
	deliveryNow = time.Now
	defer func() { deliveryNow = was }()

	n := int64(100)
	s := order.Snapshot{ID: "o-sample", Source: "counter", SourceEventID: "SAMPLE-100", KitchenNumber: &n,
		PlacedAt: time.Now().UTC(), TakenBy: "Khawar",
		Customer: order.Customer{DisplayName: "Sample Customer", Phone: "4105550100"},
		Fulfillment: order.Fulfillment{Type: "delivery", Address: &order.Address{Line1: "123 Sample St", Line2: "Apt 2",
			City: "Towson", State: "MD", PostalCode: "21204", Notes: "Side door"}},
		Items: []order.LineItem{
			{Name: "16\" Cheese Pizza", Quantity: 1, UnitPrice: 1599, LineTotal: 1798, Children: []order.LineItem{
				{Name: "Pepperoni", Quantity: 1, UnitPrice: 199}}},
			{Name: "10 pc Wings", Quantity: 1, UnitPrice: 1299, LineTotal: 1299, Notes: "Extra crispy"},
			{Name: "2 Liter Soda", Quantity: 1, UnitPrice: 399, LineTotal: 399},
		},
		Money: order.Money{Subtotal: 3496, Tax: 210, Total: 4006,
			Fees: []order.Fee{{Type: "delivery", Label: "Delivery fee", Amount: 300}}},
		Notes: "Sample ticket - not a real order",
	}
	slip := BuildDeliveryCopies(s, sc, prefs, printer.Paper{Cols: cols})
	one := firstCopy(t, slip)
	typeRaster := []byte(typeLine("DELIVERY", true))
	if !bytes.Contains(one, typeRaster) {
		t.Fatal("type line picture not found")
	}
	dots := layoutFor(cols).dots()
	out := map[string][]byte{
		"asis":     one,
		"typeface": bytes.Replace(one, typeRaster, typefaceLine(t, "DELIVERY", iconCar, dots), 1),
		"plain":    bytes.Replace(one, typeRaster, []byte(cmdCenter+cmdNormalSz+cmdBoldOff+cmdDoubleSz+"DELIVERY"+cmdFeed+cmdSizeReset+cmdLeft), 1),
	}
	// kitchen: the kitchen ticket's number commands (ESC ! 0 font A, bold, GS ! size) on Order #, centred, and
	// DELIVERY the same way under it.
	num := "Order #: 100"
	oldNum := cmdBoldOn + gsBig + num + cmdFeed
	if !bytes.Contains(one, []byte(oldNum)) {
		oldNum = cmdBoldOn + gsTall + num + cmdFeed
	}
	if !bytes.Contains(one, []byte(oldNum)) {
		t.Fatalf("Order # line not found")
	}
	size := gsBig
	if strings.Contains(oldNum, gsTall) {
		size = gsTall
	}
	k := bytes.Replace(one, []byte(oldNum), []byte(cmdCenter+cmdNormalSz+cmdBoldOn+size+num+cmdFeed+cmdLeft), 1)
	k = bytes.Replace(k, typeRaster, []byte(cmdCenter+cmdNormalSz+cmdBoldOn+cmdDoubleSz+"DELIVERY"+cmdFeed+cmdSizeReset+cmdBoldOff+cmdLeft), 1)
	out["kitchen"] = k
	final := bytes.Replace(out["typeface"], []byte(num), []byte("Order # 100"), 1)
	out["final"] = final
	if hdr := headerFor(sc, layoutFor(cols)); len(hdr) > 0 && bytes.Contains(final, hdr) {
		out["final_stacked"] = bytes.Replace(final, hdr, stackedHeader(t, sc, dots), 1)
	}
	// The drinks samples (the owner 2026-11-14): 2 x 8 Wings & Fries (a can with each) + a 2 liter, in the
	// owner's pick (final). drinks_hashtags = SoftTouch's "#####" after each drink's name (a print name);
	// drinks_invert = no marks, every drink line printed white on black across the paper.
	yes := true
	combo := func(kind, sauce, dressing, can string) order.LineItem {
		return order.LineItem{Name: "8 Wings & Fries", Quantity: 1, UnitPrice: 1299, LineTotal: 1299, Children: []order.LineItem{
			{Name: kind, Quantity: 1}, {Name: sauce, Quantity: 1}, {Name: dressing, Quantity: 1},
			{Name: can, Quantity: 1, ShowOnDriverTicket: &yes}}}
	}
	drinks := s
	drinks.Items = []order.LineItem{
		combo("Regular", "Hot Sauce", "Ranch", "W Can Coke"),
		combo("Boneless", "BBQ", "Blue Cheese", "W Can Sprite"),
		{Name: "2Lt Pepsi", Quantity: 1, UnitPrice: 365, LineTotal: 365},
	}
	drinks.Money = order.Money{Subtotal: 2963, Tax: 178, Total: 3441,
		Fees: []order.Fee{{Type: "delivery", Label: "Delivery fee", Amount: 300}}}
	isDrink := map[string]bool{"W Can Coke": true, "W Can Sprite": true, "2Lt Pepsi": true}
	finalOf := func(b []byte) []byte {
		b = bytes.Replace(b, typeRaster, typefaceLine(t, "DELIVERY", iconCar, dots), 1)
		return bytes.Replace(b, []byte(num), []byte("Order # 100"), 1)
	}
	tagged := drinks
	tagged.Items = nil
	for _, it := range drinks.Items {
		it.Children = append([]order.LineItem(nil), it.Children...)
		if isDrink[it.Name] {
			it.Name += "  #####"
		}
		for i, c := range it.Children {
			if isDrink[c.Name] {
				it.Children[i].Name += " #####"
			}
		}
		tagged.Items = append(tagged.Items, it)
	}
	out["drinks_hashtags"] = finalOf(firstCopy(t, BuildDeliveryCopies(tagged, sc, prefs, printer.Paper{Cols: cols})))
	plainDrinks := finalOf(firstCopy(t, BuildDeliveryCopies(drinks, sc, prefs, printer.Paper{Cols: cols})))
	out["drinks_invert"] = invertLines(plainDrinks, cols, func(text string) bool {
		for d := range isDrink {
			if strings.Contains(strings.ToLower(text), strings.ToLower(slipItemName("", d))) ||
				strings.Contains(strings.ToLower(text), strings.ToLower(d)) {
				return true
			}
		}
		return false
	})
	for name, b := range out {
		if err := os.WriteFile(filepath.Join(dir, name+".bin"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// stackedHeader is the store's logo centred at half the paper's width with
// the phone under it, centred, in the header's own font and size.
func stackedHeader(t *testing.T, sc storeconfig.StoreContext, dots int) []byte {
	t.Helper()
	logoState.RLock()
	logo := logoState.img
	logoState.RUnlock()
	lb := logo.Bounds()
	w := dots / 2
	h := w * lb.Dy() / lb.Dx()
	scale := float64(dots) / 576
	linePx := 22 * scale
	phone := phoneParens(strings.TrimSpace(sc.Location.Phone))
	fc := face(fontRegOnce(), linePx)
	gap := int(10 * scale)
	height := h + gap + int(linePx*1.3) + int(8*scale)
	canvas := image.NewGray(image.Rect(0, 0, dots, height))
	draw.Draw(canvas, canvas.Bounds(), image.White, image.Point{}, draw.Src)
	sc2 := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(sc2, sc2.Bounds(), image.White, image.Point{}, draw.Src)
	xdraw.CatmullRom.Scale(sc2, sc2.Bounds(), logo, lb, xdraw.Over, nil)
	// The logo's beige background (232,212,185) dithers into a grey dotted box: light pixels are paper.
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if color.GrayModel.Convert(sc2.At(x, y)).(color.Gray).Y > 175 {
				sc2.Set(x, y, color.White)
			}
		}
	}
	dither(canvas, sc2, image.Pt((dots-w)/2, 0))
	pw := font.MeasureString(fc, phone).Ceil()
	d := &font.Drawer{Dst: canvas, Src: image.Black, Face: fc, Dot: fixed.P((dots-pw)/2, h+gap+int(linePx))}
	d.DrawString(phone)
	var b bytes.Buffer
	b.WriteString(cmdCenter)
	b.Write(rasterGSv0(canvas, canvas.Bounds()))
	b.WriteString(cmdLeft)
	return b.Bytes()
}

// fontRegOnce is the header's regular font (Go Regular), loaded as headerCanvas does.
func fontRegOnce() *opentype.Font {
	fontOnce.Do(func() {
		fontReg, _ = opentype.Parse(goregular.TTF)
		fontBold, _ = opentype.Parse(gobold.TTF)
	})
	return fontReg
}

// invertLines prints every text line that drink matches white on black,
// padded to the paper's width so the band spans the whole line. Only plain
// lines (escape codes before the text) are touched; pictures are left alone.
func invertLines(b []byte, cols int, drink func(string) bool) []byte {
	parts := bytes.Split(b, []byte(cmdFeed))
	for i, p := range parts {
		if bytes.Contains(p, []byte("\x1dv0")) {
			continue
		}
		// split the leading escape codes (ESC x n / GS x n) from the text
		j := 0
		for j+2 < len(p) && (p[j] == 0x1b || p[j] == 0x1d) {
			j += 3
		}
		text := string(p[j:])
		if text == "" || strings.ContainsAny(text, "\x1b\x1d") || !drink(text) {
			continue
		}
		if pad := cols - len([]rune(text)); pad > 0 {
			text += strings.Repeat(" ", pad)
		}
		parts[i] = []byte(string(p[:j]) + cmdInvertOn + text + cmdInvertOff)
	}
	return bytes.Join(parts, []byte(cmdFeed))
}

// firstCopy is the slip up to and including its first paper cut (the Merchant Copy).
func firstCopy(t *testing.T, slip []byte) []byte {
	t.Helper()
	cut := bytes.Index(slip, []byte("\x1dV"))
	if cut < 0 {
		t.Fatal("no cut")
	}
	end := cut + 3
	if slip[cut+2] == 'A' || slip[cut+2] == 'B' {
		end = cut + 4
	}
	return slip[:end]
}

// typefaceLine draws word in Go Bold at about the printer's double size (a
// 48-dot line) with icon doubled beside it, centred on a paper dots wide, as
// one GS v 0 picture.
func typefaceLine(t *testing.T, word string, icon []string, paper int) []byte {
	t.Helper()
	ft, err := opentype.Parse(gobold.TTF)
	if err != nil {
		t.Fatal(err)
	}
	face, err := opentype.NewFace(ft, &opentype.FaceOptions{Size: 46, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		t.Fatal(err)
	}
	defer face.Close()
	const h = 52
	textW := font.MeasureString(face, word).Ceil()
	const k, gap = 2, 24
	iconW, iconH := len(icon[0])*k, len(icon)*k
	total := textW + gap + iconW
	img := image.NewGray(image.Rect(0, 0, paper, h))
	draw.Draw(img, img.Bounds(), image.White, image.Point{}, draw.Src)
	x0 := (paper - total) / 2
	d := font.Drawer{Dst: img, Src: image.Black, Face: face, Dot: fixed.P(x0, 42)}
	d.DrawString(word)
	iy := (h - iconH) / 2
	for y, row := range icon {
		for x, c := range row {
			if c != '#' {
				continue
			}
			for dy := 0; dy < k; dy++ {
				for dx := 0; dx < k; dx++ {
					img.SetGray(x0+textW+gap+x*k+dx, iy+y*k+dy, color.Gray{})
				}
			}
		}
	}
	wb := (paper + 7) / 8
	data := make([]byte, wb*h)
	for y := 0; y < h; y++ {
		for x := 0; x < paper; x++ {
			if img.GrayAt(x, y).Y < 128 {
				data[y*wb+x/8] |= 0x80 >> (x % 8)
			}
		}
	}
	var b bytes.Buffer
	b.WriteString("\x1d\x76\x30\x00")
	b.Write([]byte{byte(wb % 256), byte(wb / 256), byte(h % 256), byte(h / 256)})
	b.Write(data)
	return b.Bytes()
}
