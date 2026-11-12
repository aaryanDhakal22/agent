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
	// MarkEventsSynced marks a flush's events in one transaction (S-7).
	MarkEventsSynced(ctx context.Context, ids []string, at time.Time) error
	// OldestUnsynced is the oldest unsynced, unparked event's time per type
	// (CM-9).
	OldestUnsynced(ctx context.Context) (map[string]time.Time, error)
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
	cm        *observability.CounterMeters
	// retryAt: a kept (refused) event's next try (in memory, per event,
	// exponential with its refusals). A refusal never backs off the whole
	// queue: only that event waits, and its subject waits behind it.
	retryAt map[string]time.Time
}

// refusedRetry is a kept event's wait before main sees it again.
func refusedRetry(refusals int64) time.Duration {
	d := minBackoff
	for i := int64(1); i < refusals && d < maxBackoff; i++ {
		d *= 2
	}
	return min(d, maxBackoff)
}

// SetCounterMeters swaps the CM-9 instruments (tests).
func (s *Sender) SetCounterMeters(cm *observability.CounterMeters) { s.cm = cm }

// SetPager wires the problem pager for parked events (nil = log only).
func (s *Sender) SetPager(p Pager) { s.pager = p }

// OnParked is called after events were parked (the web page's SSE nudge).
func (s *Sender) OnParked(fn func(n int)) { s.onParked = fn }

