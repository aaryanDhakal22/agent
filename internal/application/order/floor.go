package order

import (
	"context"
	"errors"
	"fmt"
	"quiccpos/agent/internal/application/devlog"
	"strings"
	"time"
	"unicode/utf8"

	"quiccpos/agent/internal/domain/order"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Floor flow after accept: ready, driver runs, batches and the driver roster.
// The repo writes each change + its floor event in one transaction; the
// service then wakes the sync sender and tells the tablet / web UI over SSE.

// PrepChoices are the prep-time quick picks offered at accept (minutes).
var PrepChoices = []int{10, 15, 20, 30, 45, 60}

// Floor is the summary the tablet loads on start: the open batch, the kitchen
// counter and the prep defaults.
type Floor struct {
	Batch           order.Batch `json:"batch"`
	KitchenNext     int64       `json:"kitchen_next"`
	LastPrepMinutes int64       `json:"last_prep_minutes"`
	PrepChoices     []int       `json:"prep_choices"`
	UnsyncedEvents  int64       `json:"unsynced_events"`
	// DriverResetTime is when drivers are clocked out daily (location HH:MM).
	DriverResetTime string `json:"driver_reset_time"`
	// GreenTillMinutes: tickets stay green this long after arrival (then
	// shade to red at 2x). TipTaxPercent is taken off drivers' tips.
	GreenTillMinutes int64   `json:"green_till_minutes"`
	TipTaxPercent    float64 `json:"tip_tax_percent"`
	// AUTO delivery settings (2.0.9).
	SoonReadyMinutes      int64 `json:"soon_ready_minutes"`
	DeliveryTargetMinutes int64 `json:"delivery_target_minutes"`
	// The nearby rule (2.2.0): detour | distance | bundle | off + its limits.
	NearbyRule          string  `json:"nearby_rule"`
	NearbyDetourMinutes int64   `json:"nearby_detour_minutes"`
	NearbyDistanceMi    float64 `json:"nearby_distance_mi"`
	// AutoAssign (2.2.1): the next idle driver gets ready deliveries pooled.
	AutoAssign bool `json:"auto_assign"`
	// Store is the store pin (null until main geocoded it); Map the store's
	// offline map file on this Pi (null until downloaded).
	Store *FloorStore `json:"store"`
	Map   *FloorMap   `json:"map"`
	// 2.1.0: the feature toggles, kitchen-ticket switches and default times,
	// and the store hub's status.
	Settings    order.IntegrationSettings `json:"settings"`
	Integration order.IntegrationView     `json:"integration"`
}

func (s *Service) span(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return s.tracer.Start(ctx, name, trace.WithAttributes(attrs...))
}

func spanErr(span trace.Span, err error) error {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return err
}

// Floor returns the floor summary.
func (s *Service) Floor(ctx context.Context) (*Floor, error) {
	b, err := s.repo.OpenBatch(ctx)
	if err != nil {
		return nil, err
	}
	batches, err := s.repo.ListBatches(ctx, 1)
	if err == nil && len(batches) == 1 && batches[0].ID == b.ID {
		b.OrderCount = batches[0].OrderCount
	}
	fs, err := s.repo.FloorSettings(ctx)
	if err != nil {
		return nil, err
	}
	n, _ := s.repo.CountUnsyncedEvents(ctx)
	rs, _ := s.repo.GetDriverReset(ctx)
	prefs := s.FloorPrefs(ctx)
	dp := s.DeliveryPrefs(ctx)
	store, mp := s.floorGeo()
	return &Floor{Batch: *b, KitchenNext: fs.KitchenNext, LastPrepMinutes: fs.LastPrepMinutes,
		PrepChoices: PrepChoices, UnsyncedEvents: n, DriverResetTime: rs.Time,
		GreenTillMinutes: prefs.GreenTillMinutes, TipTaxPercent: float64(prefs.TipTaxBps) / 100,
		SoonReadyMinutes: dp.SoonReadyMinutes, DeliveryTargetMinutes: dp.DeliveryTargetMinutes,
		NearbyRule: dp.NearbyRule, NearbyDetourMinutes: dp.NearbyDetourMinutes, NearbyDistanceMi: dp.NearbyDistanceMi,
		AutoAssign: dp.AutoAssign, Store: store, Map: mp, Settings: s.IntegrationSettings(), Integration: s.IntegrationView(time.Now())}, nil
}

// Ready marks an accepted order ready (terminal for pickup / dine-in).
func (s *Service) Ready(ctx context.Context, id string) (*order.StoredOrder, error) {
	ctx, span := s.span(ctx, "order.ready", attribute.String("order.id", id))
	defer span.End()
	so, err := s.repo.MarkReady(ctx, id)
	if err != nil {
		return nil, spanErr(span, err)
	}
	s.floorChanged()
	s.publishUpdated(*so)
	s.devEvent(devlog.Info, devlog.KindOrder, "%s ready", label(*so))
	if so.IsDelivery() {
		s.autoAssignBG(ctx, "ready")
	}
	return so, nil
}

// AssignDriver sends ready delivery orders out with a driver.
func (s *Service) AssignDriver(ctx context.Context, driverID string, orderIDs []string) ([]order.StoredOrder, error) {
	ctx, span := s.span(ctx, "delivery.assign", attribute.String("driver.id", driverID), attribute.Int("orders", len(orderIDs)))
	defer span.End()
	out, err := s.repo.AssignDriver(ctx, driverID, orderIDs)
	if err != nil {
		return nil, spanErr(span, err)
	}
	s.devEvent(devlog.Info, devlog.KindOrder, "Out on a run: %s", labels(out))
	s.floorChanged()
	for _, so := range out {
		s.publishUpdated(so)
	}
	for _, so := range s.resequencePools(ctx, nil) { // sent orders left their pools
		s.publishUpdated(so)
	}
	s.publishDrivers(ctx)
	s.publishDispatched(ctx, driverID, out)
	return out, nil
}

// DriverReturned marks a driver's out orders delivered and requeues them.
func (s *Service) DriverReturned(ctx context.Context, driverID string) ([]order.StoredOrder, error) {
	out, _, err := s.ReturnDriver(ctx, driverID)
	return out, err
}

// ReturnDriver is DriverReturned plus the simple auto-assign it triggers
// (2.2.1): the order ids pooled to the next driver on this call (nil when
// none).
func (s *Service) ReturnDriver(ctx context.Context, driverID string) ([]order.StoredOrder, []string, error) {
	ctx, span := s.span(ctx, "delivery.returned", attribute.String("driver.id", driverID))
	defer span.End()
	out, err := s.repo.DriverReturned(ctx, driverID)
	if err != nil {
		return nil, nil, spanErr(span, err)
	}
	if len(out) > 0 {
		s.devEvent(devlog.Info, devlog.KindOrder, "Delivered: %s", labels(out))
	}
	s.floorChanged()
	for _, so := range out {
		s.publishUpdated(so)
	}
	assigned := s.autoAssign(ctx, "returned")
	s.publishDrivers(ctx)
	return out, assigned, nil
}

// CloseBatch closes the open batch (see order.FloorRepository.CloseBatch).
func (s *Service) CloseBatch(ctx context.Context, force bool) (*order.Batch, []order.StoredOrder, error) {
	ctx, span := s.span(ctx, "batch.close", attribute.Bool("force", force))
	defer span.End()
	closed, finished, err := s.repo.CloseBatch(ctx, force)
	if err != nil {
		var lo *order.LeftoversError
		if errors.As(err, &lo) {
			return nil, lo.Orders, err // not a failure — needs confirmation
		}
		return nil, nil, spanErr(span, err)
	}
	s.floorChanged()
	s.logger.Info().Ctx(ctx).Int64("batch", closed.ID).Int("auto_finished", len(finished)).Msg("Batch closed")
	for _, so := range finished {
		s.publishUpdated(so)
	}
	if s.broker != nil {
		opened, err := s.repo.OpenBatch(ctx)
		if err == nil {
			s.broker.PublishBatchClosed(*closed, *opened)
		}
	}
	s.publishDrivers(ctx)
	s.devEvent(devlog.Info, devlog.KindOrder, "Batch %d closed (%d auto-finished)", closed.ID, len(finished))
	s.printReportsAfterClose(ctx, closed.ID, s.afterClose)
	s.forgetPOSDeliveries(ctx, closed.ID)
	return closed, finished, nil
}

// OnBatchClosed registers the hook run after a successful Close batch, once
// its driver reports printed (or failed): cmd/main.go's Pi auto-update.
func (s *Service) OnBatchClosed(fn func(ctx context.Context)) { s.onBatchClosed.Store(&fn) }

func (s *Service) afterClose(ctx context.Context) {
	if fn := s.onBatchClosed.Load(); fn != nil && *fn != nil {
		(*fn)(ctx)
	}
}

// forgetPOSDeliveries tells main to drop the points of a closed batch's POS
// deliveries (it would expire them in 24 h anyway).
func (s *Service) forgetPOSDeliveries(ctx context.Context, batchID int64) {
	const page = 500
	for off := 0; ; off += page {
		list, err := s.repo.ListByBatch(ctx, batchID, off, page)
		if err != nil {
			s.logger.Warn().Ctx(ctx).Err(err).Msg("close batch: list POS deliveries failed")
			return
		}
		for _, so := range list {
			if so.IsPOS() && so.IsDelivery() {
				s.geoForget(ctx, so.Order.ID)
			}
		}
		if len(list) < page {
			return
		}
	}
}

// ListBatches returns the open + kept closed batches, newest first.
func (s *Service) ListBatches(ctx context.Context) ([]order.Batch, error) {
	return s.repo.ListBatches(ctx, 10)
}

// ListByBatch lists one batch's orders newest-first (batchID 0 = open batch).
func (s *Service) ListByBatch(ctx context.Context, batchID int64, offset, limit int) ([]order.StoredOrder, error) {
	if batchID == 0 {
		b, err := s.repo.OpenBatch(ctx)
		if err != nil {
			return nil, err
		}
		batchID = b.ID
	}
	return s.repo.ListByBatch(ctx, batchID, offset, limit)
}

// --- drivers ---------------------------------------------------------------

func (s *Service) ListDrivers(ctx context.Context) ([]order.Driver, error) {
	return s.repo.ListDrivers(ctx)
}

func cleanName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > 40 {
		return "", fmt.Errorf("%w: driver name must be 1-40 characters", ErrBadRequest)
	}
	return name, nil
}

