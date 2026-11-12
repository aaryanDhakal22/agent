package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"quiccpos/agent/internal/observability"
	"quiccpos/agent/internal/safego"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

const tracerName = "quiccpos/agent/rpc"

// ReplyPoster sends an agent→main RPC envelope (POST /store/v1/agent/rpc with
// the store key). Satisfied by a thin adapter over mainclient in cmd/main.go.
type ReplyPoster interface {
	PostRPC(ctx context.Context, body []byte) error
}

// Exchanger is a ReplyPoster that can also hand back main's answer (HTTP
// status + body), for the reports whose reply matters (Registry.Send).
type Exchanger interface {
	ExchangeRPC(ctx context.Context, body []byte) (status int, respBody []byte, err error)
}

// CommandHandler processes a main→agent command; a non-nil reply is POSTed back
// with the request's correlation id.
type CommandHandler func(ctx context.Context, payload json.RawMessage) (any, error)

// Registry maps command types to handlers and knows how to reply/report.
type Registry struct {
	poster   ReplyPoster
	logger   zerolog.Logger
	meters   observability.Meters
	tracer   trace.Tracer
	mu       sync.RWMutex
	handlers map[string]CommandHandler
	async    map[string]*asyncHandler // the types that run off the SSE reader
}

func NewRegistry(poster ReplyPoster, logger zerolog.Logger, meters observability.Meters) *Registry {
	return &Registry{
		poster:   poster,
		logger:   logger.With().Str("module", "rpc-registry").Logger(),
		meters:   meters,
		tracer:   otel.Tracer(tracerName),
		handlers: make(map[string]CommandHandler),
	}
}

func (r *Registry) Register(msgType string, h CommandHandler) {
	r.mu.Lock()
	r.handlers[msgType] = h
	r.mu.Unlock()
}

// AsyncOptions bound a command type that runs off the SSE reader.
type AsyncOptions struct {
	// Max: handlers running at once (at least 1).
	Max int
	// Wait: how long a command over Max waits for a slot (in its own
	// goroutine) before it is answered Busy; 0 = answered at once.
	Wait time.Duration
	// MaxWaiting: commands waiting for a slot at once; one over that is
	// answered Busy at once.
	MaxWaiting int
	// Busy: the reply of a command that got no slot.
	Busy any
}

// asyncHandler is a command type that runs off the SSE reader (RegisterAsync).
type asyncHandler struct {
	opts    AsyncOptions
	sem     chan struct{} // in-flight bound
	waiting atomic.Int64  // goroutines waiting for a slot
	mu      sync.Mutex
	seen    map[string]time.Time // envelope ids already taken (dedupe)
}

// Async command dedupe: an envelope id is remembered this long, at most
// this many (a command fanned out twice never runs twice).
const (
	asyncSeenTTL = 2 * time.Minute
	asyncSeenMax = 500
)

// RegisterAsync registers a command type whose handler may take seconds
// (terminal.relay: a relayed card payment): it runs in its own goroutine
// and posts its reply from there, so the SSE reader goes on with the next
// frame (orders, store_config, other commands) and the idle watchdog never
// trips. At most opts.Max run at once; a command over that waits for a slot
// up to opts.Wait in its own goroutine (never on the reader; at most
// opts.MaxWaiting wait) and is answered opts.Busy when it gets none. An
// envelope id seen in the last 2 min is dropped. The handler's context is
// not the frame's: it outlives a dropped stream (the handler sets its own
// deadline).
func (r *Registry) RegisterAsync(msgType string, h CommandHandler, opts AsyncOptions) {
	if opts.Max < 1 {
		opts.Max = 1
	}
	r.mu.Lock()
	r.handlers[msgType] = h
	if r.async == nil {
		r.async = map[string]*asyncHandler{}
	}
	r.async[msgType] = &asyncHandler{opts: opts, sem: make(chan struct{}, opts.Max), seen: map[string]time.Time{}}
	r.mu.Unlock()
}

