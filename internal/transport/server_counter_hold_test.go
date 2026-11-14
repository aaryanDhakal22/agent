package transport

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"quiccpos/agent/internal/application/squareterm"

	"github.com/rs/zerolog"
)

// holdMain records what square.keyed_payment asked main for and answers like
// main does for a hold (APPROVED) or a completed card.
type holdMain struct {
	mu   *sync.Mutex
	asks *[]map[string]any
}

func (m holdMain) Send(_ context.Context, typ string, payload any) (int, json.RawMessage, error) {
	if typ != squareterm.MsgKeyed {
		return 202, nil, nil
	}
	raw, _ := json.Marshal(payload)
	var p map[string]any
	_ = json.Unmarshal(raw, &p)
	m.mu.Lock()
	*m.asks = append(*m.asks, p)
	m.mu.Unlock()
	st := "COMPLETED"
	if p["hold"] == true {
		st = "APPROVED"
	}
	out, _ := json.Marshal(map[string]any{"ok": true, "payment": map[string]any{"id": "pay_h", "status": st, "brand": "VISA", "last4": "1111", "entry": "keyed"}})
	return 200, out, nil
}

// Contract 2.10ao: a delivery's typed card is a hold. The Pi decides from
// the order type, never from a hold the client sends (call center relay).
func TestKeyedPaymentHoldIsThePisDecision(t *testing.T) {
	r := newCCRig(t)
	var mu sync.Mutex
	var asks []map[string]any
	r.s.square = squareterm.New(func() squareterm.Sender { return holdMain{mu: &mu, asks: &asks} }, zerolog.Nop())
	body := func(key, ful string, hold bool) map[string]any {
		return map[string]any{"nonce": "cnon:ok", "amount_cents": 1777, "idempotency_key": key, "reference_id": "ck-" + key,
			"fulfillment": ful, "hold": hold}
	}
	got := r.relay(t, ccAgent, "POST", "square/keyed-payments", body("sq-h-0001", "delivery", false))
	if got.Status != 200 || !strings.Contains(string(got.Body), `"held":true`) {
		t.Fatalf("a delivery's card: %d %s", got.Status, got.Body)
	}
	got = r.relay(t, ccAgent, "POST", "square/keyed-payments", body("sq-h-0002", "pickup", true))
	if got.Status != 200 || strings.Contains(string(got.Body), `"held":true`) {
		t.Fatalf("a pickup's card (the client's hold ignored): %d %s", got.Status, got.Body)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(asks) != 2 || asks[0]["hold"] != true || asks[1]["hold"] != false {
		t.Fatalf("main was asked: %+v", asks)
	}
}
