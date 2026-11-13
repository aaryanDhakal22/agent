package printer

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
	"quiccpos/agent/internal/infra/notify"
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
	// onProbe runs after every health probe (KeepCheck, ProbeNow) with the
	// address probed — the dispatcher's hook for learning printer MACs.
	onProbe func(ctx context.Context, ip string, up bool)
	// onStatus runs when KeepCheck sees the printer go down or come back
	// (the developer-mode events), with the message it logs.
	onStatus func(up bool, msg string)
	// downAfter: printer_down_after_seconds (the dispatcher's setting; nil =
	// DefaultDownAfter). now: the clock (tests).
	downAfter func() time.Duration
	now       func() time.Time
	// adminCool (2.5.2, the dispatcher's, so it outlives a roster rebuild):
	// a flapping printer's down Pushover at most once per 15 min; downPaged
	// = this outage's down went out (its "back online" goes out too).
	adminCool *notify.Cooldown
	downPaged bool
	// inFlight / lastPrintOK (unix nanos): prints to this printer, so a
	// probe failing on a busy printer isn't counted (L-5).
	inFlight    atomic.Int32
	lastPrintOK atomic.Int64
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
	var st probeState
	for {
		up := s.probeStep(ctx, &st, notifier)
		wait := delay
		if !up {
			// A failing printer is probed again soon (it may be a blip), so
			// "down" is decided on the time it has really been failing.
			wait = failingProbeEvery
		}
		select {
		case <-ctx.Done():
			s.logger.Debug().Ctx(ctx).Msg("KeepCheck stopping")
			return
		case <-time.After(wait):
		}
	}
}

// failingProbeEvery: how often a failing printer is probed again.
const failingProbeEvery = 4 * time.Second

// DefaultDownAfter is printer_down_after_seconds' default: a printer counts
// as down (one Warn, one Pushover) only after failing this long without a
// break (the owner, 2026-11-12: blips paged every few minutes).
const DefaultDownAfter = 30 * time.Second

func (s *Service) downAfterNow() time.Duration {
	if s.downAfter != nil {
		if d := s.downAfter(); d > 0 {
			return d
		}
	}
	return DefaultDownAfter
}

func (s *Service) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// probeStep runs one health probe and acts on it; it reports whether the
// probe counted as up.
//
// L-5 (owner rules, 2026-11-12):
//   - a failed probe is a Debug line;
//   - a probe that fails while a print to this printer is in flight, or
//     within downAfter of a good print, is not counted (many ESC/POS
//     printers take one connection at a time: a probe landing on a busy
//     printer fails though the printer is fine);
//   - "down" (Warn, Pushover, printer.status 0) only after the printer has
//     failed continuously for downAfter (printer_down_after_seconds);
//   - "back online" (Info, Pushover) only after a "down" was sent;
//   - (2.5.2) the developer's down Pushover at most once per 15 min per
//     printer (a flapping printer); the owner's rule is application/owneralert.
func (s *Service) probeStep(ctx context.Context, st *probeState, notifier *notify.Notifier) bool {
	name := s.printer.Name()
	nameAttr := attribute.String("printer.name", name)

	start := time.Now()
	err := s.printer.Detect(ctx)
	elapsedMs := float64(time.Since(start).Microseconds()) / 1000.0
	s.meters.PrinterDetectMs.Record(ctx, elapsedMs, metric.WithAttributes(nameAttr))
	probedAt := s.clock()
	downAfter := s.downAfterNow()

	up := err == nil
	if !up && s.printing(probedAt, downAfter) {
		s.logger.Debug().Ctx(ctx).Err(err).Msg("Printer probe failed while printing — not counted")
		return true
	}
	if s.registry != nil {
		s.registry.Record(name, s.printer.IP(), up, err, probedAt)
	}
	if s.onProbe != nil {
		s.onProbe(ctx, s.printer.IP(), up)
	}

	switch st.observe(up, probedAt, downAfter) {
	case transitionDown:
		msg := fmt.Sprintf("The %s Printer is unreachable.", name)
		if s.onStatus != nil {
			s.onStatus(false, msg)
		}
		s.logger.Warn().Ctx(ctx).Err(err).Dur("failing_for", downAfter).Msg(msg)
		s.downPaged = notifier != nil && s.adminCool.Allow("printer_down:"+name, probedAt)
		if s.downPaged {
			if nerr := notifier.SendFloor(ctx, msg, "printer-error"); nerr != nil {
				s.logger.Warn().Ctx(ctx).Err(nerr).Msg("Pushover notify failed")
			}
		}
	case transitionBack:
		msg := fmt.Sprintf("The %s Printer is back online.", name)
		if s.onStatus != nil {
			s.onStatus(true, msg)
		}
		s.logger.Info().Ctx(ctx).Msg(msg)
		if notifier != nil && s.downPaged {
			s.downPaged = false
			if nerr := notifier.SendFloor(ctx, msg, "classical"); nerr != nil {
				s.logger.Warn().Ctx(ctx).Err(nerr).Msg("Pushover notify failed")
			}
		}
	default:
		if err != nil {
			s.logger.Debug().Ctx(ctx).Err(err).Msg("Printer probe failed")
		}
	}
	// printer.status is the debounced state: 0 only once the printer was
	// declared down (Grafana's panels and alerts read the same notion).
	statusVal := int64(1)
	if st.down {
		statusVal = 0
	}
	s.meters.PrinterStatus.Record(ctx, statusVal, metric.WithAttributes(nameAttr))
	return up
}

