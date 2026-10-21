package sse

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	orderApp "quiccpos/agent/internal/application/order"
	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/storeconfig"
	"quiccpos/agent/internal/observability"
	agentrpc "quiccpos/agent/internal/rpc"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Reconnect waits (vars so tests can shorten them).
var (
	initialBackoff = 2 * time.Second
	maxBackoff     = 60 * time.Second
)

const (
	tracerName = "quiccpos/agent/sse"
)

// http11Client forces HTTP/1.1 by disabling HTTP/2 negotiation (TLS ALPN).
// SSE requires a persistent connection — HTTP/2 multiplexed streams are
// terminated by proxies on idle timeout, causing spurious disconnects.
var http11Client = &http.Client{
	Transport: otelhttp.NewTransport(&http.Transport{
		TLSNextProto: make(map[string]func(string, *tls.Conn) http.RoundTripper),
	}),
}

// StoreConfigSink is the seam between the SSE handshake and persistence /
// in-memory hot-swap. The SSE client calls it on every `store_config` frame.
type StoreConfigSink interface {
	Apply(ctx context.Context, sc storeconfig.StoreContext) error
}

// ConnStatus is an immutable snapshot of the SSE connection's health, surfaced
// to the web UI (the green light) via the controller + /api/session.
type ConnStatus struct {
	Connected      bool   // an SSE stream is currently open to main/
	AuthRejected   bool   // main/ answered 401 — the stored key is stale/revoked (needs re-link)
	ConnectedSince int64  // unix seconds of the current connection (0 = not connected)
	LastEventAt    int64  // unix seconds of the last frame received (handshake or order)
	Reconnects     int64  // cumulative reconnect attempts
	LastError      string // last connect error, human-readable ("" when healthy)
}

// ConnState is the thread-safe, shared view of the SSE connection. The
// controller owns ONE and passes it into each Client it (re)starts, so the
// snapshot survives key swaps + reconnects. HTTP handlers read Snapshot().
type ConnState struct {
	connected      atomic.Bool
	authRejected   atomic.Bool
	connectedSince atomic.Int64
	lastEventAt    atomic.Int64
	reconnects     atomic.Int64
	lastError      atomic.Value // string
	onRejected     atomic.Value // func(string) — fired once when authRejected latches false→true
	onConnected    atomic.Value // func() — fired after every successful connect
}

func NewConnState() *ConnState { return &ConnState{} }

// SetOnRejected registers a callback fired exactly once when main definitively
// rejects the key (HTTP 401). It receives the rejected key so the handler can
// ignore a stale 401 for a since-rotated token. The agent wires this to wipe
// local provisioning so a revoked device resets to the setup screen. It survives
// key swaps (the state is shared) and re-arms after a Reset.
func (s *ConnState) SetOnRejected(f func(key string)) { s.onRejected.Store(f) }

// SetOnConnected registers a callback run (in its own goroutine) after every
// successful (re)connect — the agent uses it to catch up on orders it may
// have missed while the stream was down.
func (s *ConnState) SetOnConnected(f func()) { s.onConnected.Store(f) }

// markRejected latches authRejected and fires the onRejected callback the first
// time it transitions false→true, so a steady reconnect loop against a dead key
// only triggers the wipe once.
func (s *ConnState) markRejected(key string) {
	if s.authRejected.Swap(true) {
		return // already latched — don't re-fire
	}
	if f, ok := s.onRejected.Load().(func(string)); ok && f != nil {
		go f(key)
	}
}

// Reset marks the connection as freshly (re)starting — clears the connected /
// rejected / error view so the UI shows "connecting", not a stale verdict.
// Cumulative reconnects are kept. Called by the controller on key swap.
func (s *ConnState) Reset() {
	s.connected.Store(false)
	s.authRejected.Store(false)
	s.lastError.Store("")
}

func (s *ConnState) Snapshot() ConnStatus {
	errStr, _ := s.lastError.Load().(string)
	return ConnStatus{
		Connected:      s.connected.Load(),
		AuthRejected:   s.authRejected.Load(),
		ConnectedSince: s.connectedSince.Load(),
		LastEventAt:    s.lastEventAt.Load(),
		Reconnects:     s.reconnects.Load(),
		LastError:      errStr,
	}
}

type Client struct {
	serverURL       string
	apiKey          string
	service         *orderApp.Service
	storeConfigSink StoreConfigSink
	commands        *agentrpc.Registry
	logger          zerolog.Logger
	reconnectCount  int
	streamed        bool // the current connect attempt got its 200 (Start's goroutine only)
	state           *ConnState
	tracer          trace.Tracer
	meters          observability.Meters
	propagator      propagation.TextMapPropagator
}

