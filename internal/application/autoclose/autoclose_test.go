package autoclose

import (
	"context"
	"sync"
	"testing"
	"time"

	orderApp "quiccpos/agent/internal/application/order"

	"github.com/rs/zerolog"
)

type fakeCloser struct {
	mu    sync.Mutex
	calls []string
	done  chan struct{}
}

func (f *fakeCloser) AutoCloseDue(_ context.Context, now time.Time, loc *time.Location, at string) (orderApp.AutoCloseResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, now.In(loc).Format("15:04")+"@"+at)
	f.mu.Unlock()
	f.done <- struct{}{}
	return orderApp.AutoCloseResult{}, nil
}

// The loop checks at each tick with the injected clock, setting and zone.
func TestRunChecksEachTickWithTheInjectedClock(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticks := make(chan time.Time)
	f := &fakeCloser{done: make(chan struct{}, 4)}
	ny, _ := time.LoadLocation("America/New_York")
	clock := time.Date(2026, 11, 12, 4, 0, 0, 0, ny)
	setting := "04:00"
	go Run(ctx, f, Deps{Setting: func() string { return setting }, Loc: func() *time.Location { return ny },
		Now: func() time.Time { return clock }, Ticks: ticks}, zerolog.Nop())
	ticks <- time.Time{}
	<-f.done
	ticks <- time.Time{}
	<-f.done
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) != 2 || f.calls[0] != "04:00@04:00" {
		t.Fatalf("calls = %v", f.calls)
	}
}
