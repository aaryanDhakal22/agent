package transport

import (
	"errors"
	"net/http"

	terminalApp "quiccpos/agent/internal/application/terminal"
	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/terminal"
)

// The terminal's Dispatch and Pickup bars (contract §2.10v): terminal-key
// routes over the tablet's floor work, a staff login required, journaled.

// dispatchErr: the terminal's own errors as on every /api/terminal route,
// the floor's as on the tablet's routes.
func (s *Server) dispatchErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, terminalApp.ErrDispatchOff):
		s.writeJSON(w, http.StatusServiceUnavailable, errCodeMap(err.Error(), "dispatch_unavailable"))
	case errors.Is(err, terminal.ErrLoginRequired), errors.Is(err, terminalApp.ErrBadRequest), errors.Is(err, terminal.ErrNoConfig):
		s.terminalError(w, r, err)
	default:
		s.floorErr(w, r, err)
	}
}

// GET /api/terminal/drivers
func (s *Server) handleTerminalDrivers(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	ds, err := s.terminalSvc.Drivers(r.Context(), actor(r))
	if err != nil {
		s.dispatchErr(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"drivers": ds})
}

// POST /api/terminal/deliveries/send-out {driver_id, order_ids, mark_ready}
func (s *Server) handleTerminalSendOut(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	var in terminalApp.SendOutIn
	if !s.decodeTerminal(w, r, &in) {
		return
	}
	sent, err := s.terminalSvc.SendOut(r.Context(), actor(r), in)
	if err != nil {
		s.dispatchErr(w, r, err)
		return
	}
	if sent == nil {
		sent = []order.StoredOrder{}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"orders": sent})
}

// POST /api/terminal/drivers/{id}/returned
func (s *Server) handleTerminalDriverReturned(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	res, err := s.terminalSvc.DriverReturned(r.Context(), actor(r), r.PathValue("id"))
	if err != nil {
		s.dispatchErr(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, res)
}

// POST /api/terminal/orders/{id}/time {ready_at}
func (s *Server) handleTerminalOrderTime(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	var in terminalApp.TimeIn
	if !s.decodeTerminal(w, r, &in) {
		return
	}
	res, err := s.terminalSvc.ChangeTime(r.Context(), actor(r), r.PathValue("id"), in)
	if err != nil {
		s.dispatchErr(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, res)
}
