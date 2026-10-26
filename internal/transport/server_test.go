package transport

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// TestTraceIDHeader proves the X-Trace-Id response header carries the
// otelhttp server span's trace id when a real TracerProvider is installed.
func TestTraceIDHeader(t *testing.T) {
	tp := sdktrace.NewTracerProvider()
	t.Cleanup(func() { _ = tp.Shutdown(t.Context()) })

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := otelhttp.NewHandler(traceIDHeader(inner), "test",
		otelhttp.WithTracerProvider(tp),
	)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/orders", nil))

	got := rec.Header().Get("X-Trace-Id")
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(got) {
		t.Fatalf("X-Trace-Id = %q, want 32-hex trace id", got)
	}
}

// TestTraceIDHeaderNoopTracer proves the header is simply absent (not
// garbage) when OTEL is disabled and the global no-op provider is in play.
func TestTraceIDHeaderNoopTracer(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	traceIDHeader(inner).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/orders", nil))

	if got := rec.Header().Get("X-Trace-Id"); got != "" {
		t.Fatalf("X-Trace-Id = %q, want empty without an active span", got)
	}
}

// floor.tablets.connected counts tablets, not streams.
func TestTabletsConnectedCountsDistinctTablets(t *testing.T) {
	s := &Server{tabletStreams: map[string]int{}}
	offA1 := s.tabletOnStream("a")
	offA2 := s.tabletOnStream("a") // a reconnect overlapping the old stream
	offB := s.tabletOnStream("b")
	if n := s.TabletsConnected(); n != 2 {
		t.Fatalf("connected = %d, want 2", n)
	}
	offA1()
	offB()
	if n := s.TabletsConnected(); n != 1 {
		t.Fatalf("connected = %d, want 1", n)
	}
	offA2()
	if n := s.TabletsConnected(); n != 0 {
		t.Fatalf("connected = %d, want 0", n)
	}
}
