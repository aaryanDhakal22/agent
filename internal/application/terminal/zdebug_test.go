package terminal

import (
	"fmt"
	"testing"

	"quiccpos/agent/internal/domain/order"
)

func dumpZ(t *testing.T, r *rig, batch int64) {
	in, _ := r.repo.ZInputs(r.ctx, batch)
	by := map[string][]string{}
	net := map[string][2]int64{}
	for _, p := range in.Payments {
		a, tp := p.Signed()
		v := net[p.OrderID]
		net[p.OrderID] = [2]int64{v[0] + a, v[1] + tp}
		by[p.OrderID] = append(by[p.OrderID], fmt.Sprintf("%s%s:%d+%d", map[bool]string{true: "-", false: "+"}[p.Refund], p.Tender, p.AmountCents, p.TipCents))
	}
	for _, so := range in.Orders {
		n := net[so.Order.ID]
		t.Logf("%s %s state=%s void=%v total=%d net=%d tip=%d due=%d pays=%v", so.Order.ID[:8], so.Order.Fulfillment.Type, so.Status.State,
			so.Status.CancelReason, so.Order.Money.Total, n[0], n[1], so.Order.CashDue(), by[so.Order.ID])
	}
	_ = order.CloseManual
}
