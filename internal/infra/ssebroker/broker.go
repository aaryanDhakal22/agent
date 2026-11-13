// Package ssebroker fans out events to connected mobile clients (the counter
// tablet + the web UI). Mirrors the broker in main/ but is agent-local.
// Event types: order.arrived, order.updated, order.escalated,
// drivers.updated, batch.closed (+ 2.1.0: settings.updated,
// integration.status).
package ssebroker

import (
	"context"
	"encoding/json"
	"sync"

	"quiccpos/agent/internal/domain/order"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

const tracerName = "quiccpos/agent/ssebroker"

// Event is the envelope sent to subscribed mobile clients. `Type` selects the
// SSE "event:" line; `Data` is marshalled into "data:". Downstream mobile can
// filter on Type to decide what UI affordance to present.
type Event struct {
	Type string          // sse "event:" line
	Data json.RawMessage // sse "data:" payload
	// Audience (2.5.4): who gets the frame - everyone (the zero value), every
	// client but the paired Quibase tablets, or the tablets only.
	Audience Audience
}

// Audience selects the subscribers an Event goes to (2.5.4: the orders a
// counter terminal rang that Quibase doesn't show, order.StoredOrder.ShownOnTablet).
type Audience uint8

const (
	AudienceAll        Audience = iota
	AudienceNotTablets          // the terminals, the Pi page, the hub's readers
	AudienceTablets             // the paired Quibase tablets only
)

// Broker broadcasts events to every subscribed mobile client. Current product
// spec is one mobile client per agent, but the fan-out is written generically
// so multiple subscribers are safe (dev laptops + the real device, etc.).
type Broker struct {
	mu      sync.Mutex
	clients map[chan Event]bool // true = a paired Quibase tablet
	// tabletShows says whether a tablet gets an order (nil = every order);
	// tabletSeen holds the orders whose last frame went to the tablets, so
	// one that turns hidden (a terminal edit) sends them floor.updated (they
	// refetch and drop it). Cleared at Close batch.
	tabletShows func(order.StoredOrder) bool
	tabletSeen  map[string]bool
}

func New() *Broker {
	return &Broker{clients: make(map[chan Event]bool), tabletSeen: map[string]bool{}}
}

// SetTabletFilter installs the rule for which orders the paired tablets get
// (cmd/main.go: order.StoredOrder.ShownOnTablet with the store's X). Set it
// before the server starts.
func (b *Broker) SetTabletFilter(fn func(order.StoredOrder) bool) {
	b.mu.Lock()
	b.tabletShows = fn
	b.mu.Unlock()
}

// TabletShows reports whether a paired tablet gets the order (the list
// routes filter with it too).
func (b *Broker) TabletShows(so order.StoredOrder) bool {
	b.mu.Lock()
	fn := b.tabletShows
	b.mu.Unlock()
	return fn == nil || fn(so)
}

// Subscribe adds a client that isn't a paired tablet (the terminals, the Pi
// page, the hub): it gets every order.
func (b *Broker) Subscribe() (chan Event, func()) { return b.subscribe(false) }

// SubscribeTablet adds a paired Quibase tablet: it gets only the orders
// TabletShows lets through.
func (b *Broker) SubscribeTablet() (chan Event, func()) { return b.subscribe(true) }

func (b *Broker) subscribe(tablet bool) (chan Event, func()) {
	ch := make(chan Event, 128)
	b.mu.Lock()
	b.clients[ch] = tablet
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		if _, ok := b.clients[ch]; ok { // not already dropped by publish
			delete(b.clients, ch)
			close(ch)
		}
		b.mu.Unlock()
	}
}

func (b *Broker) Subscribers() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.clients)
}

// orderArrivedPayload mirrors main/'s pattern: the domain order plus the
// current trace context so the mobile client could (if it wanted) continue the
// trace on its side. For now mobile is uninstrumented — the fields are there
// for forward-compat and to keep the pattern consistent across hops.
type orderArrivedPayload struct {
	Order       order.Snapshot  `json:"order"`
	Status      order.Status    `json:"status"`
	Geo         *order.OrderGeo `json:"geo,omitempty"`
	Traceparent string          `json:"_traceparent,omitempty"`
	Tracestate  string          `json:"_tracestate,omitempty"`
}

// PublishArrived announces a freshly-arrived order to any connected mobile
// client. Called from the order service only when auto-accept is OFF — in
// auto-accept mode the order is printed directly and mobile learns about it
// via the order-printed channel (not yet wired; see README-level plan).
func (b *Broker) PublishArrived(ctx context.Context, so order.StoredOrder) {
	tracer := otel.Tracer(tracerName)
	ctx, span := tracer.Start(ctx, "ssebroker.publish_arrived",
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(
			attribute.String("order.id", so.Order.ID),
			attribute.Int("ssebroker.subscribers", b.Subscribers()),
		),
	)
	defer span.End()

	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)

	payload := orderArrivedPayload{
		Order:       so.Order,
		Status:      so.Status,
		Geo:         so.Geo,
		Traceparent: carrier["traceparent"],
		Tracestate:  carrier["tracestate"],
	}
	data, err := json.Marshal(payload)
	if err != nil {
		span.RecordError(err)
		return
	}
	b.publishOrder(so, Event{Type: "order.arrived", Data: data})
}

