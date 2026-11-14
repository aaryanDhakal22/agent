package transport

import (
	"errors"
	"net/http"
	"strconv"

	"quiccpos/agent/internal/application/squareterm"
	terminalApp "quiccpos/agent/internal/application/terminal"
)

// POST /api/terminal/refunds - a general refund (contract 2.10x): cash back
// with no order, or (batch J) money back on a card through Square.
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

// GET /api/terminal/refunds/card-payments?last4=&amount_cents= - the general
// card refund's fallback (contract 2.10x, batch J): the store's Square card
// payments that match, for the manager to pick one.
func (s *Server) handleTerminalCardPayments(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	q := r.URL.Query()
	cents, err := strconv.ParseInt(q.Get("amount_cents"), 10, 64)
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("amount_cents is a whole number of cents", "bad_request"))
		return
	}
	list, err := s.terminalSvc.GeneralRefundCardPayments(r.Context(), actor(r), q.Get("last4"), cents)
	if err != nil {
		var ref *squareterm.Refused
		if errors.As(err, &ref) {
			s.squareError(w, err, "")
			return
		}
		s.terminalError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"payments": list})
}