// cleanPosNumber checks a driver number ("" = none): 1-12 letters or digits.
func cleanPosNumber(v string) (string, error) {
	v = strings.TrimSpace(v)
	if len(v) > 12 {
		return "", fmt.Errorf("%w: driver number must be at most 12 characters", ErrBadRequest)
	}
	for _, r := range v {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return "", fmt.Errorf("%w: driver number must be letters and digits only", ErrBadRequest)
		}
	}
	return v, nil
}

// CreateDriver adds a driver (posNumber "" = no POS number).
func (s *Service) CreateDriver(ctx context.Context, name, posNumber string) (*order.Driver, error) {
	name, err := cleanName(name)
	if err != nil {
		return nil, err
	}
	if posNumber, err = cleanPosNumber(posNumber); err != nil {
		return nil, err
	}
	d, err := s.repo.CreateDriver(ctx, name, posNumber)
	if err != nil {
		return nil, err
	}
	s.driversChanged(ctx)
	return d, nil
}

// UpdateDriver renames a driver and/or sets their POS number ("" clears);
// nil leaves a field as is.
func (s *Service) UpdateDriver(ctx context.Context, id string, name, posNumber *string) error {
	if name == nil && posNumber == nil {
		return fmt.Errorf("%w: give a name or a pos_number", ErrBadRequest)
	}
	var clean string
	var err error
	if posNumber != nil {
		if clean, err = cleanPosNumber(*posNumber); err != nil {
			return err
		}
	}
	if name != nil {
		n, err := cleanName(*name)
		if err != nil {
			return err
		}
		if err := s.repo.RenameDriver(ctx, id, n); err != nil {
			return err
		}
	}
	if posNumber != nil {
		if err := s.repo.SetDriverPosNumber(ctx, id, clean); err != nil {
			return err
		}
	}
	s.driversChanged(ctx)
	return nil
}

