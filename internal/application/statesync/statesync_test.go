package statesync

import (
	"context"
	"errors"
	"testing"

	"quiccpos/agent/internal/domain/order"

	"github.com/rs/zerolog"
)

func TestSendSkipsUnchangedUntilReset(t *testing.T) {
	st := order.AgentState{Version: 1, KitchenNext: 1100}
	sent := 0
	fail := false
	s := New(func(context.Context) (order.AgentState, error) { return st, nil },
		func(_ context.Context, typ string, _ any) error {
			if typ != MsgAgentState {
				t.Fatalf("type %q", typ)
			}
			if fail {
				return errors.New("offline")
			}
			sent++
			return nil
		}, nil, zerolog.Nop())
	ctx := context.Background()
	_ = s.Send(ctx)
	_ = s.Send(ctx)
	if sent != 1 {
		t.Fatalf("unchanged state sent %d times", sent)
	}
	st.KitchenNext = 1101
	_ = s.Send(ctx)
	if sent != 2 {
		t.Fatal("a changed state must be sent")
	}
	s.Reset()
	_ = s.Send(ctx)
	if sent != 3 {
		t.Fatal("Reset must force the next send")
	}
	fail = true
	st.KitchenNext = 1102
	if err := s.Send(ctx); err == nil {
		t.Fatal("want error")
	}
	fail = false
	st.KitchenNext = 1101 // back to the last *sent* value: still re-sent after a failure
	_ = s.Send(ctx)
	if sent != 4 {
		t.Fatal("a failed send must force the next one")
	}
}
