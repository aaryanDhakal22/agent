package order

import (
	"context"
	"fmt"
	"strings"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"

	"go.opentelemetry.io/otel/attribute"
)

// DocPrinter prints a non-order slip on the receipt printer with failover
// (the printer.Dispatcher's PrintDocument). build gets the intended printer's
// name when the slip went to a fallback.
type DocPrinter = func(ctx context.Context, label string, build func(reroutedFrom string) []byte) (printer.TicketResult, error)

// ReportBuilder renders one finished driver report (receipt.BuildDriverReport
// with the live store context).
type ReportBuilder = func(rep order.DriverReport, reroutedFrom string) []byte

// SetDocumentPrinting wires driver-report printing (cmd/main.go).
func (s *Service) SetDocumentPrinting(print DocPrinter, build ReportBuilder) {
	s.printDoc, s.buildReport = print, build
}

// FloorPrefs returns the store's floor settings (defaults on a read error).
func (s *Service) FloorPrefs(ctx context.Context) order.FloorPrefs {
	p, err := s.repo.FloorPrefs(ctx)
	if err != nil {
		s.logger.Warn().Ctx(ctx).Err(err).Msg("read floor prefs failed; using defaults")
		return order.DefaultFloorPrefs
	}
	return p
}

// SetFloorPrefs validates and stores the floor settings (green-till 1–240
// min, tip tax 0–50%) and tells the tablets.
func (s *Service) SetFloorPrefs(ctx context.Context, p order.FloorPrefs) error {
	if p.GreenTillMinutes < 1 || p.GreenTillMinutes > 240 {
		return fmt.Errorf("%w: green_till_minutes must be 1-240", ErrBadRequest)
	}
	if p.TipTaxBps < 0 || p.TipTaxBps > 5000 {
		return fmt.Errorf("%w: tip tax must be 0-50%%", ErrBadRequest)
	}
	if err := s.repo.SetFloorPrefs(ctx, p); err != nil {
		return err
	}
	if s.broker != nil {
		s.broker.PublishFloorChanged()
	}
	return nil
}

// UndoReturn reverts the latest "returned" tap: the driver is out again at
// their old place in the queue (their orders stay delivered).
func (s *Service) UndoReturn(ctx context.Context, driverID string) (*order.Driver, error) {
	ctx, span := s.span(ctx, "delivery.undo_return", attribute.String("driver.id", driverID))
	defer span.End()
	d, err := s.repo.UndoReturn(ctx, driverID, time.Now())
	if err != nil {
		return nil, spanErr(span, err)
	}
	s.driversChanged(ctx)
	return d, nil
}

// DriverReports returns the finished driver reports of a batch (0 = open).
func (s *Service) DriverReports(ctx context.Context, batchID int64) ([]order.DriverReport, error) {
	reps, err := s.repo.DriverReports(ctx, batchID)
	if err != nil {
		return nil, err
	}
	tax := s.FloorPrefs(ctx).TipTaxBps
	for i := range reps {
		reps[i].Finish(tax)
	}
	return reps, nil
}

// ReportPrint is the outcome of printing one driver's slip.
type ReportPrint struct {
	DriverName string `json:"driver_name"`
	printer.TicketResult
}

// PrintDriverReports prints one slip per driver for a batch (0 = open) and
// returns what printed where. It errors only when the reports can't be built
// or document printing isn't wired; per-slip failures are in the results.
func (s *Service) PrintDriverReports(ctx context.Context, batchID int64) ([]ReportPrint, error) {
	ctx, span := s.span(ctx, "batch.driver_reports.print", attribute.Int64("batch.id", batchID))
	defer span.End()
	if s.printDoc == nil || s.buildReport == nil {
		return nil, spanErr(span, fmt.Errorf("driver report printing is not configured"))
	}
	reps, err := s.DriverReports(ctx, batchID)
	if err != nil {
		return nil, spanErr(span, err)
	}
	out := make([]ReportPrint, 0, len(reps))
	var failed []string
	for _, rep := range reps {
		rep := rep
		res, err := s.printDoc(ctx, "driver-report", func(from string) []byte { return s.buildReport(rep, from) })
		if err != nil {
			failed = append(failed, rep.DriverName)
		}
		out = append(out, ReportPrint{DriverName: rep.DriverName, TicketResult: res})
	}
	span.SetAttributes(attribute.Int("reports", len(reps)), attribute.Int("failed", len(failed)))
	if len(failed) > 0 {
		s.alertFloor(ctx, fmt.Sprintf("Driver report NOT PRINTED for %s (batch %d) — reprint from the tablet's Batch tab", strings.Join(failed, ", "), batchID))
	}
	return out, nil
}

// printReportsAfterClose prints the closed batch's driver reports in the
// background (the close-batch response doesn't wait on the printer).
func (s *Service) printReportsAfterClose(ctx context.Context, batchID int64) {
	if s.printDoc == nil || s.buildReport == nil {
		return
	}
	go func(ctx context.Context) {
		res, err := s.PrintDriverReports(ctx, batchID)
		if err != nil {
			s.logger.Error().Ctx(ctx).Err(err).Int64("batch", batchID).Msg("driver reports after close failed")
			s.alertFloor(ctx, fmt.Sprintf("Driver reports for batch %d could not be printed: %v", batchID, err))
			return
		}
		s.logger.Info().Ctx(ctx).Int64("batch", batchID).Int("reports", len(res)).Msg("Driver reports printed after close")
	}(context.WithoutCancel(ctx))
}

// alertFloor sends a floor-problem Pushover in the background.
func (s *Service) alertFloor(ctx context.Context, msg string) {
	s.logger.Warn().Ctx(ctx).Msg(msg)
	if s.notifier == nil {
		return
	}
	go func(ctx context.Context) {
		if err := s.notifier.SendFloor(ctx, msg, "siren"); err != nil {
			s.logger.Warn().Ctx(ctx).Err(err).Msg("Pushover alert failed (non-fatal)")
		}
	}(context.WithoutCancel(ctx))
}
