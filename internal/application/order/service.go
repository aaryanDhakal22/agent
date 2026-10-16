package order

import (
	"context"
	"errors"
	"fmt"
	"strings"

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
// the printer.Dispatcher's Execute (plan the order's tickets, print each with
// failover). It errors only when nothing printed; partial trouble is in the
// Outcome. Tests swap in a recording fake.
type PrintFunc = func(ctx context.Context, snap order.Snapshot) (printer.Outcome, error)

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
	tracer   trace.Tracer
	meters   observability.Meters
	// onFloorEvent is called after every committed floor change so the sync
	// sender pushes it to main right away (set via OnFloorEvent).
	onFloorEvent func()
	// Driver-report printing (SetDocumentPrinting).
	printDoc    DocPrinter
	buildReport ReportBuilder
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
func (s *Service) OnFloorEvent(fn func()) { s.onFloorEvent = fn }

func (s *Service) floorChanged() {
	if s.onFloorEvent != nil {
		s.onFloorEvent()
	}
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

	autoAccept, err := s.settings.GetAutoAccept(ctx)
	if err != nil {
		s.logger.Warn().Ctx(ctx).Err(err).Msg("Failed to read auto-accept setting, defaulting to manual")
		autoAccept = false
	}
	span.SetAttributes(attribute.Bool("order.auto_accept", autoAccept))

	if autoAccept {
		s.logger.Info().Ctx(ctx).Str("order_id", id.String()).Msg("Auto-accept ON — accepting immediately")
		if _, err := s.Accept(ctx, id, nil); err != nil {
			s.logger.Error().Ctx(ctx).Err(err).Str("order_id", id.String()).Msg("Auto-accept failed — order remains arrived")
			span.RecordError(err)
			// Fall through to broadcast so mobile sees the pending order.
		} else {
			return nil
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
	ctx, span := s.tracer.Start(ctx, "order.accept",
		trace.WithAttributes(attribute.String("order.id", id.String())),
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
	} else if fs, err := s.repo.FloorSettings(ctx); err == nil {
		prep = int(fs.LastPrepMinutes)
	}
	if prep < 0 || prep > 240 {
		return fail(fmt.Errorf("%w: prep_minutes must be 0-240", ErrBadRequest))
	}

	so, err := s.repo.ClaimAccept(ctx, id, prep)
	if err != nil {
		return fail(err)
	}
	span.SetAttributes(attribute.Int64("order.kitchen_number", deref(so.Status.KitchenNumber)))
	out, err := s.print(ctx, printable(*so))
	if err != nil {
		// Revert even if the request was cancelled mid-print.
		rctx := context.WithoutCancel(ctx)
		if rerr := s.repo.RevertAccept(rctx, id); rerr != nil {
			s.logger.Error().Ctx(ctx).Err(rerr).Str("order_id", id.String()).Msg("Failed to revert accept after print failure")
		} else if back, gerr := s.repo.GetByID(rctx, id); gerr == nil {
			s.publishUpdated(*back)
		}
		s.alertPrintIssues(ctx, so.Order, out, true)
		return fail(fmt.Errorf("%w: %w", ErrNothingPrinted, err))
	}
	// The tickets printed: the accept must reach main even if the tablet's
	// request was cancelled meanwhile.
	if err := s.repo.RecordAccepted(context.WithoutCancel(ctx), *so); err != nil {
		s.logger.Error().Ctx(ctx).Err(err).Str("order_id", id.String()).Msg("Failed to record accepted floor event (main won't see it)")
	}
	s.floorChanged()

	s.logger.Info().Ctx(ctx).Str("order_id", id.String()).Int64("kitchen_number", deref(so.Status.KitchenNumber)).
		Int("tickets", len(out.Printed)).Msg("Order accepted and printed")
	s.alertPrintIssues(ctx, so.Order, out, true)
	s.publishUpdated(*so)
	return so, nil
}

// ErrBadRequest marks caller-input errors (mapped to 400 by the transport).
var ErrBadRequest = errors.New("bad request")

// ErrNothingPrinted: the accept was reverted because no ticket printed
// anywhere (mapped to 502 by the transport; the order is still arrived).
var ErrNothingPrinted = errors.New("nothing printed, order not accepted")

// printable is the snapshot to print: the order with its kitchen number.
func printable(so order.StoredOrder) order.Snapshot {
	snap := so.Order
	if so.Status.KitchenNumber != nil {
		snap.KitchenNumber = so.Status.KitchenNumber
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

// PrintSnapshot prints a snapshot directly, with no persistence, notification,
// or mobile broadcast. Backs the web UI's "Reprint" for a historical order that
// was fetched from main/ and isn't in the agent's local rolling window.
func (s *Service) PrintSnapshot(ctx context.Context, snap order.Snapshot) error {
	ctx, span := s.tracer.Start(ctx, "order.print_snapshot",
		trace.WithAttributes(attribute.String("order.id", snap.ID)),
	)
	defer span.End()
	out, err := s.print(ctx, snap)
	s.alertPrintIssues(ctx, snap, out, false)
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
		_, err := s.Accept(ctx, uuid.MustParse(so.Order.ID), nil)
		return err
	}
	s.broadcastArrived(ctx, *so)
	return nil
}

// Reprint prints the order again without changing its state or PrintedDate.
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

	out, err := s.print(ctx, printable(*so))
	s.alertPrintIssues(ctx, so.Order, out, false)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "print failed")
		return nil, err
	}
	s.meters.OrdersReprinted.Add(ctx, 1, metric.WithAttributes(attribute.String("source", "mobile")))
	s.logger.Info().Ctx(ctx).Str("order_id", id.String()).Msg("Order reprinted")
	return so, nil
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
	return inserted, nil
}

func (s *Service) broadcastArrived(ctx context.Context, so order.StoredOrder) {
	if s.broker == nil {
		return
	}
	s.broker.PublishArrived(ctx, so)
}

// alertPrintIssues sends ONE Pushover for an order's print trouble: tickets
// that printed nowhere, tickets rerouted off a dead station, and (on the first
// print only — withUnrouted) items main couldn't match to a printer, which
// went to every station and mean the menu needs fixing.
func (s *Service) alertPrintIssues(ctx context.Context, snap order.Snapshot, out printer.Outcome, withUnrouted bool) {
	var parts []string
	for _, f := range out.Failed {
		parts = append(parts, fmt.Sprintf("NOT PRINTED: %s ticket for %s", f.Kind, f.Station))
	}
	for _, p := range out.Printed {
		if p.ReroutedFrom != "" {
			parts = append(parts, fmt.Sprintf("%s ticket rerouted to %s", p.ReroutedFrom, p.PrintedOn))
		}
	}
	if withUnrouted && len(out.Unrouted) > 0 {
		parts = append(parts, "no printer for: "+strings.Join(out.Unrouted, ", ")+" (sent to every station)")
	}
	if len(parts) == 0 {
		return
	}
	who := snap.Customer.DisplayName
	if who == "" {
		who = "(no name)"
	}
	msg := fmt.Sprintf("Order %s: %s", who, strings.Join(parts, "; "))
	s.logger.Warn().Ctx(ctx).Str("order_id", snap.ID).Msg(msg)
	if s.notifier == nil {
		return
	}
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
