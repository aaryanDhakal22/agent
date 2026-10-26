package order

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"quiccpos/agent/internal/application/dispatch"
	"quiccpos/agent/internal/domain/order"

	"go.opentelemetry.io/otel/attribute"
)

// The "nearby" rule (2.2.0, GET /api/deliveries/next-check): before staff
// send the next driver out, is an unassigned delivery near his run? The
// candidates are the deliveries AUTO would consider — in nobody's pool, not
// out, ready or ready within soon_ready_minutes, located — and the run is the
// driver's pool:
//
//   - detour:   inserting it into his run (store -> stops -> store, the
//     cheapest place) adds <= nearby_detour_minutes of driving;
//   - distance: it is <= nearby_distance_mi (straight line) from a stop
//     already in his pool;
//   - bundle:   AUTO, run for this driver alone with his pool kept, would
//     add it to his run;
//   - off:      never.
//
// Drive times are the ones AUTO uses: main's (order.geo store times + the
// pairs), else the offline straight-line estimate (trip.go).

// NextCheck is GET /api/deliveries/next-check's answer.
type NextCheck struct {
	DriverID       *string  `json:"driver_id"`
	Rule           string   `json:"rule"`
	NearbyOrderIDs []string `json:"nearby_order_ids"`
	Reason         string   `json:"reason"`
}

// nearbyBudgetMs bounds the bundle rule's AUTO run (one driver: tiny).
const nearbyBudgetMs = 300

// metersPerMile converts the distance rule's miles.
const metersPerMile = 1609.344

// nearbyHit is one nearby order with what made it nearby.
type nearbyHit struct {
	so    order.StoredOrder
	score float64 // detour seconds / meters; lower = nearer
	near  order.StoredOrder
}

// NextCheck answers whether an unassigned delivery is near the next
// available driver's run (driverID "" = the next in queue order).
func (s *Service) NextCheck(ctx context.Context, driverID string) (*NextCheck, error) {
	ctx, span := s.span(ctx, "delivery.next_check", attribute.String("driver.id", driverID))
	defer span.End()
	now := time.Now()
	prefs := s.DeliveryPrefs(ctx)
	res := &NextCheck{Rule: prefs.NearbyRule, NearbyOrderIDs: []string{}}

	drivers, err := s.repo.ListDrivers(ctx)
	if err != nil {
		return nil, spanErr(span, err)
	}
	var drv *order.Driver
	for i := range drivers {
		d := &drivers[i]
		if (driverID == "" && d.Status == order.DriverAvailable) || (driverID != "" && d.ID == driverID) {
			drv = d
			break
		}
	}
	if drv == nil {
		return res, nil
	}
	id := drv.ID
	res.DriverID = &id
	if drv.Status != order.DriverAvailable || prefs.NearbyRule == order.NearbyOff {
		return res, nil
	}

	open, err := s.repo.ListOpenDeliveries(ctx)
	if err != nil {
		return nil, spanErr(span, err)
	}
	soon := now.Add(time.Duration(prefs.SoonReadyMinutes) * time.Minute)
	var pool, cands []order.StoredOrder
	for _, so := range open {
		if (so.Status.State != order.StateAccepted && so.Status.State != order.StateReady) || so.Status.CameBack {
			continue
		}
		switch so.Status.PoolDriverID {
		case drv.ID:
			pool = append(pool, so)
		case "":
			if so.Geo.Located() && readySoon(so, soon) {
				cands = append(cands, so)
			}
		}
	}
	if len(pool) == 0 || len(cands) == 0 {
		return res, nil
	}
	if seqIntact(pool) {
		pool = sortBySeq(pool)
	} else {
		pool = s.orderByTrip(ctx, pool)
	}

	var hits []nearbyHit
	switch prefs.NearbyRule {
	case order.NearbyDetour:
		hits = s.nearbyDetour(ctx, pool, cands, float64(prefs.NearbyDetourMinutes)*60)
	case order.NearbyDistance:
		hits = nearbyDistance(pool, cands, prefs.NearbyDistanceMi*metersPerMile)
	case order.NearbyBundle:
		hits, err = s.nearbyBundle(ctx, now, drv.ID, cands)
		if err != nil {
			return nil, spanErr(span, err)
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score < hits[j].score })
	for _, h := range hits {
		res.NearbyOrderIDs = append(res.NearbyOrderIDs, h.so.Order.ID)
	}
	if len(hits) > 0 {
		res.Reason = nearbyReason(prefs.NearbyRule, hits, drv.Name)
	}
	span.SetAttributes(attribute.String("nearby.rule", prefs.NearbyRule), attribute.Int("nearby.orders", len(hits)))
	return res, nil
}

