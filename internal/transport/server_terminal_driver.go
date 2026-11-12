package transport

import (
	"context"
	"errors"
	"net/http"

	orderApp "quiccpos/agent/internal/application/order"
	terminalApp "quiccpos/agent/internal/application/terminal"
	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/terminal"
)

// The terminal's driver view (contract/agent.md §2.10b):
// /api/terminal/driver/*, a paired terminal's key (or the web-UI session)
// with a driver logged in on it.

// driverError maps the driver view's errors; the rest are the terminal's.
func (s *Server) driverError(w http.ResponseWriter, r *http.Request, err error) {
	var tk *order.ErrTaken
	switch {
	case errors.As(err, &tk):
		s.writeJSON(w, http.StatusConflict, map[string]any{"error": "someone else got these first, or they can't be claimed (any more)",
			"code": "taken", "order_ids": tk.IDs})
	case errors.Is(err, terminal.ErrDriverOnly):
		s.writeJSON(w, http.StatusForbidden, errCodeMap(err.Error(), "driver_only"))
	case errors.Is(err, terminal.ErrNotYourOrder):
		s.writeJSON(w, http.StatusForbidden, errCodeMap(err.Error(), "not_your_order"))
	case errors.Is(err, terminal.ErrDriverOut):
		s.writeJSON(w, http.StatusConflict, errCodeMap(err.Error(), "driver_out"))
	case errors.Is(err, orderApp.ErrPrintingUnavailable):
		s.writeJSON(w, http.StatusServiceUnavailable, errCodeMap("This Pi has no printer to print reports on.", "printing_unavailable"))
	case errors.Is(err, order.ErrNotFound):
		s.writeJSON(w, http.StatusNotFound, errCodeMap("no such order", "not_found"))
	case errors.Is(err, terminal.ErrLoginRequired), errors.Is(err, terminalApp.ErrBadRequest):
		s.terminalError(w, r, err)
	case errors.Is(err, order.ErrNotTippable), errors.Is(err, order.ErrTipLocked), errors.Is(err, order.ErrInvalidState),
		errors.Is(err, order.ErrDriverNotFound), errors.Is(err, order.ErrDriverNotAvailable), errors.Is(err, order.ErrNoOrders),
		errors.Is(err, orderApp.ErrBadRequest):
		s.floorErr(w, r, err)
	default:
		s.terminalError(w, r, err)
	}
}

// GET /api/terminal/driver/available
func (s *Server) handleDriverAvailable(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	list, err := s.terminalSvc.Available(r.Context(), actor(r))
	if err != nil {
		s.driverError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, list)
}

// POST /api/terminal/driver/claim {order_ids}
func (s *Server) handleDriverClaim(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	var in terminalApp.ClaimIn
	if !s.decodeTerminal(w, r, &in) {
		return
	}
	res, err := s.terminalSvc.Claim(r.Context(), actor(r), in)
	if err != nil {
		s.driverError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, res)
}

// GET /api/terminal/driver/mine
func (s *Server) handleDriverMine(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	v, err := s.terminalSvc.Mine(r.Context(), actor(r))
	if err != nil {
		s.driverError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, v)
}

// POST /api/terminal/driver/orders/{id}/tip {tip_cents}
func (s *Server) handleDriverTip(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	var in terminalApp.TipIn
	if !s.decodeTerminal(w, r, &in) {
		return
	}
	res, err := s.terminalSvc.Tip(r.Context(), actor(r), r.PathValue("id"), in)
	if err != nil {
		s.driverError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, res)
}

// POST /api/terminal/driver/orders/{id}/close
func (s *Server) handleDriverClose(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	res, err := s.terminalSvc.CloseOrder(r.Context(), actor(r), r.PathValue("id"))
	if err != nil {
		s.driverError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, res)
}

// GET /api/terminal/driver/report
func (s *Server) handleDriverReportGet(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	rep, err := s.terminalSvc.Report(r.Context(), actor(r))
	if err != nil {
		s.driverError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, rep)
}

// POST /api/terminal/driver/report — print it on the receipt printer.
func (s *Server) handleDriverReportPrint(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	// The slip prints to the end even if the terminal gives up waiting.
	res, err := s.terminalSvc.PrintReport(context.WithoutCancel(r.Context()), actor(r))
	switch {
	case err != nil:
		s.driverError(w, r, err)
	case res.Error != "":
		s.writeJSON(w, http.StatusBadGateway, map[string]any{"error": res.Error, "code": "nothing_printed",
			"driver_id": res.DriverID, "driver_name": res.DriverName, "report": res.Report})
	default:
		s.writeJSON(w, http.StatusOK, res)
	}
}