func New(repo Repo, post Poster, logger zerolog.Logger, meters observability.Meters) *Sender {
	return &Sender{
		repo:    repo,
		post:    post,
		wake:    make(chan struct{}, 1),
		logger:  logger.With().Str("module", "floorsync").Logger(),
		meters:  meters,
		tracer:  otel.Tracer("quiccpos/agent/floorsync"),
		now:     time.Now,
		cm:      observability.Counter(),
		retryAt: map[string]time.Time{},
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
// A kept (refused) event waits for its own retry time (refusedRetry) and is
// skipped until then; everything else goes on (no queue-wide backoff).
func (s *Sender) Flush(ctx context.Context) error {
	defer s.recordBacklog(ctx)
	for round := 0; round < 200; round++ {
		listed, err := s.repo.ListUnsyncedEvents(ctx, listWindow)
		if err != nil {
			return fmt.Errorf("list unsynced: %w", err)
		}
		now := s.now()
		var events []order.FloorEvent
		for _, e := range listed {
			// e.Refusals > 0: still refused in the DB (a Retry of a parked
			// event resets it, so it goes at once).
			if at, ok := s.retryAt[e.ID]; ok && e.Refusals > 0 && now.Before(at) {
				continue
			}
			events = append(events, e)
			if len(events) == batchSize {
				break
			}
		}
		if len(events) == 0 {
			return nil
		}
		if err := s.send(ctx, events); err != nil {
			return err
		}
	}
	return nil
}

// listWindow: how many unsynced events a flush looks at to find batchSize
// it may send (kept ones waiting for their retry are skipped).
const listWindow = 1000

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
			// One at a time; an event behind a refused one of its subject
			// waits (the next flush holds it, contract 2.10s §3).
			var first error
			held := map[string]bool{}
			for _, e := range events {
				if e.Subject != "" && held[e.Subject] {
					continue
				}
				if err := s.send(ctx, []order.FloorEvent{e}); err != nil {
					if first == nil {
						first = err
					}
					if e.Subject != "" {
						held[e.Subject] = true
					}
				}
			}
			return first
		}
		e := events[0]
		if KeepRefused(e.Type, "invalid") {
			// M-2: a money / record event is never dropped on a 400 / 413
			// (a Pi / main version drift during an update): it is a refusal
			// like an "invalid" answer - kept, retried, parked past the
			// thresholds (dead letter + page + Retry).
			s.logger.Error().Ctx(ctx).Str("event_id", e.ID).Str("type", e.Type).Int("status", status).
				Msg("main rejected a floor event outright: kept (refusal), it parks if main keeps refusing it")
			parked, err := s.refuse(ctx, e.ID, fmt.Sprintf("http_%d", status))
			if err != nil {
				return err
			}
			if parked {
				s.noteParked(1)
			}
			return nil // it retries on its own time (refusedRetry); the queue moves on
		}
		s.logger.Error().Ctx(ctx).Str("event_id", e.ID).Str("type", e.Type).
			Int("status", status).Msg("main rejected a floor event outright — dropping it")
		s.count(ctx, e.Type, "rejected", 1)
		return s.repo.MarkEventSynced(ctx, e.ID, s.now())
	}
	if err == nil && status != http.StatusOK && status != http.StatusAccepted {
		err = fmt.Errorf("main answered %d", status)
	}
	if err != nil {
		for _, e := range events {
			_ = s.repo.MarkEventFailed(ctx, e.ID, err.Error())
			s.count(ctx, e.Type, "error", 1)
		}
		return err
	}
	if status == http.StatusAccepted {
		for _, e := range events {
			s.count(ctx, e.Type, "unsupported", 1)
		}
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
	byID := make(map[string]Result, len(results.Results))
	for _, r := range results.Results {
		if known[r.EventID] {
			byID[r.EventID] = r
		}
	}
	synced, kept, parked, waiting := 0, 0, 0, 0
	var done []string
	refusedSubject := map[string]bool{}
	for _, e := range events {
		r, ok := byID[e.ID]
		if !ok {
			continue
		}
		// main applied this event's subject out of order behind a refused
		// one (e.g. order.status after a refused order.placed): it isn't
		// marked synced, the next flush holds it behind the refused event.
		if e.Subject != "" && refusedSubject[e.Subject] && r.Result != "applied" && r.Result != "duplicate" {
			waiting++
			continue
		}
		// Money and records that exist only on the Pi until main takes them:
		// a refusal keeps the event queued (retried with the backoff; main
		// looks at a refused event again) rather than lose it - until it was
		// refused too often or too long: then it is parked (contract 2.4e)
		// so the rest of the queue moves on. Later events of its subject
		// wait behind it from its first refusal (contract 2.10s §3).
		if KeepRefused(typeOf[r.EventID], r.Result) {
			p, err := s.refuse(ctx, r.EventID, r.Result)
			if err != nil {
				return err
			}
			if e.Subject != "" {
				refusedSubject[e.Subject] = true
			}
			if p {
				parked++
			} else {
				kept++
			}
			continue
		}
		done = append(done, r.EventID)
		synced++
		res := "ok"
		if r.Result == "not_found" || r.Result == "invalid" {
			res = "rejected"
			s.logger.Warn().Ctx(ctx).Str("event_id", r.EventID).Str("type", typeOf[r.EventID]).Str("result", r.Result).
				Msg("main rejected a floor event (dropped)")
		}
		s.count(ctx, typeOf[r.EventID], res, 1)
	}
	// One transaction for the flush's synced events (S-7: one fsync on the
	// SD card, not one per event).
	if len(done) > 0 {
		if err := s.repo.MarkEventsSynced(ctx, done, now); err != nil {
			return fmt.Errorf("mark synced: %w", err)
		}
		for _, id := range done {
			delete(s.retryAt, id)
		}
	}
	s.noteParked(parked)
	// Kept and waiting events retry on their own time (no queue-wide
	// backoff): the rest of the queue keeps flowing.
	if synced+parked+kept+waiting < len(events) {
		return fmt.Errorf("main acknowledged %d of %d events", synced, len(events))
	}
	return nil
}

// MoneyTypes are the events that exist only on the Pi until main takes them
// (contract 2.10s §3, HARDENING-PLAN M-2): the counter orders and their
// money, changes and voids, the customers, the drawers, the Z and the
// journal. A refusal of one (invalid / not_found, or a 400 / 413) never
// marks it synced: it is kept, retried and parked past the thresholds.
var MoneyTypes = map[string]bool{
	"order.placed": true, "order.payment": true, "order.changed": true, "order.void": true,
	"customer.upsert": true, "drawer.opened": true, "drawer.closed": true, "drawer.movement": true,
	"z.report": true, "journal.entries": true,
}

