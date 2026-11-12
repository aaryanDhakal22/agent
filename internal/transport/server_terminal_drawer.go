package transport

import (
	"errors"
	"net/http"
	"strconv"

	terminalApp "quiccpos/agent/internal/application/terminal"
	"quiccpos/agent/internal/domain/terminal"
)

// Cash drawers (contract/agent.md §2.10k) and the Z report (§2.10l).

func (s *Server) drawerError(w http.ResponseWriter, r *http.Request, err error) {
	var open *terminal.DrawerOpen
	var has *terminal.CashierHasDrawer
	var cash terminalApp.ErrNotEnoughCash
	switch {
	case errors.As(err, &open):
		s.writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "code": "drawer_open", "drawer": open.Drawer})
	case errors.As(err, &has):
		s.writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "code": "cashier_has_drawer", "drawer": has.Drawer})
	case errors.As(err, &cash):
		s.writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "code": "not_enough_cash", "expected_cents": cash.Expected})
	case errors.Is(err, terminal.ErrNoDrawer):
		s.writeJSON(w, http.StatusConflict, errCodeMap(err.Error(), "no_drawer"))
	case errors.Is(err, terminal.ErrDrawerClosed):
		s.writeJSON(w, http.StatusConflict, errCodeMap(err.Error(), "drawer_closed"))
	case errors.Is(err, terminal.ErrSameCashier):
		s.writeJSON(w, http.StatusConflict, errCodeMap(err.Error(), "same_cashier"))
	case errors.Is(err, terminal.ErrMovementKeyReused):
		s.writeJSON(w, http.StatusConflict, errCodeMap(err.Error(), "client_key_reused"))
	case errors.Is(err, terminal.ErrDrawerNotFound):
		s.writeJSON(w, http.StatusNotFound, errCodeMap(err.Error(), "drawer_not_found"))
	case errors.Is(err, terminal.ErrDrawerNotAllowed):
		s.writeJSON(w, http.StatusForbidden, errCodeMap(err.Error(), "drawer_not_allowed"))
	default:
		s.terminalError(w, r, err)
	}
}

// GET /api/terminal/drawer
func (s *Server) handleTerminalDrawer(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	v, err := s.terminalSvc.Drawer(r.Context(), actor(r))
	if err != nil {
		s.drawerError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, v)
}

// GET /api/terminal/drawers?batch=<id> (default: the open batch)
func (s *Server) handleTerminalDrawers(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	var batch int64
	if v := r.URL.Query().Get("batch"); v != "" && v != "current" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 {
			s.writeJSON(w, http.StatusBadRequest, errCodeMap("batch is a batch id or current", "bad_request"))
			return
		}
		batch = n
	}
	list, err := s.terminalSvc.Drawers(r.Context(), batch)
	if err != nil {
		s.drawerError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"drawers": list})
}

// POST /api/terminal/drawer/open {start_cents | denominations}
func (s *Server) handleTerminalDrawerOpen(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	var in terminalApp.DrawerOpenIn
	if !s.decodeTerminal(w, r, &in) {
		return
	}
	d, err := s.terminalSvc.OpenDrawer(r.Context(), actor(r), in)
	if err != nil {
		s.drawerError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, map[string]any{"drawer": d})
}

func (s *Server) drawerMove(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.terminalReady(w) {
			return
		}
		var in terminalApp.DrawerMoveIn
		if !s.decodeTerminal(w, r, &in) {
			return
		}
		res, err := s.terminalSvc.MoveDrawer(r.Context(), actor(r), kind, in)
		if err != nil {
			s.drawerError(w, r, err)
			return
		}
		s.writeJSON(w, http.StatusOK, res)
	}
}

// POST /api/terminal/drawer/close {counted_cents | denominations, drawer_id?, manager_pin?}
func (s *Server) handleTerminalDrawerClose(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	var in terminalApp.DrawerCloseIn
	if !s.decodeTerminal(w, r, &in) {
		return
	}
	d, err := s.terminalSvc.CloseDrawer(r.Context(), actor(r), in)
	if err != nil {
		s.drawerError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"drawer": d})
}

// POST /api/terminal/drawer/handover {counted_cents | denominations}
func (s *Server) handleTerminalDrawerHandover(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	var in terminalApp.DrawerCloseIn
	if !s.decodeTerminal(w, r, &in) {
		return
	}
	res, err := s.terminalSvc.HandoverDrawer(r.Context(), actor(r), in)
	if err != nil {
		s.drawerError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, res)
}

// GET /api/terminal/z?batch=<id> (one) or the kept ones, newest first.
func (s *Server) handleTerminalZ(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	s.writeZ(w, r, r.URL.Query().Get("batch"))
}

// GET /api/batches/{id}/z (tablets, the Pi page)
func (s *Server) handleBatchZ(w http.ResponseWriter, r *http.Request) {
	if s.terminalSvc == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, errCodeMap("terminals aren't wired", "not_available"))
		return
	}
	s.writeZ(w, r, r.PathValue("id"))
}

func (s *Server) writeZ(w http.ResponseWriter, r *http.Request, batch string) {
	if batch == "" {
		list, err := s.terminalSvc.ZReports(r.Context(), 10)
		if err != nil {
			s.writeError(w, r, http.StatusInternalServerError, err)
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]any{"reports": list})
		return
	}
	id, err := strconv.ParseInt(batch, 10, 64)
	if err != nil || id < 1 {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("batch is a batch id", "bad_request"))
		return
	}
	z, err := s.terminalSvc.ZReport(r.Context(), id)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	if z == nil {
		s.writeJSON(w, http.StatusNotFound, errCodeMap("no Z report for that batch (still open, not built yet, or no longer kept)", "not_found"))
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"report": z})
}
