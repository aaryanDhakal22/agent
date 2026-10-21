package sse

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

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
