package sse

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func testClient(url string, idle time.Duration) *Client {
	return &Client{serverURL: url, apiKey: "k", idleTimeout: idle, logger: zerolog.Nop()}
}

// A stream that sends headers and then goes silent (half-open, stalled proxy)
// is dropped after the idle timeout instead of hanging forever.
func TestConnectDropsASilentStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	connects := 0
	c := testClient(srv.URL, 200*time.Millisecond)
	c.OnConnect(func() { connects++ })
	start := time.Now()
	_, err := c.connect(context.Background())
	if err == nil || !strings.Contains(err.Error(), "keepalives stopped") {
		t.Fatalf("err = %v, want the idle-timeout error", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("took %v to notice the silent stream", d)
	}
	if connects != 1 {
		t.Fatalf("OnConnect ran %d times, want 1", connects)
	}
}

// A server that never answers the request is cut off by the same timer.
func TestConnectGivesUpOnNoHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := testClient(srv.URL, 200*time.Millisecond)
	c.OnConnect(func() { t.Error("OnConnect ran without a 200") })
	up, err := c.connect(context.Background())
	if err == nil || !strings.Contains(err.Error(), "keepalives stopped") || up != 0 {
		t.Fatalf("up=%v err=%v", up, err)
	}
}

// Keepalives keep the stream alive past the idle timeout, and connect reports
// how long it was up.
func TestKeepalivesKeepTheStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, ": connected\n\n")
		w.(http.Flusher).Flush()
		for i := 0; i < 8; i++ {
			time.Sleep(50 * time.Millisecond)
			fmt.Fprint(w, ": keepalive\n\n")
			w.(http.Flusher).Flush()
		}
	}))
	defer srv.Close()

	up, err := testClient(srv.URL, 200*time.Millisecond).connect(context.Background())
	if err != nil {
		t.Fatalf("err = %v, want a clean close", err)
	}
	if up < 350*time.Millisecond {
		t.Fatalf("up = %v, want ~400ms", up)
	}
}
