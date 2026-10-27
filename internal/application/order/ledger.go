package order

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"quiccpos/agent/internal/domain/order"

	"go.opentelemetry.io/otel/attribute"
)

// The POS driver ledger (2.2.3, see domain/order/ledger.go): the hub PUTs it
// every ~15 s; a report about to print asks for a fresh one through the feed
// and waits a few seconds for it. In memory only.

var (
	// LedgerWait is how long a print waits for the hub's fresh ledger.
	LedgerWait = 5 * time.Second
	// LedgerFreshFor: an older ledger isn't used (the report says the POS
	// wasn't checked).
	LedgerFreshFor = 90 * time.Second
	// ledgerAskEvery: one driver_ledger.requested per this, however many
	// reports print.
	ledgerAskEvery = 2 * time.Second
)

// ErrPrintingUnavailable: the Pi can't print documents (no printers wired).
var ErrPrintingUnavailable = errors.New("driver report printing is not configured")

// ledgerState is the last ledger the hub sent.
type ledgerState struct {
	mu       sync.Mutex
	ledger   *order.DriverLedger
	at       time.Time     // when it arrived (Pi clock)
	asked    time.Time     // the last driver_ledger.requested
	arrived  chan struct{} // closed + replaced on every PUT
	received int64         // PUTs since start
}

// SetDriverLedger stores the hub's ledger (PUT /api/integration/driver-ledger).
func (s *Service) SetDriverLedger(ctx context.Context, l order.DriverLedger) error {
	if err := l.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrBadRequest, err)
	}
	now := time.Now()
	s.ledger.mu.Lock()
	s.ledger.ledger, s.ledger.at = &l, now
	s.ledger.received++
	if s.ledger.arrived != nil {
		close(s.ledger.arrived)
	}
	s.ledger.arrived = make(chan struct{})
	s.ledger.mu.Unlock()
	s.logger.Debug().Ctx(ctx).Str("day", l.OperationDay).Int("checks", len(l.Checks)).Msg("POS driver ledger received")
	return nil
}

// ledgerNow returns the last ledger and when it arrived (nil = none yet).
func (s *Service) ledgerNow() (*order.DriverLedger, time.Time) {
	s.ledger.mu.Lock()
	defer s.ledger.mu.Unlock()
	return s.ledger.ledger, s.ledger.at
}

// RefreshLedger asks the hub for a fresh ledger and waits up to LedgerWait
// for one that arrives after the ask. Nothing is asked while the hub isn't
// connected, nor from a hub that never sent a ledger since the Pi started
// (an older hub: it would never answer) — the report then says the POS
// wasn't checked, or uses the last ledger while it is fresh. Returns
// whether a fresh one arrived.
func (s *Service) RefreshLedger(ctx context.Context) bool {
	s.ledger.mu.Lock()
	sends := s.ledger.received > 0
	s.ledger.mu.Unlock()
	if !sends || !s.IntegrationView(time.Now()).Connected {
		return false
	}
	ctx, span := s.span(ctx, "driver_ledger.refresh")
	defer span.End()
	s.ledger.mu.Lock()
	if s.ledger.arrived == nil {
		s.ledger.arrived = make(chan struct{})
	}
	start := time.Now()
	ask := start.Sub(s.ledger.asked) >= ledgerAskEvery
	if ask {
		s.ledger.asked = start
	}
	// A ledger that arrived after the latest ask will do (two slips a second
	// apart share one answer).
	want := s.ledger.asked
	s.ledger.mu.Unlock()
	if ask {
		if err := s.repo.AppendFeed(ctx, order.FeedDriverLedgerRequested, start, map[string]any{"at": start.UTC()}); err != nil {
			s.logger.Warn().Ctx(ctx).Err(err).Msg("ask the hub for a driver ledger failed")
			return false
		}
		s.feedNotify()
	}
	deadline := time.NewTimer(LedgerWait)
	defer deadline.Stop()
	for {
		s.ledger.mu.Lock()
		at, ch := s.ledger.at, s.ledger.arrived
		s.ledger.mu.Unlock()
		if !at.Before(want) {
			span.SetAttributes(attribute.Bool("fresh", true), attribute.Int64("wait_ms", time.Since(start).Milliseconds()))
			return true
		}
		select {
		case <-ch:
		case <-deadline.C:
			span.SetAttributes(attribute.Bool("fresh", false))
			s.logger.Info().Ctx(ctx).Msg("no fresh POS driver ledger within the wait; using the last one if recent")
			return false
		case <-ctx.Done():
			return false
		}
	}
}

