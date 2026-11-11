package receipt

import (
	"fmt"
	"strings"

	"quiccpos/agent/internal/domain/order"
)

// Half and half (TERMINAL-PLAN Stage 0b, TERMINAL-HALVES.md). A pizza line
// whose modifier children carry a portion prints on the kitchen ticket as
//
//	1x 16 X-LARGE ½ PEPPERONI | ½ HAWAIIAN   (the summary, item size)
//	 HALF & HALF                             (inverse, when the halves differ)
//	WHOLE PIE   (no toppings all over)
//	1ST HALF    PEPPERONI
//	2ND HALF    HAWAIIAN
//
// "+ ½" in the summary = added on top, "|" = two different halves. A split
// pie always prints both halves; NO prints in inverse on its own half; 1st /
// 2nd, never left / right. Specialties print by name (recipes come with the
// menu editor). The customer receipt keeps one line per modifier, grouped
// whole → 1st → 2nd with a "1st:" / "2nd:" prefix; the condensed receipt
// shows the summary. Lines without portions print as before.

// halfSign is ½ while a line is built and wrapped (one rune); printable
// turns it into ½ in the printers' code page (PC437 0xAB, what ESC @
// selects). glue keeps "½" on the line of the word after it.
const halfSign = "½"

var printer437 = strings.NewReplacer("½", "\xab", "\x1f", " ")

func printable(s string) string { return printer437.Replace(s) }

func glue(s string) string { return strings.ReplaceAll(s, halfSign+" ", halfSign+"\x1f") }

const halfIndent = "            " // under the 12-wide WHOLE PIE / 1ST HALF column

// pie is a half-and-half line read from its children: [0] = the whole pie,
// [1] / [2] = the halves.
type pie struct {
	base string
	spec [3]string
	only [3]bool
	add  [3][]string
	no   [3][]string
}

func halfIndex(portion string) int {
	switch portion {
	case order.PortionFirst:
		return 1
	case order.PortionSecond:
		return 2
	}
	return 0
}

// bareName is a child's option name without the readable decorations main
// puts on it for older Pis ("ONLY ", "NO ", " (1st half)").
func bareName(c order.LineItem) string {
	n := strings.TrimSpace(c.Name)
	n = strings.TrimSuffix(strings.TrimSuffix(n, " (1st half)"), " (2nd half)")
	switch c.Mode {
	case order.ModeNo:
		n = strings.TrimPrefix(n, "NO ")
	case order.ModeOnly:
		if c.Portion != "" { // a whole-pie ONLY keeps its word
			n = strings.TrimPrefix(n, "ONLY ")
		}
	}
	return n
}

func pieOf(it order.LineItem) pie {
	p := pie{base: strings.ToUpper(strings.TrimSpace(it.Name))}
	for _, c := range it.Children {
		k, name := halfIndex(c.Portion), strings.ToUpper(bareName(c))
		switch c.Mode {
		case order.ModeSpecialty:
			if k > 0 {
				p.spec[k] = name
			} else {
				p.add[0] = append(p.add[0], name)
			}
		case order.ModeNo:
			p.no[k] = append(p.no[k], name)
		case order.ModeOnly:
			p.only[k] = k > 0
			p.add[k] = append(p.add[k], name)
		default:
			p.add[k] = append(p.add[k], name)
		}
	}
	return p
}

// split: the halves start from different pizzas (a specialty or ONLY on one).
func (p pie) split() bool {
	return p.spec[1] != "" || p.spec[2] != "" || p.only[1] || p.only[2]
}

// half is what a split pie's half is: its specialty, its ONLY toppings
// (PLAIN for none), else the line's pizza; the specialty or pizza with its
// additions after a "+".
func (p pie) half(k int) (name string, adds []string) {
	switch {
	case p.spec[k] != "":
		return p.spec[k], p.add[k]
	case p.only[k]:
		if len(p.add[k]) == 0 {
			return "PLAIN", nil
		}
		return strings.Join(p.add[k], ", "), nil
	}
	return p.base, p.add[k]
}

func plus(name string, adds []string) string {
	if len(adds) == 0 {
		return name
	}
	return name + " + " + strings.Join(adds, ", ")
}

