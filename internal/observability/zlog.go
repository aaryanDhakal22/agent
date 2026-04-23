package observability

import (
	"github.com/rs/zerolog"
	"go.opentelemetry.io/contrib/bridges/otelzerolog"
	"go.opentelemetry.io/otel/trace"
)

// Wire attaches two hooks to a zerolog.Logger:
//   - traceHook copies trace_id/span_id from the event's context (attached
//     via zerolog Event.Ctx(ctx)) onto the event so human-readable logs can
//     be grepped / joined to a trace.
//   - otelzerolog.NewHook bridges every zerolog event into an OTLP log
//     record (respecting the global LoggerProvider installed by Setup).
//
// If observability.Setup was called without an OTLP endpoint, the bridge
// still runs but targets the no-op LoggerProvider — harmless.
func Wire(base zerolog.Logger, serviceName string) zerolog.Logger {
	return base.Hook(traceHook{}).Hook(otelzerolog.NewHook(serviceName))
}

type traceHook struct{}

func (traceHook) Run(e *zerolog.Event, _ zerolog.Level, _ string) {
	ctx := e.GetCtx()
	if ctx == nil {
		return
	}
	sc := trace.SpanFromContext(ctx).SpanContext()
	if !sc.IsValid() {
		return
	}
	e.Str("trace_id", sc.TraceID().String()).
		Str("span_id", sc.SpanID().String())
}
