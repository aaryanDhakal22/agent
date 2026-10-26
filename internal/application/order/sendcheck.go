package order

import (
	"context"
	"math"
	"sort"
	"strings"
	"time"

	"quiccpos/agent/internal/application/dispatch"
	"quiccpos/agent/internal/domain/order"

	"go.opentelemetry.io/otel/attribute"
)

// Far apart, send the first and hold the other (2.2.1). With the store's
// nearby rule on, an order that is not nearby the run being built — its
// stops taken oldest-placed first, starting with the oldest — is better left
// for a driver who is out, when his estimated return still gets it to the
// door by placed + delivery_target_minutes. AUTO applies it inside the plan
// (dispatch.HoldRule: reason held_for_returning_driver); Send Out asks GET
// /api/drivers/{id}/send-check first and may leave those orders behind
// (exclude_order_ids). The check never blocks anything.

// holdRule is AUTO's hold rule from the delivery settings (nil when the
// nearby rule is off).
func holdRule(p order.DeliveryPrefs) *dispatch.HoldRule {
	switch p.NearbyRule {
	case order.NearbyDetour, order.NearbyDistance, order.NearbyBundle:
		return &dispatch.HoldRule{Rule: p.NearbyRule, DetourMinutes: float64(p.NearbyDetourMinutes),
			DistanceM: p.NearbyDistanceMi * metersPerMile}
	}
	return nil
}

// SendCheck is GET /api/drivers/{id}/send-check's answer.
type SendCheck struct {
	Hold []SendHold `json:"hold"`
}

// SendHold is one order better held for a returning driver.
type SendHold struct {
	OrderID string           `json:"order_id"`
	Address string           `json:"address"`
	HeldFor dispatch.HeldFor `json:"held_for"`
}

// SendCheck looks at the orders that would go out with the driver (orderIDs,
// default the driver's pool) and lists the ones to hold for a returning
// driver. Empty when the nearby rule is off, no driver is out, or all fits.
func (s *Service) SendCheck(ctx context.Context, driverID string, orderIDs []string) (*SendCheck, error) {
	ctx, span := s.span(ctx, "delivery.send_check", attribute.String("driver.id", driverID))
	defer span.End()
	res := &SendCheck{Hold: []SendHold{}}
	now := time.Now()
	drivers, err := s.repo.ListDrivers(ctx)
	if err != nil {
		return nil, spanErr(span, err)
	}
	found := false
	for _, d := range drivers {
		found = found || d.ID == driverID
	}
	if !found {
		return nil, spanErr(span, order.ErrDriverNotFound)
	}
	prefs := s.DeliveryPrefs(ctx)
	if holdRule(prefs) == nil {
		return res, nil
	}
	open, err := s.repo.ListOpenDeliveries(ctx)
	if err != nil {
		return nil, spanErr(span, err)
	}

	// The returning driver: the earliest estimated return of a driver out.
	outRuns := map[string][]order.StoredOrder{}
	for _, so := range open {
		if so.Status.State == order.StateOnDelivery {
			outRuns[so.Status.DriverID] = append(outRuns[so.Status.DriverID], so)
		}
	}
	var back *dispatch.HeldFor
	for _, d := range drivers {
		if d.Status != order.DriverOut || d.ID == driverID {
			continue
		}
		at := s.estimateReturn(ctx, outRuns[d.ID], now).UTC()
		if back == nil || at.Before(back.ReturnAt) {
			back = &dispatch.HeldFor{DriverID: d.ID, Name: d.Name, ReturnAt: at}
		}
	}
	if back == nil {
		return res, nil
	}

	want := map[string]bool{}
	for _, id := range orderIDs {
		want[id] = true
	}
	var run []order.StoredOrder
	for _, so := range open {
		if so.Status.State != order.StateAccepted && so.Status.State != order.StateReady {
			continue
		}
		if (len(want) > 0 && want[so.Order.ID]) || (len(want) == 0 && so.Status.PoolDriverID == driverID) {
			run = append(run, so)
		}
	}
	if len(run) < 2 {
		return res, nil
	}
	sort.SliceStable(run, func(i, j int) bool {
		a, b := planOrder(run[i]), planOrder(run[j])
		if !a.PlacedAt.Equal(b.PlacedAt) {
			return a.PlacedAt.Before(b.PlacedAt)
		}
		return a.ID < b.ID
	})
	lt := s.legTimes(ctx, run)
	target := time.Duration(prefs.DeliveryTargetMinutes) * time.Minute
	kept := []order.StoredOrder{run[0]}
	for _, c := range run[1:] {
		if !c.Geo.Located() || !s.inTimeFor(lt, c, back.ReturnAt, target) || s.nearRun(ctx, now, prefs, driverID, kept, c) {
			kept = append(kept, c)
			continue
		}
		res.Hold = append(res.Hold, SendHold{OrderID: c.Order.ID, Address: addressLine(c), HeldFor: *back})
	}
	span.SetAttributes(attribute.Int("hold", len(res.Hold)))
	return res, nil
}

