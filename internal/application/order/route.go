package order

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"quiccpos/agent/internal/application/devlog"
	"quiccpos/agent/internal/application/dispatch"
	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/infra/notify"
)

// Driver routes (2.3.7): Send Out pushes the driver's phone (Pushover, by
// device name) a Google Maps link store → stops → store, in the run's stop
// order — which AUTO decides when no plan did.

var (
	// ErrBadDevice: not a Pushover device name (1-25 of [A-Za-z0-9_-]).
	ErrBadDevice = errors.New("a Pushover device name is 1-25 letters, digits, - or _")
	// ErrNoDevice: the driver has no Pushover device.
	ErrNoDevice = errors.New("the driver has no Pushover device")
)

// RouteError is a route Pushover that didn't go out; Code is what the
// tablet shows (device_not_found | pushover_rejected | pushover_unreachable
// | not_configured).
type RouteError struct {
	Code string
	Err  error
}

func (e *RouteError) Error() string { return e.Code + ": " + e.Err.Error() }
func (e *RouteError) Unwrap() error { return e.Err }

const (
	routeURLMax   = 512 // Pushover's url limit
	routeStopsMax = 9   // Google Maps directions waypoints
	routeBudgetMs = 150
)

// cleanDevice validates a driver's Pushover device ("" = none).
func cleanDevice(d string) (string, error) {
	d = strings.TrimSpace(d)
	if d != "" && !notify.ValidDevice(d) {
		return "", ErrBadDevice
	}
	return d, nil
}

// SetDriverDevice sets ("" clears) a driver's Pushover device.
func (s *Service) SetDriverDevice(ctx context.Context, id, device string) error {
	device, err := cleanDevice(device)
	if err != nil {
		return err
	}
	if err := s.repo.SetDriverPushoverDevice(ctx, id, device); err != nil {
		return err
	}
	s.driversChanged(ctx)
	return nil
}

// ValidateDevice is cleanDevice for callers that write later (create).
func ValidateDevice(d string) error {
	_, err := cleanDevice(d)
	return err
}

// --- stop order ----------------------------------------------------------------

// autoStopOrder is the run Send Out is about to send (pool + include) in
// the order AUTO picks for this driver alone; stops AUTO can't place (no
// pin) follow in the local trip order. nil for fewer than two stops.
func (s *Service) autoStopOrder(ctx context.Context, driverID string, include []string) []string {
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
	fallback := s.localTrip(ctx, run).OrderIDs
	in, err := s.planInput(ctx, time.Now())
	if err != nil {
		return fallback
	}
	inRun := map[string]bool{}
	ids := make([]string, 0, len(run))
	for _, so := range run {
		inRun[so.Order.ID] = true
		ids = append(ids, so.Order.ID)
	}
	var orders []dispatch.Order
	for _, o := range in.Orders {
		if inRun[o.ID] {
			o.PoolDriverID, o.Ready = driverID, true // leaving now
			orders = append(orders, o)
		}
	}
	in.Orders = orders
	in.Drivers = []dispatch.Driver{{ID: driverID, Pool: ids}}
	in.OutDrivers, in.Hold, in.BudgetMs = nil, nil, routeBudgetMs
	var seq []string
	for _, r := range dispatch.Plan(*in).Runs {
		if r.DriverID == driverID {
			seq = r.OrderIDs
		}
	}
	return mergeOrder(seq, fallback, ids)
}

// mergeOrder: first's ids (those in all), then the rest of then's, then any
// of all still missing.
func mergeOrder(first, then, all []string) []string {
	in := map[string]bool{}
	for _, id := range all {
		in[id] = true
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(all))
	for _, list := range [][]string{first, then, all} {
		for _, id := range list {
			if in[id] && !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	return out
}

// --- the link ------------------------------------------------------------------

// RouteStop is one stop of a route link.
type RouteStop struct {
	Label string // "#1104"
	Lat   *float64
	Lng   *float64
	Addr  *order.Address
}

func (st RouteStop) place(short bool) string {
	if st.Lat != nil && st.Lng != nil {
		return fmt.Sprintf("%.5f,%.5f", *st.Lat, *st.Lng)
	}
	if st.Addr == nil {
		return ""
	}
	parts := []string{st.Addr.Line1}
	if !short {
		parts = append(parts, st.Addr.City, st.Addr.State)
	}
	parts = append(parts, st.Addr.PostalCode)
	var keep []string
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			keep = append(keep, p)
		}
	}
	return strings.Join(keep, " ")
}

