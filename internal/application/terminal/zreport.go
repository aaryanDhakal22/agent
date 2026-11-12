package terminal

import (
	"context"
	"errors"
	"fmt"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/terminal"
)

// The Z report (contract/agent.md §2.10l): Close batch (by hand, auto-close
// or forced) closes the open drawers and queues the batch's Z report in its
// transaction; the Pi builds it right after from the closed batch, keeps it
// and sends it to main as z.report. Never printed (the owner).

// SSE event when a Z report is built.
const EventZReport = "terminal.z.report"

// ZRepo is the Z reports' storage (repositories.OrderRepository).
type ZRepo interface {
	PendingZReports(ctx context.Context) ([]terminal.ZPending, error)
	ZInputs(ctx context.Context, batchID int64) (terminal.ZInput, error)
	SaveZReport(ctx context.Context, z terminal.Z, at time.Time) (bool, error)
	ZReport(ctx context.Context, batchID int64) (*terminal.Z, error)
	ZReports(ctx context.Context, limit int) ([]terminal.Z, error)
}

// ErrZUnavailable: the storage keeps no Z reports (never in the agent).
var ErrZUnavailable = errors.New("z reports aren't available")

func (s *Service) zrepo() ZRepo {
	if z, ok := s.repo.(ZRepo); ok {
		return z
	}
	return nil
}

// SetZDrivers wires the driver reports a Z report carries (orderApp's
// DriverReports; nil = none).
func (s *Service) SetZDrivers(fn func(ctx context.Context, batchID int64) ([]order.DriverReport, error)) {
	s.zDrivers = fn
}

// BuildPendingZ builds every queued Z report (after Close batch, and at
// start for one a crash left pending). Errors are logged; a report that
// failed stays pending for the next call.
func (s *Service) BuildPendingZ(ctx context.Context) {
	repo := s.zrepo()
	if repo == nil {
		return
	}
	s.zMu.Lock()
	defer s.zMu.Unlock()
	pend, err := repo.PendingZReports(ctx)
	if err != nil {
		s.logger.Warn().Ctx(ctx).Err(err).Msg("z report: list pending failed")
		return
	}
	for _, p := range pend {
		if _, err := s.buildZ(ctx, repo, p); err != nil {
			s.logger.Error().Ctx(ctx).Err(err).Int64("batch", p.BatchID).Msg("z report: build failed (kept pending)")
		}
	}
}

func (s *Service) buildZ(ctx context.Context, repo ZRepo, p terminal.ZPending) (*terminal.Z, error) {
	in, err := repo.ZInputs(ctx, p.BatchID)
	if err != nil {
		return nil, err
	}
	in.ID, in.CloseKind, in.Forced = p.ID, p.CloseKind, p.Forced
	in.Loc = s.orders.StoreLocation()
	in.Now = s.now()
	if s.zDrivers != nil {
		reps, err := s.zDrivers(ctx, p.BatchID)
		if err != nil {
			return nil, fmt.Errorf("driver reports: %w", err)
		}
		in.Drivers = reps
	}
	z := terminal.BuildZ(in)
	if z.Reconcile.DifferenceCents != 0 || z.Reconcile.DrawersDifferences != 0 {
		// Never expected: the money identity is checked by the property test.
		s.logger.Error().Ctx(ctx).Int64("batch", z.BatchID).Int64("difference_cents", z.Reconcile.DifferenceCents).
			Int64("drawers_difference_cents", z.Reconcile.DrawersDifferences).Msg("z report doesn't balance")
	}
	saved, err := repo.SaveZReport(ctx, z, in.Now)
	if err != nil {
		return nil, err
	}
	if saved {
		s.orders.JournalWritten() // wakes the floor sync (z.report)
		s.publish(EventZReport, map[string]any{"batch_id": z.BatchID, "report": z})
		s.logger.Info().Ctx(ctx).Int64("batch", z.BatchID).Int64("orders", z.Counts.Orders).Int64("total_cents", z.Sales.TotalCents).
			Msg("Z report built")
		s.orders.DevEvent(fmt.Sprintf("Z report for batch %d: %d orders", z.BatchID, z.Counts.Orders))
	}
	return &z, nil
}

// ZReport is a closed batch's Z report (nil = none yet).
func (s *Service) ZReport(ctx context.Context, batchID int64) (*terminal.Z, error) {
	repo := s.zrepo()
	if repo == nil {
		return nil, ErrZUnavailable
	}
	return repo.ZReport(ctx, batchID)
}

// ZReports are the Pi's kept Z reports, newest first.
func (s *Service) ZReports(ctx context.Context, limit int) ([]terminal.Z, error) {
	repo := s.zrepo()
	if repo == nil {
		return nil, ErrZUnavailable
	}
	return repo.ZReports(ctx, min(max(limit, 1), 50))
}

// AfterBatchClose is orderApp's OnClosed hook: the drawers Close batch closed
// are announced (terminal.drawer.changed) and the Z report is built.
func (s *Service) AfterBatchClose(ctx context.Context, closed order.Batch) {
	if repo := s.drawers(); repo != nil {
		for _, id := range closed.ClosedDrawers {
			if d, err := repo.DrawerByID(ctx, id); err == nil {
				s.publish(EventDrawerChanged, map[string]any{"drawer": d, "device_id": d.DeviceID})
			}
		}
	}
	s.BuildPendingZ(ctx)
}
