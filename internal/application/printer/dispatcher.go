package printer

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
	"quiccpos/agent/internal/domain/storeconfig"
	"quiccpos/agent/internal/infra/notify"
	"quiccpos/agent/internal/observability"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// PrinterFactory builds a printer.Printer for a given (name, ip). Injected at
// construction so tests can swap in a fake instead of touching the network.
type PrinterFactory func(name, ip string) printer.Printer

// ReceiptBuilder renders one order into ESC/POS bytes (Service.PrintWith).
type ReceiptBuilder func(snap order.Snapshot) []byte

// TicketBuilder is the seam between the dispatcher and the receipt formatter:
// it renders one planned Ticket (full receipt, customer receipt, or a kitchen
// station ticket) for an order. cmd/main.go injects one that reads the
// agent's live store_context.
type TicketBuilder func(snap order.Snapshot, t printer.Ticket) []byte

// Dispatcher owns the runtime printer roster (keyed by store_config printer
// id, so a printer rename in /app/store can't break routing). The roster is
// replaced whole-cloth whenever main delivers a fresh store_config.
//
// Execute splits an order into tickets (Plan: one per kitchen station + the
// customer receipt, or one full receipt for unrouted orders) and prints each,
// failing over to another printer when the intended one is unreachable.
type Dispatcher struct {
	mu        sync.RWMutex
	roster    []rosterSlot
	byID      map[string]rosterSlot
	cancelFns []context.CancelFunc

	factory         PrinterFactory
	registry        *Registry
	notifier        *notify.Notifier
	logger          zerolog.Logger
	meters          observability.Meters
	keepCheckParent context.Context
	printerDelay    time.Duration
	build           TicketBuilder
}

// rosterSlot pairs a configured printer with its live Service.
type rosterSlot struct {
	entry RosterEntry
	svc   *Service
}

func NewDispatcher(
	parentCtx context.Context,
	factory PrinterFactory,
	registry *Registry,
	notifier *notify.Notifier,
	keepCheckDelay time.Duration,
	build TicketBuilder,
	logger zerolog.Logger,
	meters observability.Meters,
) *Dispatcher {
	return &Dispatcher{
		factory:         factory,
		registry:        registry,
		notifier:        notifier,
		logger:          logger.With().Str("module", "printer-dispatcher").Logger(),
		meters:          meters,
		keepCheckParent: parentCtx,
		printerDelay:    keepCheckDelay,
		build:           build,
		byID:            make(map[string]rosterSlot),
	}
}

// Apply rebuilds the roster from a fresh store_config payload. Each call:
//
//  1. Cancels the previous KeepCheck goroutines.
//  2. Spins up a Service per printer in the new payload.
//  3. Starts a fresh KeepCheck loop per Service.
//
// Idempotent — calling Apply twice with the same payload is harmless aside
// from the (cheap) re-probe each new KeepCheck does on entry.
func (d *Dispatcher) Apply(ctx context.Context, printers []storeconfig.PrinterContext) {
	d.mu.Lock()
	defer d.mu.Unlock()

	// Stop the previous KeepCheck goroutines so they don't double up.
	for _, cancel := range d.cancelFns {
		cancel()
	}
	d.cancelFns = d.cancelFns[:0]

	d.roster = d.roster[:0]
	d.byID = make(map[string]rosterSlot, len(printers))

	d.logger.Info().Ctx(ctx).Int("count", len(printers)).Msg("Applying new printer roster from store_config")

	names := make([]string, 0, len(printers))
	for _, p := range printers {
		names = append(names, p.Name)
	}
	d.registry.Retain(names)

	for _, p := range printers {
		dev := d.factory(p.Name, p.IP)
		svc := NewService(dev, d.registry, d.logger, d.meters)
		slot := rosterSlot{entry: RosterEntry{ID: p.ID, Name: p.Name, IsReceipt: p.IsReceipt}, svc: svc}
		d.roster = append(d.roster, slot)
		d.byID[p.ID] = slot

		childCtx, cancel := context.WithCancel(d.keepCheckParent)
		d.cancelFns = append(d.cancelFns, cancel)
		go svc.KeepCheck(childCtx, d.printerDelay, d.notifier)
	}
}

