package order

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"quiccpos/agent/internal/application/devlog"
	"sort"
	"time"

	"quiccpos/agent/internal/application/dispatch"
	"quiccpos/agent/internal/domain/order"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
)

// Driver pools, Send Out, AUTO plans, order geo and the main geo proxies
// (2.0.9). The repo writes pool changes + their floor events in one tx; the
// service wakes the sync and tells the tablets (order.updated per order).

// Agent -> main geo RPCs (request/reply over POST /store/v1/agent/rpc).
const (
	MsgGeoPin  = "geo.pin"
	MsgGeoTrip = "geo.trip"
)

// ErrNoDrivers: AUTO with no available driver (409 no_drivers).
var ErrNoDrivers = errors.New("no driver is clocked in and available")

// ErrMainOffline: a geo proxy call main didn't answer (503 main_offline).
var ErrMainOffline = errors.New("can't reach the server")

// RPCSend is rpc.Registry.Send (status + reply payload).
type RPCSend func(ctx context.Context, msgType string, payload any) (int, json.RawMessage, error)

// FloorStore is the store pin in /api/floor.
type FloorStore struct {
	Lat             float64 `json:"lat"`
	Lng             float64 `json:"lng"`
	DeliveryRadiusM int64   `json:"delivery_radius_m"`
}

// FloorMap is the store's offline map file as served by this Pi.
type FloorMap struct {
	Version string    `json:"version"`
	URL     string    `json:"url"`
	BBox    []float64 `json:"bbox"`
}

// GeoDeps are the store-config + main hooks the delivery features need
// (cmd/main.go wires them).
type GeoDeps struct {
	// Floor returns the store pin and the downloaded map (nil when unknown).
	Floor func() (*FloorStore, *FloorMap)
	// Send is the agent->main RPC request/reply; Linked says whether there is
	// a store key at all.
	Send   RPCSend
	Linked func() bool
	// Radius is the location's delivery radius from store_config (ok=false
	// before the first handshake); SetRadius patches the in-memory copy
	// after main took a new one (its store_config push follows).
	Radius    func() (int64, bool)
	SetRadius func(int64)
}

// SetGeo wires the store pin / map / main RPC hooks.
func (s *Service) SetGeo(d GeoDeps) { s.geo.Store(&d) }

func (s *Service) geoDeps() GeoDeps {
	if d := s.geo.Load(); d != nil {
		return *d
	}
	return GeoDeps{}
}

func (s *Service) floorGeo() (*FloorStore, *FloorMap) {
	if d := s.geoDeps(); d.Floor != nil {
		return d.Floor()
	}
	return nil, nil
}

func (s *Service) publishAll(list []order.StoredOrder) {
	for _, so := range list {
		s.publishUpdated(so)
	}
	if len(list) > 0 {
		s.floorChanged()
	}
}

// --- settings ----------------------------------------------------------------

// DeliveryPrefs returns the AUTO settings (defaults on a read error).
func (s *Service) DeliveryPrefs(ctx context.Context) order.DeliveryPrefs {
	p, err := s.repo.DeliveryPrefs(ctx)
	if err != nil {
		s.logger.Warn().Ctx(ctx).Err(err).Msg("read delivery prefs failed; using defaults")
		return order.DefaultDeliveryPrefs
	}
	return p
}

// SetDeliveryPrefs validates (soon-ready 0-30, target 15-120) and stores the
// AUTO settings, then tells the tablets (floor.updated).
func (s *Service) SetDeliveryPrefs(ctx context.Context, p order.DeliveryPrefs) error {
	if err := p.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrBadRequest, err)
	}
	if err := s.repo.SetDeliveryPrefs(ctx, p); err != nil {
		return err
	}
	if s.broker != nil {
		s.broker.PublishFloorChanged()
	}
	return nil
}

// --- pools -------------------------------------------------------------------

// Pool puts delivery orders into an available driver's pool (driverID "" =
// back to unassigned). All or nothing.
func (s *Service) Pool(ctx context.Context, ids []string, driverID string) ([]order.StoredOrder, error) {
	ctx, span := s.span(ctx, "delivery.pool", attribute.String("driver.id", driverID), attribute.Int("orders", len(ids)))
	defer span.End()
	out, err := s.repo.PoolOrders(ctx, ids, driverID)
	if err != nil {
		return nil, spanErr(span, err)
	}
	out = latest(out, s.resequencePools(ctx, nil))
	s.publishAll(out)
	return out, nil
}

