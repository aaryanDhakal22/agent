package rpc

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"quiccpos/agent/internal/observability"

	"github.com/rs/zerolog"
)

// RegisterAsync (terminal.relay): the handler runs off the SSE reader, at
// most N at once (the next waits for a slot a moment, in its own goroutine,
// then is answered busy), a repeated envelope id runs once.

type replyLog struct {
	mu      sync.Mutex
	replies map[string]json.RawMessage // envelope id -> the reply's payload
	types   map[string]string
}

func (l *replyLog) PostRPC(_ context.Context, body []byte) error {
	var env Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.replies[env.ID], l.types[env.ID] = env.Payload, env.Type
	return nil
}

func (l *replyLog) get(id string) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	p, ok := l.replies[id]
	return string(p), ok
}

func (l *replyLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.replies)
}

func asyncRegistry(t *testing.T) (*Registry, *replyLog) {
	t.Helper()
	meters, err := observability.NewMeters()
	if err != nil {
		t.Fatal(err)
	}
	log := &replyLog{replies: map[string]json.RawMessage{}, types: map[string]string{}}
	return NewRegistry(log, zerolog.Nop(), meters), log
}

func until(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func frame(typ, id string) []byte {
	return []byte(fmt.Sprintf(`{"type":%q,"id":%q,"payload":{"n":1}}`, typ, id))
}

// A slow relay (a card charge) doesn't hold the SSE reader: the next
// command is handled while it still runs.
func TestAsyncCommandDoesNotBlockTheReader(t *testing.T) {
	r, log := asyncRegistry(t)
	release := make(chan struct{})
	started := make(chan struct{})
	r.RegisterAsync(MsgTerminalRelay, func(ctx context.Context, _ json.RawMessage) (any, error) {
		close(started)
		<-release
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return map[string]int{"status": 200}, nil
	}, AsyncOptions{Max: 16, Wait: 2 * time.Second, MaxWaiting: 64, Busy: map[string]int{"status": 503}})
	r.Register(MsgPing, func(context.Context, json.RawMessage) (any, error) { return map[string]bool{"pong": true}, nil })

	// The frame's context ends with the frame (a dropped stream): the relay goes on.
	fctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.Handle(fctx, frame(MsgTerminalRelay, "slow-1"))
		r.Handle(fctx, frame(MsgPing, "ping-1"))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the reader is stuck behind the slow relay")
	}
	<-started
	if got, ok := log.get("ping-1"); !ok || got != `{"pong":true}` {
		t.Fatalf("the ping after the relay was answered first: %q %v", got, ok)
	}
	if _, ok := log.get("slow-1"); ok {
		t.Fatal("the relay hasn't answered yet")
	}
	cancel()
	close(release)
	until(t, "the relay's reply", func() bool { _, ok := log.get("slow-1"); return ok })
	if got, _ := log.get("slow-1"); got != `{"status":200}` || log.types["slow-1"] != MsgTerminalRelay+".reply" {
		t.Fatalf("the relay's reply: %q (%s)", got, log.types["slow-1"])
	}
}

