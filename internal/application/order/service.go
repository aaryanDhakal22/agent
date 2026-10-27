package order

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"quiccpos/agent/internal/application/devlog"
	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
	"quiccpos/agent/internal/infra/notify"
	"quiccpos/agent/internal/infra/ssebroker"
	"quiccpos/agent/internal/observability"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// PrintFunc is the seam between the order service and "actually printing":
// the printer.Dispatcher's ExecuteMode (plan the order's tickets for a mode —
// accept, receipt-only reprint, kitchen-only resend — and print each with
// failover). It errors only when nothing printed (printer.ErrNoTickets when
// the mode had nothing to print); partial trouble is in the Outcome. Tests
// swap in a recording fake.
type PrintFunc = func(ctx context.Context, snap order.Snapshot, mode printer.Mode) (printer.Outcome, error)

const tracerName = "quiccpos/agent/order"

// Service orchestrates the order lifecycle inside the agent:
//
//	SSE arrival ─▶ OnArrival ─┬─ auto-accept on  ─▶ Accept (prints)
//	                          └─ auto-accept off ─▶ broadcast to mobile,
//	                                                mobile POSTs /accept
//	                                                ─▶ Accept (prints)
//
// Accept claims the order (arrived→accepted) first, then prints its tickets
// outside any DB transaction; if nothing printed at all the claim is reverted
// so the order can be re-accepted. Pushover notifications fire after a
// successful accept — a flaky Pushover never undoes a print.
type Service struct {
	repo     order.Repository
	settings order.SettingsRepository
	print    PrintFunc
	notifier *notify.Notifier
	broker   *ssebroker.Broker
	logger   zerolog.Logger
	// routeUnscopedWarned: the "no admin device" warning is logged once.
	routeUnscopedWarned atomic.Bool
	tracer              trace.Tracer
	meters              observability.Meters
	// onFloorEvent is called after every committed floor change so the sync
	// sender pushes it to main right away (set via OnFloorEvent).
	// Set by cmd/main after the SSE client may already be running: atomic.
	onFloorEvent atomic.Pointer[func()]
	storeTZ      atomic.Pointer[func() *time.Location] // unset → time.Local
	onStored     func(orderID string)
	// Driver-report printing (SetDocumentPrinting).
	printDoc    DocPrinter
	buildReport ReportBuilder
	// receiptPrefs is the in-memory copy of the receipt settings the
	// printing path reads (LoadReceiptPrefs / SetReceiptPrefs keep it).
	receiptPrefs atomic.Pointer[order.ReceiptPrefs]
	// geo: store pin / map / main geo RPC hooks (SetGeo).
	geo atomic.Pointer[GeoDeps]
	// 2.1.0 store hub: settings copy for the print path, the hub's status
	// and the feed wake-up (integration.go); the store ids for POS orders.
	integSet atomic.Pointer[order.IntegrationSettings]
	integ    integrationState
	storeIDs atomic.Pointer[func() (string, string)]
	bg       sync.WaitGroup
	// dev: the developer-mode event ring (SetDevLog; nil = off).
	dev atomic.Pointer[devlog.Ring]
	// onBatchClosed runs after a Close batch (OnBatchClosed).
	onBatchClosed atomic.Pointer[func(ctx context.Context)]
	// autoMu: one auto-assign at a time (autoassign.go).
	autoMu sync.Mutex
	// ledger: the POS driver ledger from the store hub (2.3.0, ledger.go).
	ledger ledgerState
	// preview: the callout PREVIEW "on this tablet" audio (2.3.2,
	// callout_preview.go), in memory only.
	preview previewState
}

func NewService(
	repo order.Repository,
	settings order.SettingsRepository,
	print PrintFunc,
	notifier *notify.Notifier,
	broker *ssebroker.Broker,
	logger zerolog.Logger,
	meters observability.Meters,
) *Service {
	return &Service{
		repo:     repo,
		settings: settings,
		print:    print,
		notifier: notifier,
		broker:   broker,
		logger:   logger.With().Str("module", "order-service").Logger(),
		tracer:   otel.Tracer(tracerName),
		meters:   meters,
	}
}

