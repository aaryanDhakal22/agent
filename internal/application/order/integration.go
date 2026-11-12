package order

import (
	"context"
	"encoding/json"
	"fmt"
	"quiccpos/agent/internal/application/devlog"
	"sync"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/safego"

	"github.com/google/uuid"
)

// The store hub (2.1.0; since H3 the callout box: SoftTouch is gone). It
// reads what it must act on (order.ready, callout.test / render,
// settings.changed) from the integration feed and reports its status.
// Settings (feature toggles, kitchen-ticket switches, default times,
// finishing, callout, look) live in SQLite with an in-memory copy for the
// print path; the hub's status is in memory.

// integrationState is the service's 2.1.0 runtime state.
type integrationState struct {
	settingsMu sync.Mutex // serializes settings writes
	hubMu      sync.Mutex
	hub        order.IntegrationStatus
	hubSaved   order.IntegrationStatus // last persisted (see SetIntegrationStatus)
	hubLink    bool                    // connected, as last published
	feedMu     sync.Mutex
	feedCh     chan struct{} // closed + replaced on every feed write
}

// SetStoreIDs gives the service the tenant + location this Pi is linked to
// (from store_config) for the orders it makes itself.
func (s *Service) SetStoreIDs(fn func() (tenantID, locationID string)) { s.storeIDs.Store(&fn) }

func (s *Service) currentStoreIDs() (string, string) {
	if fn := s.storeIDs.Load(); fn != nil && *fn != nil {
		t, l := (*fn)()
		if _, err := uuid.Parse(t); err == nil {
			if _, err := uuid.Parse(l); err == nil {
				return t, l
			}
		}
	}
	return uuid.Nil.String(), uuid.Nil.String()
}

// WaitBackground waits for background work (prints, reports; tests).
func (s *Service) WaitBackground() { s.bg.Wait() }

func (s *Service) background(ctx context.Context, fn func(context.Context)) {
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		defer safego.Recover("order.background")
		fn(context.WithoutCancel(ctx))
	}()
}

// --- the feed -------------------------------------------------------------------

// feedNotify wakes long-polling feed readers (after any change that may have
// written a feed event).
func (s *Service) feedNotify() {
	s.integ.feedMu.Lock()
	if s.integ.feedCh != nil {
		close(s.integ.feedCh)
	}
	s.integ.feedCh = make(chan struct{})
	s.integ.feedMu.Unlock()
}

func (s *Service) feedWait() <-chan struct{} {
	s.integ.feedMu.Lock()
	defer s.integ.feedMu.Unlock()
	if s.integ.feedCh == nil {
		s.integ.feedCh = make(chan struct{})
	}
	return s.integ.feedCh
}

// FeedPage is GET /api/integration/events.
type FeedPage struct {
	Events []order.FeedEvent `json:"events"`
	Next   int64             `json:"next"`
}

// feedLimit caps one feed page.
const feedLimit = 200

// Feed returns the feed events after `after`, waiting up to wait for one when
// there are none yet. A cursor past the newest event (a fresh Pi after an SD
// swap) starts again from the oldest kept.
func (s *Service) Feed(ctx context.Context, after int64, wait time.Duration) (*FeedPage, error) {
	if after < 0 {
		after = 0
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		ch := s.feedWait()
		events, maxSeq, err := s.repo.ListFeed(ctx, after, feedLimit)
		if err != nil {
			if ctx.Err() != nil { // the reader went away
				return &FeedPage{Events: []order.FeedEvent{}, Next: after}, nil
			}
			return nil, err
		}
		if after > maxSeq {
			after = 0
			continue
		}
		if len(events) > 0 || wait <= 0 {
			page := &FeedPage{Events: events, Next: after}
			if len(events) > 0 {
				page.Next = events[len(events)-1].Seq
			}
			return page, nil
		}
		select {
		case <-ch:
		case <-time.After(time.Second): // a write from a path that didn't notify
		case <-deadline.C:
			wait = 0
		case <-ctx.Done():
			return &FeedPage{Events: []order.FeedEvent{}, Next: after}, nil
		}
	}
}

// --- settings ---------------------------------------------------------------------

// LoadIntegrationSettings refreshes the in-memory copy from the DB (startup,
// after a restore).
func (s *Service) LoadIntegrationSettings(ctx context.Context) {
	set, err := s.repo.IntegrationSettings(ctx)
	if err != nil {
		s.logger.Warn().Ctx(ctx).Err(err).Msg("read integration settings failed; using defaults")
		set = order.DefaultIntegrationSettings()
	}
	s.integSet.Store(&set)
	if st, err := s.repo.IntegrationStatus(ctx); err == nil {
		s.integ.hubMu.Lock()
		s.integ.hub, s.integ.hubSaved = st, st
		s.integ.hubMu.Unlock()
	}
}

// IntegrationSettings are the features, ticket switches and default times in
// force (no DB read).
func (s *Service) IntegrationSettings() order.IntegrationSettings {
	if p := s.integSet.Load(); p != nil {
		return *p
	}
	return order.DefaultIntegrationSettings()
}

