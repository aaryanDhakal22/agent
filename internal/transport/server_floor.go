package transport

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	orderApp "quiccpos/agent/internal/application/order"
	"quiccpos/agent/internal/domain/device"
	"quiccpos/agent/internal/domain/order"
)

// Floor routes: ready, deliveries, drivers, batches. Gated by s.floor (paired
// tablet or web-UI session).

// floorErr maps service errors to HTTP statuses + stable codes.
func (s *Server) floorErr(w http.ResponseWriter, r *http.Request, err error) {
	var lo *order.LeftoversError
	switch {
	case errors.As(err, &lo):
		s.writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "code": "batch_has_leftovers", "leftovers": lo.Orders})
	case errors.Is(err, order.ErrNotFound):
		s.writeJSON(w, http.StatusNotFound, errCodeMap(err.Error(), "not_found"))
	case errors.Is(err, order.ErrDriverNotFound):
		s.writeJSON(w, http.StatusNotFound, errCodeMap(err.Error(), "driver_not_found"))
	case errors.Is(err, order.ErrDriverNotAvailable):
		s.writeJSON(w, http.StatusConflict, errCodeMap(err.Error(), "driver_not_available"))
	case errors.Is(err, order.ErrAlreadyAccepted):
		s.writeJSON(w, http.StatusConflict, errCodeMap(err.Error(), "already_accepted"))
	case errors.Is(err, order.ErrInvalidState):
		s.writeJSON(w, http.StatusConflict, errCodeMap(err.Error(), "invalid_state"))
	case errors.Is(err, order.ErrNoOrders), errors.Is(err, orderApp.ErrBadRequest):
		s.writeJSON(w, http.StatusBadRequest, errCodeMap(err.Error(), "bad_request"))
	default:
		s.writeError(w, r, http.StatusInternalServerError, err)
	}
}

func decodeBody(r *http.Request, v any) error {
	if r.ContentLength == 0 {
		return nil
	}
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		return errors.Join(orderApp.ErrBadRequest, err)
	}
	return nil
}

// GET /api/floor — open batch, kitchen counter, prep picks, sync backlog.
func (s *Server) handleFloor(w http.ResponseWriter, r *http.Request) {
	f, err := s.service.Floor(r.Context())
	if err != nil {
		s.floorErr(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, f)
}

// POST /api/orders/{id}/ready
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, err)
		return
	}
	so, err := s.service.Ready(r.Context(), id.String())
	if err != nil {
		s.floorErr(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, so)
}

// POST /api/deliveries/assign {driver_id, order_ids}
func (s *Server) handleAssign(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DriverID string   `json:"driver_id"`
		OrderIDs []string `json:"order_ids"`
	}
	if err := decodeBody(r, &req); err != nil {
		s.floorErr(w, r, err)
		return
	}
	out, err := s.service.AssignDriver(r.Context(), req.DriverID, req.OrderIDs)
	if err != nil {
		s.floorErr(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, out)
}

// POST /api/drivers/{id}/returned
func (s *Server) handleDriverReturned(w http.ResponseWriter, r *http.Request) {
	out, err := s.service.DriverReturned(r.Context(), r.PathValue("id"))
	if err != nil {
		s.floorErr(w, r, err)
		return
	}
	if out == nil {
		out = []order.StoredOrder{}
	}
	s.writeJSON(w, http.StatusOK, out)
}

// GET /api/drivers
func (s *Server) handleListDrivers(w http.ResponseWriter, r *http.Request) {
	ds, err := s.service.ListDrivers(r.Context())
	if err != nil {
		s.floorErr(w, r, err)
		return
	}
	if ds == nil {
		ds = []order.Driver{}
	}
	s.writeJSON(w, http.StatusOK, ds)
}

