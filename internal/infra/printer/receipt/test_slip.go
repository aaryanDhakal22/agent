package receipt

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"quiccpos/agent/internal/domain/storeconfig"
)

// BuildTestSlip renders the short slip the tablet's "test print" puts on one
// printer: which printer it was meant for (so a slip on the wrong printer is
// obvious), the store, the tablet that asked and the local time. ASCII only —
// names typed with other characters are shown with '?' rather than risking
// codepage garbage on the very slip that is supposed to prove the printer works.
// It is laid out for the printer's width (cols, width.go) and says which one.
func BuildTestSlip(printerName string, sc storeconfig.StoreContext, tablet string, at time.Time, cols int) []byte {
	printerName, sc, tablet = cleanName(printerName), scrubStore(sc), cleanName(tablet)
	lay := layoutFor(cols)
	loc := time.Local
	if tz, err := time.LoadLocation(sc.Location.Timezone); sc.Location.Timezone != "" && err == nil {
		loc = tz
	}
	var buf bytes.Buffer
	w := func(s string) { buf.WriteString(s) }
	nl := func() { buf.WriteString(cmdFeed) }

	w(cmdInit)
	w(cmdBaseSz)
	w(cmdCenter)
	w(cmdBoldOn)
	w(cmdDoubleSz)
	w(wrapJoin("TEST - Quibase setup", lay.big(), ""))
	nl()
	nl()
	nameSz, nameW := bigName(strings.ToUpper(asciiOnly(printerName)), lay)
	w(nameSz)
	w(wrapJoin(strings.ToUpper(asciiOnly(printerName)), nameW, ""))
	nl()
	w(cmdBaseSz)
	w(cmdBoldOff)
	nl()
	w(cmdLeft)
	w(lay.rule("-"))
	nl()
	if store := asciiOnly(sc.StoreName()); store != "" {
		w(wrapJoin("Store:   "+store, lay.cols, "         "))
		nl()
	}
	if t := asciiOnly(tablet); t != "" {
		w(wrapJoin("Tablet:  "+t, lay.cols, "         "))
		nl()
	}
	w("Printed: " + at.In(loc).Format("Mon Jan 2 3:04 PM"))
	nl()
	w(fmt.Sprintf("Width:   %d characters", lay.cols))
	nl()
	w(lay.rule("-"))
	nl()
	w(cmdCenter)
	w(cmdBoldOn)
	w(wrapJoin("If you can read this, this printer works.", lay.cols, ""))
	w(cmdBoldOff)
	nl()

	w(cmdFeed + cmdFeed + cmdFeed + cmdFeed)
	w(cmdCut)
	return buf.Bytes()
}

// asciiOnly replaces every non-printable-ASCII character with '?' and trims.
func asciiOnly(s string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		if r >= 0x20 && r < 0x7F {
			b.WriteRune(r)
		} else {
			b.WriteByte('?')
		}
	}
	return b.String()
}

// The width test (contract/agent.md §2.3c "Paper width"; the same ruler as the terminal's printer test,
// terminal/lib/platform/printer/profile.dart): one line of RulerLength characters, longer than any paper
// (30 dashes, then A-Z and 2-9, one per column). The printer wraps what doesn't fit, so the LAST symbol on
// the ruler's first printed line names the printer's width: B = 32, L = 42, R = 48. Nothing to count.
const (
	RulerLength  = 64
	RulerSymbols = "ABCDEFGHIJKLMNOPQRSTUVWXYZ23456789"
)

// rulerFirstColumn is the column of the ruler's first symbol (A = 31).
const rulerFirstColumn = RulerLength - len(RulerSymbols) + 1

// WidthRuler is the width test's ruler line.
func WidthRuler() string { return strings.Repeat("-", rulerFirstColumn-1) + RulerSymbols }

// ColumnsOfSymbol is the columns a ruler symbol stands for (B = 32, L = 42, R = 48, 9 = 64); false when
// it isn't one (0 and 1 aren't: they look like O and I).
func ColumnsOfSymbol(symbol string) (int, bool) {
	s := strings.ToUpper(strings.TrimSpace(symbol))
	if len(s) != 1 {
		return 0, false
	}
	i := strings.Index(RulerSymbols, s)
	if i < 0 {
		return 0, false
	}
	return rulerFirstColumn + i, true
}

// SymbolOfColumns is the ruler symbol printed in column cols ("" outside A-9).
func SymbolOfColumns(cols int) string {
	i := cols - rulerFirstColumn
	if i < 0 || i >= len(RulerSymbols) {
		return ""
	}
	return RulerSymbols[i : i+1]
}

// BuildWidthTest is the width test slip for one printer: its name, the question, the ruler (font A,
// single size, never wrapped by us: the printer's own wrap is the answer). Every other line fits 32
// columns, so the slip reads the same on any printer.
func BuildWidthTest(printerName string, at time.Time) []byte {
	printerName = cleanName(printerName)
	var buf bytes.Buffer
	w := func(s string) { buf.WriteString(s) }
	nl := func() { buf.WriteString(cmdFeed) }
	w(cmdInit)
	w(cmdNormalSz)
	w(cmdCenter)
	w(cmdBoldOn)
	w("WIDTH TEST")
	nl()
	w(wrapJoin(strings.ToUpper(asciiOnly(printerName)), Cols58, ""))
	nl()
	w(cmdBoldOff)
	w(cmdLeft)
	nl()
	w("Which letter is LAST on the")
	nl()
	w("first line below?")
	nl()
	nl()
	w(WidthRuler())
	nl()
	nl()
	w("Pick that letter on the Pi's")
	nl()
	w("Printers page.")
	nl()
	w(at.Format("Mon Jan 2 3:04 PM"))
	nl()
	w(cmdFeed + cmdFeed + cmdFeed + cmdFeed)
	w(cmdCut)
	return buf.Bytes()
}
