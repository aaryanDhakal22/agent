package transport

import (
	"encoding/json"
	"fmt"
	"net/http"

	orderApp "quiccpos/agent/internal/application/order"
)

// Website / channel orders on the floor (2.5.0, weborder stage 3): ADD TIP,
// CANCEL ORDER and Quibase's online-ordering controls (proxied to main).

// POST /api/orders/{id}/tip {tip_cents}
func (s *Server) handleSetTip(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap(err.Error(), "bad_request"))
		return
	}
	var req struct {
		TipCents *int64 `json:"tip_cents"`
	}
	if err := decodeBody(r, &req); err != nil {
		s.floorErr(w, r, err)
		return
	}
	if req.TipCents == nil {
		s.floorErr(w, r, fmt.Errorf("%w: tip_cents is required", orderApp.ErrBadRequest))
		return
	}
	so, err := s.service.SetTip(r.Context(), id.String(), *req.TipCents)
	if err != nil {
		s.floorErr(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, so)
}

// POST /api/orders/{id}/cancel-order {reason, note?}
func (s *Server) handleCancelOrder(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap(err.Error(), "bad_request"))
		return
	}
	var req struct {
		Reason string `json:"reason"`
		Note   string `json:"note"`
	}
	if err := decodeBody(r, &req); err != nil {
		s.floorErr(w, r, err)
		return
	}
	so, err := s.service.CancelOrder(r.Context(), id.String(), req.Reason, req.Note)
	if err != nil {
		s.floorErr(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, so)
}

// writeOnline answers an online-ordering proxy call with main's reply.
func (s *Server) writeOnline(w http.ResponseWriter, r *http.Request, reply json.RawMessage, err error) {
	if err != nil {
		s.floorErr(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, reply)
}

// GET /api/online
func (s *Server) handleOnlineStatus(w http.ResponseWriter, r *http.Request) {
	reply, err := s.service.OnlineStatus(r.Context())
	s.writeOnline(w, r, reply, err)
}

// POST /api/online/pause {preset, note}
func (s *Server) handleOnlinePause(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Preset string `json:"preset"`
		Note   string `json:"note"`
	}
	if err := decodeBody(r, &req); err != nil {
		s.floorErr(w, r, err)
		return
	}
	reply, err := s.service.PauseOnline(r.Context(), req.Preset, req.Note)
	s.writeOnline(w, r, reply, err)
}

// DELETE /api/online/pause
func (s *Server) handleOnlineResume(w http.ResponseWriter, r *http.Request) {
	reply, err := s.service.ResumeOnline(r.Context())
	s.writeOnline(w, r, reply, err)
}

// GET /api/online/menu
func (s *Server) handleMenuStock(w http.ResponseWriter, r *http.Request) {
	reply, err := s.service.MenuStock(r.Context())
	s.writeOnline(w, r, reply, err)
}

// POST /api/online/stock {kind, id, in_stock, hold}
func (s *Server) handleSetStock(w http.ResponseWriter, r *http.Request) {
	var req orderApp.StockChange
	if err := decodeBody(r, &req); err != nil {
		s.floorErr(w, r, err)
		return
	}
	reply, err := s.service.SetStock(r.Context(), req)
	s.writeOnline(w, r, reply, err)
}