func (s *Service) RenameDriver(ctx context.Context, id, name string) error {
	name, err := cleanName(name)
	if err != nil {
		return err
	}
	if err := s.repo.RenameDriver(ctx, id, name); err != nil {
		return err
	}
	s.driversChanged(ctx)
	return nil
}

func (s *Service) RemoveDriver(ctx context.Context, id string) error {
	unpooled, err := s.repo.RemoveDriver(ctx, id)
	if err != nil {
		return err
	}
	s.publishAll(unpooled)
	s.driversChanged(ctx)
	return nil
}

func (s *Service) ReorderDrivers(ctx context.Context, ids []string) error {
	if err := s.repo.ReorderDrivers(ctx, ids); err != nil {
		return err
	}
	s.driversChanged(ctx)
	return nil
}

// ClockIn puts a driver into the queue (at the back) and opens their shift.
func (s *Service) ClockIn(ctx context.Context, id string) (*order.Driver, error) {
	d, err := s.repo.ClockIn(ctx, id)
	if err != nil {
		return nil, err
	}
	s.driversChanged(ctx)
	s.autoAssignBG(ctx, "clock_in")
	return d, nil
}

// ClockOut takes an available driver out of the queue and closes the shift.
func (s *Service) ClockOut(ctx context.Context, id string) (*order.Driver, error) {
	d, unpooled, err := s.repo.ClockOut(ctx, id)
	if err != nil {
		return nil, err
	}
	s.publishAll(unpooled)
	s.driversChanged(ctx)
	return d, nil
}