// latest returns base with each order replaced by its newer copy in newer,
// followed by the newer orders base doesn't have.
func latest(base, newer []order.StoredOrder) []order.StoredOrder {
	idx := map[string]int{}
	out := append([]order.StoredOrder(nil), base...)
	for i, so := range out {
		idx[so.Order.ID] = i
	}
	for _, so := range newer {
		if i, ok := idx[so.Order.ID]; ok {
			out[i] = so
		} else {
			idx[so.Order.ID] = len(out)
			out = append(out, so)
		}
	}
	return out
}

// resequencePools renumbers (pool_seq 1..n) every pool whose numbering is no
// longer 1..n over its members (an order joined, left or moved). prefer gives
// a driving order to put first for a driver (the AUTO plan's run on confirm)
// and forces that driver's renumbering; everything else comes in the fastest
// order from the drive times (orders without a location last, by placed
// time). Returns the orders whose number changed.
func (s *Service) resequencePools(ctx context.Context, prefer map[string][]string) []order.StoredOrder {
	open, err := s.repo.ListOpenDeliveries(ctx)
	if err != nil {
		s.logger.Warn().Ctx(ctx).Err(err).Msg("pool renumbering: list deliveries failed")
		return nil
	}
	pools := map[string][]order.StoredOrder{}
	var drivers []string
	for _, so := range open {
		if so.Status.PoolDriverID == "" || (so.Status.State != order.StateAccepted && so.Status.State != order.StateReady) {
			continue
		}
		if pools[so.Status.PoolDriverID] == nil {
			drivers = append(drivers, so.Status.PoolDriverID)
		}
		pools[so.Status.PoolDriverID] = append(pools[so.Status.PoolDriverID], so)
	}
	sort.Strings(drivers)
	var changed []order.StoredOrder
	for _, d := range drivers {
		run := pools[d]
		if prefer[d] == nil && seqIntact(run) {
			continue
		}
		out, err := s.repo.SetPoolSeq(ctx, d, s.poolOrder(ctx, run, prefer[d]))
		if err != nil {
			s.logger.Warn().Ctx(ctx).Err(err).Str("driver_id", d).Msg("pool renumbering failed")
			continue
		}
		changed = append(changed, out...)
	}
	if len(changed) > 0 {
		s.floorChanged()
	}
	return changed
}

// seqIntact: every order of the pool is numbered and the numbers are 1..n.
func seqIntact(run []order.StoredOrder) bool {
	seen := make([]bool, len(run)+1)
	for _, so := range run {
		q := so.Status.PoolSeq
		if q == nil || *q < 1 || *q > int64(len(run)) || seen[*q] {
			return false
		}
		seen[*q] = true
	}
	return true
}

// poolOrder: the preferred ids that are in the pool (in that order), then
// the rest in the fastest driving order.
func (s *Service) poolOrder(ctx context.Context, run []order.StoredOrder, prefer []string) []string {
	in := map[string]bool{}
	for _, so := range run {
		in[so.Order.ID] = true
	}
	ids := make([]string, 0, len(run))
	used := map[string]bool{}
	for _, id := range prefer {
		if in[id] && !used[id] {
			ids, used[id] = append(ids, id), true
		}
	}
	var rest []order.StoredOrder
	for _, so := range run {
		if !used[so.Order.ID] {
			rest = append(rest, so)
		}
	}
	if len(rest) == 0 {
		return ids
	}
	sort.SliceStable(rest, func(i, j int) bool { return placedAt(rest[i]).Before(placedAt(rest[j])) })
	return append(ids, s.localTrip(ctx, rest).OrderIDs...)
}

func placedAt(so order.StoredOrder) time.Time {
	if !so.Order.PlacedAt.IsZero() {
		return so.Order.PlacedAt
	}
	return so.Status.ArrivalDate
}

// SendOutRequest is POST /api/drivers/{id}/send-out.
type SendOutRequest struct {
	MarkReady bool     `json:"mark_ready"`
	Include   []string `json:"include_order_ids"`
	PlanID    string   `json:"plan_id"`
	// Exclude (2.2.1): pool orders that stay behind, back to unassigned
	// (staff took the send-check's hold advice).
	Exclude []string `json:"exclude_order_ids"`
}

