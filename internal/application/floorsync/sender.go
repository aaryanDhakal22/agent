// Package floorsync pushes the agent's floor events (accept / ready / driver /
// delivered / roster changes) to main over the duplex channel's up-leg.
//
// Every floor change is written to the floor_events table in the same SQLite
// transaction as the change itself; this sender drains that outbox oldest
// first, in batches, as one `floor.events` RPC envelope per POST. main applies
// each event idempotently (event_id) and forward-only, and replies with a
// per-event result; any definitive result marks the event synced. Transport
// failures back off exponentially and retry — the floor keeps working through
// an internet drop and main catches up afterwards.
package floorsync

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/observability"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// MsgFloorEvents is the RPC envelope type (agent → main report with a reply).
const MsgFloorEvents = "floor.events"

const (
	batchSize    = 50
	idleInterval = 30 * time.Second
	minBackoff   = 5 * time.Second
	maxBackoff   = 5 * time.Minute
	// An older main answers 202 (unknown type): wait long before retrying.
	unsupportedBackoff = 5 * time.Minute
)

// Poster POSTs one RPC envelope to main, returning the HTTP status + body.
type Poster func(ctx context.Context, body []byte) (int, []byte, error)

// Result is main's verdict for one event.
type Result struct {
	EventID string `json:"event_id"`
	Result  string `json:"result"` // applied | duplicate | stale | not_found | invalid
}

type envelope struct {
	Type    string          `json:"type"`
	ID      string          `json:"id"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Sender drains the floor_events outbox to main.
type Sender struct {
	repo    order.FloorRepository
	post    Poster
	wake    chan struct{}
	logger  zerolog.Logger
	meters  observability.Meters
	tracer  trace.Tracer
	now     func() time.Time
	backoff time.Duration
}

func New(repo order.FloorRepository, post Poster, logger zerolog.Logger, meters observability.Meters) *Sender {
	return &Sender{
		repo:   repo,
		post:   post,
		wake:   make(chan struct{}, 1),
		logger: logger.With().Str("module", "floorsync").Logger(),
		meters: meters,
		tracer: otel.Tracer("quiccpos/agent/floorsync"),
		now:    time.Now,
	}
}

// Wake asks for an immediate flush (non-blocking; coalesces).
func (s *Sender) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Run flushes on every Wake and on a timer until ctx is done.
func (s *Sender) Run(ctx context.Context) {
	s.logger.Info().Msg("Floor sync started")
	for {
		wait := idleInterval
		if err := s.Flush(ctx); err != nil {
			wait = s.nextBackoff(err)
			s.logger.Warn().Err(err).Dur("retry_in", wait).Msg("Floor sync failed; will retry")
		} else {
			s.backoff = 0
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.wake:
			timer.Stop()
			// A wake during backoff still waits out the backoff — hammering a
			// down main on every tap helps nobody.
			if s.backoff > 0 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(s.backoff):
				}
			}
		case <-timer.C:
		}
	}
}

type unsupportedError struct{}

func (unsupportedError) Error() string { return "main does not support floor.events yet (202)" }

func (s *Sender) nextBackoff(err error) time.Duration {
	if _, ok := err.(unsupportedError); ok {
		s.backoff = unsupportedBackoff
		return s.backoff
	}
	switch {
	case s.backoff == 0:
		s.backoff = minBackoff
	case s.backoff < maxBackoff:
		s.backoff *= 2
		if s.backoff > maxBackoff {
			s.backoff = maxBackoff
		}
	}
	return s.backoff
}

// Flush sends every unsynced event (in batches) and returns the first error.
func (s *Sender) Flush(ctx context.Context) error {
	defer s.recordBacklog(ctx)
	for {
		events, err := s.repo.ListUnsyncedEvents(ctx, batchSize)
		if err != nil {
			return fmt.Errorf("list unsynced: %w", err)
		}
		if len(events) == 0 {
			return nil
		}
		if err := s.send(ctx, events); err != nil {
			return err
		}
		if len(events) < batchSize {
			return nil
		}
	}
}

func (s *Sender) send(ctx context.Context, events []order.FloorEvent) (err error) {
	ctx, span := s.tracer.Start(ctx, "floorsync.send", trace.WithAttributes(attribute.Int("events", len(events))))
	defer func() {
		if err != nil {
			span.RecordError(err)
		}
		span.End()
	}()

	payloads := make([]json.RawMessage, len(events))
	for i, e := range events {
		payloads[i] = e.Payload
	}
	inner, _ := json.Marshal(map[string]any{"events": payloads})
	body, _ := json.Marshal(envelope{Type: MsgFloorEvents, ID: uuid.NewString(), Payload: inner})

	status, resp, err := s.post(ctx, body)
	if err == nil && (status == http.StatusBadRequest || status == http.StatusRequestEntityTooLarge) {
		// main rejected the batch as a whole: isolate the bad event(s) so one
		// poison event can't hold up every later change forever.
		if len(events) > 1 {
			for _, e := range events {
				if err := s.send(ctx, []order.FloorEvent{e}); err != nil {
					return err
				}
			}
			return nil
		}
		s.logger.Error().Ctx(ctx).Str("event_id", events[0].ID).Str("type", events[0].Type).
			Int("status", status).Msg("main rejected a floor event outright — dropping it")
		s.count(ctx, "rejected", 1)
		return s.repo.MarkEventSynced(ctx, events[0].ID, s.now())
	}
	if err == nil && status != http.StatusOK && status != http.StatusAccepted {
		err = fmt.Errorf("main answered %d", status)
	}
	if err != nil {
		for _, e := range events {
			_ = s.repo.MarkEventFailed(ctx, e.ID, err.Error())
		}
		s.count(ctx, "error", len(events))
		return err
	}
	if status == http.StatusAccepted {
		s.count(ctx, "unsupported", len(events))
		return unsupportedError{}
	}

	var reply envelope
	var results struct {
		Results []Result `json:"results"`
	}
	if err := json.Unmarshal(resp, &reply); err != nil {
		return fmt.Errorf("decode reply: %w", err)
	}
	if err := json.Unmarshal(reply.Payload, &results); err != nil {
		return fmt.Errorf("decode results: %w", err)
	}
	now := s.now()
	known := make(map[string]bool, len(events))
	for _, e := range events {
		known[e.ID] = true
	}
	synced := 0
	for _, r := range results.Results {
		if !known[r.EventID] {
			continue
		}
		if err := s.repo.MarkEventSynced(ctx, r.EventID, now); err != nil {
			return fmt.Errorf("mark synced: %w", err)
		}
		synced++
		res := "ok"
		if r.Result == "not_found" || r.Result == "invalid" {
			res = "rejected"
			s.logger.Warn().Ctx(ctx).Str("event_id", r.EventID).Str("result", r.Result).Msg("main rejected a floor event (dropped)")
		}
		s.count(ctx, res, 1)
	}
	if synced < len(events) {
		return fmt.Errorf("main acknowledged %d of %d events", synced, len(events))
	}
	return nil
}

func (s *Sender) count(ctx context.Context, result string, n int) {
	s.meters.FloorEventsSent.Add(ctx, int64(n), metric.WithAttributes(attribute.String("result", result)))
}

func (s *Sender) recordBacklog(ctx context.Context) {
	if n, err := s.repo.CountUnsyncedEvents(ctx); err == nil {
		s.meters.FloorSyncBacklog.Record(ctx, n)
	}
}
