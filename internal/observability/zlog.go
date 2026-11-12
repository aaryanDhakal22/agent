package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"time"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/trace"
)

// Wire attaches the trace hook to a zerolog.Logger: it copies
// trace_id/span_id from the event's context (attached via zerolog
// Event.Ctx(ctx)) onto the event, so human-readable logs can be grepped /
// joined to a trace, and LogWriter can give the OTLP record its trace
// context. The OTLP bridge itself is LogWriter (CM-0): build the logger on
// it, then Wire it.
func Wire(base zerolog.Logger, _ string) zerolog.Logger {
	return base.Hook(traceHook{})
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

// LogWriter tees every zerolog line to out and to an OTLP log record
// (hardening CM-0). A zerolog hook can't see an event's fields
// (rs/zerolog#493), so the bridge sits at the writer: it gets each event's
// JSON, and every top-level key becomes a log attribute (Loki keeps them as
// structured metadata) — order_id, amount_cents, staff_id … — except
// level / time / message (the record's own severity, timestamp and body),
// trace_id / span_id (the record's trace context) and the secret keys
// (pin, manager_pin, password, …: never shipped even if a line had one).
// With no OTLP endpoint the global provider is a no-op and nothing is parsed.
func LogWriter(out io.Writer, serviceName string) zerolog.LevelWriter {
	return &otlpWriter{out: out, logger: global.GetLoggerProvider().Logger(serviceName)}
}

type otlpWriter struct {
	out    io.Writer
	logger otellog.Logger
}

// secretLogKeys never leave the Pi as log attributes.
var secretLogKeys = map[string]bool{
	"pin": true, "manager_pin": true, "password": true, "secret": true, "token": true,
	"key": true, "api_key": true, "device_key": true, "store_api_key": true,
}

func (w *otlpWriter) Write(p []byte) (int, error) {
	return w.WriteLevel(zerolog.NoLevel, p)
}

func (w *otlpWriter) WriteLevel(level zerolog.Level, p []byte) (int, error) {
	w.emit(level, p)
	if lw, ok := w.out.(zerolog.LevelWriter); ok {
		return lw.WriteLevel(level, p)
	}
	return w.out.Write(p)
}

func (w *otlpWriter) emit(level zerolog.Level, p []byte) {
	sev := otelSeverity(level)
	if !w.logger.Enabled(context.Background(), otellog.EnabledParameters{Severity: sev}) {
		return
	}
	dec := json.NewDecoder(bytes.NewReader(p))
	dec.UseNumber()
	var fields map[string]any
	if err := dec.Decode(&fields); err != nil {
		return
	}
	var r otellog.Record
	r.SetTimestamp(time.Now())
	r.SetSeverity(sev)
	r.SetSeverityText(level.String())
	msg, _ := fields["message"].(string)
	r.SetBody(attribute.StringValue(msg))
	ctx := context.Background()
	traceID, _ := fields["trace_id"].(string)
	spanID, _ := fields["span_id"].(string)
	if tid, err := trace.TraceIDFromHex(traceID); err == nil {
		if sid, err := trace.SpanIDFromHex(spanID); err == nil {
			ctx = trace.ContextWithSpanContext(ctx, trace.NewSpanContext(trace.SpanContextConfig{
				TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled, Remote: true}))
		}
	}
	attrs := make([]attribute.KeyValue, 0, len(fields))
	for k, v := range fields {
		switch k {
		case "level", "time", "message", "trace_id", "span_id":
			continue
		}
		if secretLogKeys[k] {
			continue
		}
		attrs = append(attrs, logAttr(k, v))
	}
	r.AddAttributes(attrs...)
	w.logger.Emit(ctx, r)
}

// logAttr converts one decoded JSON value: strings, integers, floats and
// bools keep their type; arrays and objects go as their JSON text.
func logAttr(k string, v any) attribute.KeyValue {
	switch x := v.(type) {
	case string:
		return attribute.String(k, x)
	case bool:
		return attribute.Bool(k, x)
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return attribute.Int64(k, i)
		}
		if f, err := x.Float64(); err == nil {
			return attribute.Float64(k, f)
		}
		return attribute.String(k, x.String())
	case nil:
		return attribute.String(k, "")
	default:
		b, _ := json.Marshal(x)
		return attribute.String(k, string(b))
	}
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
