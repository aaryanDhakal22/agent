package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"quiccpos/agent/internal/observability"

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

// Handle processes one `command` SSE frame: decode, run the handler, POST the reply.
func (r *Registry) Handle(ctx context.Context, raw []byte) {
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		// No type is known yet — can't attribute this to a command type.
		r.logger.Error().Ctx(ctx).Err(err).Msg("bad command envelope")
		return
	}

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

	r.mu.RLock()
	h, ok := r.handlers[env.Type]
	r.mu.RUnlock()
	if !ok {
		r.meters.RPCCommandsReceived.Add(ctx, 1, metric.WithAttributes(typeAttr, attribute.String("result", "unknown")))
		r.logger.Debug().Ctx(ctx).Str("type", env.Type).Msg("no handler for command type")
		return
	}
	reply, err := h(ctx, env.Payload)
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
