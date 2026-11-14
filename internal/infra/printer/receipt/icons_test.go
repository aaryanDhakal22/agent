package receipt

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
)

// iconsAsText reads the type lines' pictures back as the fake printer does:
// "PICKUP [icon bag]", at every paper width.
func iconsAsText(b []byte) []byte {
	for _, w := range []string{"PICKUP", "COUNTER", "DELIVERY"} {
		name := "bag"
		if w == "DELIVERY" {
			name = "car"
		}
		for _, cols := range []int{Cols80, Cols72, Cols58} {
			b = bytes.ReplaceAll(b, []byte(typeLine(w, true, layoutFor(cols))), []byte(w+" [icon "+name+"]\n"))
		}
	}
	return b
}

// The owner, 2026-11-14 (#16, then the receipt design): the type's word in Go
// Bold at 46 px, a 24-dot gap and the icon at twice its size, as ONE GS v 0
// picture as wide as the paper and 52 dots tall, the group centred.
func TestTypeLineIcons(t *testing.T) {
	for _, c := range []struct {
		word string
		icon []string
	}{{"PICKUP", iconBag}, {"COUNTER", iconBag}, {"DELIVERY", iconCar}} {
		for _, cols := range []int{Cols80, Cols72, Cols58} {
			lay := layoutFor(cols)
			raw := typeLine(c.word, true, lay)
			if !strings.HasPrefix(raw, "\x1dv0\x00") || strings.HasSuffix(raw, "\n") {
				t.Fatalf("%s: not a GS v 0 picture of its own: %q", c.word, raw[:8])
			}
			wb, rows := int(raw[4])|int(raw[5])<<8, int(raw[6])|int(raw[7])<<8
			if rows != typeRows || wb*8 != lay.dots() || len(raw) != 8+wb*rows {
				t.Fatalf("%s at %d: %d x %d bytes", c.word, cols, wb, rows)
			}
			data := []byte(raw[8:])
			dot := func(x, y int) bool { return data[y*wb+x/8]&(0x80>>(x%8)) != 0 }
			word := typeWords[c.word]
			x0 := (lay.dots() - (word.w + typeGap + len(c.icon[0])*2)) / 2
			// centred: as much paper left of the ink as right of the group
			left, right := lay.dots(), 0
			for y := 0; y < rows; y++ {
				for x := 0; x < lay.dots(); x++ {
					if dot(x, y) {
						left, right = min(left, x), max(right, x)
					}
				}
			}
			if left < x0 || right >= x0+word.w+typeGap+len(c.icon[0])*2 || x0 < 0 {
				t.Errorf("%s at %d: ink %d..%d outside the centred group at %d", c.word, cols, left, right, x0)
			}
			ix, iy := x0+word.w+typeGap, (typeRows-48)/2
			for y, row := range c.icon {
				for x, ch := range row {
					if dot(ix+2*x, iy+2*y) != (ch == '#') || dot(ix+2*x+1, iy+2*y+1) != (ch == '#') {
						t.Fatalf("%s: the icon's dot (%d,%d) differs", c.word, x, y)
					}
				}
			}
			for x := ix - typeGap; x < ix; x++ {
				for y := 0; y < rows; y++ {
					if dot(x, y) {
						t.Fatalf("%s: the gap before the icon has ink at (%d,%d)", c.word, x, y)
					}
				}
			}
			// A printer without pictures (strike_images off): the word as text, centred, bold, double size.
			if got := typeLine(c.word, false, lay); got != cmdCenter+cmdBoldOn+gsBig+c.word+"\n"+gsNormal+cmdBoldOff+cmdLeft {
				t.Errorf("%s without pictures = %q", c.word, got)
			}
		}
	}
	// The same bytes as the terminal's (icons.dart) and the fake printer's (type_line_picture): sha256 of the
	// picture's rows, the terminal's test/printer/ticket_round_test.dart holds the same table.
	for key, want := range typeLineSHA {
		var word string
		var cols int
		fmt.Sscanf(strings.Replace(key, "@", " ", 1), "%s %d", &word, &cols)
		raw := typeLine(word, true, layoutFor(cols))
		if got := fmt.Sprintf("%x", sha256.Sum256([]byte(raw[8:]))); got != want {
			t.Errorf("%s: sha256 %s, want %s (change icons.dart and the fake printer with it)", key, got, want)
		}
	}
	if typeIcon("DINE IN") != nil || typeIcon("ORDER") != nil {
		t.Error("only pickup, counter and delivery carry an icon")
	}
	if got := typeLine("DINE IN", true, defaultLayout); got != cmdCenter+cmdBoldOn+gsBig+"DINE IN\n"+gsNormal+cmdBoldOff+cmdLeft {
		t.Errorf("DINE IN = %q", got)
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
	if n := bytes.Count(copies, []byte(typeLine("DELIVERY", true, defaultLayout))); n != 2 {
		t.Errorf("the delivery copies carry %d car lines, want 2", n)
	}
	pickup := counterOrder()
	pickup.Fulfillment.Type = "pickup"
	rcpt := Build(pickup, sc, printer.Ticket{Kind: printer.TicketReceipt, Station: "Front"}, prefs)
	if !bytes.Contains(rcpt, []byte("Order #12\n"+gsNormal+cmdBoldOff+cmdFeed+typeLine("PICKUP", true, defaultLayout))) {
		t.Errorf("the bag line right after the Order #:\n%q", rcpt)
	}
	prefs.StrikeImages = false
	if plain := Build(pickup, sc, printer.Ticket{Kind: printer.TicketReceipt, Station: "Front"}, prefs); bytes.Contains(plain, []byte(typeLine("PICKUP", true, defaultLayout))) ||
		!bytes.Contains(plain, []byte(cmdCenter+cmdBoldOn+gsBig+"PICKUP\n")) {
		t.Errorf("without pictures: the word alone")
	}
	// Kitchen tickets keep their own type line (no icon).
	if st := BuildStation(pickup, pizzaTicket(pickup)); bytes.Contains(st, []byte(typeLine("PICKUP", true, defaultLayout))) {
		t.Error("a kitchen ticket carries the icon")
	}
}

// typeLineSHA: sha256 of each type line picture's rows (word@cols), shared with the terminal and the fake printer.
var typeLineSHA = map[string]string{
	"DELIVERY@48": "9266ffaea1a056f4a7b592046a3f69d60a0fb37723ae60b644138968fe88df18",
	"DELIVERY@42": "c9a9c1edfe28d3999a73b1ed7b5cb5363d663cbc334a78803827519fffafcc2f",
	"DELIVERY@32": "ca4db5858857a1935a101f8824b5c8062a62548f739d956828f1fd30c575aaad",
	"PICKUP@48":   "513299909d61523212d45446a6eae940785a36469cfcaca79cb10e31d7f512ca",
	"PICKUP@42":   "507799be21c6888a873b2db063e465d81afe6b2b7ced348cc2a1c92e1ece518a",
	"PICKUP@32":   "26e476cebb7f1039edd42297130036046afbc4f339a93a886905531178dcfdc7",
	"COUNTER@48":  "87f0a006920ee56479d0ee038e55b14a5e5487c72d81dc7cb5a4bb5e8a7688ea",
	"COUNTER@42":  "df5ce35c3b4a924e1c2626d14b1dc0c4c0551a1d1cd5af3a54cac062cd8896fb",
	"COUNTER@32":  "27c8850263f2fe14010d92f5e40349e94ec2fbaf8187c23f33f7f9cbb71f182b",
}