// RouteLink builds the Google Maps directions link store → stops → store
// (no store pin: from the phone's location to the last stop), within
// Pushover's 512-character url and Google's 9 waypoints. It returns the link
// and how many stops it carries (the first n).
func RouteLink(store *FloorStore, stops []RouteStop) (string, int) {
	var places []string
	for _, st := range stops {
		if p := st.place(false); p != "" {
			places = append(places, p)
		}
	}
	build := func(places []string) string {
		q := url.Values{"api": {"1"}, "travelmode": {"driving"}}
		via := places
		if store != nil {
			home := fmt.Sprintf("%.5f,%.5f", store.Lat, store.Lng)
			q.Set("origin", home)
			q.Set("destination", home)
		} else if len(places) > 0 {
			q.Set("destination", places[len(places)-1])
			via = places[:len(places)-1]
		}
		if len(via) > 0 {
			q.Set("waypoints", strings.Join(via, "|"))
		}
		return "https://www.google.com/maps/dir/?" + q.Encode()
	}
	max := routeStopsMax
	if store == nil {
		max++ // the last stop is the destination, not a waypoint
	}
	if len(places) > max {
		places = places[:max]
	}
	link := build(places)
	if len(link) > routeURLMax { // shorter addresses: line 1 + zip
		places = places[:0]
		for _, st := range stops {
			if p := st.place(true); p != "" {
				places = append(places, p)
			}
		}
		if len(places) > max {
			places = places[:max]
		}
		link = build(places)
	}
	for len(link) > routeURLMax && len(places) > 1 {
		places = places[:len(places)-1]
		link = build(places)
	}
	return link, len(places)
}

// routeText is the message: the stops in order, and what didn't fit.
func routeText(stops []RouteStop, carried int) (title, text string) {
	labels := make([]string, 0, len(stops))
	for _, st := range stops {
		labels = append(labels, st.Label)
	}
	n := len(stops)
	title = fmt.Sprintf("Route · %d stop", n)
	if n != 1 {
		title += "s"
	}
	text = strings.Join(labels, " → ")
	if carried < n {
		text += fmt.Sprintf(" (+%d more on the ticket)", n-carried)
	}
	if text == "" {
		text = "Route"
	}
	return title, text
}

func routeStops(sent []order.StoredOrder) []RouteStop {
	run := append([]order.StoredOrder(nil), sent...)
	sort.SliceStable(run, func(i, j int) bool {
		a, b := run[i].Status.RunSeq, run[j].Status.RunSeq
		return a != nil && (b == nil || *a < *b)
	})
	stops := make([]RouteStop, 0, len(run))
	for _, so := range run {
		st := RouteStop{Label: label(so), Addr: so.Order.Fulfillment.Address}
		if so.Geo.Located() {
			st.Lat, st.Lng = so.Geo.Lat, so.Geo.Lng
		}
		stops = append(stops, st)
	}
	return stops
}

// --- sending -------------------------------------------------------------------

// DriverRoute is SSE driver.route: how a Send Out's route Pushover went.
type DriverRoute struct {
	DriverID   string `json:"driver_id"`
	DriverName string `json:"driver_name"`
	OK         bool   `json:"ok"`
	Code       string `json:"code,omitempty"`
	Stops      int    `json:"stops"`
}