// ListShifts returns driver shifts that started since `since` (newest first).
func (s *Service) ListShifts(ctx context.Context, since time.Time) ([]order.Shift, error) {
	return s.repo.ListShifts(ctx, since)
}

func (s *Service) GetDriverReset(ctx context.Context) (order.DriverReset, error) {
	return s.repo.GetDriverReset(ctx)
}

// SetDriverResetTime sets the daily clock-out time ("HH:MM", location time).
func (s *Service) SetDriverResetTime(ctx context.Context, hhmm string) error {
	if _, err := time.Parse("15:04", hhmm); err != nil || len(hhmm) != 5 {
		return fmt.Errorf("%w: time must be HH:MM (24h)", ErrBadRequest)
	}
	if err := s.repo.SetDriverResetTime(ctx, hhmm); err != nil {
		return err
	}
	// Moving the time to one that already passed today must not fire a reset
	// right now: count the latest scheduled one as done. Not "now" — a driver
	// out on a run (clocked in before now) would then be clocked out when
	// they return.
	hm, _ := time.Parse("15:04", hhmm)
	return s.repo.MarkDriverReset(ctx, lastScheduledReset(time.Now(), hm, s.tz()))
}

// SetStoreTZ gives the service the location's timezone (from store_config).
func (s *Service) SetStoreTZ(tz func() *time.Location) { s.storeTZ.Store(&tz) }

func (s *Service) tz() *time.Location {
	if fn := s.storeTZ.Load(); fn != nil && *fn != nil {
		if l := (*fn)(); l != nil {
			return l
		}
	}
	return time.Local
}

// lastScheduledReset is the most recent hm (local wall time in loc) at or
// before now.
func lastScheduledReset(now, hm time.Time, loc *time.Location) time.Time {
	local := now.In(loc)
	due := time.Date(local.Year(), local.Month(), local.Day(), hm.Hour(), hm.Minute(), 0, 0, loc)
	if local.Before(due) {
		due = due.AddDate(0, 0, -1)
	}
	return due
}

