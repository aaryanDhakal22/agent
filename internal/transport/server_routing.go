package transport

import (
	"errors"
	"net/http"

	"quiccpos/agent/internal/domain/printroute"
)

// Routing & emergency reroute (the owner, 2026-11-12; contract §2.3c, spec
// §4.42): which printer stands in when one can't print. The Pi web UI's
// Settings › Routing tab; device auth like the other printer settings.

// routingBody is the answer of both routes: the setting, the printers with
// their live status and, per printer, the order its tickets would fail over
// in right now.
func (s *Server) routingBody() map[string]any {
	d := s.diag.Dispatcher
	return map[string]any{
		"routing":  d.Routing(),
		"printers": d.RoutingView(),
		"modes":    []string{printroute.ModeNone, printroute.ModeOne, printroute.ModePerPrinter},
	}
}

// GET /api/settings/routing -> {routing, printers, modes}.
func (s *Server) handleGetRouting(w http.ResponseWriter, r *http.Request) {
	if s.diag.Dispatcher == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, errCodeMap("printing isn't available", "not_available"))
		return
	}
	s.writeJSON(w, http.StatusOK, s.routingBody())
}

// PUT /api/settings/routing {mode, emergency_printer_id, targets} -> the GET
// body | 400 {error, code: bad_request | bad_mode | no_emergency_printer |
// unknown_printer | too_many_targets}.
func (s *Server) handleSetRouting(w http.ResponseWriter, r *http.Request) {
	if s.diag.Dispatcher == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, errCodeMap("printing isn't available", "not_available"))
		return
	}
	var req printroute.Routing
	if err := decodeBody(r, &req); err != nil || r.ContentLength == 0 {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("send the routing setting as a JSON object", "bad_request"))
		return
	}
	_, err := s.diag.Dispatcher.SaveRouting(r.Context(), req)
	var ve *printroute.ValidationError
	if errors.As(err, &ve) {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap(ve.Reason, ve.Code))
		return
	}
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	if s.broker != nil {
		// Its own event: settings.updated carries the integration settings
		// whole, and the tablets read it that way.
		s.broker.Publish("routing.updated", s.diag.Dispatcher.Routing())
	}
	s.writeJSON(w, http.StatusOK, s.routingBody())
}

// readRouting is the settings history's view of the setting.
func (s *Server) readRouting(r *http.Request) any {
	if s.diag.Dispatcher == nil {
		return nil
	}
	return s.diag.Dispatcher.Routing()
}