// ledgerFor says how the ledger can be used for a batch window.
func (s *Service) ledgerFor(from time.Time, to *time.Time, now time.Time) (led *order.DriverLedger, asOf *time.Time, state, note string) {
	l, at := s.ledgerNow()
	v := s.IntegrationView(now)
	if l == nil && v.LastSeenAt == nil {
		return nil, nil, order.PosNone, "" // no POS hub at this store
	}
	if l == nil {
		if !v.Connected {
			return nil, nil, order.PosMissing, "POS link down"
		}
		return nil, nil, order.PosMissing, "no POS data from the hub yet"
	}
	t := at
	asOf = &t
	if now.Sub(at) > LedgerFreshFor {
		return nil, asOf, order.PosStale, "POS data too old (hub offline?)"
	}
	if !l.Covers(from, to, now) {
		return nil, asOf, order.PosOtherDay, "POS day " + l.OperationDay + " is another day"
	}
	return l, asOf, order.PosOK, ""
}

// batchOf returns a batch (0 = the open one).
func (s *Service) batchOf(ctx context.Context, id int64) (*order.Batch, error) {
	if id == 0 {
		return s.repo.OpenBatch(ctx)
	}
	bs, err := s.repo.ListBatches(ctx, 1000)
	if err != nil {
		return nil, err
	}
	for i := range bs {
		if bs[i].ID == id {
			return &bs[i], nil
		}
	}
	return nil, order.ErrNotFound
}

// mergedReports builds a batch's driver reports merged with the POS ledger
// (when the store has one) and finished with the tip tax. ensure (may be
// nil) gets a report even with no deliveries.
func (s *Service) mergedReports(ctx context.Context, batchID int64, ensure *order.Driver) ([]order.DriverReport, error) {
	reps, err := s.repo.DriverReports(ctx, batchID)
	if err != nil {
		return nil, err
	}
	b, err := s.batchOf(ctx, batchID)
	if err != nil {
		return nil, err
	}
	if ensure != nil && !hasReport(reps, ensure.ID) {
		reps = append(reps, order.DriverReport{BatchID: b.ID, BatchOpenedAt: b.OpenedAt, BatchClosedAt: b.ClosedAt,
			DriverID: ensure.ID, DriverName: ensure.Name, Lines: []order.DriverReportLine{}})
	}
	now := time.Now()
	led, asOf, state, note := s.ledgerFor(b.OpenedAt, b.ClosedAt, now)
	in := order.MergeInput{Reports: reps, BatchID: b.ID, BatchOpenedAt: b.OpenedAt, BatchClosedAt: b.ClosedAt,
		Ledger: led, LedgerAsOf: asOf, State: state, StaleNote: note}
	if state != order.PosNone {
		if in.Drivers, err = s.repo.ListDrivers(ctx); err != nil {
			return nil, err
		}
	}
	if state == order.PosOK {
		orders, err := s.repo.ListByBatch(ctx, b.ID, 0, 100000)
		if err != nil {
			return nil, err
		}
		in.POS = map[string]order.StoredOrder{}
		for _, so := range orders {
			if so.IsPOS() && so.Status.ExtRef != "" {
				in.POS[so.Status.ExtRef] = so
			}
		}
	}
	out := order.MergeDriverReports(in)
	tax := s.FloorPrefs(ctx).TipTaxBps
	for i := range out {
		out[i].Finish(tax)
	}
	return out, nil
}

func hasReport(reps []order.DriverReport, driverID string) bool {
	for _, r := range reps {
		if r.DriverID == driverID {
			return true
		}
	}
	return false
}

