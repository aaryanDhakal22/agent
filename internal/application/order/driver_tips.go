package order

import (
	"context"
	"sync/atomic"

	"quiccpos/agent/internal/domain/order"
)

// Card tips the driver types before Close batch (contract/agent.md §2.10am,
// batch F, the owner 2026-11-14). The tips live in the driver_tips table;
// the floor counts them on every driver report at once (driverReports) and
// asks the terminal service to put them on the orders at the press of Close
// batch (OnClosePress) and, for website card holds, at the driver's
// clock-out (main captures his holds then: OnShiftClosing).

// DriverTipStore reads the typed tips (repositories.OrderRepository).
type DriverTipStore interface {
	DriverTipsOfBatch(ctx context.Context, batchID int64) (map[string]order.DriverTip, error)
}

type driverTipHooks struct {
	store atomic.Pointer[DriverTipStore]
	// press runs at the press of Close batch, before anything closes.
	press atomic.Pointer[func(ctx context.Context)]
	// shift runs before a driver's shift closes ("" = every driver's: the
	// daily auto clock-out).
	shift atomic.Pointer[func(ctx context.Context, driverID string)]
}

// SetDriverTips wires the typed tips into the driver reports (cmd/main.go).
func (s *Service) SetDriverTips(st DriverTipStore) { s.drvTips.store.Store(&st) }

// OnClosePress registers what runs at the press of Close batch (every
// trigger: Quibase / web, a terminal, the auto-close), before the close is
// checked or done: the terminal service applies the typed tips.
func (s *Service) OnClosePress(fn func(ctx context.Context)) { s.drvTips.press.Store(&fn) }

// OnShiftClosing registers what runs before a driver clocks out (driverID;
// "" = the daily auto clock-out of everyone): his typed tips on website card
// orders go on before main captures them.
func (s *Service) OnShiftClosing(fn func(ctx context.Context, driverID string)) { s.drvTips.shift.Store(&fn) }

func (s *Service) closePressed(ctx context.Context) {
	if fn := s.drvTips.press.Load(); fn != nil && *fn != nil {
		(*fn)(ctx)
	}
}

func (s *Service) shiftClosing(ctx context.Context, driverID string) {
	if fn := s.drvTips.shift.Load(); fn != nil && *fn != nil {
		(*fn)(ctx, driverID)
	}
}

// applyTypedTips puts a batch's typed tips on its reports (before Finish).
// A read error leaves the reports as they are (logged).
func (s *Service) applyTypedTips(ctx context.Context, batchID int64, reps []order.DriverReport) {
	p := s.drvTips.store.Load()
	if p == nil || *p == nil || len(reps) == 0 {
		return
	}
	tips, err := (*p).DriverTipsOfBatch(ctx, batchID)
	if err != nil {
		s.logger.Warn().Ctx(ctx).Err(err).Int64("batch", batchID).Msg("read the typed driver tips failed; reports without them")
		return
	}
	for i := range reps {
		order.ApplyTypedTips(&reps[i], tips)
	}
}
