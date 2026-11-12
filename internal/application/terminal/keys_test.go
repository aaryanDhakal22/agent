package terminal

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/terminal"
)

var testKeys atomic.Int64

// tk is a fresh client_key for a test's payment / change (one per tap,
// contract 2.10s).
func tk() string { return fmt.Sprintf("tk-%08d", testKeys.Add(1)) }

// refundAll gives back everything an order holds (the void's own per-tender
// split, as a manager), so it can be voided (contract 2.10s §5).
func refundAll(t *testing.T, r *rig, a Actor, id string) []PaymentResult {
	t.Helper()
	paid, err := r.repo.OrderPayments(r.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var out []PaymentResult
	for _, p := range voidRefunds(paid, id, "x", terminal.Staff{}, a, time.Now(), nil) {
		res, err := r.svc.AddPayment(r.ctx, a, id, PaymentIn{ClientKey: tk(), Tender: p.Tender, AmountCents: p.AmountCents, TipCents: p.TipCents,
			Refund: true, Reason: "void", ManagerPIN: "9876"})
		if err != nil {
			t.Fatalf("refund %s %d+%d: %v", p.Tender, p.AmountCents, p.TipCents, err)
		}
		out = append(out, *res)
	}
	return out
}
