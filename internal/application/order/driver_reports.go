package order

import (
	"context"
	"errors"
	"fmt"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/pricing"
	"quiccpos/agent/internal/domain/printer"

	"go.opentelemetry.io/otel/attribute"
)

// Driver reports any time (2.3.0): one driver's so far, printed from the
// Drivers tab, at every clock-out and at Close batch (reports.go).

// ErrPrintingUnavailable: the Pi can't print documents (no printers wired).
var ErrPrintingUnavailable = errors.New("driver report printing is not configured")

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

// driverReports builds a batch's driver reports finished with the tip tax.
// ensure (may be nil) gets a report even with no deliveries.
func (s *Service) driverReports(ctx context.Context, batchID int64, ensure *order.Driver) ([]order.DriverReport, error) {
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
	out := reps
	tax := s.FloorPrefs(ctx).TipTaxBps
	pay := s.DriverPayCents()
	for i := range out {
		out[i].Finish(tax, pay)
	}
	return out, nil
}

// SetDriverPay wires the store's per-delivery driver pay (the counter
// menu's pricing settings, delivery_driver_cents); nil = the default $3.
func (s *Service) SetDriverPay(fn func() (int64, bool)) { s.driverPay.Store(&fn) }

// DriverPayCents is the driver's pay for one delivered delivery.
func (s *Service) DriverPayCents() int64 {
	if fn := s.driverPay.Load(); fn != nil {
		if c, ok := (*fn)(); ok {
			return max(c, 0)
		}
	}
	return pricing.DefaultDriverShareCents
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

// DriverReport is one driver's report for the open batch so far
// (GET /api/drivers/{id}/report).
func (s *Service) DriverReport(ctx context.Context, driverID string) (*order.DriverReport, error) {
	d, err := s.activeDriver(ctx, driverID)
	if err != nil {
		return nil, err
	}
	reps, err := s.driverReports(ctx, 0, d)
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
// (PRINT REPORT on the Drivers tab, and every clock-out): the slip on the
// receipt printer with failover.
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
	rep, err := s.DriverReport(ctx, driverID)
	if err != nil {
		return nil, spanErr(span, err)
	}
	r := *rep
	res, perr := s.printDoc(ctx, "driver-report", func(on printer.Paper) []byte { return s.buildReport(r, on) })
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
		Int("deliveries", rep.Deliveries).Msg("Driver report printed")
	return out, nil
}

// ReportPrinted is SSE driver.report_printed: how a background driver-report
// print (clock-out) went.
type ReportPrinted struct {
	DriverID     string `json:"driver_id"`
	DriverName   string `json:"driver_name"`
	Trigger      string `json:"trigger"` // clock_out
	Printed      bool   `json:"printed"`
	PrintedOn    string `json:"printed_on,omitempty"`
	ReroutedFrom string `json:"rerouted_from,omitempty"`
	Error        string `json:"error,omitempty"`
	Code         string `json:"code,omitempty"` // nothing_printed | report_failed
}

// PrintDriverReportInBackground prints a driver's report without the caller
// waiting (clock-out): the print runs in the background, and the outcome goes to the tablets as SSE
// driver.report_printed (a slip that printed nowhere also pages the floor).
// False when this Pi can't print documents (nothing happens).
func (s *Service) PrintDriverReportInBackground(ctx context.Context, d order.Driver, trigger string) bool {
	if !s.CanPrintReports() {
		return false
	}
	s.background(ctx, func(ctx context.Context) {
		ev := ReportPrinted{DriverID: d.ID, DriverName: d.Name, Trigger: trigger}
		res, err := s.PrintDriverReport(ctx, d.ID, trigger)
		switch {
		case err != nil:
			s.logger.Warn().Ctx(ctx).Err(err).Str("driver", d.Name).Msg("driver report in the background failed")
			ev.Error, ev.Code = err.Error(), "report_failed"
			s.alertFloor(ctx, fmt.Sprintf("Driver report NOT PRINTED for %s (%s): %v", d.Name, trigger, err))
		case !res.Printed():
			ev.DriverName, ev.Error, ev.Code = res.DriverName, res.Error, "nothing_printed"
		default:
			ev.DriverName, ev.Printed, ev.PrintedOn, ev.ReroutedFrom = res.DriverName, true, res.PrintedOn, res.ReroutedFrom
		}
		if s.broker != nil {
			s.broker.Publish("driver.report_printed", ev)
		}
	})
	return true
}
