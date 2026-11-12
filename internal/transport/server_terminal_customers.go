package transport

import (
	"net/http"
	"strconv"

	terminalApp "quiccpos/agent/internal/application/terminal"
)

// The store's customers for the terminals (TERMINAL-PLAN 1d, contract
// §2.10j): lookup by phone (offline), add / edit, recent orders.

// GET /api/terminal/customers?phone=<digits>
func (s *Server) handleTerminalCustomers(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	v, err := s.terminalSvc.LookupCustomers(r.Context(), r.URL.Query().Get("phone"))
	if err != nil {
		s.terminalError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, v)
}

// POST /api/terminal/customers
func (s *Server) handleTerminalAddCustomer(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	var in terminalApp.CustomerIn
	if !s.decodeTerminal(w, r, &in) {
		return
	}
	v, err := s.terminalSvc.AddCustomer(r.Context(), actor(r), in)
	if err != nil {
		s.terminalError(w, r, err)
		return
	}
	status := http.StatusOK
	if v.Created {
		status = http.StatusCreated
	}
	s.writeJSON(w, status, v)
}

// PATCH /api/terminal/customers/{id}
func (s *Server) handleTerminalEditCustomer(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	var in terminalApp.CustomerPatch
	if !s.decodeTerminal(w, r, &in) {
		return
	}
	v, err := s.terminalSvc.EditCustomer(r.Context(), actor(r), r.PathValue("id"), in)
	if err != nil {
		s.terminalError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, v)
}

// POST /api/terminal/customers/{id}/addresses
func (s *Server) handleTerminalAddCustomerAddress(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	var in terminalApp.AddressIn
	if !s.decodeTerminal(w, r, &in) {
		return
	}
	v, err := s.terminalSvc.AddCustomerAddress(r.Context(), actor(r), r.PathValue("id"), in)
	if err != nil {
		s.terminalError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, v)
}

// GET /api/terminal/customers/{id}/orders?limit=N
func (s *Server) handleTerminalCustomerOrders(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	v, err := s.terminalSvc.CustomerOrdersOf(r.Context(), r.PathValue("id"), limit)
	if err != nil {
		s.terminalError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, v)
}

// GET /api/terminal/addresses?q= — address suggestions as you type, from an
// open-data address index for the delivery area published to the Pi like
// the map file. Not built yet: 501.
func (s *Server) handleTerminalAddresses(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusNotImplemented, errCodeMap("address suggestions aren't on this Pi yet", "not_implemented"))
}
