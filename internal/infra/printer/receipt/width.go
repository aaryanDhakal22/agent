package receipt

import "strings"

// Every slip is laid out for the printer it prints on (contract/agent.md §2.3c "Paper width"): a printer's
// print width is one of three, set per printer in main (Store › Printers, `print_columns`) or with the
// width test on the Pi's Printers page (BuildWidthTest). All of Lombardi's printers take the same 80 mm
// roll; the Online printer's head prints a narrower area (its 48-character lines wrapped, agent 2.3.3), so
// it is a 42-column printer.
const (
	Cols80 = 48 // 576 dots: an 80 mm head
	Cols72 = 42 // 512 dots: a 72 mm print area (an 80 mm roll on a narrower head)
	Cols58 = 32 // 384 dots: 58 mm paper
)

// NormalizeCols is the print width a slip is laid out for: 0 (not set, an older main) = 80 mm; anything
// else the widest of the three that fits.
func NormalizeCols(cols int) int {
	switch {
	case cols <= 0 || cols >= Cols80:
		return Cols80
	case cols >= Cols72:
		return Cols72
	}
	return Cols58
}

// DotsOf is a print width in dots (font A is 12 dots a character).
func DotsOf(cols int) int {
	switch NormalizeCols(cols) {
	case Cols72:
		return 512
	case Cols58:
		return 384
	}
	return 576
}

// layout is one printer's line widths in each text size.
type layout struct{ cols int }

func layoutFor(cols int) layout { return layout{NormalizeCols(cols)} }

// defaultLayout is the 80 mm layout (a slip built without a printer: the old entry points, tests).
var defaultLayout = layout{Cols80}

func (l layout) dots() int { return DotsOf(l.cols) }

// big is the line at double width (GS ! 0x11): 24 / 21 / 16.
func (l layout) big() int { return l.dots() / 24 }

// xl is the line at triple width (GS ! 0x22, the "quad" size): 16 / 14 / 10.
func (l layout) xl() int { return l.dots() / 36 }

// huge is the line at quadruple width (GS ! 0x33): 12 / 10 / 8.
func (l layout) huge() int { return l.dots() / 48 }

// fontB is the line in font B (9 dots a character): 64 / 56 / 42.
func (l layout) fontB() int { return l.dots() / 9 }

// The customer receipt's item list: "<qty> <name> <price>" (3 + 1 + name + 1 + 7), a modifier
// "     <name> <price>" (5 + name + 1 + 7), a totals row's label right-aligned so the amount ends the line.

func (l layout) nameW() int      { return l.cols - 12 }
func (l layout) modW() int       { return l.cols - 13 }
func (l layout) freeModW() int   { return l.cols - 5 }
func (l layout) pairIndent() int { return l.cols - 22 }

// rule is a separator line across the paper.
func (l layout) rule(ch string) string { return strings.Repeat(ch, l.cols) }

// sizeCols is how many characters fit at a receipt-settings text size (textSize's command).
func (l layout) sizeCols(size string) int {
	switch textSize(size) {
	case cmdBaseSz:
		return l.cols
	case cmdDoubleSz:
		return l.big()
	case cmdHugeSz:
		return l.huge()
	}
	return l.xl()
}
