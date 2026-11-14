package receipt

import (
	"encoding/hex"
	"strings"
)

// The order type's line on the customer slips (the owner, 2026-11-14, #16,
// and the receipt design he confirmed the same day; contract §2.1j step 3):
// "PICKUP" / "COUNTER" with a line-art takeout bag, "DELIVERY" with a
// line-art car, centred on the paper. Thermal printers can't print emoji and
// GS v 0 can't share a text line, so the whole line is ONE GS v 0 picture as
// wide as the paper (576 / 512 / 384 dots), typeRows dots tall: the word in
// Go Bold at 46 px (type_words.go, about the printer's double size), a
// 24-dot gap, then the icon at twice its size, the group centred. The rows
// are byte-identical to the terminal's (lib/platform/printer/icons.dart) and
// to the fake printer's decoder (deploy/fakeprinter: "DELIVERY [icon car]").

// iconBag is a takeout (shopping) bag in outline: 24 x 24 dots.
var iconBag = []string{
	"........................",
	"........................",
	"..........####..........",
	".........######.........",
	"........##....##........",
	".......##......##.......",
	".......##......##.......",
	".....##############.....",
	".....##############.....",
	".....##..........##.....",
	"....##..##....##..##....",
	"....##..##....##..##....",
	"....##............##....",
	"....##............##....",
	"....##............##....",
	"...##..............##...",
	"...##..............##...",
	"...##..............##...",
	"...##..............##...",
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

// The type line's picture.
const (
	typeRows      = 52 // dots tall
	typeBaseline  = 42 // the word's baseline
	typeWordPx    = 46 // Go Bold's size (type_words.go)
	typeGap       = 24 // dots between the word and the icon
	typeIconScale = 2  // each icon dot printed 2 x 2
)

// typeArt is one word of type_words.go: w dots wide, typeRows rows of hex.
type typeArt struct {
	w    int
	rows []string
}

// typeLine is the slip's type line laid out for lay: the word + its icon as
// one paper-wide GS v 0 picture when the printer prints pictures (pictures =
// receipt_prefs.strike_images), else the word as text, centred, bold, at
// double size (tall only when it can't fit wide). No line feed after a
// picture (it is a line of its own); a text line ends with one.
func typeLine(typ string, pictures bool, lay layout) string {
	icon := typeIcon(typ)
	if word, ok := typeWords[typ]; ok && icon != nil && pictures {
		return typeRaster(word, icon, lay.dots())
	}
	size := gsBig
	if len(dclean(typ))*2 > lay.cols {
		size = gsTall
	}
	return cmdCenter + cmdBoldOn + size + dclean(typ) + cmdFeed + gsNormal + cmdBoldOff + cmdLeft
}

// typeRaster draws word, the gap and icon (doubled, centred on the line's
// height) as one group centred on a paper dots wide (GS v 0, m = 0).
func typeRaster(word typeArt, icon []string, dots int) string {
	iconW, iconH := len(icon[0])*typeIconScale, len(icon)*typeIconScale
	x0 := max((dots-(word.w+typeGap+iconW))/2, 0)
	wb := (dots + 7) / 8
	data := make([]byte, wb*typeRows)
	set := func(x, y int) {
		if x >= 0 && x < wb*8 && y >= 0 && y < typeRows {
			data[y*wb+x/8] |= 0x80 >> (x % 8)
		}
	}
	for y, row := range word.rows {
		bits, _ := hex.DecodeString(row)
		for x := 0; x < word.w; x++ {
			if bits[x/8]&(0x80>>(x%8)) != 0 {
				set(x0+x, y)
			}
		}
	}
	ix, iy := x0+word.w+typeGap, (typeRows-iconH)/2
	for y, row := range icon {
		for x, c := range row {
			if c != '#' {
				continue
			}
			for dy := 0; dy < typeIconScale; dy++ {
				for dx := 0; dx < typeIconScale; dx++ {
					set(ix+x*typeIconScale+dx, iy+y*typeIconScale+dy)
				}
			}
		}
	}
	var b strings.Builder
	b.WriteString("\x1d\x76\x30")
	b.Write([]byte{0, byte(wb % 256), byte(wb / 256), typeRows % 256, typeRows / 256})
	b.Write(data)
	return b.String()
}