// printing: a print to this printer is in flight, or one succeeded within
// window — a failed probe then means "busy", not "down".
func (s *Service) printing(now time.Time, window time.Duration) bool {
	if s.inFlight.Load() > 0 {
		return true
	}
	last := s.lastPrintOK.Load()
	return last != 0 && now.Sub(time.Unix(0, last)) < window
}

// startJob marks a print in flight; the returned func ends it (ok = printed).
func (s *Service) startJob() func(ok bool) {
	s.inFlight.Add(1)
	return func(ok bool) {
		if ok {
			s.lastPrintOK.Store(s.clock().UnixNano())
		}
		s.inFlight.Add(-1)
	}
}

type transition int

const (
	transitionNone transition = iota
	transitionDown
	transitionBack
)

// probeState debounces the health probes: down once failing continuously for
// downAfter; back at the first good probe after a down.
type probeState struct {
	down         bool
	failingSince time.Time
}

func (p *probeState) observe(up bool, now time.Time, downAfter time.Duration) transition {
	if up {
		p.failingSince = time.Time{}
		if p.down {
			p.down = false
			return transitionBack
		}
		return transitionNone
	}
	if p.failingSince.IsZero() {
		p.failingSince = now
	}
	if !p.down && now.Sub(p.failingSince) >= downAfter {
		p.down = true
		return transitionDown
	}
	return transitionNone
}

// PrintWith lets callers override the receipt builder — useful in tests and
// in cmd/main.go where the store_config-aware builder is wired in.
func (s *Service) PrintWith(ctx context.Context, snap order.Snapshot, build ReceiptBuilder) error {
	name := s.printer.Name()
	nameAttr := attribute.String("printer.name", name)

	ctx, span := s.tracer.Start(ctx, "printer.print",
		trace.WithAttributes(
			nameAttr,
			attribute.String("order.id", snap.ID),
			attribute.String("order.fulfillment_type", snap.Fulfillment.Type),
		),
	)
	defer span.End()
	done := s.startJob()
	ok := false
	defer func() { done(ok) }()

	s.logger.Info().Ctx(ctx).Str("order_id", snap.ID).Msg("Detecting printer availability before print")
	if err := s.printer.Detect(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "detect failed")
		s.meters.OrdersPrinted.Add(ctx, 1, metric.WithAttributes(nameAttr, attribute.String("result", "detect_failed")))
		s.logger.Error().Ctx(ctx).Err(err).Str("order_id", snap.ID).Msg("Printer not reachable, skipping print")
		// Tag the error so the dispatcher can classify a whole-order print
		// failure as "no_printer" (unreachable) vs "write_error". errors.Is
		// still resolves the underlying detect error for existing callers.
		return fmt.Errorf("%w: %w", ErrPrinterUnreachable, err)
	}

	s.logger.Debug().Ctx(ctx).Str("order_id", snap.ID).Msg("Building receipt commands")
	commands := build(snap)
	span.SetAttributes(attribute.Int("receipt.bytes", len(commands)))

	start := time.Now()
	err := s.printer.Print(ctx, printer.PrintJob{Commands: commands})
	elapsedMs := float64(time.Since(start).Microseconds()) / 1000.0
	s.meters.PrinterWriteMs.Record(ctx, elapsedMs, metric.WithAttributes(nameAttr))

	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "print failed")
		s.meters.OrdersPrinted.Add(ctx, 1, metric.WithAttributes(nameAttr, attribute.String("result", "error")))
		s.logger.Error().Ctx(ctx).Err(err).Str("order_id", snap.ID).Msg("Failed to print receipt")
		return err
	}

	s.meters.OrdersPrinted.Add(ctx, 1, metric.WithAttributes(nameAttr, attribute.String("result", "ok")))
	s.logger.Info().Ctx(ctx).Str("order_id", snap.ID).Msg("Receipt printed successfully")
	ok = true
	return nil
}

// PrintBytes prints a prepared ESC/POS job that isn't an order (a driver
// report): detect, then write. label names the job in logs and spans.
func (s *Service) PrintBytes(ctx context.Context, label string, commands []byte) error {
	nameAttr := attribute.String("printer.name", s.printer.Name())
	ctx, span := s.tracer.Start(ctx, "printer.print_document",
		trace.WithAttributes(nameAttr, attribute.String("job", label), attribute.Int("receipt.bytes", len(commands))))
	defer span.End()
	done := s.startJob()
	ok := false
	defer func() { done(ok) }()
	if err := s.printer.Detect(ctx); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "detect failed")
		return fmt.Errorf("%w: %w", ErrPrinterUnreachable, err)
	}
	if err := s.printer.Print(ctx, printer.PrintJob{Commands: commands}); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "print failed")
		return err
	}
	s.logger.Info().Ctx(ctx).Str("job", label).Msg("Document printed")
	ok = true
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
// actions the user just took. Returns the Detect error (nil = reachable).
func (s *Service) ProbeNow(ctx context.Context) error {
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
	if s.onProbe != nil {
		s.onProbe(ctx, s.printer.IP(), up)
	}

	if err != nil {
		span.RecordError(err)
		s.logger.Warn().Ctx(ctx).Err(err).Msg("ProbeNow: printer unreachable")
		return err
	}
	s.logger.Info().Ctx(ctx).Msg("ProbeNow: printer reachable")
	return nil
}