// SendOut sends the driver out with their pool (plus Include), like
// AssignDriver: on_delivery, driver out, drivers.dispatched (name flash).
// Cooking orders refuse with *order.ErrNotReady unless MarkReady.
func (s *Service) SendOut(ctx context.Context, driverID string, req SendOutRequest) ([]order.StoredOrder, error) {
	ctx, span := s.span(ctx, "delivery.send_out", attribute.String("driver.id", driverID),
		attribute.Bool("mark_ready", req.MarkReady), attribute.String("plan.id", req.PlanID))
	defer span.End()

	meta := order.RunMeta{PlanID: req.PlanID, Exclude: req.Exclude}
	pool := s.poolOf(ctx, driverID)
	if req.PlanID != "" { // Send Out while AUTO's yellow stops show
		if plan, err := s.loadPlan(ctx, req.PlanID); err == nil {
			for _, run := range plan.Output.Runs {
				if run.DriverID == driverID {
					meta.SuggestedIDs, meta.StopOrder = run.SuggestedIDs, run.OrderIDs
				}
			}
		} else {
			s.logger.Warn().Ctx(ctx).Err(err).Str("plan_id", req.PlanID).Msg("send-out: AUTO plan not found (logged without it)")
		}
	} else if planID := poolPlan(pool); planID != "" { // the pool came from a confirmed plan
		meta.PlanID = planID
		if plan, err := s.loadPlan(ctx, planID); err == nil {
			for _, run := range plan.Output.Runs {
				if run.DriverID == driverID {
					meta.SuggestedIDs = run.SuggestedIDs
				}
			}
		}
		if meta.SuggestedIDs == nil {
			meta.SuggestedIDs = []string{}
		}
	}
	if meta.StopOrder == nil && len(req.Include) == 0 && seqIntact(pool) {
		meta.StopOrder = seqOrder(pool) // the order the tablet shows
	}
	if meta.StopOrder == nil {
		meta.StopOrder = s.expectedStopOrder(ctx, driverID, req.Include)
	}
	span.SetAttributes(attribute.String("plan.used", meta.PlanID))

	sent, readied, err := s.repo.SendOut(ctx, driverID, req.Include, req.MarkReady, meta)
	if err != nil {
		return nil, spanErr(span, err)
	}
	span.SetAttributes(attribute.Int("orders", len(sent)), attribute.Int("marked_ready", len(readied)))
	s.devEvent(devlog.Info, devlog.KindOrder, "Out on a run: %s", labels(sent))
	s.floorChanged()
	for _, so := range sent {
		s.publishUpdated(so)
	}
	for _, id := range req.Exclude { // left behind, unassigned
		if uid, err := uuid.Parse(id); err == nil {
			if so, err := s.repo.GetByID(ctx, uid); err == nil && so.Status.State != order.StateOnDelivery {
				s.publishUpdated(*so)
			}
		}
	}
	// An include may have taken orders out of another driver's pool.
	for _, so := range s.resequencePools(ctx, nil) {
		s.publishUpdated(so)
	}
	s.publishDrivers(ctx)
	s.publishDispatched(ctx, driverID, sent)
	s.logger.Info().Ctx(ctx).Str("driver_id", driverID).Int("orders", len(sent)).Int("marked_ready", len(readied)).
		Str("plan_id", req.PlanID).Msg("Driver sent out with their pool")
	return sent, nil
}

func (s *Service) publishDispatched(ctx context.Context, driverID string, sent []order.StoredOrder) {
	if s.broker == nil {
		return
	}
	if ds, err := s.repo.ListDrivers(ctx); err == nil {
		for _, d := range ds {
			if d.ID == driverID {
				s.broker.PublishDispatched(d, sent)
			}
		}
	}
}

// poolOf: the driver's pooled orders (accepted|ready).
func (s *Service) poolOf(ctx context.Context, driverID string) []order.StoredOrder {
	open, err := s.repo.ListOpenDeliveries(ctx)
	if err != nil {
		return nil
	}
	var pool []order.StoredOrder
	for _, so := range open {
		if so.Status.PoolDriverID == driverID && (so.Status.State == order.StateAccepted || so.Status.State == order.StateReady) {
			pool = append(pool, so)
		}
	}
	return pool
}

