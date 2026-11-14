package transport

import (
	"errors"
	"net/http"

	terminalApp "quiccpos/agent/internal/application/terminal"
	"quiccpos/agent/internal/domain/terminal"
)

// POST /api/terminal/checks/{id}/tenders/{tid}/void - void one tender from
// Cash Out (contract §2.10al, batch C): any cashier, cash back out of the
// drawer, a card refunded on Square.
func (s *Server) handleTerminalTenderVoid(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	var in terminalApp.TenderVoidIn
	if !s.decodeTerminal(w, r, &in) {
		return
	}
	res, err := s.terminalSvc.VoidTender(r.Context(), actor(r), r.PathValue("id"), r.PathValue("tid"), in)
	switch {
	case err == nil:
		s.writeJSON(w, http.StatusOK, res)
	case errors.Is(err, terminal.ErrTenderNotFound):
		s.writeJSON(w, http.StatusNotFound, errCodeMap(err.Error(), "not_found"))
	case errors.Is(err, terminal.ErrNotVoidableTender):
		s.writeJSON(w, http.StatusConflict, errCodeMap(err.Error(), "not_voidable_tender"))
	case errors.Is(err, terminal.ErrTenderRefunded):
		s.writeJSON(w, http.StatusConflict, errCodeMap(err.Error(), "tender_refunded"))
	case errors.Is(err, terminal.ErrDriverNotAllowed):
		s.writeJSON(w, http.StatusForbidden, errCodeMap(err.Error(), "driver_not_allowed"))
	default:
		s.terminalError(w, r, err)
	}
}