// readySoon: ready, or estimated ready by soon (AUTO's candidate rule).
func readySoon(so order.StoredOrder, soon time.Time) bool {
	o := planOrder(so)
	return o.Ready || (!o.ReadyAt.IsZero() && !o.ReadyAt.After(soon))
}

func sortBySeq(pool []order.StoredOrder) []order.StoredOrder {
	out := append([]order.StoredOrder(nil), pool...)
	sort.SliceStable(out, func(i, j int) bool { return seqLess(out[i], out[j]) })
	return out
}

// orderByTrip puts the pool in the fastest driving order (located first).
func (s *Service) orderByTrip(ctx context.Context, pool []order.StoredOrder) []order.StoredOrder {
	by := map[string]order.StoredOrder{}
	for _, so := range pool {
		by[so.Order.ID] = so
	}
	out := make([]order.StoredOrder, 0, len(pool))
	for _, id := range s.localTrip(ctx, pool).OrderIDs {
		out = append(out, by[id])
	}
	return out
}

// legTimes are drive seconds between the store (node "") and orders, from
// main's times where known, else the straight-line estimate.
type legTimes struct {
	store tripPoint
	pts   map[string]tripPoint
	geo   map[string]*order.OrderGeo
	pairs map[string]map[string]int64
}

func (s *Service) legTimes(ctx context.Context, list []order.StoredOrder) legTimes {
	lt := legTimes{pts: map[string]tripPoint{}, geo: map[string]*order.OrderGeo{}}
	if st, _ := s.floorGeo(); st != nil {
		lt.store = tripPoint{st.Lat, st.Lng, true}
	}
	ids := make([]string, 0, len(list))
	for _, so := range list {
		if so.Geo.Located() {
			lt.pts[so.Order.ID] = tripPoint{*so.Geo.Lat, *so.Geo.Lng, true}
			lt.geo[so.Order.ID] = so.Geo
			ids = append(ids, so.Order.ID)
		}
	}
	if len(ids) > 1 {
		if p, err := s.repo.DriveTimes(ctx, ids); err == nil {
			lt.pairs = p
		}
	}
	return lt
}

// sec is the drive time a -> b ("" = the store); ok=false when unknown.
func (lt legTimes) sec(a, b string) (float64, bool) {
	switch {
	case a == "" && b == "":
		return 0, true
	case a == "":
		if to, _, ok := storeTimes(lt.geo[b]); ok {
			return float64(to), true
		}
		if lt.store.ok {
			return haversineM(lt.store, lt.pts[b]) * roadFactor / estSpeedMps, true
		}
		return 0, false
	case b == "":
		if _, from, ok := storeTimes(lt.geo[a]); ok {
			return float64(from), true
		}
		if lt.store.ok {
			return haversineM(lt.pts[a], lt.store) * roadFactor / estSpeedMps, true
		}
		return 0, false
	}
	if v, ok := lt.pairs[a][b]; ok {
		return float64(v), true
	}
	return haversineM(lt.pts[a], lt.pts[b]) * roadFactor / estSpeedMps, true
}