// OnFloorEvent registers the hook run after each committed floor change
// (cmd/main.go wires the floorsync sender's Wake).
func (s *Service) OnFloorEvent(fn func()) { s.onFloorEvent.Store(&fn) }

func (s *Service) floorChanged() {
	if fn := s.onFloorEvent.Load(); fn != nil && *fn != nil {
		(*fn)()
	}
	s.feedNotify() // the same transaction may have written a hub feed event
}

// OnArrival is called by the SSE listener when a new order lands from main/.
func (s *Service) OnArrival(ctx context.Context, snap order.Snapshot) error {
	id := snap.MustParseID()
	customerName := snap.Customer.DisplayName

	ctx, span := s.tracer.Start(ctx, "order.on_arrival",
		trace.WithAttributes(
			attribute.String("order.id", id.String()),
			attribute.String("order.source_event_id", snap.SourceEventID),
			attribute.String("order.fulfillment_type", snap.Fulfillment.Type),
			attribute.String("order.customer_name", customerName),
			attribute.Int("order.item_count", len(snap.Items)),
			attribute.Int64("order.total_cents", snap.Money.Total),
		),
	)
	defer span.End()

	s.logger.Info().Ctx(ctx).
		Str("order_id", id.String()).
		Str("customer_name", customerName).
		Msg("Order arrived from main/, persisting as 'arrived'")

	inserted, err := s.storeArrived(ctx, snap)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "persist failed")
		return fmt.Errorf("persist arrived: %w", err)
	}
	if !inserted {
		// main redelivered an order we already hold (SSE reconnect / outbox
		// retry). Its floor state is ours — don't re-announce it as new or
		// auto-accept it again.
		span.SetAttributes(attribute.Bool("order.redelivered", true))
		s.logger.Info().Ctx(ctx).Str("order_id", id.String()).Msg("Order already stored — redelivery ignored")
		return nil
	}
	s.devEvent(devlog.Info, devlog.KindOrder, "Order %s arrived (%s)", devlog.Short(id.String()), snap.Fulfillment.Type)

	autoAccept, err := s.settings.GetAutoAccept(ctx)
	if err != nil {
		s.logger.Warn().Ctx(ctx).Err(err).Msg("Failed to read auto-accept setting, defaulting to manual")
		autoAccept = false
	}
	span.SetAttributes(attribute.Bool("order.auto_accept", autoAccept))

	if autoAccept {
		// A future order isn't cooked now: it is scheduled with the default
		// time for its type as the lead, and auto-accepted when it rings
		// again at ring_at (RaiseDue) — that accept prints.
		if cur, gerr := s.repo.GetByID(ctx, id); gerr == nil && cur.Status.Future && cur.Status.State == order.StateArrived {
			lead := int(s.IntegrationSettings().Times.For(cur.Order.Fulfillment.Type))
			span.SetAttributes(attribute.Bool("order.future", true))
			_, err := s.Schedule(ctx, id.String(), lead)
			if err == nil {
				s.logger.Info().Ctx(ctx).Str("order_id", id.String()).Int("lead_minutes", lead).Msg("Auto-accept ON — future order scheduled")
				return nil
			}
			s.logger.Error().Ctx(ctx).Err(err).Str("order_id", id.String()).Msg("Auto-schedule failed — order rings")
			span.RecordError(err)
		} else {
			s.logger.Info().Ctx(ctx).Str("order_id", id.String()).Msg("Auto-accept ON — accepting immediately")
			if _, err := s.AutoAccept(ctx, id); err != nil {
				s.logger.Error().Ctx(ctx).Err(err).Str("order_id", id.String()).Msg("Auto-accept failed — order remains arrived")
				span.RecordError(err)
				// Fall through to broadcast so mobile sees the pending order.
			} else {
				return nil
			}
		}
	}

	// Announce the stored row (batch, arrival time) — or, if auto-accept
	// raced with a manual accept, whatever state it's in now.
	so, err := s.repo.GetByID(ctx, id)
	if err != nil {
		so = &order.StoredOrder{Order: snap, Status: order.Status{State: order.StateArrived}}
	}
	if so.Status.State == order.StateArrived {
		s.broadcastArrived(ctx, *so)
	} else {
		s.publishUpdated(*so)
	}
	return nil
}