func New(serverURL, apiKey string, svc *orderApp.Service, sink StoreConfigSink, commands *agentrpc.Registry, state *ConnState, logger zerolog.Logger, meters observability.Meters) *Client {
	if state == nil {
		state = NewConnState()
	}
	return &Client{
		serverURL:       serverURL,
		apiKey:          apiKey,
		service:         svc,
		storeConfigSink: sink,
		commands:        commands,
		state:           state,
		logger:          logger.With().Str("module", "sse-client").Logger(),
		tracer:          otel.Tracer(tracerName),
		meters:          meters,
		propagator:      otel.GetTextMapPropagator(),
	}
}

func (c *Client) Start(ctx context.Context) {
	c.logger.Info().Ctx(ctx).Str("server_url", c.serverURL).Msg("SSE client starting")
	backoff := initialBackoff
	for {
		c.logger.Info().Ctx(ctx).Int("attempt", c.reconnectCount+1).Dur("backoff", backoff).
			Msg("Connecting to main server SSE stream")

		c.streamed = false
		err := c.connect(ctx)
		if c.streamed {
			// The stream was up (HTTP 200): a drop after that starts over at
			// the short wait instead of where the last outage left off.
			backoff = initialBackoff
		}
		if ctx.Err() != nil {
			c.logger.Info().Msg("SSE client stopped — context cancelled")
			return
		}
		if err != nil {
			c.logger.Error().Err(err).Int("reconnect_count", c.reconnectCount).Dur("retry_in", backoff).
				Msg("SSE connection failed, will retry")
		} else {
			c.logger.Warn().Int("reconnect_count", c.reconnectCount).Dur("retry_in", backoff).
				Msg("SSE stream closed cleanly by server, will reconnect")
		}

		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			c.logger.Info().Msg("SSE client stopped — context cancelled during backoff")
			return
		}

		backoff = min(backoff*2, maxBackoff)
		c.reconnectCount++
		c.state.reconnects.Store(int64(c.reconnectCount))
		c.meters.SSEReconnects.Add(ctx, 1)
	}
}

func (c *Client) connect(ctx context.Context) error {
	url := strings.TrimRight(c.serverURL, "/") + "/api/v1/events/orders"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	// X-STORE-API-KEY authenticates this agent to main/. main's auth
	// middleware hashes it, resolves the device row, and uses the device's
	// (tenant_id, location_id) to scope which orders this connection
	// receives.
	req.Header.Set("X-STORE-API-KEY", c.apiKey)

	c.logger.Debug().Ctx(ctx).Str("url", url).Msg("Dialing main server (HTTP/1.1 forced)")

	// Whatever happens below, we're not connected once this returns.
	defer c.state.connected.Store(false)

	resp, err := http11Client.Do(req)
	if err != nil {
		c.state.lastError.Store("cannot reach main: " + err.Error())
		return fmt.Errorf("dial: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusUnauthorized {
			// The stored STORE_API_KEY is stale/revoked — main has no matching
			// device. Latch it (firing the onRejected callback once) so the agent
			// wipes local provisioning and the web UI drops to the setup screen.
			c.state.markRejected(c.apiKey)
			c.state.lastError.Store("key rejected by main (401) — re-provision the device")
		} else {
			c.state.lastError.Store(fmt.Sprintf("main returned HTTP %d", resp.StatusCode))
		}
		return fmt.Errorf("unexpected status %d from SSE endpoint", resp.StatusCode)
	}

	// Connected. A valid 200 means the key authenticated — clear any prior
	// rejection/error and stamp the connection time.
	c.streamed = true
	c.state.connected.Store(true)
	c.state.connectedSince.Store(time.Now().Unix())
	c.state.authRejected.Store(false)
	c.state.lastError.Store("")

	c.logger.Info().Ctx(ctx).Str("url", url).Int("reconnect_count", c.reconnectCount).
		Msg("Connected to main server SSE stream")
	if f, ok := c.state.onConnected.Load().(func()); ok && f != nil {
		go f()
	}

	return c.readEvents(ctx, resp.Body)
}

