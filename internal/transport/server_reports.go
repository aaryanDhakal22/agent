package transport

import (
	"context"
	"errors"
	"net/http"

	orderApp "quiccpos/agent/internal/application/order"
	"quiccpos/agent/internal/domain/order"
)

// Driver reports any time (2.3.0).

// POST /api/drivers/{id}/report/print — print it on the receipt printer
// → 200 {driver_id, driver_name,
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