// poolPlan: the AUTO plan most of the pool's orders were confirmed from (ties:
// the plan of the earliest stop), "" when none were.
func poolPlan(pool []order.StoredOrder) string {
	sorted := append([]order.StoredOrder(nil), pool...)
	sort.SliceStable(sorted, func(i, j int) bool { return seqLess(sorted[i], sorted[j]) })
	counts := map[string]int{}
	best := ""
	for _, so := range sorted {
		p := so.Status.PoolPlanID
		if p == "" {
			continue
		}
		counts[p]++
		if best == "" || counts[p] > counts[best] {
			best = p
		}
	}
	return best
}

func seqLess(a, b order.StoredOrder) bool {
	switch {
	case a.Status.PoolSeq == nil:
		return false
	case b.Status.PoolSeq == nil:
		return true
	}
	return *a.Status.PoolSeq < *b.Status.PoolSeq
}

func seqOrder(pool []order.StoredOrder) []string {
	sorted := append([]order.StoredOrder(nil), pool...)
	sort.SliceStable(sorted, func(i, j int) bool { return seqLess(sorted[i], sorted[j]) })
	ids := make([]string, 0, len(sorted))
	for _, so := range sorted {
		ids = append(ids, so.Order.ID)
	}
	return ids
}

// expectedStopOrder is the fastest driving order of the run Send Out is about
// to send (pool + include), for the delivery.run stop estimates.
func (s *Service) expectedStopOrder(ctx context.Context, driverID string, include []string) []string {
	open, err := s.repo.ListOpenDeliveries(ctx)
	if err != nil {
		return nil
	}
	want := map[string]bool{}
	for _, id := range include {
		want[id] = true
	}
	var run []order.StoredOrder
	for _, so := range open {
		if so.Status.PoolDriverID == driverID || want[so.Order.ID] {
			run = append(run, so)
		}
	}
	if len(run) < 2 {
		return nil
	}
	t := s.localTrip(ctx, run)
	return t.OrderIDs
}

// --- geo ---------------------------------------------------------------------

// OnGeo stores main's order.geo push and tells the tablets.
func (s *Service) OnGeo(ctx context.Context, g order.GeoUpdate) (*order.StoredOrder, error) {
	ctx, span := s.span(ctx, "order.geo", attribute.String("order.id", g.OrderID), attribute.String("geo.state", g.State))
	defer span.End()
	switch g.State {
	case order.GeoLocated, order.GeoNotFound, order.GeoPending, order.GeoCheck:
	default:
		return nil, spanErr(span, fmt.Errorf("%w: geo state %q", ErrBadRequest, g.State))
	}
	if g.State != order.GeoCheck {
		g.Candidates = nil
	} else if len(g.Candidates) > order.MaxGeoCandidates {
		g.Candidates = g.Candidates[:order.MaxGeoCandidates]
	}
	if g.State != order.GeoLocated {
		g.Corrected = false
	}
	if len(g.Note) > 200 {
		g.Note = g.Note[:200]
	}
	so, err := s.repo.UpsertGeo(ctx, g)
	if err != nil {
		return nil, spanErr(span, err)
	}
	s.publishUpdated(*so)
	s.feedNotify() // a confirmed address goes to the hub
	return so, nil
}

// HandleGeoCommand is the order.geo RPC handler: {"ok":true}, or
// {"ok":false,"code":"not_found"|"bad_request"} (main stops pushing those);
// any other failure sends no reply, so main pushes it again.
func (s *Service) HandleGeoCommand(ctx context.Context, payload json.RawMessage) (any, error) {
	var g order.GeoUpdate
	if err := json.Unmarshal(payload, &g); err != nil || g.OrderID == "" {
		return map[string]any{"ok": false, "code": "bad_request"}, nil
	}
	_, err := s.OnGeo(ctx, g)
	switch {
	case errors.Is(err, order.ErrNotFound):
		return map[string]any{"ok": false, "code": "not_found"}, nil
	case errors.Is(err, ErrBadRequest):
		return map[string]any{"ok": false, "code": "bad_request"}, nil
	case err != nil:
		return nil, err
	}
	return map[string]any{"ok": true}, nil
}

