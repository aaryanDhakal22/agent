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
	printer  printer.Printer
	registry *Registry
	logger   zerolog.Logger
	tracer   trace.Tracer
	meters   observability.Meters
}

func NewService(p printer.Printer, registry *Registry, logger zerolog.Logger, meters observability.Meters) *Service {
	if registry != nil {
		registry.Register(p.Name())
	}
	return &Service{
		printer:  p,
		registry: registry,
		logger:   logger.With().Str("module", "printer-service").Str("printer_name", p.Name()).Logger(),
		tracer:   otel.Tracer(tracerName),
		meters:   meters,
	}
}

// KeepCheck runs until ctx is cancelled, polling printer reachability. Probes
// immediately on entry, then every `delay` thereafter, so the Registry has a
// real status inside ~3s (detect timeout) of agent start rather than having
// to wait a full delay cycle. Transitions are sent to Pushover and recorded
// as a printer.status gauge point and in the in-memory Registry that the
// mobile-facing /api/printers endpoint reads.
func (s *Service) KeepCheck(ctx context.Context, delay time.Duration, notifier *notify.Notifier) {
	name := s.printer.Name()
	nameAttr := attribute.String("printer.name", name)
	lastStatusKnown := false
	lastUp := false

	for {
		s.logger.Debug().Ctx(ctx).Msg("Checking printer availability")

		start := time.Now()
		err := s.printer.Detect(ctx)
		probedAt := time.Now()
		elapsedMs := float64(probedAt.Sub(start).Microseconds()) / 1000.0
		s.meters.PrinterDetectMs.Record(ctx, elapsedMs, metric.WithAttributes(nameAttr))

		up := err == nil
		statusVal := int64(0)
		if up {
			statusVal = 1
		}
		s.meters.PrinterStatus.Record(ctx, statusVal, metric.WithAttributes(nameAttr))

		if s.registry != nil {
			s.registry.Record(name, s.printer.IP(), up, err, probedAt)
		}

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

		select {
		case <-ctx.Done():
			s.logger.Info().Ctx(ctx).Msg("KeepCheck stopping")
			return
		case <-time.After(delay):
		}
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

// Printer returns the underlying printer handle so callers (namely the
// Manager) can flip the IP. Intentionally returns the interface, not the
// concrete type — keeps the domain boundary honest.
func (s *Service) Printer() printer.Printer {
	return s.printer
}

// ProbeNow runs a single Detect + Registry.Record cycle, outside the
// KeepCheck loop. Used by the mobile-initiated IP update flow to give
// immediate feedback ("did that new IP work?") rather than waiting up to
// PRINTER_DETECT_DELAY for the next scheduled probe. Intentionally does not
// send Pushover notifications — those are for unattended transitions, not
// actions the user just took.
func (s *Service) ProbeNow(ctx context.Context) {
	name := s.printer.Name()
	nameAttr := attribute.String("printer.name", name)

	ctx, span := s.tracer.Start(ctx, "printer.probe_now",
		trace.WithAttributes(nameAttr),
	)
	defer span.End()

	start := time.Now()
	err := s.printer.Detect(ctx)
	probedAt := time.Now()
	elapsedMs := float64(probedAt.Sub(start).Microseconds()) / 1000.0
	s.meters.PrinterDetectMs.Record(ctx, elapsedMs, metric.WithAttributes(nameAttr))

	up := err == nil
	statusVal := int64(0)
	if up {
		statusVal = 1
	}
	s.meters.PrinterStatus.Record(ctx, statusVal, metric.WithAttributes(nameAttr))

	if s.registry != nil {
		s.registry.Record(name, s.printer.IP(), up, err, probedAt)
	}

	if err != nil {
		span.RecordError(err)
		s.logger.Warn().Ctx(ctx).Err(err).Msg("ProbeNow: printer unreachable")
		return
	}
	s.logger.Info().Ctx(ctx).Msg("ProbeNow: printer reachable")
}