// Accept transitions an order from arrived to accepted and prints it: claim
// (kitchen number + prep estimate), then print, then revert the claim only if
// NOTHING printed (a partial print — e.g. one station unreachable with no
// fallback — keeps the order accepted and raises a Pushover instead, since the
// kitchen already has tickets). prepMinutes nil = the last estimate used.
func (s *Service) Accept(ctx context.Context, id uuid.UUID, prepMinutes *int) (*order.StoredOrder, error) {
	so, err := s.accept(ctx, id, prepMinutes, false)
	if err == nil && so.IsDelivery() {
		s.autoAssignBG(ctx, "accepted")
	}
	return so, err
}

// AutoAccept is Accept by auto-accept (a new order, a re-rung future order,
// a web-UI resend): the order is marked auto_accepted, and the tablets get
// its order.updated right after the claim — before the print — so they show
// the "accepted automatically" banner at once (never order.arrived, unless
// nothing printed and the claim was reverted).
func (s *Service) AutoAccept(ctx context.Context, id uuid.UUID) (*order.StoredOrder, error) {
	so, err := s.accept(ctx, id, nil, true)
	if err == nil && so.IsDelivery() {
		s.autoAssignBG(ctx, "accepted")
	}
	return so, err
}

func (s *Service) accept(ctx context.Context, id uuid.UUID, prepMinutes *int, auto bool) (*order.StoredOrder, error) {
	ctx, span := s.tracer.Start(ctx, "order.accept",
		trace.WithAttributes(attribute.String("order.id", id.String()), attribute.Bool("order.auto_accepted", auto)),
	)
	defer span.End()

	fail := func(err error) (*order.StoredOrder, error) {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		s.logger.Error().Ctx(ctx).Err(err).Str("order_id", id.String()).Msg("Accept failed")
		return nil, err
	}

	prep := 0
	if prepMinutes != nil {
		prep = *prepMinutes
	} else if cur, err := s.repo.GetByID(ctx, id); err == nil && cur.Status.Rerung && cur.Status.ScheduledLeadMinutes != nil {
		// A future order ringing again: its lead time is the prep (ready at
		// promised_at), no time dialog.
		prep = int(*cur.Status.ScheduledLeadMinutes)
	} else if fs, err := s.repo.FloorSettings(ctx); err == nil {
		prep = int(fs.LastPrepMinutes)
	}
	if prep < 0 || prep > 240 {
		return fail(fmt.Errorf("%w: prep_minutes must be 0-240", ErrBadRequest))
	}

	so, err := s.repo.ClaimAccept(ctx, id, prep, auto)
	if err != nil {
		return fail(err)
	}
	span.SetAttributes(attribute.Int64("order.kitchen_number", deref(so.Status.KitchenNumber)))
	if auto {
		s.publishUpdated(*so) // the tablet's banner shouldn't wait for the printer
	}
	// Once claimed, the print runs to the end even if the tablet's request is
	// cancelled (Wi-Fi blip, app killed) — otherwise the remaining stations
	// and failovers are skipped mid-job. The dispatcher caps a job at 25 s.
	printStart := time.Now()
	out, err := s.print(context.WithoutCancel(ctx), printable(*so), printer.ModeAccept)
	s.recordPrintTime(ctx, *so, printStart, out, err)
	if err != nil {
		// Revert even if the request was cancelled mid-print.
		rctx := context.WithoutCancel(ctx)
		if rerr := s.repo.RevertAccept(rctx, id); rerr != nil {
			s.logger.Error().Ctx(ctx).Err(rerr).Str("order_id", id.String()).Msg("Failed to revert accept after print failure")
		} else if back, gerr := s.repo.GetByID(rctx, id); gerr == nil {
			s.publishUpdated(*back)
		}
		s.devEvent(devlog.Error, devlog.KindPrint, "%s NOT accepted: nothing printed", label(*so))
		s.alertPrintIssues(ctx, *so, out, true)
		return fail(fmt.Errorf("%w: %w", ErrNothingPrinted, err))
	}
	// The tickets printed: the accept must reach main even if the tablet's
	// request was cancelled meanwhile.
	if err := s.repo.RecordAccepted(context.WithoutCancel(ctx), *so, out.InsertExpected); err != nil {
		s.logger.Error().Ctx(ctx).Err(err).Str("order_id", id.String()).Msg("Failed to record accepted floor event (main won't see it)")
	}
	s.floorChanged()
	s.recordAcceptTime(ctx, *so)

	s.logger.Info().Ctx(ctx).Str("order_id", id.String()).Int64("kitchen_number", deref(so.Status.KitchenNumber)).
		Int("tickets", len(out.Printed)).Bool("auto", auto).Msg("Order accepted and printed")
	how := "accepted"
	if auto {
		how = "auto-accepted"
	}
	s.devEvent(devlog.Info, devlog.KindOrder, "%s %s (prep %d min)", label(*so), how, prep)
	s.printEvents(label(*so), out)
	s.alertPrintIssues(ctx, *so, out, true)
	s.publishUpdated(*so)
	return so, nil
}