// readEvents is the SSE parser. Handles two event types today:
//   - `store_config` — handshake frame main/ sends on every connect (first
//     event before any orders flow). Contains the StoreContext starter pack.
//   - `order` — a canonical Snapshot wrapped with optional trace context.
//
// Unknown event types are logged and skipped so a future event added on
// main's side doesn't crash the agent.
func (c *Client) readEvents(ctx context.Context, r io.Reader) error {
	c.logger.Debug().Ctx(ctx).Msg("Starting SSE event loop")

	scanner := bufio.NewScanner(r)
	// 1 MB/line; default 64 KB is too small for orders with many items.
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)

	var eventType, dataLine string
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			c.logger.Info().Msg("Context cancelled — exiting SSE event loop")
			return ctx.Err()
		default:
		}

		line := scanner.Text()
		c.logger.Trace().Str("line", line).Msg("SSE raw line received")

		if line == "" {
			if eventType != "" && dataLine != "" {
				c.dispatch(ctx, eventType, dataLine)
			}
			eventType = ""
			dataLine = ""
			continue
		}

		if rest, ok := strings.CutPrefix(line, "event:"); ok {
			eventType = strings.TrimSpace(rest)
		} else if rest, ok := strings.CutPrefix(line, "data:"); ok {
			dataLine = strings.TrimSpace(rest)
		}
		// Lines starting with `:` are SSE comments / keepalive — silently
		// ignored.
	}

	if err := scanner.Err(); err != nil {
		c.logger.Error().Ctx(ctx).Err(err).Msg("SSE scanner error")
		return err
	}
	c.logger.Info().Ctx(ctx).Msg("SSE stream reached EOF")
	return nil
}

func (c *Client) dispatch(ctx context.Context, eventType, data string) {
	// Any real frame off the stream proves the connection is live end-to-end.
	c.state.lastEventAt.Store(time.Now().Unix())
	switch eventType {
	case "store_config":
		c.handleStoreConfig(ctx, data)
	case "order":
		c.handleOrderEvent(ctx, data)
	case "command":
		if c.commands != nil {
			c.commands.Handle(ctx, []byte(data))
		}
	default:
		c.logger.Debug().Ctx(ctx).Str("event_type", eventType).Msg("SSE event ignored (unknown type)")
	}
}

func (c *Client) handleStoreConfig(ctx context.Context, data string) {
	var sc storeconfig.StoreContext
	if err := json.Unmarshal([]byte(data), &sc); err != nil {
		c.logger.Error().Ctx(ctx).Err(err).Str("raw_data", data).Msg("Failed to unmarshal store_config — skipping")
		return
	}
	c.logger.Info().Ctx(ctx).
		Str("tenant_id", sc.Tenant.ID).
		Str("location_id", sc.Location.ID).
		Str("location_name", sc.Location.Name).
		Int("printer_count", len(sc.Printers)).
		Msg("store_config received from main/")
	if c.storeConfigSink == nil {
		return
	}
	if err := c.storeConfigSink.Apply(ctx, sc); err != nil {
		c.logger.Error().Ctx(ctx).Err(err).Msg("Failed to apply store_config")
	}
}

// tracedOrderEvent is a Snapshot plus an optional inline trace context that
// main/'s broker injects. Trace fields are optional so an uninstrumented main
// would still produce a valid order event.
type tracedOrderEvent struct {
	order.Snapshot
	Traceparent string `json:"_traceparent,omitempty"`
	Tracestate  string `json:"_tracestate,omitempty"`
}

func (c *Client) handleOrderEvent(parentCtx context.Context, data string) {
	var env tracedOrderEvent
	if err := json.Unmarshal([]byte(data), &env); err != nil {
		c.logger.Error().Ctx(parentCtx).Err(err).Str("raw_data", data).
			Msg("Failed to unmarshal SSE order event — skipping")
		return
	}

	linkCtx := parentCtx
	if env.Traceparent != "" {
		carrier := propagation.MapCarrier{"traceparent": env.Traceparent}
		if env.Tracestate != "" {
			carrier["tracestate"] = env.Tracestate
		}
		linkCtx = c.propagator.Extract(parentCtx, carrier)
	}

	snap := env.Snapshot
	ctx, span := c.tracer.Start(linkCtx, "sse.receive",
		trace.WithAttributes(
			attribute.String("order.id", snap.ID),
			attribute.String("order.source", snap.Source),
			attribute.String("order.source_event_id", snap.SourceEventID),
			attribute.String("order.fulfillment_type", snap.Fulfillment.Type),
			attribute.Bool("sse.traceparent.present", env.Traceparent != ""),
		),
	)
	defer span.End()

	// Funnel inflow: one order frame parsed off the SSE stream and about to be
	// handed to the order service.
	c.meters.OrdersReceived.Add(ctx, 1)

	c.logger.Info().Ctx(ctx).
		Str("order_id", snap.ID).
		Str("source_event_id", snap.SourceEventID).
		Str("customer", snap.Customer.DisplayName).
		Str("service_type", snap.Fulfillment.Type).
		Int64("total_cents", snap.Money.Total).
		Int("item_count", len(snap.Items)).
		Msg("Order received via SSE")

	if err := c.service.OnArrival(ctx, snap); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "on-arrival failed")
		c.logger.Error().Ctx(ctx).Err(err).Str("order_id", snap.ID).
			Msg("Failed to handle SSE order")
		return
	}
}
