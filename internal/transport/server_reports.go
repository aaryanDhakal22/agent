package transport

import (
	"context"
	"errors"
	"net/http"

	orderApp "quiccpos/agent/internal/application/order"
	"quiccpos/agent/internal/domain/order"
)

// Driver reports any time + the POS driver ledger (2.3.0).

// GET /api/drivers/{id}/report — the driver's report for the open batch so
// far, merged with the POS ledger as it is now.
func (s *Server) handleDriverReport(w http.ResponseWriter, r *http.Request) {
	rep, err := s.service.DriverReport(r.Context(), r.PathValue("id"))
	if err != nil {
		s.floorErr(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, rep)
}

// POST /api/drivers/{id}/report/print — print it on the receipt printer
// (a fresh POS ledger first, ≤ 5 s) → 200 {driver_id, driver_name,
// printed_on, rerouted_from?, report} · 502 nothing_printed · 503
// printing_unavailable · 404 driver_not_found.
func (s *Server) handlePrintDriverReport(w http.ResponseWriter, r *http.Request) {
	// The slip prints to the end even if the tablet gives up waiting.
	res, err := s.service.PrintDriverReport(context.WithoutCancel(r.Context()), r.PathValue("id"), "button")
	switch {
	case errors.Is(err, orderApp.ErrPrintingUnavailable):
		s.writeJSON(w, http.StatusServiceUnavailable, errCodeMap("This Pi has no printer to print reports on.", "printing_unavailable"))
		return
	case err != nil:
		s.floorErr(w, r, err)
		return
	case !res.Printed():
		s.writeJSON(w, http.StatusBadGateway, map[string]any{"error": res.Error, "code": "nothing_printed",
			"driver_id": res.DriverID, "driver_name": res.DriverName, "report": res.Report})
		return
	}
	s.writeJSON(w, http.StatusOK, res)
}

// clockOutAnswer is the Driver plus whether their report is printing in
// the background (the outcome comes as SSE driver.report_printed).
type clockOutAnswer struct {
	*order.Driver
	ReportPrinting bool `json:"report_printing,omitempty"`
}

// PUT /api/integration/driver-ledger — the hub's POS delivery checks of the
// day (kept in memory) → 204 · 400.
func (s *Server) handleIntegrationDriverLedger(w http.ResponseWriter, r *http.Request) {
	var l order.DriverLedger
	if err := decodeBody(r, &l); err != nil {
		s.floorErr(w, r, err)
		return
	}
	if err := s.service.SetDriverLedger(r.Context(), l); err != nil {
		s.floorErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
