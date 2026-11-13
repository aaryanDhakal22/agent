package terminal

import (
	"errors"
	"testing"

	"quiccpos/agent/internal/domain/order"
)

// Agent 2.5.3: a counter order and an online CASH order are voidable at a terminal; an online order paid
// online (all or part, also edited with an item left for the door) is ErrPaidOnline; another source isn't.
func TestVoidableSource(t *testing.T) {
	card := []order.PaymentSummary{{Method: "card", AmountCents: 1499}}
	edited := func(paid int64) order.Snapshot {
		return order.Snapshot{Source: order.SourceOnline, Money: order.Money{Total: 1634}, Payments: card,
			SourceMeta: map[string]any{order.MetaPi: map[string]any{"online": order.OnlineBase{Money: order.Money{Total: 1499}, PaidCents: paid}}}}
	}
	for name, c := range map[string]struct {
		snap order.Snapshot
		want error
	}{
		"counter":                   {order.Snapshot{Source: order.SourceCounter, Money: order.Money{Total: 1000}}, nil},
		"online cash (no payments)": {order.Snapshot{Source: order.SourceOnline, Money: order.Money{Total: 1500}}, nil},
		"online cash payment":       {order.Snapshot{Source: order.SourceOnline, Money: order.Money{Total: 1500}, Payments: []order.PaymentSummary{{Method: "cash", AmountCents: 1500}}}, nil},
		"online paid by card":       {order.Snapshot{Source: order.SourceOnline, Money: order.Money{Total: 1499}, Payments: card}, ErrPaidOnline},
		"online part card":          {order.Snapshot{Source: order.SourceOnline, Money: order.Money{Total: 1500}, Payments: []order.PaymentSummary{{Method: "card", AmountCents: 500}, {Method: "cash", AmountCents: 1000}}}, ErrPaidOnline},
		"prepaid + item for door":   {edited(1499), ErrPaidOnline},
		"cash edited":               {edited(0), nil},
		"website":                   {order.Snapshot{Source: order.SourceWeb, Money: order.Money{Total: 1500}}, ErrNotCounterOrder},
	} {
		if got := VoidableSource(c.snap); !errors.Is(got, c.want) && !(got == nil && c.want == nil) {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
}

// The Z counts an online cash order voided at a terminal as voided (2.5.3).
func TestZCountsOnlineVoid(t *testing.T) {
	so := order.StoredOrder{Order: order.Snapshot{ID: "o1", Source: order.SourceOnline, Money: order.Money{Total: 1500}},
		Status: order.Status{State: order.StateCancelled, CancelReason: VoidReason}}
	z := BuildZ(ZInput{Orders: []order.StoredOrder{so}})
	if z.Counts.Voided != 1 || z.Voids.Count != 1 || z.Voids.Cents != 1500 {
		t.Fatalf("Z: counts %+v voids %+v", z.Counts, z.Voids)
	}
}