// Pin forwards a staff-dropped pin for a delivery address to main (geo.pin);
// main answers later with order.geo (quality pinned).
func (s *Service) Pin(ctx context.Context, id string, lat, lng float64) error {
	ctx, span := s.span(ctx, "order.pin", attribute.String("order.id", id))
	defer span.End()
	uid, err := uuid.Parse(id)
	if err != nil {
		return spanErr(span, fmt.Errorf("%w: invalid order id", ErrBadRequest))
	}
	if math.IsNaN(lat) || math.IsNaN(lng) || lat < -90 || lat > 90 || lng < -180 || lng > 180 || (lat == 0 && lng == 0) {
		return spanErr(span, fmt.Errorf("%w: lat/lng out of range", ErrBadRequest))
	}
	if _, err := s.repo.GetByID(ctx, uid); err != nil {
		return spanErr(span, err)
	}
	d := s.geoDeps()
	if d.Send == nil || (d.Linked != nil && !d.Linked()) {
		return spanErr(span, ErrMainOffline)
	}
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	status, reply, err := d.Send(rctx, MsgGeoPin, map[string]any{"order_id": uid.String(), "lat": lat, "lng": lng})
	if err != nil || status != http.StatusOK {
		s.logger.Warn().Ctx(ctx).Err(err).Int("status", status).Msg("geo.pin not answered")
		return spanErr(span, ErrMainOffline)
	}
	var res struct {
		OK   bool   `json:"ok"`
		Code string `json:"code"`
	}
	_ = json.Unmarshal(reply, &res)
	switch {
	case res.OK:
		return nil
	case res.Code == "not_found":
		return spanErr(span, order.ErrNotFound)
	default:
		return spanErr(span, fmt.Errorf("%w: the server didn't accept the pin (%s)", ErrBadRequest, res.Code))
	}
}

// Trip is a driver pool's route (GET /api/drivers/{id}/pool/trip).
type Trip struct {
	OrderIDs    []string `json:"order_ids"`
	Polyline    string   `json:"polyline"`
	DistanceM   int64    `json:"distance_m"`
	DurationS   int64    `json:"duration_s"`
	Approximate bool     `json:"approximate"`
}

// PoolTrip returns the fastest driving order for the given orders (default:
// the driver's pool) with main's road route (geo.trip); when main can't be
// reached, the local order from the drive times with approximate=true and no
// polyline (the tablet draws straight segments).
func (s *Service) PoolTrip(ctx context.Context, driverID string, ids []string) (*Trip, error) {
	ctx, span := s.span(ctx, "delivery.pool_trip", attribute.String("driver.id", driverID))
	defer span.End()
	ds, err := s.repo.ListDrivers(ctx)
	if err != nil {
		return nil, spanErr(span, err)
	}
	found := false
	for _, d := range ds {
		found = found || d.ID == driverID
	}
	if !found {
		return nil, spanErr(span, order.ErrDriverNotFound)
	}
	open, err := s.repo.ListOpenDeliveries(ctx)
	if err != nil {
		return nil, spanErr(span, err)
	}
	byID := map[string]order.StoredOrder{}
	for _, so := range open {
		byID[so.Order.ID] = so
	}
	var run []order.StoredOrder
	if len(ids) == 0 {
		for _, so := range open {
			if so.Status.PoolDriverID == driverID && so.Status.State != order.StateOnDelivery {
				run = append(run, so)
			}
		}
		sort.SliceStable(run, func(i, j int) bool { return pooledBefore(run[i], run[j]) })
	} else {
		for _, id := range dedupeIDs(ids) {
			so, ok := byID[id]
			if !ok {
				uid, perr := uuid.Parse(id)
				if perr != nil {
					return nil, spanErr(span, fmt.Errorf("%w: invalid order id %q", ErrBadRequest, id))
				}
				got, gerr := s.repo.GetByID(ctx, uid)
				if gerr != nil {
					return nil, spanErr(span, gerr)
				}
				so = *got
			}
			run = append(run, so)
		}
	}
	if len(run) == 0 {
		return nil, spanErr(span, order.ErrPoolEmpty)
	}
	local := s.localTrip(ctx, run)

	d := s.geoDeps()
	if d.Send != nil && (d.Linked == nil || d.Linked()) {
		reqIDs := make([]string, 0, len(run))
		for _, so := range run {
			reqIDs = append(reqIDs, so.Order.ID)
		}
		rctx, cancel := context.WithTimeout(ctx, 8*time.Second)
		status, reply, err := d.Send(rctx, MsgGeoTrip, map[string]any{"order_ids": reqIDs})
		cancel()
		var remote Trip
		if err == nil && status == http.StatusOK && json.Unmarshal(reply, &remote) == nil && len(remote.OrderIDs) > 0 {
			remote.Approximate = false
			return &remote, nil
		}
		s.logger.Info().Ctx(ctx).Err(err).Int("status", status).Msg("geo.trip unavailable; using the local order")
	}
	return &local, nil
}

