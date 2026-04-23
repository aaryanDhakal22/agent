package printer

import (
	"context"
	"fmt"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
	"quiccpos/agent/internal/infra/notify"
	"quiccpos/agent/internal/infra/printer/receipt"
	"quiccpos/agent/internal/observability"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

const tracerName = "quiccpos/agent/printer"

type Service struct {
	printer printer.Printer
	logger  zerolog.Logger
	tracer  trace.Tracer
	meters  observability.Meters
}

func NewService(p printer.Printer, logger zerolog.Logger, meters observability.Meters) *Service {
	return &Service{
		printer: p,
		logger:  logger.With().Str("module", "printer-service").Str("printer_name", p.Name()).Logger(),
		tracer:  otel.Tracer(tracerName),
		meters:  meters,
	}
}

// KeepCheck runs until ctx is cancelled, polling printer reachability every
// `delay`. Status transitions are both sent to Pushover and recorded as a
// printer.status gauge point.
func (s *Service) KeepCheck(ctx context.Context, delay time.Duration, notifier *notify.Notifier) {
	name := s.printer.Name()
	nameAttr := attribute.String("printer.name", name)
	lastStatusKnown := false
	lastUp := false

	for {
		select {
		case <-ctx.Done():
			s.logger.Info().Ctx(ctx).Msg("KeepCheck stopping")
			return
		case <-time.After(delay):
		}

		s.logger.Debug().Ctx(ctx).Msg("Checking printer availability")

		start := time.Now()
		err := s.printer.Detect(ctx)
		elapsedMs := float64(time.Since(start).Microseconds()) / 1000.0
		s.meters.PrinterDetectMs.Record(ctx, elapsedMs, metric.WithAttributes(nameAttr))

		up := err == nil
		statusVal := int64(0)
		if up {
			statusVal = 1
		}
		s.meters.PrinterStatus.Record(ctx, statusVal, metric.WithAttributes(nameAttr))

		if err != nil {
			if !lastStatusKnown || lastUp {
				msg := fmt.Sprintf("The %s Printer is unreachable.", name)
				s.logger.Warn().Ctx(ctx).Err(err).Msg(msg)
				if notifier != nil {
					if nerr := notifier.Send(ctx, msg, "printer-error"); nerr != nil {
						s.logger.Warn().Ctx(ctx).Err(nerr).Msg("Pushover notify failed")
					}
				}
			}
			lastStatusKnown = true
			lastUp = false

			// Back off a bit after a failure so we don't hammer the network.
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
			continue
		}

		if lastStatusKnown && !lastUp {
			msg := fmt.Sprintf("The %s Printer is back online.", name)
			s.logger.Info().Ctx(ctx).Msg(msg)
			if notifier != nil {
				if nerr := notifier.Send(ctx, msg, "classical"); nerr != nil {
					s.logger.Warn().Ctx(ctx).Err(nerr).Msg("Pushover notify failed")
				}
			}
		}
		lastStatusKnown = true
		lastUp = true
		s.logger.Debug().Ctx(ctx).Msg("Printer reachable")
	}
}

// Print builds ESC/POS commands from the order and sends them to the printer.
func (s *Service) Print(ctx context.Context, o order.OrderRequest) error {
	name := s.printer.Name()
	nameAttr := attribute.String("printer.name", name)

	ctx, span := s.tracer.Start(ctx, "printer.print",
		trace.WithAttributes(
			nameAttr,
			attribute.Int("order.id", o.OrderID),
			attribute.String("order.service_type", o.ServiceType),
		),
	)
	defer span.End()

	s.logger.Info().Ctx(ctx).Int("order_id", o.OrderID).Msg("Detecting printer availability before print")
	if err := s.printer.Detect(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "detect failed")
		s.meters.OrdersPrinted.Add(ctx, 1, metric.WithAttributes(nameAttr, attribute.String("result", "detect_failed")))
		s.logger.Error().Ctx(ctx).Err(err).Int("order_id", o.OrderID).Msg("Printer not reachable, skipping print")
		return err
	}

	s.logger.Debug().Ctx(ctx).Int("order_id", o.OrderID).Msg("Building receipt commands")
	commands := receipt.Build(o)
	span.SetAttributes(attribute.Int("receipt.bytes", len(commands)))

	start := time.Now()
	err := s.printer.Print(ctx, printer.PrintJob{Commands: commands})
	elapsedMs := float64(time.Since(start).Microseconds()) / 1000.0
	s.meters.PrinterWriteMs.Record(ctx, elapsedMs, metric.WithAttributes(nameAttr))

	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "print failed")
		s.meters.OrdersPrinted.Add(ctx, 1, metric.WithAttributes(nameAttr, attribute.String("result", "error")))
		s.logger.Error().Ctx(ctx).Err(err).Int("order_id", o.OrderID).Msg("Failed to print receipt")
		return err
	}

	s.meters.OrdersPrinted.Add(ctx, 1, metric.WithAttributes(nameAttr, attribute.String("result", "ok")))
	s.logger.Info().Ctx(ctx).Int("order_id", o.OrderID).Msg("Receipt printed successfully")
	return nil
}

// Name exposes the underlying printer's name for telemetry labels.
func (s *Service) Name() string {
	return s.printer.Name()
}
