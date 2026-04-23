// Package ssebroker fans out events to connected mobile clients. Mirrors the
// broker in main/ but is agent-local: the only subscriber is the in-house
// mobile interface. One event type today (`order.arrived`), more later.
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
	ch := make(chan Event, 16)
	b.mu.Lock()
	b.clients[ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.clients, ch)
		close(ch)
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
	Order       order.OrderRequest `json:"order"`
	Status      order.Status       `json:"status"`
	Traceparent string             `json:"_traceparent,omitempty"`
	Tracestate  string             `json:"_tracestate,omitempty"`
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
			attribute.Int("order.id", so.Order.OrderID),
			attribute.Int("ssebroker.subscribers", b.Subscribers()),
		),
	)
	defer span.End()

	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)

	payload := orderArrivedPayload{
		Order:       so.Order,
		Status:      so.Status,
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

func (b *Broker) publish(e Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.clients {
		// Non-blocking: if a mobile client is slow, we drop this event rather
		// than block the order-arrival path. Mobile can re-sync the pending
		// queue via GET /api/orders/arrived on reconnect.
		select {
		case ch <- e:
		default:
		}
	}
}