func pooledBefore(a, b order.StoredOrder) bool {
	switch {
	case a.Status.PooledAt == nil || b.Status.PooledAt == nil:
		return a.Status.ArrivalDate.Before(b.Status.ArrivalDate)
	case !a.Status.PooledAt.Equal(*b.Status.PooledAt):
		return a.Status.PooledAt.Before(*b.Status.PooledAt)
	}
	return a.Status.ArrivalDate.Before(b.Status.ArrivalDate)
}

func dedupeIDs(ids []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// --- AUTO --------------------------------------------------------------------

// AutoPlan is POST /api/deliveries/auto-plan's answer.
type AutoPlan struct {
	PlanID     string                `json:"plan_id"`
	CreatedAt  time.Time             `json:"created_at"`
	Estimated  bool                  `json:"estimated"`
	Fallback   bool                  `json:"fallback"`
	SolveMs    int64                 `json:"solve_ms"`
	Cost       float64               `json:"cost"`
	Runs       []dispatch.Run        `json:"runs"`
	Unassigned []dispatch.Unassigned `json:"unassigned"`
}

// storedPlan is auto_plans.plan_json: what AUTO saw and what it suggested.
type storedPlan struct {
	Input  dispatch.Input  `json:"input"`
	Output dispatch.Output `json:"output"`
}

func (s *Service) loadPlan(ctx context.Context, id string) (*storedPlan, error) {
	raw, err := s.repo.GetAutoPlan(ctx, id)
	if err != nil {
		return nil, err
	}
	var p storedPlan
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("decode plan %s: %w", id, err)
	}
	return &p, nil
}

// PlanDeliveries runs AUTO over the floor: the available drivers in queue
// order (with their pools), the unassigned + pooled deliveries, the drive
// times main pre-sent. It changes nothing on the floor; the plan is stored
// (auto_plans) for confirm, Send Out and the tuning log.
func (s *Service) PlanDeliveries(ctx context.Context) (*AutoPlan, error) {
	ctx, span := s.span(ctx, "delivery.auto_plan")
	defer span.End()
	now := time.Now()
	in, err := s.planInput(ctx, now)
	if err != nil {
		return nil, spanErr(span, err)
	}
	out := dispatch.Plan(*in)
	if out.Runs == nil {
		out.Runs = []dispatch.Run{}
	}
	if out.Unassigned == nil {
		out.Unassigned = []dispatch.Unassigned{}
	}
	for i := range out.Runs {
		if out.Runs[i].SuggestedIDs == nil {
			out.Runs[i].SuggestedIDs = []string{}
		}
	}
	plan := &AutoPlan{PlanID: uuid.NewString(), CreatedAt: now.UTC().Truncate(time.Millisecond), Estimated: out.Estimated,
		Fallback: out.Fallback, SolveMs: out.SolveMs, Cost: out.Cost, Runs: out.Runs, Unassigned: out.Unassigned}
	raw, err := json.Marshal(storedPlan{Input: *in, Output: out})
	if err != nil {
		return nil, spanErr(span, err)
	}
	if err := s.repo.SaveAutoPlan(ctx, plan.PlanID, now, raw); err != nil {
		return nil, spanErr(span, err)
	}
	span.SetAttributes(attribute.String("plan.id", plan.PlanID), attribute.Int("runs", len(out.Runs)),
		attribute.Int("unassigned", len(out.Unassigned)), attribute.Int64("solve_ms", out.SolveMs), attribute.Bool("fallback", out.Fallback))
	s.logger.Info().Ctx(ctx).Str("plan_id", plan.PlanID).Int("runs", len(out.Runs)).Int("unassigned", len(out.Unassigned)).
		Int64("solve_ms", out.SolveMs).Bool("fallback", out.Fallback).Bool("estimated", out.Estimated).Msg("AUTO plan made")
	return plan, nil
}

