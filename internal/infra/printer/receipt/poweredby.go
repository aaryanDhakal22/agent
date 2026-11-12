package receipt

import (
	"bytes"
	_ "embed"
	"image"
	"image/png"
	"sync"
)

// The brand kit's 80 mm receipt logo (quicc-ops brand/quiccpos-brand/receipt/,
// 1-bit, 576 dots wide: the Q mark + "QuiccPOS"). Every Pi receipt printer is
// 80 mm.
//
//go:embed quiccpos-receipt-80mm-576dots.png
var logo80mmPNG []byte

// Font B, single size: the small "powered by" line.
const cmdFontBSz = "\x1b\x21\x01"

var (
	poweredOnce  sync.Once
	poweredBytes []byte
)

// poweredByFooter is the last thing on every customer receipt before its
// feed + cut (owner, 2026-11-12): "powered by" in the small font, then the
// QuiccPOS logo as a GS v 0 raster, centred; the base size and left
// alignment back after it. Not on station tickets, driver reports, the
// unsettled list or the test slip.
func poweredByFooter() string {
	poweredOnce.Do(func() {
		var b bytes.Buffer
		b.WriteString(cmdFeed + cmdCenter + cmdFontBSz + "powered by" + cmdFeed)
		if img, err := png.Decode(bytes.NewReader(logo80mmPNG)); err == nil {
			b.Write(rasterGSv0(img))
		}
		b.WriteString(cmdBaseSz + cmdLeft)
		poweredBytes = b.Bytes()
	})
	return string(poweredBytes)
}

// rasterGSv0 turns an image into GS v 0 m=0 (dark pixels = black dots, MSB =
// the leftmost dot), its blank top and bottom rows left out (paper).
func rasterGSv0(img image.Image) []byte {
	r := img.Bounds()
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