// sendRoute pushes the driver their route in the background after a Send
// Out (nothing for a driver with no device), then tells the tablets.
func (s *Service) sendRoute(ctx context.Context, driverID string, sent []order.StoredOrder) {
	if s.notifier == nil || len(sent) == 0 {
		return
	}
	d, err := s.driverByID(ctx, driverID)
	if err != nil || d.PushoverDevice == "" {
		return
	}
	if !s.notifier.AdminScoped() && s.routeUnscopedWarned.CompareAndSwap(false, true) {
		s.logger.Warn().Ctx(ctx).Msg("drivers have Pushover devices but no admin device is set: owner alerts still reach every phone on the account")
		s.devEvent(devlog.Warn, devlog.KindOrder, "No admin Pushover device set — alerts reach the drivers' phones too")
	}
	store, _ := s.floorGeo()
	stops := routeStops(sent)
	s.background(ctx, func(ctx context.Context) {
		ctx, span := s.span(ctx, "delivery.route_notify", attribute.String("driver.id", driverID),
			attribute.Int("stops", len(stops)))
		defer span.End()
		ev := DriverRoute{DriverID: d.ID, DriverName: d.Name, OK: true, Stops: len(stops)}
		if err := s.pushRoute(ctx, d.PushoverDevice, store, stops); err != nil {
			ev.OK, ev.Code = false, routeCode(err)
			span.SetAttributes(attribute.String("route.result", ev.Code))
			s.logger.Warn().Ctx(ctx).Err(err).Str("driver", d.Name).Msg("driver route not sent")
			s.devEvent(devlog.Warn, devlog.KindOrder, "Route to %s not sent (%s)", d.Name, ev.Code)
		} else {
			s.devEvent(devlog.Info, devlog.KindOrder, "Route sent to %s (%d stops)", d.Name, len(stops))
		}
		s.countRoute(ctx, ev)
		if s.broker != nil {
			s.broker.Publish("driver.route", ev)
		}
	})
}

// pushRoute sends one route, retrying once after 5 s on a network error.
func (s *Service) pushRoute(ctx context.Context, device string, store *FloorStore, stops []RouteStop) error {
	link, carried := RouteLink(store, stops)
	title, text := routeText(stops, carried)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	err := s.notifier.SendRoute(ctx, device, title, text, link)
	if err != nil && routeCode(err) == "pushover_unreachable" {
		select {
		case <-time.After(5 * time.Second):
			err = s.notifier.SendRoute(ctx, device, title, text, link)
		case <-ctx.Done():
		}
	}
	return err
}

// TestRoute sends the driver a sample route (store → store) right away.
func (s *Service) TestRoute(ctx context.Context, driverID string) error {
	d, err := s.driverByID(ctx, driverID)
	if err != nil {
		return err
	}
	if d.PushoverDevice == "" {
		return ErrNoDevice
	}
	if s.notifier == nil {
		return &RouteError{Code: "not_configured", Err: notify.ErrNotConfigured}
	}
	store, _ := s.floorGeo()
	link, _ := RouteLink(store, nil)
	err = s.notifier.SendRoute(ctx, d.PushoverDevice, "Test route", "Your routes from the store will arrive like this.", link)
	if err != nil {
		return &RouteError{Code: routeCode(err), Err: err}
	}
	return nil
}

func routeCode(err error) string {
	var api *notify.APIError
	switch {
	case errors.Is(err, notify.ErrNotConfigured):
		return "not_configured"
	case errors.As(err, &api) && api.DeviceUnknown():
		return "device_not_found"
	case errors.As(err, &api):
		return "pushover_rejected"
	default:
		return "pushover_unreachable"
	}
}

func (s *Service) countRoute(ctx context.Context, ev DriverRoute) {
	if s.meters.DriverRouteSent == nil {
		return
	}
	result := "ok"
	switch ev.Code {
	case "":
	case "device_not_found", "not_configured":
		result = ev.Code
	case "pushover_rejected":
		result = "rejected"
	default:
		result = "unreachable"
	}
	s.meters.DriverRouteSent.Add(ctx, 1, metric.WithAttributes(attribute.String("result", result)))
}

func (s *Service) driverByID(ctx context.Context, id string) (*order.Driver, error) {
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
