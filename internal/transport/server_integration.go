package transport

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	orderApp "quiccpos/agent/internal/application/order"
	"quiccpos/agent/internal/domain/order"
)

// 2.1.0 store hub surface. Settings (features / kitchen tickets / default
// times) are device routes (tablet or web session; an integration key may
// read them). /api/integration/* is for the hub's integration key only.

// readJSON reads a JSON object body (400 on anything else).
func readJSON(r *http.Request) ([]byte, error) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, errors.Join(orderApp.ErrBadRequest, err)
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return nil, errors.Join(orderApp.ErrBadRequest, errors.New("body must be a JSON object"))
	}
	return raw, nil
}

// putSettings applies a partial JSON object onto one part of the settings
// (missing keys keep their value) and answers that part.
func (s *Server) putSettings(w http.ResponseWriter, r *http.Request, part func(*order.IntegrationSettings) any) {
	raw, err := readJSON(r)
	if err != nil {
		s.floorErr(w, r, err)
		return
	}
	set, err := s.service.UpdateIntegrationSettings(r.Context(), func(set *order.IntegrationSettings) error {
		return json.Unmarshal(raw, part(set))
	})
	if err != nil {
		s.floorErr(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, part(&set))
}

// GET /api/settings/features
func (s *Server) handleGetFeatures(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, s.service.IntegrationSettings().Features)
}

// PUT /api/settings/features {pos_pull?, order_insert?, ...}
func (s *Server) handleSetFeatures(w http.ResponseWriter, r *http.Request) {
	s.putSettings(w, r, func(set *order.IntegrationSettings) any { return &set.Features })
}

// GET /api/settings/tickets
func (s *Server) handleGetTickets(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, s.service.IntegrationSettings().Tickets)
}

// PUT /api/settings/tickets {pos_delivery?, ...}
func (s *Server) handleSetTickets(w http.ResponseWriter, r *http.Request) {
	s.putSettings(w, r, func(set *order.IntegrationSettings) any { return &set.Tickets })
}

// GET /api/settings/times
func (s *Server) handleGetTimes(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, s.service.IntegrationSettings().Times)
}

// PUT /api/settings/times {default_pickup_minutes?, default_delivery_minutes?} (each 1-240)
func (s *Server) handleSetTimes(w http.ResponseWriter, r *http.Request) {
	s.putSettings(w, r, func(set *order.IntegrationSettings) any { return &set.Times })
}

// --- future orders + ambiguous addresses (device routes) ---------------------------

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

// POST /api/orders/{id}/geo/choose {index} -> 202 {} (main answers with order.geo).
func (s *Server) handleGeoChoose(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Index *int `json:"index"`
	}
	if err := decodeBody(r, &req); err != nil {
		s.floorErr(w, r, err)
		return
	}
	if req.Index == nil {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("index is required", "bad_request"))
		return
	}
	if err := s.service.ChooseCandidate(r.Context(), r.PathValue("id"), *req.Index); err != nil {
		s.floorErr(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusAccepted, map[string]any{})
}

// POST /api/orders/{id}/geo/search {query} -> 202 {} (main answers with
// order.geo: check with places near the store, or not_found with a note).
func (s *Server) handleGeoSearch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query string `json:"query"`
	}
	if err := decodeBody(r, &req); err != nil {
		s.floorErr(w, r, err)
		return
	}
	if err := s.service.SearchAddress(r.Context(), r.PathValue("id"), req.Query); err != nil {
		s.floorErr(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusAccepted, map[string]any{})
}

// --- /api/integration/* (integration key only) -------------------------------------

type integrationLocation struct {
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	Timezone string         `json:"timezone"`
	Config   map[string]any `json:"config"`
}

type integrationDriver struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	PosNumber *string `json:"pos_number"`
	Status    string  `json:"status"`
}

type integrationPrinter struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	IP        string `json:"ip"`
	IsReceipt bool   `json:"is_receipt"`
}