// recordAcceptTime feeds floor.accept.seconds: arrival → accept, or for a
// future order ringing again, its re-ring → accept (the hours it waited
// scheduled aren't staff time). POS checks arrive accepted and never get
// here today; the source label is set anyway.
func (s *Service) recordAcceptTime(ctx context.Context, so order.StoredOrder) {
	if s.meters.FloorAcceptSeconds == nil {
		return
	}
	from := so.Status.ArrivalDate
	if so.Status.Rerung && so.Status.RingAt != nil && so.Status.RingAt.After(from) {
		from = *so.Status.RingAt
	}
	at := time.Now()
	if so.Status.AcceptedAt != nil {
		at = *so.Status.AcceptedAt
	}
	if from.IsZero() || at.Before(from) {
		return
	}
	src := "online"
	if so.IsPOS() {
		src = "pos"
	}
	s.meters.FloorAcceptSeconds.Record(ctx, at.Sub(from).Seconds(), metric.WithAttributes(attribute.String("source", src)))
}

// recordPrintTime feeds orders.print.seconds: one accept's print job (an
// online accept, or a new POS order's station tickets), from its start until
// every ticket was written or given up. result: ok = every ticket printed
// (rerouted ones included), partial = some printed and some didn't, failed =
// nothing printed. A plan with nothing to print records nothing.
func (s *Service) recordPrintTime(ctx context.Context, so order.StoredOrder, start time.Time, out printer.Outcome, err error) {
	if s.meters.OrdersPrintSeconds == nil || errors.Is(err, printer.ErrNoTickets) {
		return
	}
	result := "ok"
	switch {
	case err != nil:
		result = "failed"
	case len(out.Failed) > 0:
		result = "partial"
	}
	src := "online"
	if so.IsPOS() {
		src = "pos"
	}
	s.meters.OrdersPrintSeconds.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(
		attribute.String("result", result), attribute.String("source", src)))
}

// RecoverInterruptedAccepts runs once at startup: an order claimed for accept
// whose accept was never recorded (power cut / crash mid-print) goes back to
// arrived so it rings again — better a second ticket than none. Returns how
// many were put back.
func (s *Service) RecoverInterruptedAccepts(ctx context.Context) int {
	ids, err := s.repo.ListInterruptedAccepts(ctx)
	if err != nil {
		s.logger.Error().Ctx(ctx).Err(err).Msg("Interrupted-accept check failed")
		return 0
	}
	n := 0
	for _, raw := range ids {
		id, err := uuid.Parse(raw)
		if err != nil {
			continue
		}
		if err := s.repo.RevertAccept(ctx, id); err != nil {
			s.logger.Error().Ctx(ctx).Err(err).Str("order_id", raw).Msg("Failed to put an interrupted accept back")
			continue
		}
		n++
		s.logger.Warn().Ctx(ctx).Str("order_id", raw).Msg("Accept was interrupted (agent stopped mid-print) — order rings again")
	}
	return n
}

// ErrBadRequest marks caller-input errors (mapped to 400 by the transport).
var ErrBadRequest = errors.New("bad request")