// POST /api/drivers {name}
func (s *Server) handleCreateDriver(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := decodeBody(r, &req); err != nil {
		s.floorErr(w, r, err)
		return
	}
	d, err := s.service.CreateDriver(r.Context(), req.Name)
	if err != nil {
		s.floorErr(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, d)
}

// PATCH /api/drivers/{id} {name}
func (s *Server) handleRenameDriver(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := decodeBody(r, &req); err != nil {
		s.floorErr(w, r, err)
		return
	}
	if err := s.service.RenameDriver(r.Context(), r.PathValue("id"), req.Name); err != nil {
		s.floorErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// DELETE /api/drivers/{id}
func (s *Server) handleRemoveDriver(w http.ResponseWriter, r *http.Request) {
	if err := s.service.RemoveDriver(r.Context(), r.PathValue("id")); err != nil {
		s.floorErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /api/drivers/reorder {ids}
func (s *Server) handleReorderDrivers(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []string `json:"ids"`
	}
	if err := decodeBody(r, &req); err != nil {
		s.floorErr(w, r, err)
		return
	}
	if err := s.service.ReorderDrivers(r.Context(), req.IDs); err != nil {
		s.floorErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /api/drivers/{id}/clock-in — off → in the queue (back), shift opened.
func (s *Server) handleClockIn(w http.ResponseWriter, r *http.Request) {
	d, err := s.service.ClockIn(r.Context(), r.PathValue("id"))
	if err != nil {
		s.floorErr(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, d)
}

// POST /api/drivers/{id}/clock-out — available → off, shift closed.
func (s *Server) handleClockOut(w http.ResponseWriter, r *http.Request) {
	d, err := s.service.ClockOut(r.Context(), r.PathValue("id"))
	if err != nil {
		s.floorErr(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, d)
}

// GET /api/drivers/shifts?days=7 — shift records (newest first).
func (s *Server) handleListShifts(w http.ResponseWriter, r *http.Request) {
	days := 7
	if v, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil && v > 0 && v <= 90 {
		days = v
	}
	sh, err := s.service.ListShifts(r.Context(), time.Now().AddDate(0, 0, -days))
	if err != nil {
		s.floorErr(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, sh)
}

// GET /api/settings/driver-reset — {time, last_run}.
func (s *Server) handleGetDriverReset(w http.ResponseWriter, r *http.Request) {
	rs, err := s.service.GetDriverReset(r.Context())
	if err != nil {
		s.floorErr(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, rs)
}

// PUT /api/settings/driver-reset {time:"HH:MM"} (session).
func (s *Server) handleSetDriverReset(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Time string `json:"time"`
	}
	if err := decodeBody(r, &req); err != nil {
		s.floorErr(w, r, err)
		return
	}
	if err := s.service.SetDriverResetTime(r.Context(), req.Time); err != nil {
		s.floorErr(w, r, err)
		return
	}
	s.handleGetDriverReset(w, r)
}

// GET /api/batches — open + kept closed batches, newest first.
func (s *Server) handleListBatches(w http.ResponseWriter, r *http.Request) {
	bs, err := s.service.ListBatches(r.Context())
	if err != nil {
		s.floorErr(w, r, err)
		return
	}
	if bs == nil {
		bs = []order.Batch{}
	}
	s.writeJSON(w, http.StatusOK, bs)
}

// POST /api/device/unpair — a paired tablet revokes its own key ("Unpair" in
// the app). Session callers get 400 (use Settings → revoke instead).
func (s *Server) handleUnpairSelf(w http.ResponseWriter, r *http.Request) {
	d, ok := r.Context().Value(deviceCtxKey{}).(*device.Device)
	if !ok {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("only a paired tablet can unpair itself", "not_a_device"))
		return
	}
	if err := s.devices.Revoke(r.Context(), d.ID, time.Now()); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /api/batch/close {force} — 409 batch_has_leftovers lists unfinished
// orders unless force is true (they are then auto-finished).
func (s *Server) handleCloseBatch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Force bool `json:"force"`
	}
	if err := decodeBody(r, &req); err != nil {
		s.floorErr(w, r, err)
		return
	}
	closed, finished, err := s.service.CloseBatch(r.Context(), req.Force)
	if err != nil {
		s.floorErr(w, r, err)
		return
	}
	if finished == nil {
		finished = []order.StoredOrder{}
	}
	opened, err := s.service.Floor(r.Context())
	if err != nil {
		s.floorErr(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"closed": closed, "opened": opened.Batch, "auto_finished": finished})
}

// parseBatch reads ?batch=current|all|<id> (default current).
func parseBatch(r *http.Request) (all bool, id int64, ok bool) {
	switch v := r.URL.Query().Get("batch"); v {
	case "", "current":
		return false, 0, true
	case "all":
		return true, 0, true
	default:
		n, err := strconv.ParseInt(v, 10, 64)
		return false, n, err == nil && n > 0
	}
}
