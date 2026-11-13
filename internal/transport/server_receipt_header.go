package transport

import (
	"net/http"
	"strconv"

	"quiccpos/agent/internal/infra/printer/receipt"
)

// handleTerminalReceiptHeader is GET /api/terminal/receipt-header?cols=48|42|32
// (contract §2.1j): the receipts' header — the store's logo on the left, its
// name / address / phone on the right — as ESC/POS raster bytes for the
// terminal's own printouts, with an ETag (304 on If-None-Match). 204 when the
// store has no receipt logo: the terminal prints the centered text header.
func (s *Server) handleTerminalReceiptHeader(w http.ResponseWriter, r *http.Request) {
	cols := 48
	if v := r.URL.Query().Get("cols"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || (n != 48 && n != 42 && n != 32) {
			s.writeJSON(w, http.StatusBadRequest, errCodeMap("cols must be 48, 42 or 32", "bad_cols"))
			return
		}
		cols = n
	}
	raster, etag := receipt.HeaderRaster(s.storeContext(), cols)
	if raster == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-cache")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(raster)))
	_, _ = w.Write(raster)
}
