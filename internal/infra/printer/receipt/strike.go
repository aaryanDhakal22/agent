package receipt

import (
	"image"

	"golang.org/x/image/font/basicfont"
)

// A removed line on a kitchen change ticket prints "(VOID)" and the item
// struck through. ESC/POS has no strike-through, so the item is a small
// picture: an ESC * 24-dot bit image in the same line as the text, the
// characters drawn from the public-domain X11 6x13 font (golang.org/x/image
// basicfont) at twice their size - 12 x 24 dots, the size of the printer's
// own font A - with a line through the middle.

const (
	strikeCharDots = 12 // font A's width
	strikeRows     = 24 // one ESC * 33 band
	// strikeTop / strikeBottom: the line through a capital letter's middle
	// (glyph rows 2-10, drawn from row 1 at twice the size).
	strikeTop    = 10
	strikeBottom = 12
)

// strikeImage is ESC * 33 (24-dot double density) for text, struck through.
// Characters outside the font print as "?".
func strikeImage(text string) string {
	f := basicfont.Face7x13
	runes := []rune(text)
	cols := len(runes) * strikeCharDots
	data := make([]byte, cols*3)
	set := func(x, y int) { data[x*3+y/8] |= 0x80 >> (y % 8) }
	for i, r := range runes {
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
	for x := 0; x < cols; x++ {
		for y := strikeTop; y <= strikeBottom; y++ {
			set(x, y)
		}
	}
	return "\x1b*\x21" + string([]byte{byte(cols % 256), byte(cols / 256)}) + string(data)
}

// glyphRow is the rune's glyph index in the face's mask.
func glyphRow(f *basicfont.Face, r rune) (int, bool) {
	for _, rg := range f.Ranges {
		if r >= rg.Low && r < rg.High {
			return int(r-rg.Low) + rg.Offset, true
		}
	}
	return 0, false
}

func inked(m image.Image, x, y int) bool {
	_, _, _, a := m.At(x, y).RGBA()
	return a > 0x7fff
}
