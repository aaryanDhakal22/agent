package order

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"

	printerApp "quiccpos/agent/internal/application/printer"
	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/infra/ssebroker"
	"quiccpos/agent/internal/infra/notify"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const tracerName = "quiccpos/agent/order"

// Service orchestrates the order lifecycle inside the agent:
//
//	SSE arrival ─▶ OnArrival ─┬─ auto-accept on  ─▶ Accept (prints)
//	                          └─ auto-accept off ─▶ broadcast to mobile,
//	                                                mobile POSTs /accept
//	                                                ─▶ Accept (prints)
//
// All DB state transitions are done inside a repo transaction; the physical
// print is inside that same transaction so that a failed print rolls the
// state back to `arrived` for re-attempt. Pushover notifications fire AFTER
// the transaction commits — a flaky Pushover never rolls back a successful
// print.
type Service struct {
	repo           order.Repository
	settings       order.SettingsRepository
	printerService *printerApp.Service
	notifier       *notify.Notifier
	broker         *ssebroker.Broker
	logger         zerolog.Logger
	tracer         trace.Tracer
}

func NewService(
	repo order.Repository,
	settings order.SettingsRepository,
	printerService *printerApp.Service,
	notifier *notify.Notifier,
	broker *ssebroker.Broker,
	logger zerolog.Logger,
) *Service {
	return &Service{
		repo:           repo,
		settings:       settings,
		printerService: printerService,
		notifier:       notifier,
		broker:         broker,
		logger:         logger.With().Str("module", "order-service").Logger(),
		tracer:         otel.Tracer(tracerName),
	}
}

// OnArrival is called by the SSE listener when a new order lands from main/.
// Persists the order as `arrived`, then either auto-accepts it (printing
// immediately) or broadcasts to mobile for manual acceptance.
func (s *Service) OnArrival(ctx context.Context, o order.OrderRequest) error {
	customerName := o.Customer.FirstName + " " + o.Customer.LastName

	ctx, span := s.tracer.Start(ctx, "order.on_arrival",
		trace.WithAttributes(
			attribute.Int("order.id", o.OrderID),
			attribute.String("order.service_type", o.ServiceType),
			attribute.String("order.customer_name", customerName),
			attribute.Int("order.item_count", len(o.Items)),
			attribute.Float64("order.total", o.OrderTotal),
		),
	)
	defer span.End()

	s.logger.Info().Ctx(ctx).
		Int("order_id", o.OrderID).
		Str("customer_name", customerName).
		Msg("Order arrived from main/, persisting as 'arrived'")

	if err := s.repo.UpsertArrived(ctx, o); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "persist failed")
		return fmt.Errorf("persist arrived: %w", err)
	}

	autoAccept, err := s.settings.GetAutoAccept(ctx)
	if err != nil {
		// Auto-accept read failure shouldn't kill the order — default to
		// broadcast so mobile can decide. Log and continue.
		s.logger.Warn().Ctx(ctx).Err(err).Msg("Failed to read auto-accept setting, defaulting to manual")
		autoAccept = false
	}
	span.SetAttributes(attribute.Bool("order.auto_accept", autoAccept))

	if autoAccept {
		s.logger.Info().Ctx(ctx).Int("order_id", o.OrderID).Msg("Auto-accept ON — accepting immediately")
		if _, err := s.Accept(ctx, o.OrderID); err != nil {
			// Don't return — the order is still persisted as arrived and can
			// be manually accepted once the printer is back.
			s.logger.Error().Ctx(ctx).Err(err).Int("order_id", o.OrderID).Msg("Auto-accept failed — order remains arrived")
			span.RecordError(err)
			// Fall through to broadcast so mobile sees the pending order.
		} else {
			return nil
		}
	}

	// Manual-accept path: tell the mobile interface a new order is waiting.
	s.broadcastArrived(ctx, order.StoredOrder{
		Order: o,
		Status: order.Status{
			State: order.StateArrived,
		},
	})
	return nil
}

// Accept transitions an order from arrived to accepted. The DB update and the
// physical print happen inside one transaction — a printer failure rolls back
// the state change so the order can be re-accepted. Pushover notification is
// best-effort and fires AFTER commit.
func (s *Service) Accept(ctx context.Context, id int) (*order.StoredOrder, error) {
	ctx, span := s.tracer.Start(ctx, "order.accept",
		trace.WithAttributes(attribute.Int("order.id", id)),
	)
	defer span.End()

	so, err := s.repo.AcceptAndPrint(ctx, id, s.printerService.Print)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		s.logger.Error().Ctx(ctx).Err(err).Int("order_id", id).Msg("Accept failed")
		return nil, err
	}

	s.logger.Info().Ctx(ctx).Int("order_id", id).Msg("Order accepted and printed")

	// Notify Pushover after the commit — a Pushover outage never rolls back a
	// successful print.
	s.notifyAccepted(ctx, so.Order)
	return so, nil
}

// Reprint prints the order again without changing its state or PrintedDate.
// Works regardless of current state — mobile uses this to re-run any order
// from history.
func (s *Service) Reprint(ctx context.Context, id int) (*order.StoredOrder, error) {
	ctx, span := s.tracer.Start(ctx, "order.reprint",
		trace.WithAttributes(attribute.Int("order.id", id)),
	)
	defer span.End()

	so, err := s.repo.GetByID(ctx, id)
	if err != nil {
		span.RecordError(err)
		return nil, err
	}

	if err := s.printerService.Print(ctx, so.Order); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "print failed")
		return nil, err
	}
	s.logger.Info().Ctx(ctx).Int("order_id", id).Msg("Order reprinted")
	return so, nil
}

func (s *Service) GetByID(ctx context.Context, id int) (*order.StoredOrder, error) {
	ctx, span := s.tracer.Start(ctx, "order.get_by_id",
		trace.WithAttributes(attribute.Int("order.id", id)),
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

func (s *Service) broadcastArrived(ctx context.Context, so order.StoredOrder) {
	if s.broker == nil {
		return
	}
	s.broker.PublishArrived(ctx, so)
}

func (s *Service) notifyAccepted(ctx context.Context, o order.OrderRequest) {
	if s.notifier == nil {
		return
	}
	serviceType := formatServiceType(o.ServiceType)
	customerName := o.Customer.FirstName + " " + o.Customer.LastName
	msg := fmt.Sprintf("%s Order : %s", serviceType, customerName)

	var sound string
	switch {
	case o.Payments == nil:
		sound = "cash-order"
	case o.OrderTotal > 50:
		if rand.IntN(2)+1 == 1 {
			sound = "obama-order"
		} else {
			sound = "donald-order"
		}
	default:
		sound = "credit-order"
	}

	if err := s.notifier.Send(ctx, msg, sound); err != nil {
		s.logger.Warn().Ctx(ctx).Err(err).Msg("Pushover notify failed (non-fatal)")
	}
}

func formatServiceType(t string) string {
	switch strings.ToLower(t) {
	case "delivery":
		return "Delivery"
	case "pickup":
		return "Pickup"
	default:
		if t != "" {
			return strings.ToUpper(t)
		}
		return "Online Order"
	}
}
