package order

import (
	"context"

	"quiccpos/agent/internal/domain/order"

	"go.opentelemetry.io/otel/attribute"
)

// PrintBatchDriverReport prints one driver's report slip of a batch (0 =
// the open batch) on the receipt printer with failover - the terminal's
// Manager tab (contract §2.10m). order.ErrNotFound when the driver has no
// report in that batch.
func (s *Service) PrintBatchDriverReport(ctx context.Context, batchID int64, driverID string) (*ReportPrint, error) {
	ctx, span := s.span(ctx, "batch.driver_report.print", attribute.Int64("batch.id", batchID))
	defer span.End()
	if !s.CanPrintReports() {
		return nil, spanErr(span, ErrPrintingUnavailable)
	}
	reps, err := s.DriverReports(ctx, batchID)
	if err != nil {
		return nil, spanErr(span, err)
	}
	for _, rep := range reps {
		if rep.DriverID != driverID {
			continue
		}
		rep := rep
		res, _ := s.printDoc(ctx, "driver-report", func(from string) []byte { return s.buildReport(rep, from) })
		return &ReportPrint{DriverName: rep.DriverName, TicketResult: res}, nil
	}
	return nil, spanErr(span, order.ErrNotFound)
}
