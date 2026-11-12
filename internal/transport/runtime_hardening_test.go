package transport

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"quiccpos/agent/internal/infra/ssebroker"
	"quiccpos/agent/internal/safego"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// CM-1: http.server.request.duration carries the mux's route although
// capBodies hands the mux a copy of the request.
func TestRequestMetricsKeepTheRoute(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/terminal/orders/{id}/payments", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
	})
	s := &Server{logger: zerolog.Nop()}
	h := s.Handler(mux, otelhttp.WithMeterProvider(mp))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/terminal/orders/abc/payments", strings.NewReader("{}")))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status %d", rec.Code)
	}
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "http.server.request.duration" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Histogram[float64]).DataPoints {
				if v, ok := dp.Attributes.Value("http.route"); ok && v.AsString() == "POST /api/terminal/orders/{id}/payments" ||
					ok && v.AsString() == "/api/terminal/orders/{id}/payments" {
					return
				}
				t.Fatalf("attributes %v: no http.route", dp.Attributes.ToSlice())
			}
		}
	}
	t.Fatal("no http.server.request.duration recorded")
}

// F-1: a handler's panic is a 500 and counted; the server keeps serving.
func TestHandlerPanicIsA500(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /boom", func(http.ResponseWriter, *http.Request) { panic("poison") })
	mux.HandleFunc("GET /ok", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	h := (&Server{logger: zerolog.Nop()}).Handler(mux)
	before := safego.Count()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), `"internal"`) {
		t.Fatalf("panic answered %d %s", rec.Code, rec.Body)
	}
	if safego.Count() != before+1 {
		t.Fatalf("panics counted %d", safego.Count()-before)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ok", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("after the panic: %d", rec.Code)
	}
}

// R-2: a client that stops reading /api/events is dropped within the write
// deadline (its goroutine and broker subscription go away).
func TestStalledEventsClientIsDropped(t *testing.T) {
	eventsWriteTimeout = 300 * time.Millisecond
	t.Cleanup(func() { eventsWriteTimeout = 10 * time.Second })
	broker := ssebroker.New()
	s := &Server{logger: zerolog.Nop(), broker: broker, tabletStreams: map[string]int{}, terminalStreams: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(s.handleEvents))
	defer srv.Close()
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetReadBuffer(4096)
	}
	_, _ = conn.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"))
	if _, err := bufio.NewReader(conn).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	// Never read again; flood the stream until the socket buffers fill.
	big := strings.Repeat("x", 64*1024)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if broker.Subscribers() == 0 {
			return
		}
		broker.Publish("flood", map[string]string{"x": big})
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("subscribers still %d", broker.Subscribers())
}
