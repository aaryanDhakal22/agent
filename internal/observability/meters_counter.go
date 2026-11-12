package observability

import (
	"context"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// CounterMeters are the counter (POS terminal) instruments of HARDENING-PLAN
// R-1a (CM-3 … CM-6, CM-8 … CM-10), recorded by application/terminal,
// floorsync and the close paths. Bounded attribute values only: no order
// ids, customer data, staff names or PINs (who goes in log fields).
type CounterMeters struct {
	// CM-3 terminal.orders.placed{type, source, result}
	OrdersPlaced metric.Int64Counter
	// CM-4 terminal.payments{tender, kind, result}
	Payments metric.Int64Counter
	// CM-5 terminal.voids{role, refunded, kind}
	Voids metric.Int64Counter
	// CM-6 terminal.overrides{action, approver} + terminal.overrides.cents
	Overrides      metric.Int64Counter
	OverridesCents metric.Int64Counter
	// CM-8 terminal.pin.failures{kind} + terminal.pin.lockouts{kind}
	PinFailures metric.Int64Counter
	PinLockouts metric.Int64Counter
	// CM-9 floor.sync.oldest_age.seconds{class} + floor.sync.parked;
	// floor.sync.events.sent{class, result} (the class split of
	// floor.events.sent, which keeps its own series).
	SyncOldestAge metric.Float64Gauge
	SyncParked    metric.Int64Gauge
	SyncSent      metric.Int64Counter
	// CM-10 terminal.z.reports{result} + batch.closes{trigger, result}
	ZReports    metric.Int64Counter
	BatchCloses metric.Int64Counter
}

// Known label values (each counter starts at 0 for these, so increase()
// sees the first event).
var (
	PlaceTypes    = []string{"counter", "pickup", "delivery"}
	PlaceResults  = []string{"ok", "duplicate", "key_reused", "manager_required", "cart_invalid", "over_balance", "menu_unavailable", "error"}
	PayTenders    = []string{"cash", "card", "square_app", "manual_card", "comp"}
	PayKinds      = []string{"sale", "refund", "tip"}
	PayResults    = []string{"ok", "duplicate", "key_reused", "over_balance", "refund_too_big", "not_payable", "manager_required", "error"}
	OverrideActs  = []string{"discount", "price_change", "comp", "employee_meal", "refund", "card_refund", "cash_for_card", "void", "pay_out", "drawer_close", "close_batch"}
	PinKinds      = []string{"login", "manager_check", "inline_manager"}
	SyncClasses   = []string{"money", "floor"}
	ZResults      = []string{"built", "build_failed", "unbalanced"}
	CloseTriggers = []string{"terminal", "tablet", "web", "auto"}
	CloseResults  = []string{"ok", "leftovers", "unsettled", "error"}
)

var (
	counterOnce sync.Once
	counterM    *CounterMeters
)

// Counter is the process's counter instruments (built on first use from the
// global meter provider; instruments made before Setup are delegated by
// otel's global once the provider is set).
func Counter() *CounterMeters {
	counterOnce.Do(func() {
		cm, err := NewCounterMeters(otel.GetMeterProvider())
		if err != nil {
			cm = &CounterMeters{}
		}
		counterM = cm
	})
	return counterM
}

// NewCounterMeters builds the instruments on mp (tests pass a ManualReader's
// provider).
func NewCounterMeters(mp metric.MeterProvider) (*CounterMeters, error) {
	m := mp.Meter(MeterName)
	cm := &CounterMeters{}
	var err error
	for _, c := range []struct {
		dst  *metric.Int64Counter
		name string
		desc string
	}{
		{&cm.OrdersPlaced, "terminal.orders.placed", "Counter orders placed at a terminal (type, source, result)"},
		{&cm.Payments, "terminal.payments", "Terminal payments, refunds and tips (tender, kind, result)"},
		{&cm.Voids, "terminal.voids", "Counter orders voided or wasted (role, refunded, kind)"},
		{&cm.Overrides, "terminal.overrides", "Manager approvals of an action past a cashier's limit (action, approver)"},
		{&cm.OverridesCents, "terminal.overrides.cents", "Cents an approved override moved (action, approver)"},
		{&cm.PinFailures, "terminal.pin.failures", "Wrong PINs at a terminal (kind)"},
		{&cm.PinLockouts, "terminal.pin.lockouts", "PIN throttles engaged at a terminal (kind)"},
		{&cm.SyncSent, "floor.sync.events.sent", "Floor events by class (money / floor) and result"},
		{&cm.ZReports, "terminal.z.reports", "Z reports (result=built|build_failed|unbalanced)"},
		{&cm.BatchCloses, "batch.closes", "Batch closes (trigger, result)"},
	} {
		if *c.dst, err = m.Int64Counter(c.name, metric.WithDescription(c.desc)); err != nil {
			return nil, err
		}
	}
	if cm.SyncOldestAge, err = m.Float64Gauge("floor.sync.oldest_age", metric.WithUnit("s"),
		metric.WithDescription("Age of the oldest unsynced floor event (class=money|floor; 0 = none)")); err != nil {
		return nil, err
	}
	if cm.SyncParked, err = m.Int64Gauge("floor.sync.parked", metric.WithDescription("Parked floor events (dead letters)")); err != nil {
		return nil, err
	}
	return cm, nil
}

// Zero adds 0 for every known label combination (call once at start).
func (c *CounterMeters) Zero(ctx context.Context) {
	if c == nil || c.OrdersPlaced == nil {
		return
	}
	attrs := metric.WithAttributes
	for _, t := range PlaceTypes {
		for _, r := range PlaceResults {
			c.OrdersPlaced.Add(ctx, 0, attrs(attribute.String("type", t), attribute.String("source", "counter"), attribute.String("result", r)))
		}
	}
	for _, t := range PayTenders {
		for _, k := range PayKinds {
			for _, r := range PayResults {
				c.Payments.Add(ctx, 0, attrs(attribute.String("tender", t), attribute.String("kind", k), attribute.String("result", r)))
			}
		}
	}
	for _, role := range []string{"cashier", "manager"} {
		for _, k := range []string{"void", "waste"} {
			c.Voids.Add(ctx, 0, attrs(attribute.String("role", role), attribute.String("refunded", "false"), attribute.String("kind", k)))
		}
	}
	for _, a := range OverrideActs {
		for _, ap := range []string{"self_manager", "manager_pin"} {
			o := attrs(attribute.String("action", a), attribute.String("approver", ap))
			c.Overrides.Add(ctx, 0, o)
			c.OverridesCents.Add(ctx, 0, o)
		}
	}
	for _, k := range PinKinds {
		c.PinFailures.Add(ctx, 0, attrs(attribute.String("kind", k)))
		c.PinLockouts.Add(ctx, 0, attrs(attribute.String("kind", k)))
	}
	for _, r := range ZResults {
		c.ZReports.Add(ctx, 0, attrs(attribute.String("result", r)))
	}
	for _, t := range CloseTriggers {
		for _, r := range CloseResults {
			c.BatchCloses.Add(ctx, 0, attrs(attribute.String("trigger", t), attribute.String("result", r)))
		}
	}
}

// Add helpers: nil-safe so a service built without meters (tests) records
// nothing.

func (c *CounterMeters) Placed(ctx context.Context, typ, result string) {
	if c == nil || c.OrdersPlaced == nil {
		return
	}
	c.OrdersPlaced.Add(ctx, 1, metric.WithAttributes(attribute.String("type", typ), attribute.String("source", "counter"),
		attribute.String("result", result)))
}

func (c *CounterMeters) Payment(ctx context.Context, tender, kind, result string) {
	if c == nil || c.Payments == nil {
		return
	}
	c.Payments.Add(ctx, 1, metric.WithAttributes(attribute.String("tender", tender), attribute.String("kind", kind),
		attribute.String("result", result)))
}

func (c *CounterMeters) Void(ctx context.Context, role, kind string, refunded bool) {
	if c == nil || c.Voids == nil {
		return
	}
	r := "false"
	if refunded {
		r = "true"
	}
	c.Voids.Add(ctx, 1, metric.WithAttributes(attribute.String("role", role), attribute.String("refunded", r), attribute.String("kind", kind)))
}

func (c *CounterMeters) Override(ctx context.Context, action, approver string, cents int64) {
	if c == nil || c.Overrides == nil {
		return
	}
	o := metric.WithAttributes(attribute.String("action", action), attribute.String("approver", approver))
	c.Overrides.Add(ctx, 1, o)
	if cents > 0 {
		c.OverridesCents.Add(ctx, cents, o)
	}
}

func (c *CounterMeters) PinFailure(ctx context.Context, kind string, lockout bool) {
	if c == nil || c.PinFailures == nil {
		return
	}
	c.PinFailures.Add(ctx, 1, metric.WithAttributes(attribute.String("kind", kind)))
	if lockout {
		c.PinLockouts.Add(ctx, 1, metric.WithAttributes(attribute.String("kind", kind)))
	}
}

func (c *CounterMeters) Sync(ctx context.Context, class, result string, n int) {
	if c == nil || c.SyncSent == nil || n == 0 {
		return
	}
	c.SyncSent.Add(ctx, int64(n), metric.WithAttributes(attribute.String("class", class), attribute.String("result", result)))
}

func (c *CounterMeters) Z(ctx context.Context, result string) {
	if c == nil || c.ZReports == nil {
		return
	}
	c.ZReports.Add(ctx, 1, metric.WithAttributes(attribute.String("result", result)))
}

func (c *CounterMeters) Close(ctx context.Context, trigger, result string) {
	if c == nil || c.BatchCloses == nil {
		return
	}
	c.BatchCloses.Add(ctx, 1, metric.WithAttributes(attribute.String("trigger", trigger), attribute.String("result", result)))
}
