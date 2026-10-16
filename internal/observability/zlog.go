package observability

import (
	"context"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/trace"
)

// Wire attaches two hooks to a zerolog.Logger:
//   - traceHook copies trace_id/span_id from the event's context (attached
//     via zerolog Event.Ctx(ctx)) onto the event so human-readable logs can
//     be grepped / joined to a trace.
//   - otelLogHook bridges every zerolog event into an OTLP log
//     record (respecting the global LoggerProvider installed by Setup).
//
// If observability.Setup was called without an OTLP endpoint, the bridge
// still runs but targets the no-op LoggerProvider — harmless.
func Wire(base zerolog.Logger, serviceName string) zerolog.Logger {
	return base.Hook(traceHook{}).Hook(newOtelLogHook(serviceName))
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

// otelLogHook bridges zerolog events into OTLP log records via the global
// LoggerProvider. It replaces the upstream contrib otelzerolog bridge, which
// was abandoned unreleased in 2024 and doesn't build against otel/log v1.x.
// Like upstream, fields aren't copied (zerolog can't enumerate them —
// rs/zerolog#493); the message, severity and trace context are.
type otelLogHook struct {
	logger otellog.Logger
}

func newOtelLogHook(name string) otelLogHook {
	return otelLogHook{logger: global.GetLoggerProvider().Logger(name)}
}

func (h otelLogHook) Run(e *zerolog.Event, level zerolog.Level, msg string) {
	var r otellog.Record
	r.SetSeverity(otelSeverity(level))
	r.SetSeverityText(level.String())
	r.SetBody(attribute.StringValue(msg))
	ctx := e.GetCtx()
	if ctx == nil {
		ctx = context.Background()
	}
	h.logger.Emit(ctx, r)
}

func otelSeverity(level zerolog.Level) otellog.Severity {
	switch level {
	case zerolog.TraceLevel:
		return otellog.SeverityTrace
	case zerolog.DebugLevel:
		return otellog.SeverityDebug
	case zerolog.InfoLevel:
		return otellog.SeverityInfo
	case zerolog.WarnLevel:
		return otellog.SeverityWarn
	case zerolog.ErrorLevel:
		return otellog.SeverityError
	case zerolog.PanicLevel:
		return otellog.SeverityFatal1
	case zerolog.FatalLevel:
		return otellog.SeverityFatal2
	default:
		return otellog.SeverityUndefined
	}
}
