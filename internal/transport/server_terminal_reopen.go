package transport

import (
	"errors"
	"net/http"

	terminalApp "quiccpos/agent/internal/application/terminal"
	"quiccpos/agent/internal/domain/terminal"
)

// POST /api/terminal/checks/{id}/reopen — reopen a check from All checks
// (contract/agent.md §2.10ak, batch B): any cashier, the open batch only.
func (s *Server) handleTerminalReopen(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	res, err := s.terminalSvc.Reopen(r.Context(), actor(r), r.PathValue("id"))
	switch {
	case err == nil:
		s.writeJSON(w, http.StatusOK, res)
	case errors.Is(err, terminal.ErrBatchClosed):
		s.writeJSON(w, http.StatusConflict, errCodeMap(err.Error(), "batch_closed"))
	case errors.Is(err, terminal.ErrReopenVoided):
		s.writeJSON(w, http.StatusConflict, errCodeMap(err.Error(), "voided"))
	case errors.Is(err, terminal.ErrNotReopenable):
		s.writeJSON(w, http.StatusConflict, errCodeMap(err.Error(), "not_reopenable"))
	case errors.Is(err, terminalApp.ErrReopenUnsupported):
		s.writeJSON(w, http.StatusNotFound, errCodeMap(err.Error(), "not_found"))
	default:
		s.reportError(w, r, err)
	}
}
