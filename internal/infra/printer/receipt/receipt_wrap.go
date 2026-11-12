package receipt

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// The customer receipt's long texts wrap instead of being cut or left to the printer (the print torture,
// quicc-ops tests/printcheck, 2026-11-12: names cut at 29 characters, notes / addresses / fee labels the
// printer broke mid-word, a struck-through line drawn wider than the paper). A text that fits prints the
// same bytes as before, so the goldens of ordinary orders don't move.

// paperWidth is the 80 mm line in the receipt's base font (48 characters; the receipt keeps v1's 41-column
// layout for its item list, lineWidth).
const paperWidth = 48

// itemNameWidth is the item list's name column ("%-3d %-29s %7s").
const itemNameWidth = 29

// receiptItemLines is an item's line(s): "<qty> <name> <price>", the name wrapped in its column, the rest
// of it under the name (4 spaces in, the price column left empty). A price wider than its 7 characters
// takes room from the name, so it still ends on the price column.
func receiptItemLines(qty int, name string, unitCents int64) []string {
	price := dollars(unitCents)
	nameW := itemNameWidth - max(0, utf8.RuneCountInString(price)-7)
	if nameW == itemNameWidth && utf8.RuneCountInString(name) <= nameW {
		return []string{itemLine(qty, name, unitCents)}
	}
	parts := wordWrap(name, nameW, "")
	out := []string{fmt.Sprintf("%-3d %-*s %7s", qty, nameW, parts[0], price)}
	for _, p := range parts[1:] {
		out = append(out, "    "+p)
	}
	return out
}

// receiptModLines is a modifier's line(s) under its item: 5 spaces in, a price on the price column when it
// has one; the rest of a long name 7 spaces in (under the modifier's text, never under the price).
func receiptModLines(name string, priceCents int64) []string {
	if priceCents == 0 {
		if utf8.RuneCountInString(name) <= 36 {
			return []string{modifierLine(name, 0)}
		}
		return wordWrap("     "+name, lineWidth, "       ")
	}
	price := dollars(priceCents)
	nameW := 28 - max(0, utf8.RuneCountInString(price)-7)
	if nameW == 28 && utf8.RuneCountInString(name) <= nameW {
		return []string{modifierLine(name, priceCents)}
	}
	parts := wordWrap(name, nameW, "")
	out := []string{fmt.Sprintf("     %-*s %7s", nameW, parts[0], price)}
	for _, p := range parts[1:] {
		out = append(out, "       "+p)
	}
	return out
}

// pairLines is a totals row ("Subtotal:   $12.00" on the price column); a label too long for its room
// wraps above the amount, which stays on the price column.
func pairLines(label, value string) []string {
	if 19+utf8.RuneCountInString(label)+1+utf8.RuneCountInString(value) <= lineWidth {
		return []string{rightPair(label, value)}
	}
	room := lineWidth - utf8.RuneCountInString(value) - 1
	parts := wordWrap(label, room, "  ")
	var out []string
	for i, p := range parts {
		if i == len(parts)-1 {
			return append(out, leftRight(rightAlign(p, room), value, lineWidth))
		}
		out = append(out, rightAlign(p, room))
	}
	return out
}

// wrapped is text wrapped at width with the continuation indent; a text that fits is one line, as is.
func wrapped(text string, width int, indent string) []string {
	if utf8.RuneCountInString(text) <= width {
		return []string{text}
	}
	return wordWrap(text, width, indent)
}

// padBand is " text " for a double-size line: the space either side kept on every wrapped line.
func padBand(text string, width int) []string {
	var out []string
	for _, l := range wrapped(strings.TrimSpace(text), width-2, "") {
		out = append(out, " "+l+" ")
	}
	return out
}

// wrapJoin is wrapped's lines joined by line feeds, for a writer that ends the text with one more: the
// style and alignment in force carry over to every line.
func wrapJoin(text string, width int, indent string) string {
	return strings.Join(wrapped(text, width, indent), cmdFeed)
}
