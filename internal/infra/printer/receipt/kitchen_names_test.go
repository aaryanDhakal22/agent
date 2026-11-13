package receipt

import (
	"fmt"
	"strings"
	"testing"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
)

// The kitchen ticket's item names (the owner, 2.5.2, from a real ticket "2  WHOLE WINGS COMBO (6 /
// PIECE)"): Title Case with the inch mark for every order; an online vendor order's size on its own
// first "- " line ("Serving" still hidden, "Serving (2 Piece)" → "- 2 Piece"); a store order keeps
// "Name (Size)", the bracket never split by a wrap. 48 / 42 / 32 columns, goldens
// testdata/station_names_*.golden.
func TestKitchenItemNames(t *testing.T) {
	items := func() []order.LineItem {
		return []order.LineItem{
			kitchenLine(2, "WHOLE WINGS COMBO", "6 / Piece", "p-pizza", kid("Can of Ginger Ale"), kid("Side of Fries")),
			kitchenLine(1, "Garlic Knots", "Serving (2 Piece)", "p-pizza"),
			kitchenLine(1, "Turkey Club", "Serving", "p-pizza"),
			kitchenLine(1, "cheese pizza", "12 Medium", "p-pizza"),
			kitchenLine(1, "8 Sub", "", "p-pizza"),
			kitchenLine(1, "Extra Long Philly Cheesesteak Sandwich Platter", "Large Family Size", "p-pizza"),
		}
	}
	for _, cols := range []int{48, 42, 32} {
		for _, src := range []string{order.SourceOnline, order.SourceCounter} {
			snap := kitchenSnap("pickup", src, "Ana", 1104, items()...)
			tk := printer.Ticket{Kind: printer.TicketStation, TargetID: "p-pizza", Station: "Pizza", Mine: snap.Items, Cols: cols}
			got := string(BuildStation(snap, tk))
			txt := got
			var want []string
			if src == order.SourceOnline {
				want = []string{"2  Whole Wings Combo\n", H8 + "- 6 Piece\n" + H8 + "- Can of Ginger Ale\n" + H8 + "- Side of Fries\n",
					"1  Garlic Knots\n", H8 + "- 2 Piece\n", "1  Turkey Club\n", "1  Cheese Pizza\n", H8 + "- 12\" Medium\n", "1  8\" Sub\n"}
			} else {
				want = []string{"2  Whole Wings", "(6 / Piece)", "1  Turkey Club\n", "1  8\" Sub\n", "Cheese Pizza", "(12\" Medium)"}
				if strings.Contains(txt, "6 /\n") || strings.Contains(txt, "(12\"\n") || strings.Contains(txt, "(Large\n") {
					t.Errorf("%s %d: a bracketed size split by the wrap\n%s", src, cols, txt)
				}
			}
			for _, w := range want {
				if !strings.Contains(strings.ReplaceAll(strings.ReplaceAll(txt, "\n   ", " "), "\x1f", " "), w) && !strings.Contains(txt, w) {
					t.Errorf("%s %d: missing %q\n%s", src, cols, w, txt)
				}
			}
			if strings.Contains(strings.ToLower(txt), "serving") || strings.Contains(txt, "WHOLE WINGS") {
				t.Errorf("%s %d: Serving or capitals\n%s", src, cols, txt)
			}
			golden(t, fmt.Sprintf("testdata/station_names_%s_%d.golden", src, cols), []byte(got))
		}
	}
}
