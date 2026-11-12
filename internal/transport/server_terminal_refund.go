package transport

import (
	"net/http"

	terminalApp "quiccpos/agent/internal/application/terminal"
)

// POST /api/terminal/refunds - a general refund (contract 2.10x): cash back
// with no order.
func (s *Server) handleTerminalGeneralRefund(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	var in terminalApp.GeneralRefundIn
	if !s.decodeTerminal(w, r, &in) {
		return
	}
	res, err := s.terminalSvc.GeneralRefund(r.Context(), actor(r), in)
	if err != nil {
		s.terminalError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, res)
}
