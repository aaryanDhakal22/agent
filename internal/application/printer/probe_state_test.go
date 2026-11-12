package printer

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
	"quiccpos/agent/internal/observability"

	"github.com/rs/zerolog"
)

// L-5 (owner, 2026-11-12): failing for less than downAfter never pages, no
// matter how often; failing longer pages once; "back" only after a "down".
func TestProbeStateTimeWindow(t *testing.T) {
	const x = 30 * time.Second
	var p probeState
	now := time.Unix(1_700_000_000, 0)
	var downs, backs int
	step := func(up bool, d time.Duration) {
		now = now.Add(d)
		switch p.observe(up, now, x) {
		case transitionDown:
			downs++
		case transitionBack:
			backs++
		}
	}
	// Flapping: fail 20 s, then a good probe, again and again.
	for i := 0; i < 10; i++ {
		for j := 0; j < 5; j++ {
			step(false, 4*time.Second)
		}
		step(true, 4*time.Second)
	}
	if downs != 0 || backs != 0 {
		t.Fatalf("failing < %v: downs %d backs %d, want 0/0", x, downs, backs)
	}
	// Really down: failing 60 s pages once.
	for j := 0; j < 15; j++ {
		step(false, 4*time.Second)
	}
	if downs != 1 {
		t.Fatalf("downs %d, want 1", downs)
	}
	step(true, 4*time.Second)
	step(true, 25*time.Second)
	if backs != 1 {
		t.Fatalf("backs %d, want 1", backs)
	}
}

// busyPrinter takes one connection at a time, like many ESC/POS printers: a
// probe while a print holds the connection is refused.
type busyPrinter struct {
	mu      sync.Mutex
	busy    bool
	dead    bool
	release chan struct{}
}

func (b *busyPrinter) Detect(context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.busy || b.dead {
		return errors.New("connection refused")
	}
	return nil
}
func (b *busyPrinter) Name() string { return "Pizza" }
func (b *busyPrinter) IP() string   { return "fake" }
func (b *busyPrinter) SetIP(string) {}
func (b *busyPrinter) Print(context.Context, printer.PrintJob) error {
	b.mu.Lock()
	b.busy = true
	b.mu.Unlock()
	<-b.release
	b.mu.Lock()
	b.busy = false
	b.mu.Unlock()
	return nil
}

// A probe during a long print (refused: the printer is busy) and shortly
// after it never counts toward "down"; a printer that is really gone still
// pages once after the window, and "back" follows only that.
func TestProbeDuringPrintIsNotDown(t *testing.T) {
	meters, _ := observability.NewMeters()
	bp := &busyPrinter{release: make(chan struct{})}
	svc := NewService(bp, NewRegistry(), zerolog.Nop(), meters)
	now := time.Unix(1_700_000_000, 0)
	var mu sync.Mutex
	svc.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
	svc.downAfter = func() time.Duration { return 30 * time.Second }
	var events []bool
	svc.onStatus = func(up bool, _ string) { events = append(events, up) }

	printed := make(chan error, 1)
	go func() {
		printed <- svc.PrintWith(context.Background(), order.Snapshot{ID: "o1"}, func(order.Snapshot) []byte { return []byte("x") })
	}()
	for svc.inFlight.Load() == 0 || !func() bool { bp.mu.Lock(); defer bp.mu.Unlock(); return bp.busy }() {
		time.Sleep(time.Millisecond)
	}
	var st probeState
	for i := 0; i < 20; i++ { // 80 s of refused probes while printing
		advance(4 * time.Second)
		if !svc.probeStep(context.Background(), &st, nil) {
			t.Fatal("a probe during a print counted as a failure")
		}
	}
	close(bp.release)
	if err := <-printed; err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 || st.down {
		t.Fatalf("paged during a print: %v", events)
	}
	// Now really dead: excused for 30 s after the good print, then failing
	// for 30 s more → one "down".
	bp.mu.Lock()
	bp.dead = true
	bp.mu.Unlock()
	for i := 0; i < 20; i++ {
		advance(4 * time.Second)
		svc.probeStep(context.Background(), &st, nil)
	}
	bp.mu.Lock()
	bp.dead = false
	bp.mu.Unlock()
	advance(4 * time.Second)
	svc.probeStep(context.Background(), &st, nil)
	if len(events) != 2 || events[0] || !events[1] {
		t.Fatalf("events %v, want [down, back]", events)
	}
}