// planInput builds the optimizer input from the floor.
func (s *Service) planInput(ctx context.Context, now time.Time) (*dispatch.Input, error) {
	drivers, err := s.repo.ListDrivers(ctx)
	if err != nil {
		return nil, err
	}
	open, err := s.repo.ListOpenDeliveries(ctx)
	if err != nil {
		return nil, err
	}
	prefs := s.DeliveryPrefs(ctx)
	in := &dispatch.Input{Now: now.UTC(), TargetMinutes: int(prefs.DeliveryTargetMinutes),
		SoonReadyMinutes: int(prefs.SoonReadyMinutes), BudgetMs: autoPlanBudgetMs,
		Orders: []dispatch.Order{}, Drivers: []dispatch.Driver{}, OutDrivers: []dispatch.OutDriver{},
		Times: dispatch.Times{Store: dispatch.StoreTimes{To: map[string]int{}, From: map[string]int{}}, Pairs: map[string]map[string]int{}}}
	if store, _ := s.floorGeo(); store != nil {
		in.Store = &dispatch.LatLng{Lat: store.Lat, Lng: store.Lng}
	}
	// 2.2.1: far apart, send the first and hold the other for a returning
	// driver — by the store's nearby rule (off = no hold).
	in.Hold = holdRule(prefs)

	available := map[string]bool{}
	for _, d := range drivers { // queue order (position)
		if d.Status == order.DriverAvailable {
			available[d.ID] = true
		}
	}
	if len(available) == 0 {
		return nil, ErrNoDrivers
	}

	pools := map[string][]order.StoredOrder{}
	outRuns := map[string][]order.StoredOrder{}
	var ids []string
	for _, so := range open {
		switch so.Status.State {
		case order.StateOnDelivery:
			outRuns[so.Status.DriverID] = append(outRuns[so.Status.DriverID], so)
			continue
		case order.StateAccepted, order.StateReady:
		default:
			continue
		}
		if so.Status.CameBack {
			continue // held until staff release it (2.2.1)
		}
		if so.Status.PoolDriverID != "" {
			if !available[so.Status.PoolDriverID] {
				continue // pooled with a driver who isn't in the queue now
			}
			pools[so.Status.PoolDriverID] = append(pools[so.Status.PoolDriverID], so)
		}
		in.Orders = append(in.Orders, planOrder(so))
		ids = append(ids, so.Order.ID)
		if to, from, ok := storeTimes(so.Geo); ok {
			in.Times.Store.To[so.Order.ID], in.Times.Store.From[so.Order.ID] = to, from
		}
	}
	for _, d := range drivers {
		switch d.Status {
		case order.DriverAvailable:
			pool := pools[d.ID]
			sort.SliceStable(pool, func(i, j int) bool { return pooledBefore(pool[i], pool[j]) })
			ids := make([]string, 0, len(pool))
			for _, so := range pool {
				ids = append(ids, so.Order.ID)
			}
			in.Drivers = append(in.Drivers, dispatch.Driver{ID: d.ID, Name: d.Name, Pool: ids})
		case order.DriverOut:
			in.OutDrivers = append(in.OutDrivers, dispatch.OutDriver{ID: d.ID, Name: d.Name, ReturnAt: s.estimateReturn(ctx, outRuns[d.ID], now)})
		}
	}
	pairs, err := s.repo.DriveTimes(ctx, ids)
	if err != nil {
		return nil, err
	}
	for a, row := range pairs {
		m := make(map[string]int, len(row))
		for b, sec := range row {
			m[b] = int(sec)
		}
		in.Times.Pairs[a] = m
	}
	return in, nil
}

// planOrder maps a floor order to the optimizer's.
func planOrder(so order.StoredOrder) dispatch.Order {
	o := dispatch.Order{ID: so.Order.ID, PlacedAt: so.Order.PlacedAt, Ready: so.Status.State == order.StateReady,
		PoolDriverID: so.Status.PoolDriverID}
	if o.PlacedAt.IsZero() {
		o.PlacedAt = so.Order.ReceivedAt
	}
	if o.PlacedAt.IsZero() {
		o.PlacedAt = so.Status.ArrivalDate
	}
	switch {
	case o.Ready && so.Status.ReadyAt != nil:
		o.ReadyAt = *so.Status.ReadyAt
	case so.Status.EstimatedReadyAt != nil:
		o.ReadyAt = *so.Status.EstimatedReadyAt
	case so.Status.AcceptedAt != nil && so.Status.PrepMinutes != nil:
		o.ReadyAt = so.Status.AcceptedAt.Add(time.Duration(*so.Status.PrepMinutes) * time.Minute)
	}
	if so.Geo.Located() {
		o.Lat, o.Lng, o.Located = *so.Geo.Lat, *so.Geo.Lng, true
	}
	return o
}

