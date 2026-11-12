package receipt

import (
	"bytes"
	_ "embed"
	"image"
	"image/png"
	"sync"
)

// The brand kit's receipt logos (quicc-ops brand/quiccpos-brand/receipt/,
// 1-bit: the Q mark + "QuiccPOS"): 80 mm (576 dots wide) and 58 mm (384). A
// 72 mm print area (512 dots) prints the 80 mm logo's middle 512 dots: its
// ink is 456 dots wide (x 58-513), so nothing is cut.
//
//go:embed quiccpos-receipt-80mm-576dots.png
var logo80mmPNG []byte

//go:embed quiccpos-receipt-58mm-384dots.png
var logo58mmPNG []byte

// Font B, single size: the small "powered by" line.
const cmdFontBSz = "\x1b\x21\x01"

var (
	poweredMu    sync.Mutex
	poweredBytes = map[int][]byte{}
)

// poweredByFooter is the last thing on every customer receipt before its
// feed + cut (owner, 2026-11-12): "powered by" in the small font, then the
// QuiccPOS logo for the printer's width as a GS v 0 raster, centred; the
// base size and left alignment back after it. Not on station tickets, driver
// reports, the unsettled list or the test slip.
func poweredByFooter(l layout) string {
	poweredMu.Lock()
	defer poweredMu.Unlock()
	if b, ok := poweredBytes[l.cols]; ok {
		return string(b)
	}
	var b bytes.Buffer
	b.WriteString(cmdFeed + cmdCenter + cmdFontBSz + "powered by" + cmdFeed)
	src := logo80mmPNG
	if l.dots() <= 384 {
		src = logo58mmPNG
	}
	if img, err := png.Decode(bytes.NewReader(src)); err == nil {
		r := img.Bounds()
		if over := r.Dx() - l.dots(); over > 0 { // the middle of the 80 mm logo
			r.Min.X += over / 2
			r.Max.X = r.Min.X + l.dots()
		}
		b.Write(rasterGSv0(img, r))
	}
	b.WriteString(cmdBaseSz + cmdLeft)
	poweredBytes[l.cols] = b.Bytes()
	return b.String()
}

// rasterGSv0 turns an image into GS v 0 m=0 (dark pixels = black dots, MSB =
// the leftmost dot) over the rectangle r of it, its blank top and bottom rows
// left out (paper).
func rasterGSv0(img image.Image, r image.Rectangle) []byte {
	wb := (r.Dx() + 7) / 8
	black := func(x, y int) bool {
		c, _, _, a := img.At(x, y).RGBA()
		return a > 0x8000 && c < 0x8000
	}
	rowInk := func(y int) bool {
		for x := r.Min.X; x < r.Max.X; x++ {
			if black(x, y) {
				return true
			}
		}
		return false
	}
	top, bottom := r.Min.Y, r.Max.Y-1
	for top <= bottom && !rowInk(top) {
		top++
	}
	for bottom >= top && !rowInk(bottom) {
		bottom--
	}
	h := bottom - top + 1
	if h <= 0 {
		return nil
	}
	out := []byte{0x1d, 0x76, 0x30, 0x00, byte(wb), byte(wb >> 8), byte(h), byte(h >> 8)}
	for y := top; y <= bottom; y++ {
		row := make([]byte, wb)
		for x := r.Min.X; x < r.Max.X; x++ {
			if black(x, y) {
				i := x - r.Min.X
				row[i/8] |= 0x80 >> (i % 8)
			}
		}
		out = append(out, row...)
	}
	return out
}
