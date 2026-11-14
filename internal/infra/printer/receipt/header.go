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
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
	_ "golang.org/x/image/webp" // or a WebP

	"quiccpos/agent/internal/domain/storeconfig"
)

// The receipt header with the store's logo (the owner, 2026-11-12, stacked
// 2026-11-14; contract §2.1j): one raster as wide as the paper (576 / 512 /
// 384 dots for 48 / 42 / 32 columns), the logo centred at half the paper's
// width (its light background printed as paper, dithered to black and white)
// and the store's phone centred under it, drawn with Go Regular (BSD,
// golang.org/x/image). No logo = no raster: the slip prints the details
// centred as text.

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
	fontOnce sync.Once
	fontReg  *opentype.Font
)

func face(f *opentype.Font, px float64) font.Face {
	fc, err := opentype.NewFace(f, &opentype.FaceOptions{Size: px, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		return nil
	}
	return fc
}

// renderHeader draws the logo with the store's phone under it on a white
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

// headerLines are what the header draws besides the logo: the store's phone
// alone (the owner 2026-11-14: no name or address text under the logo).
func headerLines(sc storeconfig.StoreContext) []string {
	if ph := strings.TrimSpace(sc.Location.Phone); ph != "" {
		return []string{phoneParens(ph)}
	}
	return nil
}

// headerLight: a logo pixel at least this light prints as paper (Lombardi's
// beige background, 232,212,185, dithered into a grey dotted box).
const headerLight = 175

// headerCanvas is the header the owner picked (2026-11-14): the logo alone,
// centred, half the paper's width (at most as tall), its light background as
// paper, then the lines (the phone) centred under it in Go Regular at 22 px
// on 80 mm, scaled with the paper.
func headerCanvas(logo image.Image, lines []string, dots int) *image.Gray {
	fontOnce.Do(func() {
		fontReg, _ = opentype.Parse(goregular.TTF)
	})
	lb := logo.Bounds()
	lw, lh := lb.Dx(), lb.Dy()
	if lw <= 0 || lh <= 0 {
		return nil
	}
	scale := float64(dots) / 576
	linePx := 22 * scale
	gap := int(10 * scale)
	w := dots / 2
	h := w * lh / lw
	if h > dots/2 { // a tall logo: as tall as the box is wide
		h, w = dots/2, (dots/2)*lw/lh
	}
	w, h = max(w, 1), max(h, 1)
	type textLine struct {
		s  string
		fc font.Face
	}
	var tl []textLine
	for _, l := range lines {
		if l = strings.TrimSpace(l); l == "" {
			continue
		}
		px := linePx
		fc := face(fontReg, px)
		for fc != nil && px > 10 && font.MeasureString(fc, l).Ceil() > dots {
			px--
			fc = face(fontReg, px)
		}
		if fc != nil {
			tl = append(tl, textLine{l, fc})
		}
	}
	lineH := int(linePx * 1.3)
	height := h + int(8*scale)
	if len(tl) > 0 {
		height += gap + lineH*len(tl)
	}
	canvas := image.NewGray(image.Rect(0, 0, dots, height))
	draw.Draw(canvas, canvas.Bounds(), image.White, image.Point{}, draw.Src)
	// The logo, scaled onto white (transparency = paper), light pixels paper, then dithered.
	scaled := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(scaled, scaled.Bounds(), image.White, image.Point{}, draw.Src)
	xdraw.CatmullRom.Scale(scaled, scaled.Bounds(), logo, lb, xdraw.Over, nil)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if color.GrayModel.Convert(scaled.At(x, y)).(color.Gray).Y > headerLight {
				scaled.Set(x, y, color.White)
			}
		}
	}
	dither(canvas, scaled, image.Pt((dots-w)/2, 0))
	y := h + gap
	for _, l := range tl {
		lwid := font.MeasureString(l.fc, l.s).Ceil()
		d := &font.Drawer{Dst: canvas, Src: image.Black, Face: l.fc, Dot: fixed.P(max((dots-lwid)/2, 0), y+int(linePx))}
		d.DrawString(l.s)
		y += lineH
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
