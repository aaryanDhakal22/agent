package sse

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/observability"
)

// After a connection that reached the stream, the next reconnect waits the
// short initial backoff again — it used to keep doubling for the life of the
// process, so after a handful of main restarts every reconnect waited ~60 s.
func TestReconnectBackoffResetsAfterAConnectedStream(t *testing.T) {
	initialBackoff, maxBackoff = 40*time.Millisecond, 2*time.Second
	t.Cleanup(func() { initialBackoff, maxBackoff = 2*time.Second, 60*time.Second })

	var mu sync.Mutex
	var hits []time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits = append(hits, time.Now())
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Close at once: a stream that was up, then dropped.
	}))
	defer srv.Close()

	meters, err := observability.NewMeters()
	if err != nil {
		t.Fatal(err)
	}
	c := New(srv.URL, "key", nil, nil, nil, nil, zerolog.Nop(), meters)
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	c.Start(ctx)

	mu.Lock()
	defer mu.Unlock()
	if len(hits) < 6 {
		// Doubling would allow only 40+80+160+320 ms → 5 connects in 700 ms.
		t.Fatalf("%d connects in 700ms, want ≥ 6 (backoff should stay at %v after a good stream)", len(hits), initialBackoff)
	}
}

func newTestClient(t *testing.T, url string) *Client {
	t.Helper()
	meters, err := observability.NewMeters()
	if err != nil {
		t.Fatal(err)
	}
	return New(url, "key", nil, nil, nil, nil, zerolog.Nop(), meters)
}

// F-5: a stream that answers 200 and then goes silent is given up after the
// idle timeout (and reconnected), instead of blocking the reader forever.
func TestIdleStreamIsDropped(t *testing.T) {
	idleTimeout = 200 * time.Millisecond
	t.Cleanup(func() { idleTimeout = 45 * time.Second })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done() // stall: never a byte
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL)
	start := time.Now()
	err := c.connect(context.Background())
	if err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("connect returned %v after %v, want an idle error within ~0.2 s", err, time.Since(start))
	}
	if !strings.Contains(c.state.Snapshot().LastError, "silent") {
		t.Fatalf("last error %q", c.state.Snapshot().LastError)
	}
}

// F-5: a server that never sends headers doesn't hang the connect.
func TestHeaderTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()
	if baseTransport.ResponseHeaderTimeout != 15*time.Second || baseTransport.TLSHandshakeTimeout != 10*time.Second {
		t.Fatalf("transport timeouts %v / %v", baseTransport.ResponseHeaderTimeout, baseTransport.TLSHandshakeTimeout)
	}
	baseTransport.ResponseHeaderTimeout = 300 * time.Millisecond
	defer func() { baseTransport.ResponseHeaderTimeout = 15 * time.Second }()
	c := newTestClient(t, srv.URL)
	start := time.Now()
	if err := c.connect(context.Background()); err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("connect = %v after %v", err, time.Since(start))
	}
}

// F-13: the Date header of the 200 sets the skew; 5 min off is a problem.
func TestClockSkewFromDateHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", time.Now().Add(5*time.Minute).UTC().Format(http.TimeFormat))
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL)
	_ = c.connect(context.Background())
	st := c.state.Snapshot()
	if !st.SkewKnown || st.ClockSkewSeconds < 290 || st.ClockSkewSeconds > 310 {
		t.Fatalf("skew %+v", st)
	}
}

// F-1: an order frame that panics is recovered; the second panic of the same
// order fires onPoison once, and the reader keeps going.
func TestOrderFramePanicIsRecovered(t *testing.T) {
	c := newTestClient(t, "http://unused") // nil order service: OnArrival panics
	var poisoned []string
	c.state.SetOnPoison(func(_ context.Context, snap order.Snapshot) { poisoned = append(poisoned, snap.ID) })
	frame := `{"id":"0193a000-0000-7000-8000-000000000001","source":"online"}`
	body := "event: order\ndata: " + frame + "\n\nevent: order\ndata: " + frame + "\n\nevent: unknown\ndata: {}\n\n"
	if err := c.readEvents(context.Background(), strings.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	if len(poisoned) != 1 || poisoned[0] != "0193a000-0000-7000-8000-000000000001" {
		t.Fatalf("poisoned %v", poisoned)
	}
}
