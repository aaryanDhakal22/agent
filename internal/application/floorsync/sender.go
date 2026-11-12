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

// Repo is the outbox the sender drains (repositories.OrderRepository).
type Repo interface {
	ListUnsyncedEvents(ctx context.Context, limit int) ([]order.FloorEvent, error)
	MarkEventSynced(ctx context.Context, id string, at time.Time) error
	MarkEventFailed(ctx context.Context, id, errMsg string) error
	CountUnsyncedEvents(ctx context.Context) (int64, error)
	// Dead letters (contract 2.4e).
	RefuseEvent(ctx context.Context, id, result string, at time.Time) (order.Refusal, error)
	ParkEvent(ctx context.Context, ref order.Refusal, result string, at time.Time) (bool, error)
	SyncParkSettings(ctx context.Context) (order.SyncPark, error)
	SetSyncParkAlertedAt(ctx context.Context, at time.Time) error
	CountParkedEvents(ctx context.Context) (int64, error)
}

// Pager sends the owner a Pushover (notify.Notifier.Send).
type Pager interface {
	Send(ctx context.Context, message, sound string) error
}

// AlertEvery: at most one parked-events Pushover an hour.
const AlertEvery = time.Hour

// Sender drains the floor_events outbox to main.
type Sender struct {
	repo    Repo
	post    Poster
	wake    chan struct{}
	logger  zerolog.Logger
	meters  observability.Meters
	tracer  trace.Tracer
	now     func() time.Time
	backoff time.Duration
	pager   Pager
	// unalerted: events parked since the last Pushover (one an hour).
	unalerted int
	onParked  func(n int)
}

// SetPager wires the problem pager for parked events (nil = log only).
func (s *Sender) SetPager(p Pager) { s.pager = p }

// OnParked is called after events were parked (the web page's SSE nudge).
func (s *Sender) OnParked(fn func(n int)) { s.onParked = fn }

func New(repo Repo, post Poster, logger zerolog.Logger, meters observability.Meters) *Sender {
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
		s.alertParked(ctx)
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
	typeOf := make(map[string]string, len(events))
	for _, e := range events {
		known[e.ID] = true
		typeOf[e.ID] = e.Type
	}
	synced, kept, parked := 0, 0, 0
	var park *order.SyncPark
	for _, r := range results.Results {
		if !known[r.EventID] {
			continue
		}
		// Money and records that exist only on the Pi until main takes them:
		// a refusal keeps the event queued (retried with the backoff; main
		// looks at a refused event again) rather than lose it - until it was
		// refused too often or too long: then it is parked (contract 2.4e)
		// so the rest of the queue moves on.
		if KeepRefused(typeOf[r.EventID], r.Result) {
			s.count(ctx, "rejected", 1)
			ref, err := s.repo.RefuseEvent(ctx, r.EventID, r.Result, now)
			if err != nil {
				return fmt.Errorf("count refusal: %w", err)
			}
			if park == nil {
				p, err := s.repo.SyncParkSettings(ctx)
				if err != nil {
					return fmt.Errorf("park settings: %w", err)
				}
				park = &p
			}
			if park.Due(ref.Refusals, ref.FirstAt, now) {
				ok, err := s.repo.ParkEvent(ctx, ref, r.Result, now)
				if err != nil {
					return fmt.Errorf("park event: %w", err)
				}
				if ok {
					parked++
					s.count(ctx, "parked", 1)
					s.logger.Error().Ctx(ctx).Str("event_id", r.EventID).Str("type", ref.Type).Str("result", r.Result).
						Int64("refusals", ref.Refusals).Msg("main refused a floor event for good: parked (dead letter), the queue moves on")
				}
				continue
			}
			s.logger.Warn().Ctx(ctx).Str("event_id", r.EventID).Str("type", typeOf[r.EventID]).Str("result", r.Result).
				Int64("refusals", ref.Refusals).Msg("main refused a floor event the Pi must not lose (kept, retried)")
			kept++
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
	if parked > 0 {
		s.unalerted += parked
		if s.onParked != nil {
			s.onParked(parked)
		}
	}
	if kept > 0 {
		return fmt.Errorf("main refused %d event(s) the Pi keeps; retried later", kept)
	}
	if synced+parked < len(events) {
		return fmt.Errorf("main acknowledged %d of %d events", synced, len(events))
	}
	return nil
}

// KeepRefused reports whether an event main refused stays queued instead of
// being marked synced: a counter order (order.placed, a main older than
// terminal 0d answers invalid), a payment (order.payment: a main before
// terminal 1d refuses one on an online order; not_found while its counter
// order is still queued), a counter customer (customer.upsert, a main
// before 1d), and stage 2's drawers and Z report (drawer.*, z.report: they
// exist only on the Pi). Every other refusal is final (marked synced,
// logged). A kept event is parked after too many refusals (contract 2.4e).
func KeepRefused(typ, result string) bool {
	switch typ {
	case "order.placed", "customer.upsert", "z.report", "drawer.opened", "drawer.closed":
		return result == "invalid"
	case "order.payment", "drawer.movement":
		return result == "invalid" || result == "not_found"
	}
	return false
}

// alertParked pages the owner about parked events: at most one Pushover an
// hour (the last one is kept in settings, so a restart doesn't page again),
// with the count parked since the last one and the total.
func (s *Sender) alertParked(ctx context.Context) {
	if s.unalerted == 0 {
		return
	}
	p, err := s.repo.SyncParkSettings(ctx)
	if err != nil {
		return
	}
	now := s.now()
	if p.AlertedAt != nil && now.Sub(*p.AlertedAt) < AlertEvery {
		return
	}
	total, _ := s.repo.CountParkedEvents(ctx)
	n := s.unalerted
	s.unalerted = 0
	if err := s.repo.SetSyncParkAlertedAt(ctx, now); err != nil {
		s.logger.Warn().Ctx(ctx).Err(err).Msg("parked alert: save time failed")
	}
	if s.pager == nil {
		return
	}
	msg := fmt.Sprintf("Floor sync: main refused %d event(s) for good; parked so the rest keeps syncing (%d parked in all). Retry them on the Pi page (Overview).", n, total)
	if err := s.pager.Send(ctx, msg, "siren"); err != nil {
		s.logger.Warn().Ctx(ctx).Err(err).Msg("parked alert: Pushover failed (non-fatal)")
	}
}

func (s *Sender) count(ctx context.Context, result string, n int) {
	s.meters.FloorEventsSent.Add(ctx, int64(n), metric.WithAttributes(attribute.String("result", result)))
}

func (s *Sender) recordBacklog(ctx context.Context) {
	if n, err := s.repo.CountUnsyncedEvents(ctx); err == nil {
		s.meters.FloorSyncBacklog.Record(ctx, n)
	}
}
