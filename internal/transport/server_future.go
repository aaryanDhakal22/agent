package transport

import (
	"errors"
	"net/http"
	"time"

	orderApp "quiccpos/agent/internal/application/order"
)

// Future orders (device routes): the first confirm (schedule) and staff's
// actions on a waiting one (2.4.3: MAKE NOW, CHANGE DUE TIME, PRINT NOW,
// CANCEL).

// POST /api/orders/{id}/schedule {lead_minutes} -> StoredOrder | 409 not_future.
func (s *Server) handleSchedule(w http.ResponseWriter, r *http.Request) {
	var req struct {
		LeadMinutes *int `json:"lead_minutes"`
	}
	if err := decodeBody(r, &req); err != nil {
		s.floorErr(w, r, err)
		return
	}
	if req.LeadMinutes == nil {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("lead_minutes is required", "bad_request"))
		return
	}
	so, err := s.service.Schedule(r.Context(), r.PathValue("id"), *req.LeadMinutes)
	if err != nil {
		s.floorErr(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, so)
}

// POST /api/orders/{id}/make-now {prep_minutes}
func (s *Server) handleMakeNow(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PrepMinutes *int `json:"prep_minutes"`
	}
	if err := decodeBody(r, &req); err != nil {
		s.floorErr(w, r, err)
		return
	}
	if req.PrepMinutes == nil {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("prep_minutes is required", "bad_request"))
		return
	}
	so, err := s.service.MakeNow(r.Context(), r.PathValue("id"), *req.PrepMinutes)
	if err != nil {
		if errors.Is(err, orderApp.ErrNothingPrinted) {
			s.writeJSON(w, http.StatusBadGateway, errCodeMap(err.Error(), "nothing_printed"))
			return
		}
		s.floorErr(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, so)
}

// POST /api/orders/{id}/due {promised_at}
func (s *Server) handleChangeDue(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PromisedAt *time.Time `json:"promised_at"`
	}
	if err := decodeBody(r, &req); err != nil {
		s.floorErr(w, r, err)
		return
	}
	if req.PromisedAt == nil {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("promised_at is required", "bad_request"))
		return
	}
	so, err := s.service.ChangeDue(r.Context(), r.PathValue("id"), *req.PromisedAt)
	if err != nil {
		s.floorErr(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, so)
}

// POST /api/orders/{id}/print-now {kitchen, receipt}
func (s *Server) handlePrintNow(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Kitchen bool `json:"kitchen"`
		Receipt bool `json:"receipt"`
	}
	if err := decodeBody(r, &req); err != nil {
		s.floorErr(w, r, err)
		return
	}
	so, out, err := s.service.PrintNow(r.Context(), r.PathValue("id"), req.Kitchen, req.Receipt)
	switch {
	case err == nil:
	case errors.Is(err, orderApp.ErrNothingForKitchen):
		s.writeJSON(w, http.StatusConflict, errCodeMap(err.Error(), "nothing_for_kitchen"))
		return
	case errors.Is(err, orderApp.ErrFuturePrintFailed):
		s.writeJSON(w, http.StatusBadGateway, errCodeMap(err.Error(), "print_failed"))
		return
	default:
		s.floorErr(w, r, err)
		return
	}
	printed, failed, rerouted := []string{}, []string{}, []reroutedDTO{}
	for _, p := range out.Printed {
		printed = append(printed, p.Station)
		if p.ReroutedFrom != "" {
			rerouted = append(rerouted, reroutedDTO{Station: p.Station, PrintedOn: p.PrintedOn})
		}
	}
	for _, f := range out.Failed {
		failed = append(failed, f.Station)
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"order": so, "printed": printed, "failed": failed, "rerouted": rerouted})
}

// POST /api/orders/{id}/cancel
func (s *Server) handleCancelFuture(w http.ResponseWriter, r *http.Request) {
	so, err := s.service.CancelFuture(r.Context(), r.PathValue("id"))
	if err != nil {
		s.floorErr(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, so)
}
