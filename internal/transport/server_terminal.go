package transport

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	terminalApp "quiccpos/agent/internal/application/terminal"
	"quiccpos/agent/internal/domain/device"
	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/terminal"
)

// The POS terminals' API (terminal stage 0d, contract/agent.md §2.10):
// /api/terminal/*, a paired terminal's key or the web-UI session ("web").

// SetTerminal wires the terminal service (/api/terminal/*).
func (s *Server) SetTerminal(t *terminalApp.Service) { s.terminalSvc = t }

// actor is the terminal a request came from (the web session is "web").
func actor(r *http.Request) terminalApp.Actor {
	if d, ok := r.Context().Value(deviceCtxKey{}).(*device.Device); ok {
		return terminalApp.Actor{DeviceID: d.ID, DeviceName: d.Name}
	}
	return terminalApp.Actor{DeviceID: "web", DeviceName: "Pi web page"}
}

func (s *Server) terminalReady(w http.ResponseWriter) bool {
	if s.terminalSvc == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, errCodeMap("the terminal API isn't running", "terminal_unavailable"))
		return false
	}
	return true
}

func (s *Server) decodeTerminal(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("invalid body", "bad_request"))
		return false
	}
	return true
}

// terminalError maps the terminal flow's errors to {error, code} answers.
func (s *Server) terminalError(w http.ResponseWriter, r *http.Request, err error) {
	var mgr *terminal.ManagerRequired
	var thr *terminal.Throttled
	var cart *terminal.CartInvalid
	var oe *terminal.OpenElsewhere
	var due *terminal.CashDue
	switch {
	case errors.As(err, &oe):
		s.writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "code": "open_elsewhere", "holder": oe.Holder})
	case errors.As(err, &due):
		s.writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "code": "cash_due", "balance_cents": due.BalanceCents})
	case errors.Is(err, terminal.ErrNotPickable):
		s.writeJSON(w, http.StatusConflict, errCodeMap(err.Error(), "not_pickable"))
	case errors.Is(err, terminal.ErrTooManyDrafts):
		s.writeJSON(w, http.StatusConflict, errCodeMap(err.Error(), "too_many_drafts"))
	case errors.As(err, &mgr):
		s.writeJSON(w, http.StatusForbidden, map[string]any{"error": err.Error(), "code": "manager_required",
			"action": mgr.Action, "limit": mgr.Limit, "requested": mgr.Requested})
	case errors.As(err, &thr):
		w.Header().Set("Retry-After", strconv.Itoa(int(thr.Wait.Seconds())+1))
		s.writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": err.Error(), "code": "too_many_attempts",
			"retry_after": int(thr.Wait.Seconds()) + 1})
	case errors.As(err, &cart):
		s.writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error(), "code": "cart_invalid", "errors": cart.Issues})
	case errors.Is(err, terminalApp.ErrBadRequest):
		s.writeJSON(w, http.StatusBadRequest, errCodeMap(strings.TrimPrefix(err.Error(), "bad request: "), "bad_request"))
	case errors.Is(err, terminal.ErrBadPIN):
		s.writeJSON(w, http.StatusBadRequest, errCodeMap(err.Error(), "bad_pin"))
	case errors.Is(err, terminal.ErrWrongPIN):
		s.writeJSON(w, http.StatusUnauthorized, errCodeMap(err.Error(), "wrong_pin"))
	case errors.Is(err, terminal.ErrLoginRequired):
		s.writeJSON(w, http.StatusUnauthorized, errCodeMap(err.Error(), "login_required"))
	case errors.Is(err, terminal.ErrManagerPINWrong):
		s.writeJSON(w, http.StatusForbidden, errCodeMap(err.Error(), "manager_pin_invalid"))
	case errors.Is(err, terminal.ErrNoMenu):
		s.writeJSON(w, http.StatusServiceUnavailable, errCodeMap(err.Error(), "menu_unavailable"))
	case errors.Is(err, terminal.ErrNoConfig):
		s.writeJSON(w, http.StatusServiceUnavailable, errCodeMap(err.Error(), "config_unavailable"))
	case errors.Is(err, terminal.ErrOverBalance):
		s.writeJSON(w, http.StatusConflict, errCodeMap(err.Error(), "over_balance"))
	case errors.Is(err, terminal.ErrRefundTooBig):
		s.writeJSON(w, http.StatusConflict, errCodeMap(err.Error(), "refund_too_big"))
	case errors.Is(err, terminal.ErrNotPayable):
		s.writeJSON(w, http.StatusConflict, errCodeMap(err.Error(), "not_payable"))
	case errors.Is(err, terminal.ErrNotVoidable):
		s.writeJSON(w, http.StatusConflict, errCodeMap(err.Error(), "not_voidable"))
	case errors.Is(err, terminal.ErrNotEditable):
		s.writeJSON(w, http.StatusConflict, errCodeMap(err.Error(), "not_editable"))
	case errors.Is(err, terminal.ErrRefundFirst):
		s.writeJSON(w, http.StatusConflict, errCodeMap(err.Error(), "refund_first"))
	case errors.Is(err, terminal.ErrCustomerNotFound):
		s.writeJSON(w, http.StatusNotFound, errCodeMap(err.Error(), "customer_not_found"))
	case errors.Is(err, terminal.ErrTooManyAddresses):
		s.writeJSON(w, http.StatusConflict, errCodeMap(err.Error(), "too_many_addresses"))
	case errors.Is(err, terminalApp.ErrCustomersOff):
		s.writeJSON(w, http.StatusServiceUnavailable, errCodeMap(err.Error(), "customers_unavailable"))
	case errors.Is(err, terminal.ErrNotCounterOrder), errors.Is(err, order.ErrNotFound):
		s.writeJSON(w, http.StatusNotFound, errCodeMap("no such counter order", "not_found"))
	default:
		s.writeError(w, r, http.StatusInternalServerError, err)
	}
}

