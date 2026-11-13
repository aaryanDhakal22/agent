package receipt

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/jpeg" // the store's logo may be a JPEG
	"strings"
	"sync"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
	_ "golang.org/x/image/webp" // or a WebP

	"quiccpos/agent/internal/domain/storeconfig"
)

// The receipt header with the store's logo (the owner, 2026-11-12; contract
// §2.1j): one raster as wide as the paper (576 / 512 / 384 dots for 48 / 42 /
// 32 columns), the logo on the LEFT (at most 40 % of the width, dithered to
// black and white) and the store's name / address / phone on the RIGHT, drawn
// with the Go fonts (BSD, golang.org/x/image). No logo = no raster: the slip
// prints the details centred as text.

var logoState struct {
	sync.RWMutex
	img  image.Image
	etag string
	// cache: the header bytes by store text + width.
	cache map[string][]byte
}

// SetReceiptLogo installs the store's logo (nil = none) with its ETag; the
// headers are drawn again on their next use.
func SetReceiptLogo(img image.Image, etag string) {
	logoState.Lock()
	defer logoState.Unlock()
	logoState.img, logoState.etag, logoState.cache = img, etag, nil
}

// ReceiptLogoETag is the installed logo's ETag ("" = none).
func ReceiptLogoETag() string {
	logoState.RLock()
	defer logoState.RUnlock()
	if logoState.img == nil {
		return ""
	}
	return logoState.etag
}

// DecodeReceiptLogo reads a PNG, JPEG or WebP.
func DecodeReceiptLogo(b []byte) (image.Image, error) {
	img, _, err := image.Decode(bytes.NewReader(b))
	return img, err
}

// HeaderRaster is the header for a printer of cols columns as ESC/POS bytes
// (centred GS v 0 raster) and an ETag for it; nil when the store has no logo
// (the terminal's GET /api/terminal/receipt-header).
func HeaderRaster(sc storeconfig.StoreContext, cols int) ([]byte, string) {
	sc = scrubStore(sc)
	lay := layoutFor(cols)
	b := headerFor(sc, lay)
	if b == nil {
		return nil, ""
	}
	return b, fmt.Sprintf(`"h%s-%d-%x"`, strings.Trim(ReceiptLogoETag(), `"`), lay.cols, len(b))
}

// headerFor is the header raster for this store and paper, nil without a logo.
func headerFor(sc storeconfig.StoreContext, lay layout) []byte {
	logoState.RLock()
	img := logoState.img
	logoState.RUnlock()
	if img == nil {
		return nil
	}
	lines := headerLines(sc)
	key := fmt.Sprintf("%d|%s", lay.dots(), strings.Join(lines, "|"))
	logoState.RLock()
	if b, ok := logoState.cache[key]; ok {
		logoState.RUnlock()
		return b
	}
	logoState.RUnlock()
	b := renderHeader(img, lines, lay.dots())
	logoState.Lock()
	if logoState.cache == nil {
		logoState.cache = map[string][]byte{}
	}
	logoState.cache[key] = b
	logoState.Unlock()
	return b
}

var (
	fontOnce          sync.Once
	fontReg, fontBold *opentype.Font
)