// DriverResetDue runs the daily auto clock-out once it's past today's reset
// time (in loc) and it hasn't run since. Called on a ticker; restart-safe
// (the last run is stored).
func (s *Service) DriverResetDue(ctx context.Context, now time.Time, loc *time.Location) (int, error) {
	rs, err := s.repo.GetDriverReset(ctx)
	if err != nil {
		return 0, err
	}
	hm, err := time.Parse("15:04", rs.Time)
	if err != nil {
		return 0, nil // bad setting: never auto-reset rather than reset at a wrong time
	}
	if rs.LastRun == nil {
		// First check after install/upgrade: count the latest scheduled reset
		// as done instead of clocking everyone out mid-service because
		// "today's 10:00 passed" (and not "now": drivers clocked in during
		// setup would be clocked out on their first return).
		return 0, s.repo.MarkDriverReset(ctx, lastScheduledReset(now, hm, loc))
	}
	local := now.In(loc)
	due := time.Date(local.Year(), local.Month(), local.Day(), hm.Hour(), hm.Minute(), 0, 0, loc)
	if local.Before(due) || !rs.LastRun.Before(due) {
		return 0, nil
	}
	n, unpooled, err := s.repo.AutoClockOut(ctx, now)
	if err != nil {
		return 0, err
	}
	s.publishAll(unpooled)
	s.logger.Info().Ctx(ctx).Int("drivers", n).Int("unpooled", len(unpooled)).Str("reset_time", rs.Time).Msg("Daily driver clock-out")
	if n > 0 {
		s.driversChanged(ctx)
	}
	return n, nil
}

func (s *Service) driversChanged(ctx context.Context) {
	s.floorChanged()
	s.publishDrivers(ctx)
}

func (s *Service) publishDrivers(ctx context.Context) {
	if s.broker == nil {
		return
	}
	ds, err := s.repo.ListDrivers(ctx)
	if err != nil {
		s.logger.Warn().Ctx(ctx).Err(err).Msg("list drivers for SSE failed")
		return
	}
	s.broker.PublishDrivers(ds)
}

// --- escalation ------------------------------------------------------------

// EscalateOverdue alerts (Pushover + SSE order.escalated) for every order
// still arrived after `after`, at most once per `after` per order. Run on a
// ticker by application/escalation; state survives restarts (last_alert_at).
func (s *Service) EscalateOverdue(ctx context.Context, now time.Time, after time.Duration) (int, error) {
	overdue, err := s.repo.ListOverdueArrived(ctx, now.Add(-after))
	if err != nil {
		return 0, err
	}
	for _, so := range overdue {
		waiting := now.Sub(so.Status.ArrivalDate)
		if err := s.repo.MarkAlerted(ctx, so.Order.ID, now); err != nil {
			return 0, err
		}
		who := so.Order.Customer.DisplayName
		if who == "" {
			who = "(no name)"
		}
		msg := fmt.Sprintf("NOT ACCEPTED for %d min: %s order #%s for %s",
			int(waiting.Minutes()), formatServiceType(so.Order.Fulfillment.Type), so.Order.SourceEventID, who)
		s.logger.Warn().Ctx(ctx).Str("order_id", so.Order.ID).
			Msgf("Order %s not accepted for %d min", devlog.Short(so.Order.ID), int(waiting.Minutes()))
		if s.notifier != nil {
			if err := s.notifier.SendFloor(ctx, msg, "siren"); err != nil {
				s.logger.Warn().Ctx(ctx).Err(err).Msg("Pushover escalation failed (non-fatal)")
			}
		}
		if s.broker != nil {
			s.broker.PublishEscalated(so, int64(waiting.Seconds()))
		}
		s.meters.OrdersEscalated.Add(ctx, 1)
	}
	return len(overdue), nil
}
