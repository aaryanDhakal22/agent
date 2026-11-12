package transport

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	orderApp "quiccpos/agent/internal/application/order"
	terminalApp "quiccpos/agent/internal/application/terminal"
	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/terminal"
)

// The terminal's Manager tab (contract/agent.md §2.10m): till report,
// driver reports, and the day's close (a manager's).

// reportError maps the Manager tab's errors; the rest go to terminalError.
func (s *Server) reportError(w http.ResponseWriter, r *http.Request, err error) {
	var none *terminalApp.ErrNothingPrinted
	switch {
	case errors.Is(err, terminalApp.ErrDriverNotAllowed):
		s.writeJSON(w, http.StatusForbidden, errCodeMap(err.Error(), "driver_not_allowed"))
	case errors.Is(err, terminalApp.ErrReportNotFound):
		s.writeJSON(w, http.StatusNotFound, errCodeMap(err.Error(), "not_found"))
	case errors.Is(err, terminalApp.ErrReportsOff):
		s.writeJSON(w, http.StatusServiceUnavailable, errCodeMap(err.Error(), "reports_unavailable"))
	case errors.Is(err, terminalApp.ErrNotTippable):
		s.writeJSON(w, http.StatusConflict, errCodeMap(err.Error(), "not_tippable"))
	case errors.Is(err, orderApp.ErrPrintingUnavailable):
		s.writeJSON(w, http.StatusServiceUnavailable, errCodeMap("This Pi has no printer to print reports on.", "printing_unavailable"))
	case errors.As(err, &none):
		s.writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error(), "code": "nothing_printed",
			"batch_id": none.Result.BatchID, "failed": none.Result.Failed})
	default:
		s.terminalError(w, r, err)
	}
}

// reportBatch reads ?batch=<id>|current (default current = 0).
func reportBatch(v string) (int64, bool) {
	v = strings.TrimSpace(v)
	if v == "" || v == "current" || v == "open" {
		return 0, true
	}
	id, err := strconv.ParseInt(v, 10, 64)
	return id, err == nil && id > 0
}

// GET /api/terminal/reports/batches
func (s *Server) handleTerminalReportBatches(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	bs, err := s.terminalSvc.ReportBatches(r.Context(), actor(r))
	if err != nil {
		s.reportError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"batches": bs})
}

// GET /api/terminal/reports/till ?drawer=<id> | ?batch=<id>|current
func (s *Server) handleTerminalTill(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	q := r.URL.Query()
	drawer, term, batch := strings.TrimSpace(q.Get("drawer")), strings.TrimSpace(q.Get("terminal")), q.Get("batch")
	if drawer != "" && (strings.TrimSpace(batch) != "" || term != "") {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("a drawer stands alone: no batch or terminal with it", "bad_request"))
		return
	}
	id, ok := reportBatch(batch)
	if !ok {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("batch is a number or 'current'", "bad_request"))
		return
	}
	t, err := s.terminalSvc.Till(r.Context(), actor(r), terminalApp.TillQuery{DrawerID: drawer, TerminalID: term, BatchID: id})
	if err != nil {
		s.reportError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"report": t})
}

// GET /api/terminal/reports/drivers ?batch=<id>|current
func (s *Server) handleTerminalDriverReports(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	id, ok := reportBatch(r.URL.Query().Get("batch"))
	if !ok {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("batch is a number or 'current'", "bad_request"))
		return
	}
	v, err := s.terminalSvc.DriverReports(r.Context(), actor(r), id)
	if err != nil {
		s.reportError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, v)
}

// POST /api/terminal/reports/drivers/print {batch?, driver_id?}
func (s *Server) handleTerminalPrintDriverReports(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	var body struct {
		Batch    any    `json:"batch"`
		DriverID string `json:"driver_id"`
	}
	if r.ContentLength != 0 && !s.decodeTerminal(w, r, &body) {
		return
	}
	var raw string
	switch b := body.Batch.(type) {
	case nil:
	case string:
		raw = b
	case float64:
		raw = strconv.FormatInt(int64(b), 10)
	default:
		raw = "bad"
	}
	id, ok := reportBatch(raw)
	if !ok {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("batch is a number or 'current'", "bad_request"))
		return
	}
	// The slips print to the end even if the terminal gives up waiting.
	res, err := s.terminalSvc.PrintDriverReports(context.WithoutCancel(r.Context()), actor(r), id, strings.TrimSpace(body.DriverID))
	if err != nil {
		s.reportError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, res)
}

// POST /api/terminal/close {force?, manager_pin?} — the day's close (Close
// batch) from a terminal: a manager logged in, or a manager's PIN.
func (s *Server) handleTerminalClose(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	var body struct {
		Force      bool     `json:"force"`
		ManagerPIN string   `json:"manager_pin"`
		Approvals  []string `json:"approvals"`
	}
	if r.ContentLength != 0 && !s.decodeTerminal(w, r, &body) {
		return
	}
	res, err := s.terminalSvc.CloseBatch(terminalApp.WithApprovals(r.Context(), body.Approvals), actor(r), body.Force, strings.TrimSpace(body.ManagerPIN))
	if err != nil {
		var mgr *terminal.ManagerRequired
		switch {
		case errors.As(err, &mgr), errors.Is(err, terminal.ErrLoginRequired), errors.Is(err, terminal.ErrManagerPINWrong),
			errors.Is(err, terminalApp.ErrReportsOff):
			s.reportError(w, r, err)
		default:
			s.floorErr(w, r, err) // 409 batch_has_leftovers, ...
		}
		return
	}
	opened, err := s.service.Floor(r.Context())
	if err != nil {
		s.floorErr(w, r, err)
		return
	}
	var unsettled *order.UnsettledList
	if res.Closed != nil {
		unsettled = res.Closed.Unsettled
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"closed": res.Closed, "opened": opened.Batch, "auto_finished": res.AutoFinished,
		"unsettled": unsettled})
}

// GET /api/terminal/checks — the All Checks Overview (contract §2.10o).
func (s *Server) handleTerminalChecks(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	q, err := terminalApp.ParseChecksQuery(r.URL.Query().Get, s.service.StoreLocation())
	if err != nil {
		s.reportError(w, r, err)
		return
	}
	page, err := s.terminalSvc.Checks(r.Context(), actor(r), q)
	if err != nil {
		s.reportError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, page)
}

// GET /api/terminal/checks/{id} — one order in full.
func (s *Server) handleTerminalCheck(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	d, err := s.terminalSvc.CheckByID(r.Context(), actor(r), r.PathValue("id"))
	if err != nil {
		s.reportError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, d)
}

// POST /api/terminal/orders/{id}/payments/{payment_id}/tip {tip_cents, client_key?}
// — a tip added to a card payment afterwards (contract §2.10p).
func (s *Server) handleTerminalPaymentTip(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	var in terminalApp.PaymentTipIn
	if !s.decodeTerminal(w, r, &in) {
		return
	}
	res, err := s.terminalSvc.AddTip(r.Context(), actor(r), r.PathValue("id"), r.PathValue("payment_id"), in)
	if err != nil {
		s.reportError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, res)
}
