package transport

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"quiccpos/agent/internal/infra/ssebroker"

	"github.com/rs/zerolog"
)

// flushProbe records how many broker subscribers there were when the headers
// were first flushed.
type flushProbe struct {
	*httptest.ResponseRecorder
	broker *ssebroker.Broker
	once   sync.Once
	atHead chan int
}

func (f *flushProbe) Flush() {
	f.once.Do(func() { f.atHead <- f.broker.Subscribers() })
	f.ResponseRecorder.Flush()
}

// /api/events subscribes before its headers go out: a tablet refetches the
// floor as soon as it sees them, so an event published right after (an
// online order placed on arrival, §2.10aa: one order.updated, no ring) must
// reach it - never fall in a gap before the subscription.
func TestEventsSubscribedBeforeHeaders(t *testing.T) {
	broker := ssebroker.New()
	s := &Server{logger: zerolog.Nop(), broker: broker, tabletStreams: map[string]int{}, terminalStreams: map[string]int{}}
	ctx, cancel := context.WithCancel(context.Background())
	w := &flushProbe{ResponseRecorder: httptest.NewRecorder(), broker: broker, atHead: make(chan int, 1)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.handleEvents(w, httptest.NewRequest(http.MethodGet, "/api/events", nil).WithContext(ctx))
	}()
	select {
	case n := <-w.atHead:
		if n != 1 {
			t.Fatalf("subscribers when the headers went out: %d (the subscription must come first)", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no headers")
	}
	cancel()
	<-done
}