// ErrNothingPrinted: the accept was reverted because no ticket printed
// anywhere (mapped to 502 by the transport; the order is still arrived).
var ErrNothingPrinted = errors.New("nothing printed, order not accepted")

// printable is the snapshot to print: the order with the number staff call
// (the kitchen number, or a POS order's check number).
func printable(so order.StoredOrder) order.Snapshot {
	snap := so.Order
	if n := so.Number(); n != nil {
		snap.KitchenNumber = n
	}
	return snap
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func (s *Service) publishUpdated(so order.StoredOrder) {
	if s.broker != nil {
		s.broker.PublishUpdated(so)
	}
}

// PrintSnapshot prints a snapshot's customer receipt directly, with no
// persistence, notification, or mobile broadcast. Backs the web UI's History
// "Reprint" for an order fetched from main/ (receipt only, like Reprint).
func (s *Service) PrintSnapshot(ctx context.Context, snap order.Snapshot) error {
	ctx, span := s.tracer.Start(ctx, "order.print_snapshot",
		trace.WithAttributes(attribute.String("order.id", snap.ID)),
	)
	defer span.End()
	out, err := s.print(ctx, snap, printer.ModeReceipt)
	s.alertPrintIssues(ctx, order.StoredOrder{Order: snap}, out, false)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "print failed")
		return err
	}
	s.meters.OrdersReprinted.Add(ctx, 1, metric.WithAttributes(attribute.String("source", "webui")))
	return nil
}

// Resend pushes an order back through the restaurant as if it had just landed.
// Backs the web UI's "Resend" for an order main/ has (History tab): if the
// agent never received it, it runs the full arrival pipeline; if it already
// has it, it re-announces or reprints it (see resendExisting) — the arrival
// upsert is insert-only, so re-running OnArrival on a known order did nothing.
func (s *Service) Resend(ctx context.Context, snap order.Snapshot) error {
	id, err := uuid.Parse(snap.ID)
	if err != nil {
		return fmt.Errorf("resend: invalid order id %q: %w", snap.ID, err)
	}
	so, err := s.repo.GetByID(ctx, id)
	switch {
	case errors.Is(err, order.ErrNotFound):
		return s.OnArrival(ctx, snap)
	case err != nil:
		return err
	}
	return s.resendExisting(ctx, so)
}

// ResendByID re-sends a locally-stored order (see Resend).
func (s *Service) ResendByID(ctx context.Context, id uuid.UUID) error {
	so, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return err
	}
	return s.resendExisting(ctx, so)
}

// resendExisting handles an order the agent already stores: still waiting
// (arrived) → re-announce it to mobile (and print it if auto-accept is on, as
// a fresh arrival would); accepted or later → print it again.
func (s *Service) resendExisting(ctx context.Context, so *order.StoredOrder) error {
	if so.Status.State != order.StateArrived {
		_, err := s.Reprint(ctx, uuid.MustParse(so.Order.ID))
		return err
	}
	autoAccept, err := s.settings.GetAutoAccept(ctx)
	if err == nil && autoAccept {
		_, err := s.AutoAccept(ctx, uuid.MustParse(so.Order.ID))
		return err
	}
	s.broadcastArrived(ctx, *so)
	return nil
}

// Reprint prints the order's customer receipt again (receipt only, normal
// failover) without changing its state or PrintedDate.
func (s *Service) Reprint(ctx context.Context, id uuid.UUID) (*order.StoredOrder, error) {
	ctx, span := s.tracer.Start(ctx, "order.reprint",
		trace.WithAttributes(attribute.String("order.id", id.String())),
	)
	defer span.End()

	so, err := s.repo.GetByID(ctx, id)
	if err != nil {
		span.RecordError(err)
		return nil, err
	}

	out, err := s.print(ctx, printable(*so), printer.ModeReceipt)
	s.alertPrintIssues(ctx, *so, out, false)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "print failed")
		return nil, err
	}
	s.meters.OrdersReprinted.Add(ctx, 1, metric.WithAttributes(attribute.String("source", "mobile")))
	s.logger.Info().Ctx(ctx).Str("order_id", id.String()).Msg("Order receipt reprinted")
	return so, nil
}

