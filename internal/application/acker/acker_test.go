package acker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func TestAckBatchesAndRetries(t *testing.T) {
	var mu sync.Mutex
	var sent [][]string
	fail := true
	a := New(func(_ context.Context, typ string, payload any) error {
		mu.Lock()
		defer mu.Unlock()
		if typ != MsgOrderAck {
			t.Errorf("type %q", typ)
		}
		if fail {
			fail = false
			return errors.New("offline")
		}
		sent = append(sent, payload.(map[string]any)["order_ids"].([]string))
		return nil
	}, nil, zerolog.Nop())
	a.Ack("o1")
	a.Ack("o2")
	a.Ack("o1") // duplicate collapses
	a.Ack("")

	ctx := context.Background()
	if err := a.Flush(ctx); err == nil {
		t.Fatal("first flush should fail")
	}
	if a.Pending() != 2 {
		t.Fatalf("pending %d after a failed flush", a.Pending())
	}
	if err := a.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if a.Pending() != 0 || len(sent) != 1 || len(sent[0]) != 2 {
		t.Fatalf("pending %d sent %v", a.Pending(), sent)
	}
}

func TestAckWaitsForConfiguration(t *testing.T) {
	calls := 0
	configured := false
	a := New(func(context.Context, string, any) error { calls++; return nil }, func() bool { return configured }, zerolog.Nop())
	a.Ack("o1")
	_ = a.Flush(context.Background())
	if calls != 0 || a.Pending() != 1 {
		t.Fatal("must hold acks while unconfigured")
	}
	configured = true
	_ = a.Flush(context.Background())
	if calls != 1 || a.Pending() != 0 {
		t.Fatal("must send once configured")
	}
}

func TestRunSendsSoon(t *testing.T) {
	done := make(chan struct{})
	a := New(func(context.Context, string, any) error { close(done); return nil }, nil, zerolog.Nop())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)
	a.Ack("o1")
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("ack not sent within 3 s")
	}
}
