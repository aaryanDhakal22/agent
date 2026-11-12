package transport

import (
	"image"
	"image/color"
	"net/http"
	"net/http/httptest"
	"testing"

	"quiccpos/agent/internal/domain/storeconfig"
	"quiccpos/agent/internal/infra/printer/receipt"
)

// GET /api/terminal/receipt-header (contract §2.1j): 204 without a logo, the
// raster + ETag with one (304 on If-None-Match), 400 on a width we don't print.
func TestTerminalReceiptHeader(t *testing.T) {
	r := newCCRig(t)
	r.s.SetDiag(DiagDeps{StoreCtx: func() storeconfig.StoreContext {
		sc := storeconfig.StoreContext{}
		sc.Location.Name = "Lombardi's"
		sc.Location.Config = map[string]any{"address": "931 Taylor Ave", "phone": "410-555-0100"}
		return sc
	}})
	receipt.SetReceiptLogo(nil, "")
	t.Cleanup(func() { receipt.SetReceiptLogo(nil, "") })

	if rec := call(r.h, "GET", "/api/terminal/receipt-header?cols=42", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("needs the terminal key: %d", rec.Code)
	}
	if rec := call(r.h, "GET", "/api/terminal/receipt-header?cols=42", r.termKey, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("no logo = 204: %d %s", rec.Code, rec.Body)
	}
	if rec := call(r.h, "GET", "/api/terminal/receipt-header?cols=40", r.termKey, ""); rec.Code != http.StatusBadRequest || code(rec) != "bad_cols" {
		t.Fatalf("cols 40: %d %s", rec.Code, rec.Body)
	}

	logo := image.NewGray(image.Rect(0, 0, 200, 120))
	for y := 0; y < 120; y++ {
		for x := 0; x < 200; x++ {
			logo.SetGray(x, y, color.Gray{Y: uint8((x + y) % 256)})
		}
	}
	receipt.SetReceiptLogo(logo, `"l0123456789abcdef"`)
	rec := call(r.h, "GET", "/api/terminal/receipt-header?cols=42", r.termKey, "")
	et := rec.Header().Get("ETag")
	if rec.Code != http.StatusOK || rec.Body.Len() < 100 || et == "" {
		t.Fatalf("with a logo: %d %d bytes etag %q", rec.Code, rec.Body.Len(), et)
	}
	// GS v 0: the raster is 512 dots (64 bytes) wide at 42 columns.
	b := rec.Body.Bytes()
	for i := 0; i+5 < len(b); i++ {
		if b[i] == 0x1d && b[i+1] == 'v' && b[i+2] == '0' {
			if w := int(b[i+4]) | int(b[i+5])<<8; w != 64 {
				t.Errorf("raster width %d bytes, want 64", w)
			}
			break
		}
	}
	req := httptest.NewRequest("GET", "/api/terminal/receipt-header?cols=42", nil)
	req.Header.Set("X-Agent-Device-Key", r.termKey)
	req.Header.Set("If-None-Match", et)
	rec = httptest.NewRecorder()
	r.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotModified {
		t.Fatalf("If-None-Match: %d", rec.Code)
	}
	if rec := call(r.h, "GET", "/api/terminal/receipt-header?cols=32", r.termKey, ""); rec.Code != 200 || rec.Header().Get("ETag") == et {
		t.Fatalf("another width is another ETag: %d %q", rec.Code, rec.Header().Get("ETag"))
	}
}
