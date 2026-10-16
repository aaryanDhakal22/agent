// Package statesync reports the agent's whole floor state to main (RPC report
// `agent.state`) so a replacement Pi can pick up mid-shift: the open batch,
// the kitchen counter, the settings and the drivers with their queue + open
// shifts. Sent shortly after every floor change, on every (re)connect and at
// least every 5 minutes; identical states inside that window aren't re-sent.
package statesync

import (
	"bytes"
	"context"
	"encoding/json"
	"sync/atomic"
	"time"

	"quiccpos/agent/internal/domain/order"

	"github.com/rs/zerolog"
)

// MsgAgentState is the report type main stores.
const MsgAgentState = "agent.state"

// Reporter sends one RPC report to main (rpc.Registry.Report).
type Reporter func(ctx context.Context, msgType string, payload any) error

type Syncer struct {
	src        func(ctx context.Context) (order.AgentState, error)
	report     Reporter
	configured func() bool
	logger     zerolog.Logger
	wake       chan struct{}
	force      atomic.Bool

	last     []byte
	lastSent time.Time
}

// Debounce collects a burst of floor changes into one report.
var Debounce = time.Second

func New(src func(ctx context.Context) (order.AgentState, error), report Reporter, configured func() bool, logger zerolog.Logger) *Syncer {
	return &Syncer{src: src, report: report, configured: configured, wake: make(chan struct{}, 1), logger: logger.With().Str("module", "statesync").Logger()}
}

// Wake schedules a report (non-blocking).
func (s *Syncer) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Reset forgets the last sent state so the next report goes out even if
// nothing changed (a new connection, possibly to a fresh main).
func (s *Syncer) Reset() {
	s.force.Store(true)
	s.Wake()
}

func (s *Syncer) Run(ctx context.Context) {
	t := time.NewTicker(60 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
			select {
			case <-ctx.Done():
				return
			case <-time.After(Debounce):
			}
		case <-t.C:
		}
		if err := s.Send(ctx); err != nil {
			s.logger.Debug().Err(err).Msg("agent.state not sent; retrying on the next tick")
		}
	}
}

// Send builds and reports the state now (unless unchanged and recent).
func (s *Syncer) Send(ctx context.Context) error {
	if s.configured != nil && !s.configured() {
		return nil
	}
	st, err := s.src(ctx)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if !s.force.Swap(false) && bytes.Equal(raw, s.last) && time.Since(s.lastSent) < 5*time.Minute {
		return nil
	}
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := s.report(rctx, MsgAgentState, map[string]any{"at": time.Now().UTC(), "state": json.RawMessage(raw)}); err != nil {
		s.force.Store(true)
		return err
	}
	s.last, s.lastSent = raw, time.Now()
	return nil
}
