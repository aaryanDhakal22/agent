// Package ssebroker fans out events to connected mobile clients (the counter
// tablet + the web UI). Mirrors the broker in main/ but is agent-local.
// Event types: order.arrived, order.updated, order.escalated,
// drivers.updated, batch.closed.
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
}

// Broker broadcasts events to every subscribed mobile client. Current product
// spec is one mobile client per agent, but the fan-out is written generically
// so multiple subscribers are safe (dev laptops + the real device, etc.).
type Broker struct {
	mu      sync.Mutex
	clients map[chan Event]struct{}
}

func New() *Broker {
	return &Broker{clients: make(map[chan Event]struct{})}
}

func (b *Broker) Subscribe() (chan Event, func()) {
	ch := make(chan Event, 128)
	b.mu.Lock()
	b.clients[ch] = struct{}{}
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
	b.publish(Event{Type: "order.arrived", Data: data})
}

// PublishUpdated announces an order's new floor state (accepted, ready, on
// delivery, delivered, or reverted to arrived after a failed print).
func (b *Broker) PublishUpdated(so order.StoredOrder) {
	b.publishJSON("order.updated", so)
}

// PublishEscalated re-announces an order nobody has accepted yet.
func (b *Broker) PublishEscalated(so order.StoredOrder, waitingSeconds int64) {
	m := map[string]any{"order": so.Order, "status": so.Status, "waiting_seconds": waitingSeconds}
	if so.Geo != nil {
		m["geo"] = so.Geo
	}
	b.publishJSON("order.escalated", m)
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
func (b *Broker) PublishDispatched(driver order.Driver, orders []order.StoredOrder) {
	b.publishJSON("drivers.dispatched", map[string]any{"driver": driver, "orders": orders})
}

// PublishBatchClosed announces a closed batch and the newly opened one.
func (b *Broker) PublishBatchClosed(closed, opened order.Batch) {
	b.publishJSON("batch.closed", map[string]any{"closed": closed, "opened": opened})
}

// PublishFloorChanged tells clients the floor settings changed (they refetch
// /api/floor).
func (b *Broker) PublishFloorChanged() {
	b.publishJSON("floor.updated", map[string]any{})
}

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
	for ch := range b.clients {
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