// activeDriver finds a driver on the roster (ErrDriverNotFound).
func (s *Service) activeDriver(ctx context.Context, id string) (*order.Driver, error) {
	ds, err := s.repo.ListDrivers(ctx)
	if err != nil {
		return nil, err
	}
	for i := range ds {
		if ds[i].ID == id {
			return &ds[i], nil
		}
	}
	return nil, order.ErrDriverNotFound
}

// DriverReport is one driver's report for the open batch so far, merged
// with the POS ledger as it is now (GET /api/drivers/{id}/report).
func (s *Service) DriverReport(ctx context.Context, driverID string) (*order.DriverReport, error) {
	d, err := s.activeDriver(ctx, driverID)
	if err != nil {
		return nil, err
	}
	reps, err := s.mergedReports(ctx, 0, d)
	if err != nil {
		return nil, err
	}
	for i := range reps {
		if reps[i].DriverID == d.ID {
			return &reps[i], nil
		}
	}
	return nil, order.ErrDriverNotFound
}

// DriverReportPrint is one driver's slip printed (or not).
type DriverReportPrint struct {
	DriverID     string              `json:"driver_id"`
	DriverName   string              `json:"driver_name"`
	PrintedOn    string              `json:"printed_on,omitempty"`
	ReroutedFrom string              `json:"rerouted_from,omitempty"`
	Error        string              `json:"error,omitempty"`
	Report       *order.DriverReport `json:"report,omitempty"`
}

// Printed reports whether the slip printed somewhere.
func (p *DriverReportPrint) Printed() bool { return p != nil && p.PrintedOn != "" && p.Error == "" }

// CanPrintReports: document printing is wired (a Pi with printers).
func (s *Service) CanPrintReports() bool { return s.printDoc != nil && s.buildReport != nil }

// PrintDriverReport prints one driver's report for the open batch so far
// (PRINT REPORT on the Drivers tab, and every clock-out): a fresh POS ledger
// first (≤ LedgerWait), then the slip on the receipt printer with failover.
// The error is ErrDriverNotFound / ErrPrintingUnavailable / a build error;
// a slip that printed nowhere is a result with Error set (and a Pushover).
func (s *Service) PrintDriverReport(ctx context.Context, driverID, trigger string) (*DriverReportPrint, error) {
	ctx, span := s.span(ctx, "driver.report.print", attribute.String("driver.id", driverID), attribute.String("trigger", trigger))
	defer span.End()
	if !s.CanPrintReports() {
		return nil, spanErr(span, ErrPrintingUnavailable)
	}
	if _, err := s.activeDriver(ctx, driverID); err != nil {
		return nil, spanErr(span, err)
	}
	s.RefreshLedger(ctx)
	rep, err := s.DriverReport(ctx, driverID)
	if err != nil {
		return nil, spanErr(span, err)
	}
	r := *rep
	res, perr := s.printDoc(ctx, "driver-report", func(from string) []byte { return s.buildReport(r, from) })
	out := &DriverReportPrint{DriverID: rep.DriverID, DriverName: rep.DriverName, PrintedOn: res.PrintedOn,
		ReroutedFrom: res.ReroutedFrom, Report: rep}
	if perr != nil {
		out.Error = res.Err
		if out.Error == "" {
			out.Error = perr.Error()
		}
		span.SetAttributes(attribute.Bool("printed", false))
		s.alertFloor(ctx, fmt.Sprintf("Driver report NOT PRINTED for %s (%s) — print it again from the Drivers tab", rep.DriverName, trigger))
		return out, nil
	}
	span.SetAttributes(attribute.Bool("printed", true), attribute.Int("lines", len(rep.Lines)))
	s.logger.Info().Ctx(ctx).Str("driver", rep.DriverName).Str("trigger", trigger).Str("printer", res.PrintedOn).
		Int("deliveries", rep.Deliveries).Bool("pos_checked", rep.POS != nil && rep.POS.Checked).Msg("Driver report printed")
	return out, nil
}

// ledgerNote is a short line for logs about the POS state of reports.
func ledgerNote(reps []order.DriverReport) string {
	var parts []string
	for _, r := range reps {
		for _, l := range r.Lines {
			if l.Match == order.MatchPOSOnly {
				parts = append(parts, r.DriverName+" "+l.ExtRef)
			}
		}
	}
	return strings.Join(parts, ", ")
}
