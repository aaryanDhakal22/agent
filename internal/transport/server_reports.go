package transport

import (
	"context"
	"errors"
	"net/http"

	orderApp "quiccpos/agent/internal/application/order"
	"quiccpos/agent/internal/domain/order"
)

// Driver reports any time + the POS driver ledger (2.2.3).

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

// reportPrintOutcome is clock-out's report_print.
type reportPrintOutcome struct {
	Printed      bool   `json:"printed"`
	DriverName   string `json:"driver_name"`
	PrintedOn    string `json:"printed_on,omitempty"`
	ReroutedFrom string `json:"rerouted_from,omitempty"`
	Error        string `json:"error,omitempty"`
	Code         string `json:"code,omitempty"`
}

// clockOutAnswer is the Driver plus what printing their report did.
type clockOutAnswer struct {
	*order.Driver
	ReportPrint *reportPrintOutcome `json:"report_print,omitempty"`
}

// clockOutReport prints a driver's report right after clock-out (2.2.3).
// nil when this Pi can't print documents.
func (s *Server) clockOutReport(ctx context.Context, d *order.Driver) *reportPrintOutcome {
	if !s.service.CanPrintReports() {
		return nil
	}
	res, err := s.service.PrintDriverReport(context.WithoutCancel(ctx), d.ID, "clock-out")
	switch {
	case err != nil:
		s.logger.Warn().Ctx(ctx).Err(err).Str("driver", d.Name).Msg("driver report on clock-out failed")
		return &reportPrintOutcome{DriverName: d.Name, Error: err.Error(), Code: "report_failed"}
	case !res.Printed():
		return &reportPrintOutcome{DriverName: res.DriverName, Error: res.Error, Code: "nothing_printed"}
	}
	return &reportPrintOutcome{Printed: true, DriverName: res.DriverName, PrintedOn: res.PrintedOn, ReroutedFrom: res.ReroutedFrom}
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
