package order

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"

	printerApp "quiccpos/agent/internal/application/printer"
	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/infrastructure/notify"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const tracerName = "quiccpos/agent/order"

type Service struct {
	printerService *printerApp.Service
	notifier       *notify.Notifier
	logger         zerolog.Logger
	tracer         trace.Tracer
}

func NewService(printerService *printerApp.Service, notifier *notify.Notifier, logger zerolog.Logger) *Service {
	return &Service{
		printerService: printerService,
		notifier:       notifier,
		logger:         logger.With().Str("module", "order-service").Logger(),
		tracer:         otel.Tracer(tracerName),
	}
}

// Handle processes a received order by printing a receipt and sending a
// Pushover notification.
func (s *Service) Handle(ctx context.Context, o order.OrderRequest) error {
	customerName := o.Customer.FirstName + " " + o.Customer.LastName

	ctx, span := s.tracer.Start(ctx, "order.handle",
		trace.WithAttributes(
			attribute.Int("order.id", o.OrderID),
			attribute.String("order.service_type", o.ServiceType),
			attribute.String("order.customer_name", customerName),
			attribute.Int("order.item_count", len(o.Items)),
			attribute.Float64("order.total", o.OrderTotal),
			attribute.String("printer.target", s.printerService.Name()),
		),
	)
	defer span.End()

	s.logger.Info().Ctx(ctx).
		Int("order_id", o.OrderID).
		Str("customer_name", customerName).
		Str("service_type", o.ServiceType).
		Msg("Handling order")

	if err := s.printerService.Print(ctx, o); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "print failed")
		s.logger.Error().Ctx(ctx).
			Err(err).
			Int("order_id", o.OrderID).
			Str("customer_name", customerName).
			Msg("Failed to print receipt")
		return err
	}

	// Fire-and-consider-errors on notifications — we never want to fail an
	// order because Pushover is down.
	serviceType := formatServiceType(o.ServiceType)
	ntStr := fmt.Sprintf("%s Order : %s", serviceType, customerName)
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
	if err := s.notifier.Send(ctx, ntStr, sound); err != nil {
		s.logger.Warn().Ctx(ctx).Err(err).Msg("Pushover notify failed (non-fatal)")
	}

	s.logger.Info().Ctx(ctx).
		Int("order_id", o.OrderID).
		Str("customer_name", customerName).
		Msg("Order handled successfully")

	return nil
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
