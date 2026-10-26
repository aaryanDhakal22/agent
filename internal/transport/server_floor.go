package transport

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"quiccpos/agent/internal/application/appupdate"
	orderApp "quiccpos/agent/internal/application/order"
	"quiccpos/agent/internal/domain/device"
	"quiccpos/agent/internal/domain/order"
)

// Floor routes: ready, deliveries, drivers, batches. Gated by s.floor (paired
// tablet or web-UI session).

// floorErr maps service errors to HTTP statuses + stable codes.
func (s *Server) floorErr(w http.ResponseWriter, r *http.Request, err error) {
	var lo *order.LeftoversError
	var nr *order.ErrNotReady
	switch {
	case errors.As(err, &lo):
		s.writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "code": "batch_has_leftovers", "leftovers": lo.Orders})
	case errors.As(err, &nr):
		s.writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "code": "not_ready", "order_ids": nr.IDs})
	case errors.Is(err, orderApp.ErrNoDrivers):
		s.writeJSON(w, http.StatusConflict, errCodeMap(err.Error(), "no_drivers"))
	case errors.Is(err, orderApp.ErrPosPullOff):
		s.writeJSON(w, http.StatusConflict, errCodeMap(err.Error(), "pos_pull_off"))
	case errors.Is(err, orderApp.ErrNotFuture):
		s.writeJSON(w, http.StatusConflict, errCodeMap(err.Error(), "not_future"))
	case errors.Is(err, order.ErrPosNumberTaken):
		s.writeJSON(w, http.StatusConflict, errCodeMap(err.Error(), "pos_number_taken"))
	case errors.Is(err, orderApp.ErrMainOffline):
		s.writeJSON(w, http.StatusServiceUnavailable, errCodeMap("Can't reach the server — try again.", "main_offline"))
	case errors.Is(err, orderApp.ErrMainUnsupported):
		s.writeJSON(w, http.StatusNotImplemented, errCodeMap("The server doesn't support this yet.", "main_unsupported"))
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

// POST /api/drivers {name, pos_number?}
func (s *Server) handleCreateDriver(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name      string `json:"name"`
		PosNumber string `json:"pos_number"`
	}
	if err := decodeBody(r, &req); err != nil {
		s.floorErr(w, r, err)
		return
	}
	d, err := s.service.CreateDriver(r.Context(), req.Name, req.PosNumber)
	if err != nil {
		s.floorErr(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, d)
}

// PATCH /api/drivers/{id} {name?, pos_number?} ("" clears the number)
func (s *Server) handleRenameDriver(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name      *string `json:"name"`
		PosNumber *string `json:"pos_number"`
	}
	if err := decodeBody(r, &req); err != nil {
		s.floorErr(w, r, err)
		return
	}
	if err := s.service.UpdateDriver(r.Context(), r.PathValue("id"), req.Name, req.PosNumber); err != nil {
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
	s.dropTabletToken(d.ID)
	w.WriteHeader(http.StatusNoContent)
}

// POST /api/batch/close {force} — 409 batch_has_leftovers lists unfinished
// online orders unless force is true (they are then auto-finished). POS
// checks never block (always cancelled or finished); future orders still
// waiting for their time move into the new batch.
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

// POST /api/drivers/{id}/undo-return — 409 invalid_state when it isn't the
// latest return, is older than 10 min, or the driver moved on since.
func (s *Server) handleUndoReturn(w http.ResponseWriter, r *http.Request) {
	d, err := s.service.UndoReturn(r.Context(), r.PathValue("id"))
	if err != nil {
		s.floorErr(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, d)
}

// batchParam reads the {id} path value ("current" or 0 = the open batch).
func batchParam(r *http.Request) (int64, error) {
	v := r.PathValue("id")
	if v == "current" || v == "open" {
		return 0, nil
	}
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil || id < 0 {
		return 0, fmt.Errorf("%w: batch id must be a number or 'current'", orderApp.ErrBadRequest)
	}
	return id, nil
}

// GET /api/batches/{id}/driver-reports
func (s *Server) handleDriverReports(w http.ResponseWriter, r *http.Request) {
	id, err := batchParam(r)
	if err != nil {
		s.floorErr(w, r, err)
		return
	}
	reps, err := s.service.DriverReports(r.Context(), id)
	if err != nil {
		s.floorErr(w, r, err)
		return
	}
	if reps == nil {
		reps = []order.DriverReport{}
	}
	s.writeJSON(w, http.StatusOK, reps)
}

// POST /api/batches/{id}/driver-reports/print → {printed:[...], failed:[...]};
// 502 nothing_printed when no slip printed at all.
func (s *Server) handlePrintDriverReports(w http.ResponseWriter, r *http.Request) {
	id, err := batchParam(r)
	if err != nil {
		s.floorErr(w, r, err)
		return
	}
	res, err := s.service.PrintDriverReports(r.Context(), id)
	if err != nil {
		s.floorErr(w, r, err)
		return
	}
	printed, failed := []orderApp.ReportPrint{}, []orderApp.ReportPrint{}
	for _, p := range res {
		if p.Err != "" {
			failed = append(failed, p)
		} else {
			printed = append(printed, p)
		}
	}
	if len(res) > 0 && len(printed) == 0 {
		s.writeJSON(w, http.StatusBadGateway, map[string]any{"error": "no driver report printed", "code": "nothing_printed", "failed": failed})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"printed": printed, "failed": failed})
}

// PUT /api/settings/floor {green_till_minutes, tip_tax_percent} (session).
func (s *Server) handleSetFloorPrefs(w http.ResponseWriter, r *http.Request) {
	var req struct {
		GreenTillMinutes int64   `json:"green_till_minutes"`
		TipTaxPercent    float64 `json:"tip_tax_percent"`
	}
	if err := decodeBody(r, &req); err != nil {
		s.floorErr(w, r, err)
		return
	}
	p := order.FloorPrefs{GreenTillMinutes: req.GreenTillMinutes, TipTaxBps: int64(math.Round(req.TipTaxPercent * 100))}
	if err := s.service.SetFloorPrefs(r.Context(), p); err != nil {
		s.floorErr(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"green_till_minutes": p.GreenTillMinutes, "tip_tax_percent": float64(p.TipTaxBps) / 100})
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

// GET /api/app/latest — the cached Quibase release, or 204.
func (s *Server) handleAppLatest(w http.ResponseWriter, r *http.Request) {
	var cur *appupdate.Release
	if s.apps != nil {
		cur = s.apps.Current()
	}
	if cur == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.writeJSON(w, http.StatusOK, cur)
}

// GET /api/app/apk — the cached APK (Range supported).
func (s *Server) handleAppAPK(w http.ResponseWriter, r *http.Request) {
	var cur *appupdate.Release
	if s.apps != nil {
		cur = s.apps.Current()
	}
	if cur == nil {
		s.writeJSON(w, http.StatusNotFound, errCodeMap("no app release cached", "not_found"))
		return
	}
	f, err := os.Open(s.apps.Path(*cur))
	if err != nil {
		s.writeJSON(w, http.StatusNotFound, errCodeMap("cached APK missing", "not_found"))
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/vnd.android.package-archive")
	w.Header().Set("X-Sha256", cur.SHA256)
	http.ServeContent(w, r, filepath.Base(f.Name()), cur.CreatedAt, f)
}
