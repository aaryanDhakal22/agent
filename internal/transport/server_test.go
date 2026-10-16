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
