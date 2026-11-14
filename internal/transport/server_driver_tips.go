package transport

import (
	"errors"
	"net/http"

	terminalApp "quiccpos/agent/internal/application/terminal"
	"quiccpos/agent/internal/domain/device"
	"quiccpos/agent/internal/domain/order"
)

// Card tips the driver types before Close batch (contract/agent.md §2.10am,
// batch F): his Tips tab on the terminal and the manager's correction from
// Quibase / the web UI.

// tipError maps a typed tip's errors (409 batch_closed besides the driver
// view's own).
func (s *Server) tipError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, order.ErrTipBatchClosed) {
		s.writeJSON(w, http.StatusConflict, errCodeMap(err.Error(), "batch_closed"))
		return
	}
	s.driverError(w, r, err)
}

// GET /api/terminal/driver/tips
func (s *Server) handleDriverTips(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	v, err := s.terminalSvc.Tips(r.Context(), actor(r))
	if err != nil {
		s.tipError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, v)
}

// POST /api/terminal/driver/orders/{id}/no-tip
func (s *Server) handleDriverNoTip(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	res, err := s.terminalSvc.NoTip(r.Context(), actor(r), r.PathValue("id"))
	if err != nil {
		s.tipError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, res)
}

// PUT /api/orders/{id}/driver-tip {tip_state, tip_cents?} - Quibase (device
// key) or the web UI.
func (s *Server) handleManagerDriverTip(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	var in terminalApp.ManagerTipIn
	if !s.decodeTerminal(w, r, &in) {
		return
	}
	by, deviceID := terminalApp.TipByWeb, "web"
	if d, ok := r.Context().Value(deviceCtxKey{}).(*device.Device); ok {
		by, deviceID = terminalApp.TipByQuibase, d.ID
	}
	res, err := s.terminalSvc.ManagerTip(r.Context(), by, deviceID, r.PathValue("id"), in)
	if err != nil {
		s.tipError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, res)
}