func face(f *opentype.Font, px float64) font.Face {
	fc, err := opentype.NewFace(f, &opentype.FaceOptions{Size: px, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		return nil
	}
	return fc
}

// renderHeader draws the logo (left) and the store's lines (right) on a white
// canvas `dots` wide and returns it as a centred GS v 0 raster.
func renderHeader(logo image.Image, lines []string, dots int) []byte {
	canvas := headerCanvas(logo, lines, dots)
	if canvas == nil {
		return nil
	}
	var b bytes.Buffer
	b.WriteString(cmdCenter)
	b.Write(rasterGSv0(canvas, canvas.Bounds()))
	b.WriteString(cmdLeft)
	return b.Bytes()
}

// headerImage is the header as an image (previews, tests); nil without a logo.
func headerImage(sc storeconfig.StoreContext, lay layout) image.Image {
	logoState.RLock()
	img := logoState.img
	logoState.RUnlock()
	if img == nil {
		return nil
	}
	return headerCanvas(img, headerLines(sc), lay.dots())
}

func headerLines(sc storeconfig.StoreContext) []string {
	lines := []string{sc.StoreName(), strings.TrimSpace(sc.Location.AddressLine1), strings.TrimSpace(sc.Location.AddressLine2)}
	if ph := strings.TrimSpace(sc.Location.Phone); ph != "" {
		lines = append(lines, phoneParens(ph))
	}
	return lines
}

func headerCanvas(logo image.Image, lines []string, dots int) *image.Gray {
	fontOnce.Do(func() {
		fontReg, _ = opentype.Parse(goregular.TTF)
		fontBold, _ = opentype.Parse(gobold.TTF)
	})
	scale := float64(dots) / 576
	namePx, linePx := 30*scale, 22*scale
	gap := int(14 * scale)
	// The text block's height.
	textH, nonEmpty := 0, 0
	for i, l := range lines {
		if l == "" {
			continue
		}
		nonEmpty++
		if i == 0 {
			textH += int(namePx * 1.25)
		} else {
			textH += int(linePx * 1.3)
		}
	}
	// The logo's box: at most 40 % of the width, as tall as the text (at least 96 dots, at most 200).
	maxW := dots * 40 / 100
	boxH := min(max(textH, int(96*scale)), int(200*scale))
	lb := logo.Bounds()
	lw, lh := lb.Dx(), lb.Dy()
	if lw <= 0 || lh <= 0 {
		return nil
	}
	w, h := maxW, maxW*lh/lw
	if h > boxH {
		h, w = boxH, boxH*lw/lh
	}
	w, h = max(w, 1), max(h, 1)
	// The store's lines, each shrunk until it fits right of the logo; the
	// widest sets the text block's width.
	type textLine struct {
		s     string
		fc    font.Face
		px    float64
		lineH int
	}
	var tl []textLine
	textW, blockW := dots-w-gap, 0
	for i, l := range lines {
		if l == "" {
			continue
		}
		f, px := fontReg, linePx
		if i == 0 {
			f, px = fontBold, namePx
		}
		fc := face(f, px)
		if fc == nil {
			continue
		}
		for px > 10 && font.MeasureString(fc, l).Ceil() > textW {
			px -= 1
			fc = face(f, px)
		}
		lineH := int(px * 1.3)
		if i == 0 {
			lineH = int(px * 1.25)
		}
		blockW = max(blockW, min(font.MeasureString(fc, l).Ceil(), textW))
		tl = append(tl, textLine{l, fc, px, lineH})
	}
	height := max(h, textH) + int(8*scale)
	canvas := image.NewGray(image.Rect(0, 0, dots, height))
	draw.Draw(canvas, canvas.Bounds(), image.White, image.Point{}, draw.Src)
	// Logo + text are ONE group, centred on the paper (the owner, 2026-11-12).
	groupW := w
	if blockW > 0 {
		groupW += gap + blockW
	}
	ox := max((dots-groupW)/2, 0)
	// The logo, scaled onto white (transparency = paper), then dithered.
	scaled := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(scaled, scaled.Bounds(), image.White, image.Point{}, draw.Src)
	xdraw.CatmullRom.Scale(scaled, scaled.Bounds(), logo, lb, xdraw.Over, nil)
	top := (height - h) / 2
	dither(canvas, scaled, image.Pt(ox, top))
	// The store's lines, left-aligned right of the logo, vertically centred.
	x := ox + w + gap
	y := (height - textH) / 2
	for _, l := range tl {
		d := &font.Drawer{Dst: canvas, Src: image.Black, Face: l.fc, Dot: fixed.P(x, y+int(l.px))}
		d.DrawString(l.s)
		y += l.lineH
	}
	return canvas
}

// dither puts src onto dst at at in black and white (Floyd–Steinberg).
func dither(dst *image.Gray, src image.Image, at image.Point) {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	errs := make([]float64, (w+2)*(h+1))
	idx := func(x, y int) int { return y*(w+2) + x + 1 }
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			g := float64(color.GrayModel.Convert(src.At(b.Min.X+x, b.Min.Y+y)).(color.Gray).Y) + errs[idx(x, y)]
			out := 255.0
			if g < 128 {
				out = 0
			}
			e := g - out
			errs[idx(x+1, y)] += e * 7 / 16
			errs[idx(x-1, y+1)] += e * 3 / 16
			errs[idx(x, y+1)] += e * 5 / 16
			errs[idx(x+1, y+1)] += e * 1 / 16
			dst.SetGray(at.X+x, at.Y+y, color.Gray{Y: uint8(out)})
		}
	}
}
