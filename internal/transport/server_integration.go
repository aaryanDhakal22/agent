package transport

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	orderApp "quiccpos/agent/internal/application/order"
	"quiccpos/agent/internal/domain/order"
)

// 2.1.0 store hub surface (the callout box since H3). Settings (features /
// kitchen tickets / default times / finishing / look / callout) are device
// routes (tablet or web session; an integration key may read them).
// /api/integration/* is for the hub's integration key only.

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

// PUT /api/settings/features {future_orders?, paid_wave?}
func (s *Server) handleSetFeatures(w http.ResponseWriter, r *http.Request) {
	s.putSettings(w, r, func(set *order.IntegrationSettings) any { return &set.Features })
}

// GET /api/settings/tickets
func (s *Server) handleGetTickets(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, s.service.IntegrationSettings().Tickets)
}

// PUT /api/settings/tickets {online_cash_pickup?, online_cash_delivery?, online_card?, future_kitchen?, future_receipt?}
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

// GET /api/settings/finishing
func (s *Server) handleGetFinishing(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, s.service.IntegrationSettings().Finishing)
}

// PUT /api/settings/finishing {online_pickup_minutes?}
func (s *Server) handleSetFinishing(w http.ResponseWriter, r *http.Request) {
	s.putSettings(w, r, func(set *order.IntegrationSettings) any { return &set.Finishing })
}

// GET /api/settings/look (2.4.3)
func (s *Server) handleGetLook(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, s.service.IntegrationSettings().Look)
}

// PUT /api/settings/look {accent?, tickets?: {delivery?, pickup?, counter?}}
func (s *Server) handleSetLook(w http.ResponseWriter, r *http.Request) {
	s.putSettings(w, r, func(set *order.IntegrationSettings) any { return &set.Look })
}

// GET /api/settings/callout
func (s *Server) handleGetCalloutSettings(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, s.service.IntegrationSettings().Callout)
}

// PUT /api/settings/callout {enabled?, voice?, volume?, music_dip?, restore_seconds?, chime?, repeat?}
func (s *Server) handleSetCalloutSettings(w http.ResponseWriter, r *http.Request) {
	s.putSettings(w, r, func(set *order.IntegrationSettings) any { return &calloutPut{&set.Callout} })
}

// calloutPut is a callout PUT: missing keys keep their value, but a
// voice_lines that is there replaces the whole map (2.3.8: a voice whose
// line was cleared is left out, so merging would keep it).
type calloutPut struct{ *order.CalloutSettings }

func (c *calloutPut) UnmarshalJSON(b []byte) error {
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(b, &keys); err != nil {
		return err
	}
	if _, ok := keys["voice_lines"]; ok {
		c.VoiceLines = nil
	}
	if err := json.Unmarshal(b, c.CalloutSettings); err != nil {
		return err
	}
	if c.VoiceLines == nil {
		c.VoiceLines = map[string]string{}
	}
	return nil
}

// GET /api/callout -> {settings, hub_connected, hub}
func (s *Server) handleGetCallout(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, s.service.Callout(time.Now()))
}

// POST /api/callout/test {number, voice?, kind?} -> 202 {id, hub_connected}
func (s *Server) handleCalloutTest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Number int    `json:"number"`
		Voice  string `json:"voice"`
		Kind   string `json:"kind"`
	}
	if err := decodeBody(r, &req); err != nil {
		s.floorErr(w, r, err)
		return
	}
	res, err := s.service.CalloutTest(r.Context(), req.Kind, req.Number, req.Voice)
	if err != nil {
		s.floorErr(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusAccepted, res)
}