// first reports whether id is new (and remembers it).
func (a *asyncHandler) first(id string, now time.Time) bool {
	if id == "" {
		return true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if t, ok := a.seen[id]; ok && now.Sub(t) < asyncSeenTTL {
		return false
	}
	if len(a.seen) >= asyncSeenMax {
		var oldest string
		var at time.Time
		for k, t := range a.seen {
			if now.Sub(t) >= asyncSeenTTL {
				delete(a.seen, k)
				continue
			}
			if oldest == "" || t.Before(at) {
				oldest, at = k, t
			}
		}
		if len(a.seen) >= asyncSeenMax {
			delete(a.seen, oldest)
		}
	}
	a.seen[id] = now
	return true
}

// Handle processes one `command` SSE frame: decode, run the handler, POST the reply.
func (r *Registry) Handle(ctx context.Context, raw []byte) {
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		// No type is known yet — can't attribute this to a command type.
		r.logger.Error().Ctx(ctx).Err(err).Msg("bad command envelope")
		return
	}
	typeAttr := attribute.String("type", env.Type)
	r.mu.RLock()
	h, ok := r.handlers[env.Type]
	async := r.async[env.Type]
	r.mu.RUnlock()
	if !ok {
		r.meters.RPCCommandsReceived.Add(ctx, 1, metric.WithAttributes(typeAttr, attribute.String("result", "unknown")))
		r.logger.Debug().Ctx(ctx).Str("type", env.Type).Msg("no handler for command type")
		return
	}
	if async == nil {
		r.run(ctx, env, h)
		return
	}
	if !async.first(env.ID, time.Now()) {
		r.meters.RPCCommandsReceived.Add(ctx, 1, metric.WithAttributes(typeAttr, attribute.String("result", "duplicate")))
		r.logger.Warn().Ctx(ctx).Str("type", env.Type).Str("id", env.ID).Msg("command id seen already: dropped")
		return
	}
	actx := context.WithoutCancel(ctx) // keeps the trace, not the frame's life
	busy := func(why string) {
		r.meters.RPCCommandsReceived.Add(actx, 1, metric.WithAttributes(typeAttr, attribute.String("result", "busy")))
		r.logger.Warn().Ctx(actx).Str("type", env.Type).Int("in_flight", cap(async.sem)).Int64("waiting", async.waiting.Load()).
			Str("why", why).Msg("too many commands in flight: answered busy")
		r.run(actx, env, func(context.Context, json.RawMessage) (any, error) { return async.opts.Busy, nil })
	}
	select {
	case async.sem <- struct{}{}: // a free slot: nothing waits
		safego.Go("rpc."+env.Type, func() {
			defer func() { <-async.sem }()
			r.run(actx, env, h)
		})
		return
	default:
	}
	if async.opts.Wait <= 0 || async.waiting.Add(1) > int64(async.opts.MaxWaiting) {
		if async.opts.Wait > 0 {
			async.waiting.Add(-1)
		}
		safego.Go("rpc."+env.Type+".busy", func() { busy("no_room_to_wait") })
		return
	}
	// The wait for a slot happens here, in the command's own goroutine.
	safego.Go("rpc."+env.Type, func() {
		t := time.NewTimer(async.opts.Wait)
		defer t.Stop()
		select {
		case async.sem <- struct{}{}:
			async.waiting.Add(-1)
			defer func() { <-async.sem }()
			r.run(actx, env, h)
		case <-t.C:
			async.waiting.Add(-1)
			busy("no_slot_in_time")
		}
	})
}