// PublishUpdated announces an order's new floor state (accepted, ready, on
// delivery, delivered, or reverted to arrived after a failed print).
func (b *Broker) PublishUpdated(so order.StoredOrder) {
	data, err := json.Marshal(so)
	if err != nil {
		return
	}
	b.publishOrder(so, Event{Type: "order.updated", Data: data})
}

// PublishEscalated re-announces an order nobody has accepted yet.
func (b *Broker) PublishEscalated(so order.StoredOrder, waitingSeconds int64) {
	m := map[string]any{"order": so.Order, "status": so.Status, "waiting_seconds": waitingSeconds}
	if so.Geo != nil {
		m["geo"] = so.Geo
	}
	data, err := json.Marshal(m)
	if err != nil {
		return
	}
	b.publishOrder(so, Event{Type: "order.escalated", Data: data})
}

// publishOrder sends an order's frame: to everyone when the tablets show
// the order, else to everyone but the tablets - plus floor.updated to the
// tablets when their last frame of it showed it (they refetch and drop it).
func (b *Broker) publishOrder(so order.StoredOrder, e Event) {
	shown := b.TabletShows(so)
	b.mu.Lock()
	defer b.mu.Unlock()
	id := so.Order.ID
	if shown {
		b.tabletSeen[id] = true
		b.publishLocked(e)
		return
	}
	e.Audience = AudienceNotTablets
	b.publishLocked(e)
	if b.tabletSeen[id] {
		delete(b.tabletSeen, id)
		b.publishLocked(Event{Type: "floor.updated", Data: json.RawMessage(`{}`), Audience: AudienceTablets})
	}
}

// PublishDrivers sends the whole driver roster (queue order).
func (b *Broker) PublishDrivers(drivers []order.Driver) {
	if drivers == nil {
		drivers = []order.Driver{}
	}
	b.publishJSON("drivers.updated", drivers)
}

// PublishDispatched announces a driver sent out with orders — the tablet
// flashes the driver's name full-screen so they know it's their run.
//
// A tablet gets only the orders it shows (2.5.4); a run of hidden orders
// alone flashes nothing there.
func (b *Broker) PublishDispatched(driver order.Driver, orders []order.StoredOrder) {
	shown := make([]order.StoredOrder, 0, len(orders))
	for _, so := range orders {
		if b.TabletShows(so) {
			shown = append(shown, so)
		}
	}
	if len(shown) == len(orders) {
		b.publishJSON("drivers.dispatched", map[string]any{"driver": driver, "orders": orders})
		return
	}
	all, err := json.Marshal(map[string]any{"driver": driver, "orders": orders})
	if err != nil {
		return
	}
	b.publish(Event{Type: "drivers.dispatched", Data: all, Audience: AudienceNotTablets})
	if len(shown) > 0 {
		mine, err := json.Marshal(map[string]any{"driver": driver, "orders": shown})
		if err == nil {
			b.publish(Event{Type: "drivers.dispatched", Data: mine, Audience: AudienceTablets})
		}
	}
}

// PublishBatchClosed announces a closed batch and the newly opened one.
func (b *Broker) PublishBatchClosed(closed, opened order.Batch) {
	b.mu.Lock()
	clear(b.tabletSeen) // the tablets refetch the new batch
	b.mu.Unlock()
	b.publishJSON("batch.closed", map[string]any{"closed": closed, "opened": opened})
}

// PublishFloorChanged tells clients the floor settings changed (they refetch
// /api/floor).
func (b *Broker) PublishFloorChanged() {
	b.publishJSON("floor.updated", map[string]any{})
}

// Publish sends any event (2.1.0: settings.updated,
// integration.status).
func (b *Broker) Publish(typ string, v any) { b.publishJSON(typ, v) }

func (b *Broker) publishJSON(typ string, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	b.publish(Event{Type: typ, Data: data})
}

func (b *Broker) publish(e Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.publishLocked(e)
}

func (b *Broker) publishLocked(e Event) {
	for ch, tablet := range b.clients {
		if (tablet && e.Audience == AudienceNotTablets) || (!tablet && e.Audience == AudienceTablets) {
			continue
		}
		// Non-blocking: a client too slow to keep up is disconnected (its
		// channel closed → the SSE handler ends the stream) rather than
		// silently missing events — it reconnects and refetches everything.
		select {
		case ch <- e:
		default:
			delete(b.clients, ch)
			close(ch)
		}
	}
}