// halfSummary is the line's summary: "1x 16 X-LARGE HAWAIIAN + ½ PEPPERONI",
// "1x 16 X-LARGE ½ PEPPERONI | ½ HAWAIIAN", "... CHEESE PIZZA ½ MUSHROOMS |
// ½ SAUSAGE, ONION", "... HAWAIIAN (½ NO PINEAPPLE)".
func halfSummary(it order.LineItem) string {
	head := fmt.Sprintf("%dx", it.Quantity)
	if sz := displaySize(it.Size); sz != "" {
		head += " " + strings.ToUpper(sz)
	}
	return head + " " + halfText(it)
}

// halfText is the summary without its quantity and size.
func halfText(it order.LineItem) string {
	p := pieOf(it)
	var s string
	if p.split() {
		s = fmt.Sprintf("%s %s | %s %s", halfSign, plus(p.half(1)), halfSign, plus(p.half(2)))
	} else {
		s = plus(p.base, p.add[0])
		h1, h2 := strings.Join(p.add[1], ", "), strings.Join(p.add[2], ", ")
		switch {
		case h1 != "" && h2 != "":
			s += fmt.Sprintf(" %s %s | %s %s", halfSign, h1, halfSign, h2)
		case h1 != "":
			s += " + " + halfSign + " " + h1
		case h2 != "":
			s += " + " + halfSign + " " + h2
		}
	}
	var nos []string
	for k := 0; k < 3; k++ {
		for _, n := range p.no[k] {
			if k == 0 {
				nos = append(nos, "NO "+n)
			} else {
				nos = append(nos, halfSign+" NO "+n)
			}
		}
	}
	if len(nos) > 0 {
		s += " (" + strings.Join(nos, ", ") + ")"
	}
	return s
}

// writeHalves prints the HALF & HALF banner (a split pie) and the WHOLE PIE /
// 1ST HALF / 2ND HALF blocks at the base size.
func writeHalves(w func(string), nl func(), it order.LineItem) {
	p := pieOf(it)
	block := func(label, text string) {
		for i, line := range wordWrap(glue(text), stationWidth-len(halfIndent), "") {
			if i == 0 {
				w(fmt.Sprintf("%-12s", label))
			} else {
				w(halfIndent)
			}
			w(printable(line))
			nl()
		}
	}
	// nos prints a part's NOs in inverse, the first beside its label when
	// nothing else is on that line yet.
	nos := func(label string, k int, wrote bool) bool {
		for _, n := range p.no[k] {
			if wrote {
				w(halfIndent)
			} else {
				w(fmt.Sprintf("%-12s", label))
				wrote = true
			}
			w(cmdInvertOn)
			w(" NO " + n + " ")
			w(cmdInvertOff)
			nl()
		}
		return wrote
	}
	if p.split() {
		w(cmdBoldOn)
		w(cmdInvertOn)
		w(" HALF & HALF ")
		w(cmdInvertOff)
		w(cmdBoldOff)
		nl()
		if len(p.add[0]) > 0 {
			block("WHOLE PIE", "+ "+strings.Join(p.add[0], ", "))
		} else {
			block("WHOLE PIE", "(no toppings all over)")
		}
	} else {
		block("WHOLE PIE", p.base)
		if len(p.add[0]) > 0 {
			block("", "+ "+strings.Join(p.add[0], ", "))
		}
	}
	nos("", 0, true)
	for k := 1; k <= 2; k++ {
		label := []string{"", "1ST HALF", "2ND HALF"}[k]
		name, adds := p.half(k)
		wrote := false
		switch {
		case p.split():
			block(label, name)
			wrote = true
			if len(adds) > 0 {
				block("", "+ "+strings.Join(adds, ", "))
			}
		case len(adds) > 0:
			block(label, "+ "+strings.Join(adds, ", "))
			wrote = true
		}
		if !nos(label, k, wrote) {
			block(label, "nothing added") // a split pie always shows both halves
		}
	}
}

// receiptHalfMods is the customer receipt's compact list for a half-and-half
// line: the whole pie's modifiers first, then the 1st half's ("1st: ONLY
// Pepperoni"), then the 2nd's, each keeping its own price.
func receiptHalfMods(it order.LineItem) []order.LineItem {
	out := make([]order.LineItem, 0, len(it.Children))
	for k := 0; k < 3; k++ {
		for _, c := range it.Children {
			if halfIndex(c.Portion) != k {
				continue
			}
			if k > 0 {
				name := bareName(c)
				switch c.Mode {
				case order.ModeOnly:
					name = "ONLY " + name
				case order.ModeNo:
					name = "NO " + name
				}
				c.Name = []string{"", "1st: ", "2nd: "}[k] + name
			}
			out = append(out, c)
		}
	}
	return out
}