// GET /api/integration/config — what the hub needs to work with this Pi.
func (s *Server) handleIntegrationConfig(w http.ResponseWriter, r *http.Request) {
	sc := s.storeContext()
	cfg := sc.Location.Config
	if cfg == nil {
		cfg = map[string]any{}
	}
	set := s.service.IntegrationSettings()
	ds, err := s.service.ListDrivers(r.Context())
	if err != nil {
		s.floorErr(w, r, err)
		return
	}
	drivers := make([]integrationDriver, 0, len(ds))
	for _, d := range ds {
		var pos *string
		if d.PosNumber != "" {
			p := d.PosNumber
			pos = &p
		}
		drivers = append(drivers, integrationDriver{ID: d.ID, Name: d.Name, PosNumber: pos, Status: d.Status})
	}
	printers := []integrationPrinter{}
	if s.diag.Dispatcher != nil {
		for _, p := range s.diag.Dispatcher.RosterPrinters() {
			printers = append(printers, integrationPrinter{ID: p.ID, Name: p.Name, IP: p.IP, IsReceipt: p.IsReceipt})
		}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"location":      integrationLocation{ID: sc.Location.ID, Name: sc.Location.Name, Timezone: sc.Location.Timezone, Config: cfg},
		"features":      set.Features,
		"tickets":       set.Tickets,
		"times":         set.Times,
		"drivers":       drivers,
		"printers":      printers,
		"agent_version": s.version,
	})
}

// PUT /api/integration/status {ok, insert_mode, operation_day, problems, version} -> 204.
func (s *Server) handleIntegrationStatus(w http.ResponseWriter, r *http.Request) {
	var st order.IntegrationStatus
	if err := decodeBody(r, &st); err != nil {
		s.floorErr(w, r, err)
		return
	}
	if err := s.service.SetIntegrationStatus(r.Context(), st); err != nil {
		s.floorErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// PUT /api/integration/orders/{ext_ref} — upsert a POS check -> {order_id,
// created, changed} | 409 pos_pull_off.
func (s *Server) handleIntegrationUpsert(w http.ResponseWriter, r *http.Request) {
	var in order.ExternalOrder
	if err := decodeBody(r, &in); err != nil {
		s.floorErr(w, r, err)
		return
	}
	res, err := s.service.UpsertExternal(r.Context(), r.PathValue("ext_ref"), in)
	if err != nil {
		s.floorErr(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, res)
}

// POST /api/integration/orders/{ext_ref}/void -> the cancelled StoredOrder.
func (s *Server) handleIntegrationVoid(w http.ResponseWriter, r *http.Request) {
	so, err := s.service.VoidExternal(r.Context(), r.PathValue("ext_ref"))
	if err != nil {
		s.floorErr(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, so)
}

// PATCH /api/integration/online-orders/{order_id} {paid?, pos_check_number?,
// flag?: {text, severity} | null} -> StoredOrder.
func (s *Server) handleIntegrationPatchOnline(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Paid           *bool           `json:"paid"`
		PosCheckNumber *int64          `json:"pos_check_number"`
		Flag           json.RawMessage `json:"flag"`
	}
	if err := decodeBody(r, &req); err != nil {
		s.floorErr(w, r, err)
		return
	}
	p := order.OnlinePatch{Paid: req.Paid, PosCheckNumber: req.PosCheckNumber}
	if len(req.Flag) > 0 {
		p.SetFlag = true
		if string(bytes.TrimSpace(req.Flag)) != "null" {
			var f order.Flag
			if err := json.Unmarshal(req.Flag, &f); err != nil {
				s.writeJSON(w, http.StatusBadRequest, errCodeMap("flag must be {text, severity} or null", "bad_request"))
				return
			}
			p.Flag = &f
		}
	}
	so, err := s.service.PatchOnline(r.Context(), r.PathValue("order_id"), p)
	if err != nil {
		s.floorErr(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, so)
}

// maxFeedWait caps the events long-poll (below the proxy idle timeouts).
const maxFeedWait = 25

// GET /api/integration/events?after=<seq>&wait=<sec <= 25> -> {events, next}.
func (s *Server) handleIntegrationEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	after, err := strconv.ParseInt(q.Get("after"), 10, 64)
	if q.Get("after") != "" && (err != nil || after < 0) {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("after must be a seq (>= 0)", "bad_request"))
		return
	}
	wait := 0
	if v := q.Get("wait"); v != "" {
		if wait, err = strconv.Atoi(v); err != nil || wait < 0 {
			s.writeJSON(w, http.StatusBadRequest, errCodeMap("wait must be seconds (0-25)", "bad_request"))
			return
		}
	}
	wait = min(wait, maxFeedWait)
	page, err := s.service.Feed(r.Context(), after, time.Duration(wait)*time.Second)
	if err != nil {
		s.floorErr(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, page)
}
