package transport

import (
	"errors"
	"io"
	"net/http"
	"strings"

	orderApp "quiccpos/agent/internal/application/order"
	printerApp "quiccpos/agent/internal/application/printer"
	"quiccpos/agent/internal/domain/order"
)

// Receipt settings for Quibase's Settings -> Receipts (agent 2.0.2): read and
// change them, and print a sample with them. All device-auth routes.

// maxReceiptPrefsBody caps PUT /api/settings/receipt.
const maxReceiptPrefsBody = 8 << 10

// SetReceiptSample wires PRINT SAMPLE: build renders the sample customer
// receipt + kitchen ticket with the given settings (receipt.BuildSample with
// the live store context).
func (s *Server) SetReceiptSample(build func(order.ReceiptPrefs) []byte) { s.receiptSample = build }

// receiptPrefsBody is the answer of both receipt-settings routes.
func (s *Server) receiptPrefsBody(p order.ReceiptPrefs) map[string]any {
	sc := s.storeContext()
	loc := sc.LocationConfigString("receipt_footer", "")
	return map[string]any{
		"prefs": p,
		// What the footer line prints with these settings ("" = none).
		"footer_effective": p.Footer(loc),
		// What it prints when footer_text is null: the location's footer,
		// else ONLINE ORDER.
		"footer_store_default": order.DefaultReceiptPrefs().Footer(loc),
		// The location's own footer (main's /app/store), "" = not set — what
		// footer_text null falls back to before "ONLINE ORDER".
		"location_footer": loc,
	}
}

// GET /api/settings/receipt -> {prefs, defaults, footer_effective,
// footer_store_default, location_footer}.
func (s *Server) handleGetReceiptPrefs(w http.ResponseWriter, r *http.Request) {
	body := s.receiptPrefsBody(s.service.ReceiptPrefs(r.Context()))
	body["defaults"] = order.DefaultReceiptPrefs()
	s.writeJSON(w, http.StatusOK, body)
}

// PUT /api/settings/receipt {full prefs} -> {prefs, footer_effective,
// footer_store_default, location_footer}. A key the body lacks takes its default; 400 bad_request
// with a plain reason when a setting is out of range.
func (s *Server) handleSetReceiptPrefs(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxReceiptPrefsBody+1))
	if err != nil || len(raw) > maxReceiptPrefsBody || len(strings.TrimSpace(string(raw))) == 0 {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("send the receipt settings as a JSON object", "bad_request"))
		return
	}
	p, err := order.DecodeReceiptPrefs(raw)
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("invalid body: "+err.Error(), "bad_request"))
		return
	}
	saved, err := s.service.SetReceiptPrefs(r.Context(), p)
	if errors.Is(err, orderApp.ErrBadRequest) {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap(err.Error(), "bad_request"))
		return
	}
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	s.writeJSON(w, http.StatusOK, s.receiptPrefsBody(saved))
}

// POST /api/printers/sample {printer_id?} — a sample customer receipt + kitchen
// ticket with the current receipt settings, on that printer only (default:
// the receipt printer, else the first one). Never fails over, like the test
// print; 404 not_configured for an unknown printer (or none set up) — the
// only 404 these routes answer (the tablet reads any other as "this Pi
// predates receipt settings"), 409
// test_in_progress while a test or sample is printing.
func (s *Server) handlePrinterSample(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PrinterID string `json:"printer_id"`
	}
	if err := decodeBody(r, &req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("invalid body", "bad_request"))
		return
	}
	if s.diag.Dispatcher == nil || s.receiptSample == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, errCodeMap("printing isn't available", "not_available"))
		return
	}
	id := samplePrinter(s.diag.Dispatcher.Roster(), strings.TrimSpace(req.PrinterID))
	if id == "" {
		msg := "No printers are set up for this store."
		if req.PrinterID != "" {
			msg = "No printer with that id."
		}
		s.writeJSON(w, http.StatusNotFound, errCodeMap(msg, printerApp.TestNotConfigured))
		return
	}
	if !s.testMu.TryLock() {
		s.writeJSON(w, http.StatusConflict, errCodeMap("A printer test is already running — wait for it.", "test_in_progress"))
		return
	}
	defer s.testMu.Unlock()

	sample := s.receiptSample(s.service.PrintPrefs())
	results := s.diag.Dispatcher.PrintTest(r.Context(), id, func(printerApp.RosterEntry) []byte { return sample })
	if results == nil {
		results = []printerApp.TestResult{}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// samplePrinter picks the sample's printer: the requested id if it's on the
// roster, else (none requested) the receipt printer, else the first; "" when
// there's no such printer.
func samplePrinter(roster []printerApp.RosterEntry, want string) string {
	for _, e := range roster {
		if want != "" && e.ID == want {
			return e.ID
		}
	}
	if want != "" || len(roster) == 0 {
		return ""
	}
	for _, e := range roster {
		if e.IsReceipt {
			return e.ID
		}
	}
	return roster[0].ID
}