// POST /api/terminal/login {pin}
func (s *Server) handleTerminalLogin(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	var body struct {
		PIN string `json:"pin"`
	}
	if !s.decodeTerminal(w, r, &body) {
		return
	}
	res, err := s.terminalSvc.Login(r.Context(), actor(r), strings.TrimSpace(body.PIN))
	if err != nil {
		s.terminalError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, res)
}

// POST /api/terminal/manager-check {pin, action?}
func (s *Server) handleTerminalManagerCheck(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	var in terminalApp.ManagerCheckIn
	if !s.decodeTerminal(w, r, &in) {
		return
	}
	res, err := s.terminalSvc.ManagerCheck(r.Context(), actor(r), in)
	if err != nil {
		s.terminalError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, res)
}

// POST /api/terminal/logout
func (s *Server) handleTerminalLogout(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	s.terminalSvc.Logout(r.Context(), actor(r))
	w.WriteHeader(http.StatusNoContent)
}

// GET /api/terminal/session
func (s *Server) handleTerminalSession(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	s.writeJSON(w, http.StatusOK, s.terminalSvc.Session(actor(r)))
}

// GET /api/terminal/menu (ETag / If-None-Match)
func (s *Server) handleTerminalMenu(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	body, etag, err := s.terminalSvc.MenuView()
	if err != nil {
		s.terminalError(w, r, err)
		return
	}
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-cache")
	if match := r.Header.Get("If-None-Match"); match != "" && match == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// POST /api/terminal/quote
func (s *Server) handleTerminalQuote(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	var in terminalApp.QuoteIn
	if !s.decodeTerminal(w, r, &in) {
		return
	}
	v, err := s.terminalSvc.Quote(r.Context(), actor(r), in)
	if err != nil {
		s.terminalError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, v)
}

// POST /api/terminal/orders
func (s *Server) handleTerminalPlace(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	var in terminalApp.OrderIn
	if !s.decodeTerminal(w, r, &in) {
		return
	}
	res, err := s.terminalSvc.PlaceOrder(r.Context(), actor(r), in)
	if err != nil {
		s.terminalError(w, r, err)
		return
	}
	status := http.StatusCreated
	if res.Duplicate {
		status = http.StatusOK
	}
	s.writeJSON(w, status, res)
}

// GET /api/terminal/orders?open=1
func (s *Server) handleTerminalOrders(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	open := r.URL.Query().Get("open")
	list, err := s.terminalSvc.Orders(r.Context(), open == "1" || open == "true")
	if err != nil {
		s.terminalError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, list)
}

// GET /api/terminal/orders/{id}
func (s *Server) handleTerminalOrder(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	v, err := s.terminalSvc.Order(r.Context(), r.PathValue("id"))
	if err != nil {
		s.terminalError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, v)
}

// POST /api/terminal/orders/{id}/payments
func (s *Server) handleTerminalPayment(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	var in terminalApp.PaymentIn
	if !s.decodeTerminal(w, r, &in) {
		return
	}
	res, err := s.terminalSvc.AddPayment(r.Context(), actor(r), r.PathValue("id"), in)
	if err != nil {
		s.terminalError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, res)
}

// POST /api/terminal/orders/{id}/void
func (s *Server) handleTerminalVoid(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	var in terminalApp.VoidIn
	if !s.decodeTerminal(w, r, &in) {
		return
	}
	res, err := s.terminalSvc.Void(r.Context(), actor(r), r.PathValue("id"), in)
	if err != nil {
		s.terminalError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, res)
}

// PUT /api/terminal/orders/{id}/cart - a kitchen change after SEND (§2.10d).
func (s *Server) handleTerminalChangeCart(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	var in terminalApp.ChangeIn
	if !s.decodeTerminal(w, r, &in) {
		return
	}
	res, err := s.terminalSvc.ChangeCart(r.Context(), actor(r), r.PathValue("id"), in)
	if err != nil {
		s.terminalError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, res)
}

// POST /api/terminal/journal
func (s *Server) handleTerminalJournalPost(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	var in terminalApp.JournalIn
	if !s.decodeTerminal(w, r, &in) {
		return
	}
	e, err := s.terminalSvc.PostJournal(r.Context(), actor(r), in)
	if err != nil {
		s.terminalError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, e)
}

// GET /api/terminal/journal?limit=N
func (s *Server) handleTerminalJournal(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	limit := 100
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil {
		limit = v
	}
	list, err := s.terminalSvc.Journal(r.Context(), limit)
	if err != nil {
		s.terminalError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, list)
}

// GET /api/terminal/pickups - the Pickup list, every source (§2.10i).
func (s *Server) handleTerminalPickups(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	v, err := s.terminalSvc.Pickups(r.Context())
	if err != nil {
		s.terminalError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, v)
}

// POST /api/terminal/orders/{id}/picked-up
func (s *Server) handleTerminalPickedUp(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	v, err := s.terminalSvc.PickedUp(r.Context(), actor(r), r.PathValue("id"))
	if err != nil {
		s.terminalError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, v)
}

// GET /api/terminal/drafts
func (s *Server) handleTerminalDrafts(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	v, err := s.terminalSvc.Drafts(r.Context())
	if err != nil {
		s.terminalError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, v)
}

// PUT /api/terminal/drafts/{id}
func (s *Server) handleTerminalPutDraft(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	var in terminalApp.DraftIn
	if !s.decodeTerminal(w, r, &in) {
		return
	}
	v, err := s.terminalSvc.PutDraft(r.Context(), actor(r), r.PathValue("id"), in)
	if err != nil {
		s.terminalError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, v)
}

// DELETE /api/terminal/drafts/{id}
func (s *Server) handleTerminalDeleteDraft(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	if err := s.terminalSvc.DeleteDraft(r.Context(), actor(r), r.PathValue("id")); err != nil {
		s.terminalError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