// KeepRefused reports whether an event main refused stays queued instead of
// being marked synced: a MoneyTypes event answered invalid or not_found.
// Every other refusal is final (marked synced, logged: floor bookkeeping an
// older main may not know). A kept event is parked after too many refusals
// (contract 2.4e).
func KeepRefused(typ, result string) bool {
	// conflict (main 2.5zd): the same payment id / client_key / Square ref
	// with another body - kept and parked, never marked synced.
	return MoneyTypes[typ] && (result == "invalid" || result == "not_found" || result == "conflict")
}

// ClassOf is an event type's class on the CM-9 metrics: money | floor.
func ClassOf(typ string) string {
	if MoneyTypes[typ] {
		return "money"
	}
	return "floor"
}

// refuse counts one refusal of a kept event and parks it past the
// thresholds (true = parked now).
func (s *Sender) refuse(ctx context.Context, id, result string) (bool, error) {
	ref, err := s.repo.RefuseEvent(ctx, id, result, s.now())
	if err != nil {
		return false, fmt.Errorf("count refusal: %w", err)
	}
	if s.retryAt == nil {
		s.retryAt = map[string]time.Time{}
	}
	s.retryAt[id] = s.now().Add(refusedRetry(ref.Refusals))
	s.count(ctx, ref.Type, "rejected", 1)
	p, err := s.repo.SyncParkSettings(ctx)
	if err != nil {
		return false, fmt.Errorf("park settings: %w", err)
	}
	if !p.Due(ref.Refusals, ref.FirstAt, s.now()) {
		s.logger.Warn().Ctx(ctx).Str("event_id", id).Str("type", ref.Type).Str("result", result).
			Int64("refusals", ref.Refusals).Msg("main refused a floor event the Pi must not lose (kept, retried)")
		return false, nil
	}
	ok, err := s.repo.ParkEvent(ctx, ref, result, s.now())
	if err != nil {
		return false, fmt.Errorf("park event: %w", err)
	}
	if ok {
		s.count(ctx, ref.Type, "parked", 1)
		s.logger.Error().Ctx(ctx).Str("event_id", id).Str("type", ref.Type).Str("result", result).
			Int64("refusals", ref.Refusals).Msg("main refused a floor event for good: parked (dead letter), the queue moves on")
	}
	return ok, nil
}

// noteParked queues parked events for the hourly page and the Pi page's nudge.
func (s *Sender) noteParked(n int) {
	if n == 0 {
		return
	}
	s.unalerted += n
	if s.onParked != nil {
		s.onParked(n)
	}
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

func (s *Sender) count(ctx context.Context, typ, result string, n int) {
	if s.meters.FloorEventsSent != nil {
		s.meters.FloorEventsSent.Add(ctx, int64(n), metric.WithAttributes(attribute.String("result", result)))
	}
	s.cm.Sync(ctx, ClassOf(typ), result, n)
}

// recordBacklog: the backlog, the parked count and (CM-9) the oldest
// unsynced event's age per class (0 = none waiting).
func (s *Sender) recordBacklog(ctx context.Context) {
	if n, err := s.repo.CountUnsyncedEvents(ctx); err == nil && s.meters.FloorSyncBacklog != nil {
		s.meters.FloorSyncBacklog.Record(ctx, n)
	}
	if s.cm == nil || s.cm.SyncParked == nil {
		return
	}
	if n, err := s.repo.CountParkedEvents(ctx); err == nil {
		s.cm.SyncParked.Record(ctx, n)
	}
	oldest, err := s.repo.OldestUnsynced(ctx)
	if err != nil {
		return
	}
	age := map[string]float64{"money": 0, "floor": 0}
	now := s.now()
	for typ, at := range oldest {
		c := ClassOf(typ)
		if a := now.Sub(at).Seconds(); a > age[c] {
			age[c] = a
		}
	}
	for c, a := range age {
		s.cm.SyncOldestAge.Record(ctx, a, metric.WithAttributes(attribute.String("class", c)))
	}
}