// storeTimes: drive seconds store -> address and back (the route duration
// when main sent no separate figures).
func storeTimes(g *order.OrderGeo) (to, from int, ok bool) {
	if !g.Located() {
		return 0, 0, false
	}
	to, from = int(g.DurationS), int(g.DurationS)
	if g.StoreToS != nil {
		to = int(*g.StoreToS)
	}
	if g.ToStoreS != nil {
		from = int(*g.ToStoreS)
	}
	return to, from, to > 0 && from > 0
}

// defaultRunTime is the assumed length of a run nothing is known about.
const defaultRunTime = 25 * time.Minute

// estimateReturn: when a driver out on a run is expected back = out_at + the
// run's drive (fastest order, back to the store) + a stop at each door.
func (s *Service) estimateReturn(ctx context.Context, run []order.StoredOrder, now time.Time) time.Time {
	if len(run) == 0 {
		return now
	}
	out := now
	if run[0].Status.OutAt != nil {
		out = *run[0].Status.OutAt
	}
	t := s.localTrip(ctx, run)
	if t.DurationS == 0 {
		return out.Add(defaultRunTime)
	}
	back := int64(0)
	if last, ok := findOrder(run, t.OrderIDs[len(t.OrderIDs)-1]); ok {
		if _, from, ok := storeTimes(last.Geo); ok {
			back = int64(from)
		} else if last.Geo.Located() {
			back = t.DurationS / int64(len(t.OrderIDs))
		}
	}
	return out.Add(time.Duration(t.DurationS+back+int64(len(run))*order.StopServiceS) * time.Second)
}

func findOrder(list []order.StoredOrder, id string) (order.StoredOrder, bool) {
	for _, so := range list {
		if so.Order.ID == id {
			return so, true
		}
	}
	return order.StoredOrder{}, false
}

// ConfirmResult is POST /api/deliveries/auto-plan/{id}/confirm's answer.
type ConfirmResult struct {
	Applied []string         `json:"applied"`
	Skipped []order.PoolSkip `json:"skipped"`
}

// ConfirmPlan pools each run's suggested orders to its driver when still
// valid (unassigned, accepted|ready, driver available) and records the
// confirm; the rest come back as skipped with a reason. A non-empty only
// limits it to those suggested ids (the others are ignored, not skipped).
func (s *Service) ConfirmPlan(ctx context.Context, planID string, only []string) (*ConfirmResult, error) {
	ctx, span := s.span(ctx, "delivery.auto_plan.confirm", attribute.String("plan.id", planID))
	defer span.End()
	plan, err := s.loadPlan(ctx, planID)
	if err != nil {
		return nil, spanErr(span, err)
	}
	want := map[string]bool{}
	for _, id := range only {
		want[id] = true
	}
	runs := make([]order.PlanRun, 0, len(plan.Output.Runs))
	for _, r := range plan.Output.Runs {
		ids := r.SuggestedIDs
		if len(want) > 0 {
			ids = nil
			for _, id := range r.SuggestedIDs {
				if want[id] {
					ids = append(ids, id)
				}
			}
		}
		runs = append(runs, order.PlanRun{DriverID: r.DriverID, SuggestedIDs: ids})
	}
	applied, skipped, err := s.repo.ConfirmAutoPlan(ctx, planID, runs, time.Now())
	if err != nil {
		return nil, spanErr(span, err)
	}
	// Number each run in the plan's driving order (then any other pooled
	// orders, fastest first).
	prefer := map[string][]string{}
	for _, so := range applied {
		for _, r := range plan.Output.Runs {
			if r.DriverID == so.Status.PoolDriverID {
				prefer[r.DriverID] = r.OrderIDs
			}
		}
	}
	s.publishAll(latest(applied, s.resequencePools(ctx, prefer)))
	res := &ConfirmResult{Applied: []string{}, Skipped: skipped}
	for _, so := range applied {
		res.Applied = append(res.Applied, so.Order.ID)
	}
	if res.Skipped == nil {
		res.Skipped = []order.PoolSkip{}
	}
	s.logger.Info().Ctx(ctx).Str("plan_id", planID).Int("applied", len(res.Applied)).Int("skipped", len(res.Skipped)).Msg("AUTO plan confirmed")
	return res, nil
}

// autoPlanBudgetMs bounds AUTO's exact search on the Pi. AUTO is a button
// press, so a second is fine; dense worst cases measure ~100 ms on x86, a few
// times that on a Pi 4. Past it the plan is the best found (fallback: true).
const autoPlanBudgetMs = 1500