// previewErr answers the callout preview's own errors (else floorErr).
func (s *Server) previewErr(w http.ResponseWriter, r *http.Request, err error) {
	var un *orderApp.PreviewUnavailable
	switch {
	case errors.As(err, &un):
		s.writeJSON(w, http.StatusServiceUnavailable, errCodeMap(un.Message, un.Code))
	case errors.Is(err, orderApp.ErrPreviewNotFound):
		s.writeJSON(w, http.StatusNotFound, errCodeMap(err.Error(), "not_found"))
	case errors.Is(err, orderApp.ErrPreviewTimeout):
		s.writeJSON(w, http.StatusGatewayTimeout, errCodeMap("The store hub didn't send the voice in time - try again.", "timeout"))
	case errors.Is(err, orderApp.ErrPreviewTooLarge):
		s.writeJSON(w, http.StatusRequestEntityTooLarge, errCodeMap(err.Error(), "too_large"))
	case errors.Is(err, orderApp.ErrPreviewNotWAV):
		s.writeJSON(w, http.StatusBadRequest, errCodeMap(err.Error(), "not_wav"))
	default:
		s.floorErr(w, r, err)
	}
}

// POST /api/callout/preview {number, voice?} -> 202 {id, status} | 503
func (s *Server) handleCalloutPreview(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Number int    `json:"number"`
		Voice  string `json:"voice"`
	}
	if err := decodeBody(r, &req); err != nil {
		s.floorErr(w, r, err)
		return
	}
	res, err := s.service.CalloutPreview(r.Context(), req.Number, req.Voice)
	if err != nil {
		s.previewErr(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusAccepted, res)
}

// GET /api/callout/preview/{id}?wait=<sec> -> 200 audio/wav | 202 pending | 404 | 503 | 504
func (s *Server) handleCalloutPreviewAudio(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	wait := 0
	if v := r.URL.Query().Get("wait"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > orderApp.MaxPreviewWait {
			s.writeJSON(w, http.StatusBadRequest, errCodeMap("wait must be 0-10 seconds", "bad_request"))
			return
		}
		wait = n
	}
	wav, err := s.service.CalloutPreviewAudio(r.Context(), id, time.Duration(wait)*time.Second)
	if err != nil {
		s.previewErr(w, r, err)
		return
	}
	if wav == nil {
		s.writeJSON(w, http.StatusAccepted, map[string]string{"id": id, "status": "pending"})
		return
	}
	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Content-Length", strconv.Itoa(len(wav)))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(wav)
}

// PUT /api/integration/callout-audio/{id}: body audio/wav, or JSON
// {error, code?} when the hub could not render it -> 204
func (s *Server) handleIntegrationCalloutAudio(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		var req struct {
			Error string `json:"error"`
			Code  string `json:"code"`
		}
		if err := decodeBody(r, &req); err != nil {
			s.floorErr(w, r, err)
			return
		}
		if err := s.service.FailCalloutPreview(r.Context(), id, req.Code, req.Error); err != nil {
			s.previewErr(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, orderApp.MaxPreviewAudio))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			err = orderApp.ErrPreviewTooLarge
		} else {
			err = errors.Join(orderApp.ErrBadRequest, err)
		}
		s.previewErr(w, r, err)
		return
	}
	if err := s.service.PutCalloutAudio(r.Context(), id, raw); err != nil {
		s.previewErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- /api/integration/* (integration key only) -------------------------------------

type integrationLocation struct {
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	Timezone string         `json:"timezone"`
	Config   map[string]any `json:"config"`
}

// GET /api/integration/config — what the hub (the callout box) needs to work
// with this Pi: the location, the callout settings and the agent's version.
func (s *Server) handleIntegrationConfig(w http.ResponseWriter, r *http.Request) {
	sc := s.storeContext()
	cfg := sc.Location.Config
	if cfg == nil {
		cfg = map[string]any{}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"location":      integrationLocation{ID: sc.Location.ID, Name: sc.Location.Name, Timezone: sc.Location.Timezone, Config: cfg},
		"callout":       s.service.IntegrationSettings().Callout,
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

// --- future-order actions (2.4.3) ------------------------------------------------------------