// ErrNotAccepted: a kitchen resend of an order that has no kitchen number
// (still waiting, or cancelled) — mapped to 409 not_accepted.
var ErrNotAccepted = errors.New("order is not accepted")

// ErrNothingForKitchen: a kitchen resend of an order with no station tickets
// (e.g. only receipt-only items) — mapped to 409 nothing_for_kitchen.
var ErrNothingForKitchen = errors.New("nothing on this order goes to the kitchen")

// ErrKitchenNotPrinted: a kitchen resend where no ticket printed anywhere —
// mapped to 502 print_failed.
var ErrKitchenNotPrinted = errors.New("nothing printed")

// ResendKitchen prints the order's kitchen station tickets again ("Resend to
// kitchen", Quibase only): exactly the stations accept would print, minus the
// receipt, and ignoring the pickup-cash rule. No state change. The Outcome
// says which stations printed, failed or were rerouted.
func (s *Service) ResendKitchen(ctx context.Context, id uuid.UUID) (*order.StoredOrder, printer.Outcome, error) {
	ctx, span := s.tracer.Start(ctx, "order.resend_kitchen",
		trace.WithAttributes(attribute.String("order.id", id.String())),
	)
	defer span.End()
	fail := func(out printer.Outcome, err error) (*order.StoredOrder, printer.Outcome, error) {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, out, err
	}

	so, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return fail(printer.Outcome{}, err)
	}
	if so.Number() == nil || so.Status.State == order.StateArrived || so.Status.State == order.StateScheduled || so.Status.State == order.StateCancelled {
		return fail(printer.Outcome{}, fmt.Errorf("%w (state %s)", ErrNotAccepted, so.Status.State))
	}

	out, err := s.print(ctx, printable(*so), printer.ModeKitchen)
	if errors.Is(err, printer.ErrNoTickets) {
		return fail(out, ErrNothingForKitchen)
	}
	s.alertPrintIssues(ctx, *so, out, false)
	if err != nil {
		s.logger.Error().Ctx(ctx).Err(err).Str("order_id", id.String()).Msg("Kitchen resend printed nothing")
		return fail(out, fmt.Errorf("%w: %w", ErrKitchenNotPrinted, err))
	}
	s.meters.OrdersReprinted.Add(ctx, 1, metric.WithAttributes(attribute.String("source", "kitchen_resend")))
	s.logger.Info().Ctx(ctx).Str("order_id", id.String()).Int64("kitchen_number", deref(so.Status.KitchenNumber)).
		Int("tickets", len(out.Printed)).Int("failed", len(out.Failed)).Msg("Order resent to the kitchen")
	return so, out, nil
}

func (s *Service) GetByID(ctx context.Context, id uuid.UUID) (*order.StoredOrder, error) {
	ctx, span := s.tracer.Start(ctx, "order.get_by_id",
		trace.WithAttributes(attribute.String("order.id", id.String())),
	)
	defer span.End()
	return s.repo.GetByID(ctx, id)
}

func (s *Service) ListPage(ctx context.Context, offset, limit int) ([]order.StoredOrder, error) {
	ctx, span := s.tracer.Start(ctx, "order.list_page",
		trace.WithAttributes(attribute.Int("offset", offset), attribute.Int("limit", limit)),
	)
	defer span.End()
	return s.repo.ListPage(ctx, offset, limit)
}

func (s *Service) ListArrived(ctx context.Context) ([]order.StoredOrder, error) {
	ctx, span := s.tracer.Start(ctx, "order.list_arrived")
	defer span.End()
	return s.repo.ListArrived(ctx)
}

func (s *Service) GetAutoAccept(ctx context.Context) (bool, error) {
	ctx, span := s.tracer.Start(ctx, "order.get_auto_accept")
	defer span.End()
	return s.settings.GetAutoAccept(ctx)
}

func (s *Service) SetAutoAccept(ctx context.Context, enabled bool) error {
	ctx, span := s.tracer.Start(ctx, "order.set_auto_accept",
		trace.WithAttributes(attribute.Bool("auto_accept", enabled)),
	)
	defer span.End()
	return s.settings.SetAutoAccept(ctx, enabled)
}

// --- helpers --------------------------------------------------------------

