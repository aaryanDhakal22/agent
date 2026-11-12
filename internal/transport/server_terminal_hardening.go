package transport

import (
	"errors"
	"net/http"

	terminalApp "quiccpos/agent/internal/application/terminal"
)

// The hardening round's terminal routes (contract/agent.md §2.10s).

// POST /api/terminal/orders/{id}/waste {reason} - Void waste.
func (s *Server) handleTerminalWaste(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	var in terminalApp.WasteIn
	if !s.decodeTerminal(w, r, &in) {
		return
	}
	res, err := s.terminalSvc.Waste(r.Context(), actor(r), r.PathValue("id"), in)
	var bad *terminalApp.BadWasteReason
	if errors.As(err, &bad) {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error(), "code": "bad_reason", "reasons": bad.Reasons})
		return
	}
	if err != nil {
		s.terminalError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"order": res.Order, "already": res.Already})
}

// GET /api/terminal/unsettled - the Unsettled Check List.
func (s *Server) handleTerminalUnsettled(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	l, err := s.terminalSvc.Unsettled(r.Context())
	if err != nil {
		s.terminalError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, l)
}