// UpdateIntegrationSettings applies change to the current settings, checks
// the times (1-240), stores them (writing the feed's settings.changed) and
// tells the tablets (SSE settings.updated + integration.status).
func (s *Service) UpdateIntegrationSettings(ctx context.Context, change func(*order.IntegrationSettings) error) (order.IntegrationSettings, error) {
	s.integ.settingsMu.Lock()
	defer s.integ.settingsMu.Unlock()
	set := s.IntegrationSettings()
	if err := change(&set); err != nil {
		return set, fmt.Errorf("%w: %w", ErrBadRequest, err)
	}
	if err := set.Times.Validate(); err != nil {
		return set, fmt.Errorf("%w: %w", ErrBadRequest, err)
	}
	if err := set.Finishing.Validate(); err != nil {
		return set, fmt.Errorf("%w: %w", ErrBadRequest, err)
	}
	if err := set.Callout.Validate(); err != nil {
		return set, fmt.Errorf("%w: %w", ErrBadRequest, err)
	}
	if err := set.Look.Validate(); err != nil {
		return set, fmt.Errorf("%w: %w", ErrBadRequest, err)
	}
	if err := s.repo.SetIntegrationSettings(ctx, set); err != nil {
		return set, err
	}
	s.integSet.Store(&set)
	s.feedNotify()
	if s.broker != nil {
		s.broker.Publish("settings.updated", set)
		s.broker.Publish("integration.status", s.IntegrationView(time.Now()))
	}
	s.logger.Info().Ctx(ctx).Interface("settings", set).Msg("Integration settings changed")
	return set, nil
}

// --- hub status --------------------------------------------------------------------

// IntegrationView is the hub's status as staff see it now.
func (s *Service) IntegrationView(now time.Time) order.IntegrationView {
	s.integ.hubMu.Lock()
	st := s.integ.hub
	s.integ.hubMu.Unlock()
	return st.View(now)
}

// hubPersistEvery bounds how often an unchanged status is written (SD wear).
const hubPersistEvery = 10 * time.Minute

// SetIntegrationStatus stores the hub's status PUT (in memory; persisted when
// it changed or every 10 min) and publishes SSE integration.status.
func (s *Service) SetIntegrationStatus(ctx context.Context, st order.IntegrationStatus) error {
	switch st.InsertMode {
	case "":
		st.InsertMode = order.InsertOff
	case order.InsertOff, order.InsertShadow, order.InsertLive:
	default:
		return fmt.Errorf("%w: insert_mode must be off, shadow or live", ErrBadRequest)
	}
	if st.Problems == nil {
		st.Problems = []order.Problem{}
	}
	for i, p := range st.Problems {
		switch p.Severity {
		case order.SeverityInfo, order.SeverityWarn, order.SeveritySerious:
		case "":
			st.Problems[i].Severity = order.SeverityWarn
		default:
			return fmt.Errorf("%w: problem severity must be info, warn or serious", ErrBadRequest)
		}
	}
	now := time.Now()
	st.SeenAt = now.UTC()
	s.integ.hubMu.Lock()
	prev := s.integ.hub
	s.integ.hub = st
	persist := !sameHubStatus(st, s.integ.hubSaved) || now.Sub(s.integ.hubSaved.SeenAt) > hubPersistEvery
	if persist {
		s.integ.hubSaved = st
	}
	wasLinked := s.integ.hubLink
	s.integ.hubLink = true
	s.integ.hubMu.Unlock()
	if !wasLinked {
		s.devEvent(devlog.Info, devlog.KindHub, "Store hub connected (insert mode %s)", st.InsertMode)
	}
	if persist {
		if err := s.repo.SetIntegrationStatus(ctx, st); err != nil {
			s.logger.Warn().Ctx(ctx).Err(err).Msg("persist integration status failed (kept in memory)")
		}
	}
	if s.broker != nil && (!sameHubStatus(st, prev) || now.Sub(prev.SeenAt) > order.IntegrationStaleAfter) {
		s.broker.Publish("integration.status", s.IntegrationView(now))
	}
	return nil
}

func sameHubStatus(a, b order.IntegrationStatus) bool {
	ja, _ := json.Marshal(order.IntegrationStatus{OK: a.OK, InsertMode: a.InsertMode, OperationDay: a.OperationDay, Problems: a.Problems, Version: a.Version, Callout: a.Callout})
	jb, _ := json.Marshal(order.IntegrationStatus{OK: b.OK, InsertMode: b.InsertMode, OperationDay: b.OperationDay, Problems: b.Problems, Version: b.Version, Callout: b.Callout})
	return string(ja) == string(jb)
}

// CheckIntegrationLink publishes integration.status once when the hub goes
// quiet (no PUT for 60 s): the tablets show "Hub link down". Run on the
// escalation ticker.
func (s *Service) CheckIntegrationLink(now time.Time) {
	v := s.IntegrationView(now)
	s.integ.hubMu.Lock()
	changed := s.integ.hubLink && !v.Connected
	if changed {
		s.integ.hubLink = false
	}
	s.integ.hubMu.Unlock()
	if changed {
		s.devEvent(devlog.Warn, devlog.KindHub, "POS link down: the store hub stopped reporting")
		s.logger.Warn().Msg("POS link down: the store hub stopped reporting")
		if s.broker != nil {
			s.broker.Publish("integration.status", v)
		}
	}
}