// storeArrived persists the arrived snapshot to SQLite inside its own span and
// records the orders.stored{result} funnel counter. It does not change control
// flow — the error is returned verbatim to OnArrival.
func (s *Service) storeArrived(ctx context.Context, snap order.Snapshot) (bool, error) {
	ctx, span := s.tracer.Start(ctx, "order.store",
		trace.WithAttributes(attribute.String("order.id", snap.ID)),
	)
	defer span.End()

	inserted, err := s.repo.UpsertArrived(ctx, snap)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "persist failed")
		s.meters.OrdersStored.Add(ctx, 1, metric.WithAttributes(attribute.String("result", "error")))
		return false, err
	}
	result := "ok"
	if !inserted {
		result = "duplicate"
	}
	s.meters.OrdersStored.Add(ctx, 1, metric.WithAttributes(attribute.String("result", result)))
	// Ack every stored order — a redelivery too (main re-sends until acked).
	if s.onStored != nil {
		s.onStored(snap.ID)
	}
	return inserted, nil
}

// OnStored registers the hook run after an order is safely stored (new or
// already held) — cmd/main.go wires the acker.
func (s *Service) OnStored(fn func(orderID string)) { s.onStored = fn }

func (s *Service) broadcastArrived(ctx context.Context, so order.StoredOrder) {
	if s.broker == nil {
		return
	}
	s.broker.PublishArrived(ctx, so)
}

// alertPrintIssues sends ONE Pushover for an order's print trouble: tickets
// that printed nowhere, tickets rerouted off a dead station, and (on the first
// print only — withUnrouted) items main couldn't match to a printer, which
// went to every station and mean the menu needs fixing. The log line and the
// developer events name the order by its number only (no customer).
func (s *Service) alertPrintIssues(ctx context.Context, so order.StoredOrder, out printer.Outcome, withUnrouted bool) {
	snap := so.Order
	what := label(so)
	if so.Status.State == "" { // a bare snapshot (History reprint)
		what = snapLabel(snap)
	}
	var parts []string
	for _, f := range out.Failed {
		parts = append(parts, fmt.Sprintf("NOT PRINTED: %s ticket for %s", f.Kind, f.Station))
		s.devEvent(devlog.Error, devlog.KindPrint, "%s %s ticket for %s NOT printed", what, f.Kind, f.Station)
	}
	for _, p := range out.Printed {
		if p.ReroutedFrom != "" {
			parts = append(parts, fmt.Sprintf("%s ticket rerouted to %s", p.ReroutedFrom, p.PrintedOn))
			s.devEvent(devlog.Warn, devlog.KindPrint, "%s %s ticket rerouted to %s", what, p.ReroutedFrom, p.PrintedOn)
		}
	}
	if withUnrouted && len(out.Unrouted) > 0 {
		parts = append(parts, "no printer for: "+strings.Join(out.Unrouted, ", ")+" (sent to every station)")
		s.devEvent(devlog.Warn, devlog.KindPrint, "%s has %d item(s) with no printer (sent to every station)", what, len(out.Unrouted))
	}
	if len(parts) == 0 {
		return
	}
	s.logger.Warn().Ctx(ctx).Str("order_id", snap.ID).Strs("issues", parts).Msg("Print trouble on " + what)
	if s.notifier == nil {
		return
	}
	who := snap.Customer.DisplayName
	if who == "" {
		who = "(no name)"
	}
	msg := fmt.Sprintf("Order %s: %s", who, strings.Join(parts, "; "))
	// In the background: the accept response mustn't wait on Pushover.
	go func(ctx context.Context) {
		if err := s.notifier.SendFloor(ctx, msg, "siren"); err != nil {
			s.logger.Warn().Ctx(ctx).Err(err).Msg("Pushover print-issue alert failed (non-fatal)")
		}
	}(context.WithoutCancel(ctx))
}

func formatServiceType(t string) string {
	switch strings.ToLower(t) {
	case "delivery":
		return "Delivery"
	case "pickup":
		return "Pickup"
	case "dine_in":
		return "Dine In"
	default:
		if t != "" {
			return strings.ToUpper(t)
		}
		return "Online Order"
	}
}
