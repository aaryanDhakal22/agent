package transport

import (
	"net/http"
	"os"
	"strings"

	"quiccpos/agent/internal/application/mapfile"
	orderApp "quiccpos/agent/internal/application/order"
	"quiccpos/agent/internal/domain/order"
)

// Driver pools, Send Out, AUTO, the geo proxies, delivery settings and the
// store map file (2.0.9). Floor routes: paired tablet or web-UI session; the
// map file is LAN-open like /static/.

// SetMap wires the store map downloader (GET /api/map.pmtiles).
func (s *Server) SetMap(m *mapfile.Downloader) { s.mapFile = m }

// POST /api/deliveries/pool {order_ids, driver_id|null} -> []StoredOrder.
func (s *Server) handlePool(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OrderIDs []string `json:"order_ids"`
		DriverID *string  `json:"driver_id"`
	}
	if err := decodeBody(r, &req); err != nil {
		s.floorErr(w, r, err)
		return
	}
	driverID := ""
	if req.DriverID != nil {
		driverID = *req.DriverID
	}
	out, err := s.service.Pool(r.Context(), req.OrderIDs, driverID)
	if err != nil {
		s.floorErr(w, r, err)
		return
	}
	if out == nil {
		out = []order.StoredOrder{}
	}
	s.writeJSON(w, http.StatusOK, out)
}

// POST /api/drivers/{id}/send-out {mark_ready, include_order_ids?, plan_id?}
// -> the sent orders (now on_delivery).
func (s *Server) handleSendOut(w http.ResponseWriter, r *http.Request) {
	var req orderApp.SendOutRequest
	if err := decodeBody(r, &req); err != nil {
		s.floorErr(w, r, err)
		return
	}
	out, err := s.service.SendOut(r.Context(), r.PathValue("id"), req)
	if err != nil {
		s.floorErr(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, out)
}

// POST /api/deliveries/auto-plan {} -> the suggested runs (nothing changes).
func (s *Server) handleAutoPlan(w http.ResponseWriter, r *http.Request) {
	plan, err := s.service.PlanDeliveries(r.Context())
	if err != nil {
		s.floorErr(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, plan)
}

// POST /api/deliveries/auto-plan/{id}/confirm {order_ids?} -> {applied,
// skipped}. order_ids limits the confirm to those suggestions.
func (s *Server) handleConfirmPlan(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OrderIDs []string `json:"order_ids"`
	}
	if err := decodeBody(r, &req); err != nil {
		s.floorErr(w, r, err)
		return
	}
	res, err := s.service.ConfirmPlan(r.Context(), r.PathValue("id"), req.OrderIDs)
	if err != nil {
		s.floorErr(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, res)
}

// GET /api/drivers/{id}/pool/trip?order_ids=a,b,c -> the stops in driving
// order with main's route (approximate when main can't be reached).
func (s *Server) handlePoolTrip(w http.ResponseWriter, r *http.Request) {
	var ids []string
	for _, part := range strings.Split(r.URL.Query().Get("order_ids"), ",") {
		if p := strings.TrimSpace(part); p != "" {
			ids = append(ids, p)
		}
	}
	trip, err := s.service.PoolTrip(r.Context(), r.PathValue("id"), ids)
	if err != nil {
		s.floorErr(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, trip)
}

// POST /api/orders/{id}/pin {lat, lng} -> 202 {} (main answers with order.geo).
func (s *Server) handlePinOrder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Lat *float64 `json:"lat"`
		Lng *float64 `json:"lng"`
	}
	if err := decodeBody(r, &req); err != nil {
		s.floorErr(w, r, err)
		return
	}
	if req.Lat == nil || req.Lng == nil {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("lat and lng are required", "bad_request"))
		return
	}
	if err := s.service.Pin(r.Context(), r.PathValue("id"), *req.Lat, *req.Lng); err != nil {
		s.floorErr(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusAccepted, map[string]any{})
}

// PUT /api/settings/deliveries — any subset of {soon_ready_minutes,
// delivery_target_minutes, nearby_rule, nearby_detour_minutes,
// nearby_distance_mi, delivery_radius_m} -> 200 the full settings | 400 |
// 503 main_offline / 501 main_unsupported (a radius main couldn't take:
// nothing changed).
func (s *Server) handleSetDeliveryPrefs(w http.ResponseWriter, r *http.Request) {
	var req orderApp.DeliveryPatch
	if err := decodeBody(r, &req); err != nil {
		s.floorErr(w, r, err)
		return
	}
	out, err := s.service.UpdateDeliverySettings(r.Context(), req)
	if err != nil {
		s.floorErr(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, out)
}

// GET /api/settings/deliveries -> the full delivery settings.
func (s *Server) handleGetDeliveryPrefs(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, s.service.DeliverySettings(r.Context()))
}

// GET /api/deliveries/next-check[?driver_id=] -> is an unassigned delivery
// near the next (or that) driver's run?
func (s *Server) handleNextCheck(w http.ResponseWriter, r *http.Request) {
	out, err := s.service.NextCheck(r.Context(), strings.TrimSpace(r.URL.Query().Get("driver_id")))
	if err != nil {
		s.floorErr(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, out)
}

// GET /api/map.pmtiles — the store's offline map (Range supported), 404
// until one is downloaded.
func (s *Server) handleMapFile(w http.ResponseWriter, r *http.Request) {
	var info *mapfile.Info
	if s.mapFile != nil {
		info, _ = s.mapFile.Current()
	}
	if info == nil {
		s.writeJSON(w, http.StatusNotFound, errCodeMap("no store map on this Pi yet", "not_found"))
		return
	}
	f, err := os.Open(s.mapFile.Path())
	if err != nil {
		s.writeJSON(w, http.StatusNotFound, errCodeMap("store map file missing", "not_found"))
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "max-age=86400")
	w.Header().Set("ETag", `"`+info.SHA256+`"`)
	w.Header().Set("Access-Control-Expose-Headers", "X-Trace-Id, Content-Range, Content-Length, ETag, Accept-Ranges")
	http.ServeContent(w, r, mapfile.FileName, st.ModTime(), f)
}