// run runs one command's handler and POSTs its reply.
func (r *Registry) run(ctx context.Context, env Envelope, h CommandHandler) {
	typeAttr := attribute.String("type", env.Type)
	ctx, span := r.tracer.Start(ctx, "rpc.command.handle",
		trace.WithAttributes(attribute.String("rpc.type", env.Type)),
	)
	defer span.End()
	start := time.Now()
	defer func() {
		ms := float64(time.Since(start).Microseconds()) / 1000.0
		r.meters.RPCHandleMs.Record(ctx, ms, metric.WithAttributes(typeAttr))
	}()

	// One handler's panic is recovered (logged with its stack, counted on
	// agent.panics) and answered like an error: the SSE reader goes on.
	var reply any
	var err error
	if perr := safego.Do(ctx, "rpc."+env.Type, func() { reply, err = h(ctx, env.Payload) }); perr != nil {
		err = perr
	}
	if err != nil {
		r.meters.RPCCommandsReceived.Add(ctx, 1, metric.WithAttributes(typeAttr, attribute.String("result", "error")))
		span.RecordError(err)
		span.SetStatus(codes.Error, "handler failed")
		r.logger.Error().Ctx(ctx).Err(err).Str("type", env.Type).Msg("command handler failed")
		return
	}
	r.meters.RPCCommandsReceived.Add(ctx, 1, metric.WithAttributes(typeAttr, attribute.String("result", "ok")))

	rawReply, err := marshal(reply)
	if err != nil {
		span.RecordError(err)
		r.logger.Error().Ctx(ctx).Err(err).Msg("marshal reply")
		return
	}
	out, _ := json.Marshal(Envelope{Type: env.Type + ".reply", ID: env.ID, Payload: rawReply})
	if err := r.poster.PostRPC(ctx, out); err != nil {
		r.meters.RPCRepliesSent.Add(ctx, 1, metric.WithAttributes(typeAttr, attribute.String("result", "post_error")))
		span.RecordError(err)
		r.logger.Error().Ctx(ctx).Err(err).Str("type", env.Type).Msg("post reply failed")
		return
	}
	r.meters.RPCRepliesSent.Add(ctx, 1, metric.WithAttributes(typeAttr, attribute.String("result", "ok")))
}

// Report sends an unsolicited agent→main message (fresh correlation id).
func (r *Registry) Report(ctx context.Context, msgType string, payload any) error {
	typeAttr := attribute.String("type", msgType)
	ctx, span := r.tracer.Start(ctx, "rpc.report.send",
		trace.WithAttributes(attribute.String("rpc.type", msgType)),
	)
	defer span.End()

	raw, err := marshal(payload)
	if err != nil {
		span.RecordError(err)
		return err
	}
	out, err := json.Marshal(Envelope{Type: msgType, ID: uuid.NewString(), Payload: raw})
	if err != nil {
		span.RecordError(err)
		return err
	}
	if err := r.poster.PostRPC(ctx, out); err != nil {
		r.meters.RPCReportsSent.Add(ctx, 1, metric.WithAttributes(typeAttr, attribute.String("result", "post_error")))
		span.RecordError(err)
		span.SetStatus(codes.Error, "post failed")
		return err
	}
	r.meters.RPCReportsSent.Add(ctx, 1, metric.WithAttributes(typeAttr, attribute.String("result", "ok")))
	return nil
}

// Send is Report for a message whose reply matters: it returns main's HTTP
// status and, on a 200, the reply envelope's payload (any other status: the
// raw body). The poster must implement Exchanger.
func (r *Registry) Send(ctx context.Context, msgType string, payload any) (int, json.RawMessage, error) {
	ex, ok := r.poster.(Exchanger)
	if !ok {
		return 0, nil, errors.New("rpc: poster can't return replies")
	}
	typeAttr := attribute.String("type", msgType)
	ctx, span := r.tracer.Start(ctx, "rpc.report.send",
		trace.WithAttributes(attribute.String("rpc.type", msgType)),
	)
	defer span.End()

	raw, err := marshal(payload)
	if err != nil {
		span.RecordError(err)
		return 0, nil, err
	}
	out, err := json.Marshal(Envelope{Type: msgType, ID: uuid.NewString(), Payload: raw})
	if err != nil {
		span.RecordError(err)
		return 0, nil, err
	}
	status, body, err := ex.ExchangeRPC(ctx, out)
	if err != nil {
		r.meters.RPCReportsSent.Add(ctx, 1, metric.WithAttributes(typeAttr, attribute.String("result", "post_error")))
		span.RecordError(err)
		span.SetStatus(codes.Error, "post failed")
		return 0, nil, err
	}
	span.SetAttributes(attribute.Int("http.status_code", status))
	result := "ok"
	if status >= 300 {
		result = "rejected"
	}
	r.meters.RPCReportsSent.Add(ctx, 1, metric.WithAttributes(typeAttr, attribute.String("result", result)))
	if status == 200 {
		var env Envelope
		if json.Unmarshal(body, &env) == nil {
			return status, env.Payload, nil
		}
	}
	return status, body, nil
}

func marshal(p any) (json.RawMessage, error) {
	if p == nil {
		return nil, nil
	}
	if raw, ok := p.(json.RawMessage); ok {
		return raw, nil
	}
	b, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	return b, nil
}