// nearbyDetour: the cheapest insertion of each candidate into the run
// store -> located pool stops (in order) -> store.
func (s *Service) nearbyDetour(ctx context.Context, pool, cands []order.StoredOrder, limitS float64) []nearbyHit {
	lt := s.legTimes(ctx, append(append([]order.StoredOrder(nil), pool...), cands...))
	route := []string{""}
	stops := map[string]order.StoredOrder{}
	for _, so := range pool {
		if so.Geo.Located() {
			route = append(route, so.Order.ID)
			stops[so.Order.ID] = so
		}
	}
	if len(route) == 1 {
		return nil // no stop of his run is located
	}
	route = append(route, "")
	var hits []nearbyHit
	for _, c := range cands {
		best, at := math.Inf(1), -1
		for k := 0; k+1 < len(route); k++ {
			a, b := route[k], route[k+1]
			ac, ok1 := lt.sec(a, c.Order.ID)
			cb, ok2 := lt.sec(c.Order.ID, b)
			ab, ok3 := lt.sec(a, b)
			if !ok1 || !ok2 || !ok3 {
				continue
			}
			if d := ac + cb - ab; d < best {
				best, at = d, k
			}
		}
		// Compared in whole minutes, as the reason states it (70 s = "1 min").
		if at < 0 || math.Round(best/60)*60 > limitS {
			continue
		}
		near := route[at]
		if near == "" {
			near = route[at+1]
		}
		hits = append(hits, nearbyHit{so: c, score: math.Max(best, 0), near: stops[near]})
	}
	return hits
}

// nearbyDistance: the straight line to the closest located pool stop.
func nearbyDistance(pool, cands []order.StoredOrder, limitM float64) []nearbyHit {
	var hits []nearbyHit
	for _, c := range cands {
		cp := tripPoint{*c.Geo.Lat, *c.Geo.Lng, true}
		best := math.Inf(1)
		var near order.StoredOrder
		for _, p := range pool {
			if !p.Geo.Located() {
				continue
			}
			if d := haversineM(cp, tripPoint{*p.Geo.Lat, *p.Geo.Lng, true}); d < best {
				best, near = d, p
			}
		}
		if best <= limitM {
			hits = append(hits, nearbyHit{so: c, score: best, near: near})
		}
	}
	return hits
}

// nearbyBundle: AUTO for this driver alone (his pool kept); the candidates
// it adds to his run, in driving order.
func (s *Service) nearbyBundle(ctx context.Context, now time.Time, driverID string, cands []order.StoredOrder) ([]nearbyHit, error) {
	in, err := s.planInput(ctx, now)
	if err != nil {
		return nil, err
	}
	var only []dispatch.Driver
	for _, d := range in.Drivers {
		if d.ID == driverID {
			only = append(only, d)
		}
	}
	in.Drivers, in.BudgetMs = only, nearbyBudgetMs
	out := dispatch.Plan(*in)
	isCand := map[string]order.StoredOrder{}
	for _, c := range cands {
		isCand[c.Order.ID] = c
	}
	var hits []nearbyHit
	for _, r := range out.Runs {
		if r.DriverID != driverID {
			continue
		}
		for i, id := range r.SuggestedIDs {
			if so, ok := isCand[id]; ok {
				hits = append(hits, nearbyHit{so: so, score: float64(i)})
			}
		}
	}
	return hits, nil
}

// nearbyReason is one short sentence about the nearest hit (no customer
// data: kitchen numbers and the driver's first name).
func nearbyReason(rule string, hits []nearbyHit, driverName string) string {
	h := hits[0]
	name := strings.TrimSpace(driverName)
	if f := strings.Fields(name); len(f) > 0 {
		name = f[0]
	}
	if name == "" {
		name = "the driver"
	}
	more := ""
	if n := len(hits) - 1; n > 0 {
		more = fmt.Sprintf(" (+%d more)", n)
	}
	switch rule {
	case order.NearbyDetour:
		min := int(math.Round(h.score / 60))
		if min < 1 {
			return fmt.Sprintf("%s is on the way on %s's run%s.", label(h.so), name, more)
		}
		return fmt.Sprintf("%s adds %d min to %s's run%s.", label(h.so), min, name, more)
	case order.NearbyDistance:
		return fmt.Sprintf("%s is %.1f mi from %s%s.", label(h.so), h.score/metersPerMile, label(h.near), more)
	case order.NearbyBundle:
		return fmt.Sprintf("AUTO would add %s to %s's run%s.", label(h.so), name, more)
	}
	return ""
}