// 16 in flight: the next ones wait for a slot in their own goroutines (the
// reader never does); one that gets a slot in time runs, one that doesn't is
// answered busy, and over the cap of waiting ones busy comes at once.
func TestAsyncCommandWaitsForASlotThenBusy(t *testing.T) {
	r, log := asyncRegistry(t)
	release := make(chan struct{})
	var mu sync.Mutex
	running := 0
	const busy = `{"body":{"code":"busy"},"status":503}`
	r.RegisterAsync(MsgTerminalRelay, func(_ context.Context, p json.RawMessage) (any, error) {
		mu.Lock()
		running++
		mu.Unlock()
		<-release
		return map[string]int{"status": 200}, nil
	}, AsyncOptions{Max: 16, Wait: 300 * time.Millisecond, MaxWaiting: 3, Busy: map[string]any{"status": 503, "body": map[string]string{"code": "busy"}}})
	send := func(id string) {
		t.Helper()
		start := time.Now()
		r.Handle(context.Background(), frame(MsgTerminalRelay, id))
		if d := time.Since(start); d > 100*time.Millisecond {
			t.Fatalf("%s held the reader for %s", id, d)
		}
	}
	for i := 1; i <= 16; i++ {
		send(fmt.Sprintf("r-%d", i))
	}
	until(t, "16 relays running", func() bool { mu.Lock(); defer mu.Unlock(); return running == 16 })
	// Three wait for a slot; a fourth has no room to wait: busy at once.
	sent := time.Now()
	send("w-1")
	send("w-2")
	send("w-3")
	send("over-1")
	until(t, "busy for the one over the waiting cap", func() bool { _, ok := log.get("over-1"); return ok })
	if got, _ := log.get("over-1"); got != busy || time.Since(sent) > 250*time.Millisecond {
		t.Fatalf("over the cap: %s after %s (want busy at once)", got, time.Since(sent))
	}
	if log.count() != 1 {
		t.Fatalf("the waiting ones aren't answered yet: %d replies", log.count())
	}
	// No slot within the wait: busy, and they never ran.
	until(t, "busy for the three that waited", func() bool { return log.count() == 4 })
	if waited := time.Since(sent); waited < 300*time.Millisecond {
		t.Fatalf("answered busy after %s, before the wait was over", waited)
	}
	for _, id := range []string{"w-1", "w-2", "w-3"} {
		if got, _ := log.get(id); got != busy {
			t.Fatalf("%s: %s", id, got)
		}
	}
	mu.Lock()
	if running != 16 {
		t.Fatalf("a busy command ran: running %d", running)
	}
	mu.Unlock()
	// One that waits gets the slot a finished relay frees.
	send("w-4")
	close(release)
	until(t, "the 16 replies and the one that waited", func() bool { got, ok := log.get("w-4"); return ok && got == `{"status":200}` && log.count() == 21 })
	for i := 1; i <= 16; i++ {
		if got, _ := log.get(fmt.Sprintf("r-%d", i)); got != `{"status":200}` {
			t.Fatalf("r-%d: %s", i, got)
		}
	}
	if w := r.async[MsgTerminalRelay].waiting.Load(); w != 0 {
		t.Fatalf("waiting count %d after everything answered", w)
	}
	// Wait 0: busy at once (the old rule).
	r2, log2 := asyncRegistry(t)
	hold := make(chan struct{})
	defer close(hold)
	r2.RegisterAsync(MsgTerminalRelay, func(context.Context, json.RawMessage) (any, error) { <-hold; return nil, nil }, AsyncOptions{Max: 1, Busy: "busy"})
	r2.Handle(context.Background(), frame(MsgTerminalRelay, "a"))
	r2.Handle(context.Background(), frame(MsgTerminalRelay, "b"))
	until(t, "busy at once without a wait", func() bool { got, ok := log2.get("b"); return ok && got == `"busy"` })
}

// A command fanned out twice (the same envelope id) runs once.
func TestAsyncCommandRepeatedIDRunsOnce(t *testing.T) {
	r, log := asyncRegistry(t)
	var mu sync.Mutex
	runs := map[string]int{}
	r.RegisterAsync(MsgTerminalRelay, func(_ context.Context, p json.RawMessage) (any, error) {
		mu.Lock()
		runs[string(p)]++
		mu.Unlock()
		return map[string]int{"status": 200}, nil
	}, AsyncOptions{Max: 16, Wait: 2 * time.Second, MaxWaiting: 64})
	for i := 0; i < 3; i++ {
		r.Handle(context.Background(), []byte(`{"type":"terminal.relay","id":"same-id","payload":{"k":"a"}}`))
	}
	r.Handle(context.Background(), []byte(`{"type":"terminal.relay","id":"other-id","payload":{"k":"b"}}`))
	until(t, "both ids answered", func() bool { return log.count() == 2 })
	time.Sleep(20 * time.Millisecond) // an absence check: a dropped duplicate has nothing to wait for
	mu.Lock()
	defer mu.Unlock()
	if runs[`{"k":"a"}`] != 1 || runs[`{"k":"b"}`] != 1 {
		t.Fatalf("runs %v", runs)
	}
	// The memory is bounded and forgets after 2 min.
	a := &asyncHandler{seen: map[string]time.Time{}}
	now := time.Now()
	for i := 0; i < asyncSeenMax+50; i++ {
		if !a.first(fmt.Sprintf("id-%d", i), now) {
			t.Fatalf("id-%d is new", i)
		}
	}
	if len(a.seen) > asyncSeenMax {
		t.Fatalf("remembered %d ids (max %d)", len(a.seen), asyncSeenMax)
	}
	last := fmt.Sprintf("id-%d", asyncSeenMax+49)
	if a.first(last, now.Add(time.Minute)) {
		t.Fatal("seen a minute ago: dropped")
	}
	if !a.first(last, now.Add(asyncSeenTTL+time.Second)) {
		t.Fatal("after 2 min the id is new again")
	}
	// The other commands keep their order and are never deduplicated.
	n := 0
	r.Register(MsgPing, func(context.Context, json.RawMessage) (any, error) { n++; return nil, nil })
	r.Handle(context.Background(), frame(MsgPing, "p"))
	r.Handle(context.Background(), frame(MsgPing, "p"))
	if n != 2 {
		t.Fatalf("a plain command ran %d times", n)
	}
}
