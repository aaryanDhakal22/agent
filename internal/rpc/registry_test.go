package rpc

import (
	"context"
	"encoding/json"
	"testing"

	"quiccpos/agent/internal/observability"

	"github.com/rs/zerolog"
)

type fakeExchanger struct {
	status int
	body   []byte
	sent   Envelope
}

func (f *fakeExchanger) PostRPC(context.Context, []byte) error { return nil }
func (f *fakeExchanger) ExchangeRPC(_ context.Context, body []byte) (int, []byte, error) {
	_ = json.Unmarshal(body, &f.sent)
	return f.status, f.body, nil
}

type plainPoster struct{}

func (plainPoster) PostRPC(context.Context, []byte) error { return nil }

func TestSendReturnsTheReplyPayload(t *testing.T) {
	meters, err := observability.NewMeters()
	if err != nil {
		t.Fatal(err)
	}
	ex := &fakeExchanger{status: 200, body: []byte(`{"type":"diag.report.reply","id":"x","payload":{"id":"r1","stored":true,"duplicate":false}}`)}
	r := NewRegistry(ex, zerolog.Nop(), meters)
	status, payload, err := r.Send(context.Background(), MsgDiagReport, map[string]string{"tablet_id": "t1"})
	if err != nil || status != 200 || string(payload) != `{"id":"r1","stored":true,"duplicate":false}` {
		t.Fatalf("status=%d payload=%s err=%v", status, payload, err)
	}
	if ex.sent.Type != MsgDiagReport || ex.sent.ID == "" || string(ex.sent.Payload) != `{"tablet_id":"t1"}` {
		t.Fatalf("sent %+v", ex.sent)
	}
	ex.status, ex.body = 400, []byte(`{"error":"bad"}`)
	if status, body, _ := r.Send(context.Background(), MsgDiagReport, nil); status != 400 || string(body) != `{"error":"bad"}` {
		t.Fatalf("non-200 keeps the raw body: %d %s", status, body)
	}
	if _, _, err := NewRegistry(plainPoster{}, zerolog.Nop(), meters).Send(context.Background(), MsgDiagReport, nil); err == nil {
		t.Fatal("a poster without replies must error")
	}
}
