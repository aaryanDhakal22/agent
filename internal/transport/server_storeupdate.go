package transport

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"quiccpos/agent/internal/application/storeupdate"
	"quiccpos/agent/internal/domain/device"
)

// Store update round (contract §2.10ad): GET /api/store-update,
// POST /api/store-update/ready, the compatibility floor (426
// app_update_required) and the in-flight write count the round's quiet
// check reads.

// SetStoreUpdate wires the round coordinator (nil = no rounds, no floor).
func (s *Server) SetStoreUpdate(c *storeupdate.Coordinator) { s.storeUpdate = c }

// WritesInFlight counts the write requests (not GET / HEAD / OPTIONS, not
// the round's own ready answers) being handled now.
func (s *Server) WritesInFlight() int64 { return s.writesInFlight.Load() }

// countWrites wraps the handler stack: the store round waits while a write
// (an accept that prints, a payment, a send-out…) is being handled.
func (s *Server) countWrites(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/api/store-update/ready" {
			next.ServeHTTP(w, r)
			return
		}
		s.writesInFlight.Add(1)
		defer s.writesInFlight.Add(-1)
		next.ServeHTTP(w, r)
	})
}

// StoreUpdateDevices lists the paired tablets and terminals for the round.
func (s *Server) StoreUpdateDevices(ctx context.Context) []storeupdate.Device {
	out := []storeupdate.Device{}
	for _, t := range s.Tablets(ctx) {
		out = append(out, storeupdate.Device{ID: t.DeviceID, Name: t.Name, Kind: storeupdate.KindTablet,
			Version: t.AppVersion, Connected: t.Connected})
	}
	for _, t := range s.Terminals(ctx) {
		out = append(out, storeupdate.Device{ID: t.DeviceID, Name: t.Name, Kind: storeupdate.KindTerminal,
			Version: t.AppVersion, Connected: t.Connected})
	}
	return out
}

// roundDevice is the asking device as the round sees it (nil = not a tablet
// or terminal: the web page, an integration key).
func (s *Server) roundDevice(r *http.Request) *storeupdate.Device {
	d, ok := r.Context().Value(deviceCtxKey{}).(*device.Device)
	if !ok || d == nil {
		return nil
	}
	kind := ""
	switch d.Kind {
	case device.KindTablet:
		kind = storeupdate.KindTablet
	case device.KindTerminal:
		kind = storeupdate.KindTerminal
	default:
		return nil
	}
	return &storeupdate.Device{ID: d.ID, Name: d.Name, Kind: kind, Version: s.appVersionOf(*d), Connected: true}
}

// GET /api/store-update — the round as this device sees it.
func (s *Server) handleStoreUpdate(w http.ResponseWriter, r *http.Request) {
	if s.storeUpdate == nil {
		s.writeJSON(w, http.StatusNotFound, errCodeMap("this Pi runs no store update rounds", "not_found"))
		return
	}
	s.writeJSON(w, http.StatusOK, s.storeUpdate.View(s.roundDevice(r)))
}

// POST /api/store-update/ready {id, ready, reason?} — a device's quiet answer.
func (s *Server) handleStoreUpdateReady(w http.ResponseWriter, r *http.Request) {
	if s.storeUpdate == nil {
		s.writeJSON(w, http.StatusNotFound, errCodeMap("this Pi runs no store update rounds", "not_found"))
		return
	}
	dev := s.roundDevice(r)
	if dev == nil {
		s.writeJSON(w, http.StatusForbidden, errCodeMap("only a paired tablet or terminal answers a store update", "device_required"))
		return
	}
	var body struct {
		ID     string `json:"id"`
		Ready  *bool  `json:"ready"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.ID) == "" || body.Ready == nil {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("send {id, ready, reason?}", "bad_request"))
		return
	}
	err := s.storeUpdate.Ready(r.Context(), dev.ID, body.ID, *body.Ready, body.Reason)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, storeupdate.ErrNotPreparing), errors.Is(err, storeupdate.ErrNotInRound):
		s.writeJSON(w, http.StatusConflict, errCodeMap("no store update is asking this device now", "not_preparing"))
	default:
		s.writeError(w, r, http.StatusInternalServerError, err)
	}
}

// appGateOpen: the routes a device below the floor may still use — what it
// needs to hear the round and update itself.
func appGateOpen(r *http.Request) bool {
	p := r.URL.Path
	switch {
	case p == "/api/events", strings.HasPrefix(p, "/api/store-update"):
		return true
	case strings.HasPrefix(p, "/api/app/"), strings.HasPrefix(p, "/api/terminal/app/"):
		return true
	case p == "/api/device/self", p == "/api/device/unpair":
		return true
	case strings.HasPrefix(p, "/api/diag"):
		return true
	case r.Method == http.MethodGet && (p == "/api/pairing/window" || p == "/api/pin-guard"):
		return true
	}
	return false
}

// appGate refuses a tablet / terminal whose app is below this agent's
// compatibility floor (426 app_update_required) on every route but the
// update ones. false = answered.
func (s *Server) appGate(w http.ResponseWriter, r *http.Request, d *device.Device) bool {
	if s.storeUpdate == nil || d == nil || appGateOpen(r) {
		return true
	}
	kind := ""
	switch d.Kind {
	case device.KindTablet:
		kind = storeupdate.KindTablet
	case device.KindTerminal:
		kind = storeupdate.KindTerminal
	default:
		return true
	}
	version := r.Header.Get(quibaseVersionHeader)
	if kind == storeupdate.KindTerminal {
		version = r.Header.Get(terminalVersionHeader)
	}
	// Only what this request says: a just-updated app's first calls can go
	// without the header (unknown = never locked), never judged by the old
	// version the Pi last saw.
	locked, min := s.storeUpdate.Locked(kind, version)
	if !locked {
		return true
	}
	s.writeJSON(w, http.StatusUpgradeRequired, map[string]any{
		"error":       "this app is older than the Pi accepts: it updates itself now",
		"code":        "app_update_required",
		"min_version": min,
		"release":     s.storeUpdate.CachedFor(kind),
	})
	return false
}