// inTimeFor: leaving when the driver is back (or when it's ready, if later)
// still gets it to the door by placed + target.
func (s *Service) inTimeFor(lt legTimes, so order.StoredOrder, back time.Time, target time.Duration) bool {
	to, ok := lt.sec("", so.Order.ID)
	if !ok {
		return false
	}
	o := planOrder(so)
	leave := back
	if !o.Ready && o.ReadyAt.After(leave) {
		leave = o.ReadyAt
	}
	door := leave.Add(time.Duration(math.Round(to)) * time.Second)
	return !door.After(o.PlacedAt.Add(target))
}

// nearRun is the nearby rule of c against the kept stops (the 2.2.0 tests:
// detour, distance, or AUTO for this driver alone bundling them).
func (s *Service) nearRun(ctx context.Context, now time.Time, p order.DeliveryPrefs, driverID string, kept []order.StoredOrder, c order.StoredOrder) bool {
	cands := []order.StoredOrder{c}
	switch p.NearbyRule {
	case order.NearbyDetour:
		return len(s.nearbyDetour(ctx, kept, cands, float64(p.NearbyDetourMinutes)*60)) > 0
	case order.NearbyDistance:
		return len(nearbyDistance(kept, cands, p.NearbyDistanceMi*metersPerMile)) > 0
	case order.NearbyBundle:
		return s.bundles(ctx, now, driverID, kept, c)
	}
	return true
}

// bundles: AUTO, for this driver alone with these orders as if ready and
// unpooled, puts c on the run with the oldest kept stop.
func (s *Service) bundles(ctx context.Context, now time.Time, driverID string, kept []order.StoredOrder, c order.StoredOrder) bool {
	list := append(append([]order.StoredOrder(nil), kept...), c)
	in := dispatch.Input{Now: now.UTC(), TargetMinutes: int(s.DeliveryPrefs(ctx).DeliveryTargetMinutes), BudgetMs: nearbyBudgetMs,
		Drivers: []dispatch.Driver{{ID: driverID}},
		Times:   dispatch.Times{Store: dispatch.StoreTimes{To: map[string]int{}, From: map[string]int{}}, Pairs: map[string]map[string]int{}}}
	if store, _ := s.floorGeo(); store != nil {
		in.Store = &dispatch.LatLng{Lat: store.Lat, Lng: store.Lng}
	}
	ids := make([]string, 0, len(list))
	for _, so := range list {
		o := planOrder(so)
		o.Ready, o.PoolDriverID = true, ""
		in.Orders = append(in.Orders, o)
		ids = append(ids, so.Order.ID)
		if to, from, ok := storeTimes(so.Geo); ok {
			in.Times.Store.To[so.Order.ID], in.Times.Store.From[so.Order.ID] = to, from
		}
	}
	if pairs, err := s.repo.DriveTimes(ctx, ids); err == nil {
		for a, row := range pairs {
			m := make(map[string]int, len(row))
			for b, sec := range row {
				m[b] = int(sec)
			}
			in.Times.Pairs[a] = m
		}
	}
	for _, r := range dispatch.Plan(in).Runs {
		withFirst, withC := false, false
		for _, id := range r.OrderIDs {
			withFirst = withFirst || id == kept[0].Order.ID
			withC = withC || id == c.Order.ID
		}
		if withC {
			return withFirst
		}
	}
	return false
}

// addressLine is an order's street address ("1634 Gleneagle Rd, Apt 2").
func addressLine(so order.StoredOrder) string {
	a := so.Order.Fulfillment.Address
	if a == nil {
		return ""
	}
	line := strings.TrimSpace(a.Line1)
	if l2 := strings.TrimSpace(a.Line2); l2 != "" {
		line += ", " + l2
	}
	return line
}
