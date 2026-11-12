package transport

import (
	"context"
	"errors"
	"net/http"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/infra/database/repositories"
)

// Floor sync dead letters (contract/agent.md §2.4e): the Pi page lists the
// parked events and retries them; the parking thresholds are Pi settings.

// FloorSyncRepo is the parked-events storage (repositories.OrderRepository).
type FloorSyncRepo interface {
	ParkedEvents(ctx context.Context, limit int) (order.ParkedView, error)
	RetryParked(ctx context.Context, ids []string, by string, at time.Time) (int, error)
	SyncParkSettings(ctx context.Context) (order.SyncPark, error)
	SetSyncParkSettings(ctx context.Context, refusals, hours int) error
}

// SetFloorSync wires the parked-events routes (wake = the floor sender's).
func (s *Server) SetFloorSync(repo FloorSyncRepo, wake func()) {
	s.floorSync = repo
	s.floorSyncWake = wake
}

func (s *Server) syncReady(w http.ResponseWriter) bool {
	if s.floorSync == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, errCodeMap("floor sync isn't wired", "not_available"))
		return false
	}
	return true
}

// GET /api/sync/parked -> {parked: [DeadLetter], count, held, backlog, settings}
func (s *Server) handleSyncParked(w http.ResponseWriter, r *http.Request) {
	if !s.syncReady(w) {
		return
	}
	v, err := s.floorSync.ParkedEvents(r.Context(), 200)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	s.writeJSON(w, http.StatusOK, v)
}

type retryDTO struct {
	EventIDs []string `json:"event_ids"`
}

// POST /api/sync/parked/retry {event_ids?: [...]} (none = all) and
// POST /api/sync/parked/{id}/retry -> {retried: n, view}
func (s *Server) handleSyncRetry(w http.ResponseWriter, r *http.Request) {
	if !s.syncReady(w) {
		return
	}
	var ids []string
	if id := r.PathValue("id"); id != "" {
		ids = []string{id}
	} else if r.ContentLength != 0 {
		var body retryDTO
		if err := decodeBody(r, &body); err != nil || len(body.EventIDs) > 500 {
			s.writeJSON(w, http.StatusBadRequest, errCodeMap("event_ids: a list of up to 500 event ids (none = all)", "bad_request"))
			return
		}
		ids = body.EventIDs
	}
	n, err := s.floorSync.RetryParked(r.Context(), ids, "web", time.Now())
	if errors.Is(err, repositories.ErrNotParked) {
		s.writeJSON(w, http.StatusNotFound, errCodeMap(err.Error(), "not_parked"))
		return
	}
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	if s.floorSyncWake != nil {
		s.floorSyncWake()
	}
	v, _ := s.floorSync.ParkedEvents(r.Context(), 200)
	s.writeJSON(w, http.StatusOK, map[string]any{"retried": n, "view": v})
}

type syncParkDTO struct {
	Refusals *int `json:"refusals"`
	Hours    *int `json:"hours"`
}

func (s *Server) readSyncPark(r *http.Request) any {
	if s.floorSync == nil {
		return nil
	}
	p, err := s.floorSync.SyncParkSettings(r.Context())
	if err != nil {
		return nil
	}
	return map[string]int{"refusals": p.Refusals, "hours": p.Hours}
}

// GET /api/settings/sync -> {refusals, hours}
func (s *Server) handleGetSyncPark(w http.ResponseWriter, r *http.Request) {
	if !s.syncReady(w) {
		return
	}
	s.writeJSON(w, http.StatusOK, s.readSyncPark(r))
}

// PUT /api/settings/sync {refusals? 1-1000, hours? 1-168} (missing keys keep
// their value) -> {refusals, hours}
func (s *Server) handleSetSyncPark(w http.ResponseWriter, r *http.Request) {
	if !s.syncReady(w) {
		return
	}
	var body syncParkDTO
	if err := decodeBody(r, &body); err != nil {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("a JSON object {refusals?, hours?} is required", "bad_request"))
		return
	}
	cur, err := s.floorSync.SyncParkSettings(r.Context())
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	if body.Refusals != nil {
		cur.Refusals = *body.Refusals
	}
	if body.Hours != nil {
		cur.Hours = *body.Hours
	}
	if cur.Refusals < 1 || cur.Refusals > 1000 || cur.Hours < 1 || cur.Hours > 168 {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("refusals is 1-1000, hours 1-168", "bad_request"))
		return
	}
	if err := s.floorSync.SetSyncParkSettings(r.Context(), cur.Refusals, cur.Hours); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]int{"refusals": cur.Refusals, "hours": cur.Hours})
}