// Execute plans and prints an order's tickets (see Plan). Each ticket goes to
// its intended printer; if that fails, it fails over — the receipt printer
// first, then every other printer in roster order — with a "REROUTED from X"
// banner. It returns an error only when NOTHING printed; partial failures are
// reported in the Outcome (and counted in orders.print.failed) so the caller
// can alert without un-accepting an order whose kitchen tickets did print.
func (d *Dispatcher) Execute(ctx context.Context, snap order.Snapshot) (printer.Outcome, error) {
	// Bound the whole job: with the network down every candidate times out,
	// and staff are waiting on the accept.
	ctx, cancel := context.WithTimeout(ctx, printBudget)
	defer cancel()
	d.mu.RLock()
	roster := append([]rosterSlot(nil), d.roster...)
	d.mu.RUnlock()

	if len(roster) == 0 {
		d.meters.OrdersPrintFailed.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "no_printer")))
		return printer.Outcome{}, ErrNoPrintersAvailable
	}
	entries := make([]RosterEntry, len(roster))
	byID := make(map[string]rosterSlot, len(roster))
	for i, sl := range roster {
		entries[i] = sl.entry
		byID[sl.entry.ID] = sl
	}

	tickets, unrouted := Plan(snap, entries)
	out := printer.Outcome{Unrouted: unrouted}
	var lastErr error
	allUnreachable := true
	for _, t := range tickets {
		res, err := d.printTicket(ctx, snap, t, roster, byID)
		if err != nil {
			lastErr = err
			if !errors.Is(err, ErrPrinterUnreachable) {
				allUnreachable = false
			}
			out.Failed = append(out.Failed, res)
			continue
		}
		out.Printed = append(out.Printed, res)
	}

	if len(out.Failed) > 0 {
		reason := "ticket_failed" // some tickets printed, some printed nowhere
		if len(out.Printed) == 0 {
			reason = "write_error"
			if allUnreachable {
				reason = "no_printer"
			}
		}
		d.meters.OrdersPrintFailed.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", reason)))
	}
	if len(out.Printed) == 0 {
		if lastErr == nil {
			return out, ErrNoPrintersAvailable
		}
		return out, fmt.Errorf("nothing printed; last error: %w", lastErr)
	}
	return out, nil
}

// printTicket prints one ticket on its target, failing over if needed.
func (d *Dispatcher) printTicket(ctx context.Context, snap order.Snapshot, t printer.Ticket, roster []rosterSlot, byID map[string]rosterSlot) (printer.TicketResult, error) {
	res := printer.TicketResult{Kind: t.Kind, Station: t.Station}
	target, ok := byID[t.TargetID]
	if !ok {
		res.Err = "printer not in roster"
		return res, ErrNoPrintersAvailable
	}

	// Candidates: the target, then the receipt printer, then the rest — with
	// fallbacks the health check last saw down moved to the very end (still
	// tried: the probe may be stale), so a dead station doesn't cost a dial
	// timeout per ticket.
	var up, down []rosterSlot
	for _, sl := range roster {
		if sl.entry.ID == target.entry.ID {
			continue
		}
		if st, ok := d.registry.Get(sl.entry.Name); ok && st.LastChecked != nil && !st.Up {
			down = append(down, sl)
		} else {
			up = append(up, sl)
		}
	}
	receiptFirst := func(xs []rosterSlot) []rosterSlot {
		out := make([]rosterSlot, 0, len(xs))
		for _, sl := range xs {
			if sl.entry.IsReceipt {
				out = append(out, sl)
			}
		}
		for _, sl := range xs {
			if !sl.entry.IsReceipt {
				out = append(out, sl)
			}
		}
		return out
	}
	candidates := append([]rosterSlot{target}, receiptFirst(up)...)
	candidates = append(candidates, receiptFirst(down)...)

	var lastErr error
	for i, sl := range candidates {
		if ctx.Err() != nil {
			lastErr = fmt.Errorf("print time budget used up: %w", ctx.Err())
			break
		}
		tk := t
		if i > 0 {
			tk.ReroutedFrom = target.entry.Name
		}
		err := sl.svc.PrintWith(ctx, snap, func(s order.Snapshot) []byte { return d.build(s, tk) })
		if err == nil {
			res.PrintedOn = sl.entry.Name
			res.ReroutedFrom = tk.ReroutedFrom
			if i > 0 {
				d.logger.Warn().Ctx(ctx).Str("order_id", snap.ID).Str("intended", target.entry.Name).
					Str("printed_on", sl.entry.Name).Str("kind", string(t.Kind)).Msg("ticket rerouted — intended printer failed")
			}
			return res, nil
		}
		lastErr = err
		d.logger.Warn().Ctx(ctx).Err(err).Str("order_id", snap.ID).Str("printer", sl.entry.Name).
			Str("kind", string(t.Kind)).Msg("printer failed for ticket; trying next")
	}
	res.Err = lastErr.Error()
	return res, lastErr
}

// printBudget caps one order's printing (all tickets, all failovers).
const printBudget = 25 * time.Second

// ErrNoPrintersAvailable is returned by Execute when the agent has no printers
// configured. The order service treats it like any nothing-printed failure:
// the accept is reverted, the order stays arrived, staff retry once a printer
// is added in /app/store.
var ErrNoPrintersAvailable = fmt.Errorf("no printers configured")

// ErrPrinterUnreachable wraps a printer.Detect failure in Service.PrintWith so
// the Dispatcher can classify a whole-order print failure as "no_printer"
// (unreachable) rather than "write_error". Purely for telemetry — control flow
// and HTTP status mapping are unchanged (callers only match ErrNotFound /
// ErrAlreadyAccepted / ErrNoPrintersAvailable).
var ErrPrinterUnreachable = errors.New("printer unreachable")
