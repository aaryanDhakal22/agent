package observability

import (
	"bytes"
	"context"
	"sync"
	"testing"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/trace"
)

type memExporter struct {
	mu   sync.Mutex
	recs []sdklog.Record
}

func (m *memExporter) Export(_ context.Context, rs []sdklog.Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range rs {
		m.recs = append(m.recs, r.Clone())
	}
	return nil
}
func (m *memExporter) Shutdown(context.Context) error   { return nil }
func (m *memExporter) ForceFlush(context.Context) error { return nil }

// CM-0: every top-level field reaches the OTLP record as an attribute (order
// id, cents …), the trace context rides along, and a PIN never leaves.
func TestLogWriterShipsFields(t *testing.T) {
	exp := &memExporter{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(exp)))
	var out bytes.Buffer
	w := &otlpWriter{out: &out, logger: lp.Logger("test")}
	lg := Wire(zerolog.New(w), "test")
	tid, _ := trace.TraceIDFromHex("0102030405060708090a0b0c0d0e0f10")
	sid, _ := trace.SpanIDFromHex("0102030405060708")
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled}))
	lg.Info().Ctx(ctx).Str("order_id", "o-1").Int64("amount_cents", 2357).Bool("refund", true).
		Str("pin", "1234").Str("manager_pin", "9876").Strs("issues", []string{"a"}).Msg("Counter payment taken")

	if !bytes.Contains(out.Bytes(), []byte(`"order_id":"o-1"`)) {
		t.Fatalf("the local line lost its fields: %s", out.String())
	}
	if len(exp.recs) != 1 {
		t.Fatalf("records = %d", len(exp.recs))
	}
	r := exp.recs[0]
	if r.Body().AsString() != "Counter payment taken" {
		t.Fatalf("body %q", r.Body().AsString())
	}
	if r.TraceID() != tid || r.SpanID() != sid {
		t.Fatalf("trace context %v %v", r.TraceID(), r.SpanID())
	}
	got := map[string]attribute.Value{}
	r.WalkAttributes(func(kv attribute.KeyValue) bool { got[string(kv.Key)] = kv.Value; return true })
	if got["order_id"].AsString() != "o-1" || got["amount_cents"].AsInt64() != 2357 || !got["refund"].AsBool() {
		t.Fatalf("attributes %v", got)
	}
	if got["issues"].AsString() != `["a"]` {
		t.Fatalf("array attribute %v", got["issues"])
	}
	for _, k := range []string{"pin", "manager_pin", "message", "level", "trace_id"} {
		if _, ok := got[k]; ok {
			t.Fatalf("%s shipped as an attribute", k)
		}
	}
}
