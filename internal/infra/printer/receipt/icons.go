package receipt

import (
	"strings"

	"golang.org/x/image/font/basicfont"
)

// The order type's icon on the customer slips (the owner, 2026-11-14, #16;
// contract §2.1j step 3): "PICKUP" / "COUNTER" + one space + a line-art
// takeout bag, "DELIVERY" + one space + a line-art car. Thermal printers
// can't print emoji and GS v 0 can't share a text line, so the whole line is
// ONE GS v 0 picture, 24 dots tall (font A's height): the word drawn like
// the struck lines (strike.go: the X11 6x13 font at twice its size, 12 x 24
// dots a character), a 12-dot space, then the icon. The bitmaps are
// byte-identical to the terminal's (lib/platform/printer/icons.dart) and to
// the fake printer's decoder (deploy/fakeprinter: "PICKUP [icon bag]").

// iconBag is a takeout (shopping) bag in outline: 24 x 24 dots.
var iconBag = []string{
	"........................",
	"........................",
	"..........####..........",
	"........########........",
	".......###....###.......",
	".......##......##.......",
	"......###......###......",
	"......##........##......",
	"..####################..",
	"..####################..",
	"..##................##..",
	"..##................##..",
	"..##................##..",
	"..##................##..",
	"..##................##..",
	"..##................##..",
	"..##................##..",
	"..##................##..",
	"..##................##..",
	"..##................##..",
	"..##................##..",
	"..####################..",
	"..####################..",
	"........................",
}

// iconCar is a car from the side in outline: 36 x 24 dots.
var iconCar = []string{
	"....................................",
	"....................................",
	"....................................",
	"....................................",
	"...........############.............",
	"..........##############............",
	".........##.....##......##..........",
	"........##......##.......##.........",
	".......##.......##........##........",
	"......##........##.........########.",
	"..##################################",
	".##...............................##",
	".##...............................##",
	".##...............................##",
	".##...............................##",
	".##.....######..........######....##",
	"..####.########........########.###.",
	"......###....###......###....###....",
	"......##......##......##......##....",
	"......##......##......##......##....",
	"......###....###......###....###....",
	".......########........########.....",
	"........######..........######......",
	"....................................",
}

// typeIcon is the icon after an order type's word on the slip, nil for none
// (DINE IN, ORDER, anything else prints the word alone).
func typeIcon(typ string) []string {
	switch typ {
	case "PICKUP", "COUNTER":
		return iconBag
	case "DELIVERY":
		return iconCar
	}
	return nil
}

// typeLine is the slip's type line: the word + its icon as one GS v 0
// picture when the printer prints pictures (pictures = receipt_prefs
// .strike_images), else the word as text. No line feed after a picture (it
// is a line of its own); a text line ends with one.
func typeLine(typ string, pictures bool) string {
	icon := typeIcon(typ)
	if icon == nil || !pictures {
		return toPC437(typ) + cmdFeed
	}
	return iconRaster(typ, icon)
}

// iconRaster draws word (12 x 24 dots a character), a 12-dot space and the
// icon (24 dots tall) as a GS v 0 picture (m = 0).
func iconRaster(word string, icon []string) string {
	f := basicfont.Face7x13
	runes := []rune(foldASCII(word))
	x0 := (len(runes) + 1) * strikeCharDots
	cols := x0 + len(icon[0])
	wb := (cols + 7) / 8
	data := make([]byte, wb*strikeRows)
	set := func(x, y int) { data[y*wb+x/8] |= 0x80 >> (x % 8) }
	for i, r := range runes {
		if r == ' ' {
			continue
		}
		g, ok := glyphRow(f, r)
		if !ok {
			g, _ = glyphRow(f, '?')
		}
		for gx := 0; gx < f.Width; gx++ {
			for gy := 1; gy < f.Height; gy++ { // the top row is always blank: 12 rows -> 24 dots
				if !inked(f.Mask, gx, g*f.Height+gy) {
					continue
				}
				x, y := i*strikeCharDots+gx*2, (gy-1)*2
				set(x, y)
				set(x+1, y)
				set(x, y+1)
				set(x+1, y+1)
			}
		}
	}
	for y, row := range icon {
		for x, c := range row {
			if c == '#' {
				set(x0+x, y)
			}
		}
	}
	var b strings.Builder
	b.WriteString("\x1d\x76\x30")
	b.Write([]byte{0, byte(wb % 256), byte(wb / 256), strikeRows, 0})
	b.Write(data)
	return b.String()
}
