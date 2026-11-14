package order

import (
	"testing"
	"time"
)

// Batch F (§2.10am #25): a typed tip counts as a card tip on the report at
// once (off the handout after the tip tax); once the press of Close batch
// applied it, it isn't counted twice.
func TestApplyTypedTipsCountsOnceAndComesOffTheHandout(t *testing.T) {
	t.Parallel()
	rep := DriverReport{Lines: []DriverReportLine{
		{OrderID: "card", Tippable: true, TipState: TipStateNone, TotalCents: 2000, SlipTotalCents: 2000},
		{OrderID: "cash", Cash: true, CashCents: 3000, TotalCents: 3000, SlipTotalCents: 3000},
		{OrderID: "notip", Tippable: true, TipState: TipStateNone, TotalCents: 1000, SlipTotalCents: 1000},
	}}
	base := rep
	base.Lines = append([]DriverReportLine(nil), rep.Lines...)
	base.Finish(600, 300, false)
	ApplyTypedTips(&rep, map[string]DriverTip{
		"card":  {OrderID: "card", State: TipStateTipped, TipCents: 500},
		"notip": {OrderID: "notip", State: TipStateNoTip},
	})
	rep.Finish(600, 300, false)
	if rep.TipCents != 500 || rep.TypedTipCents != 500 || rep.TipsToType != 0 || rep.OwedCents != base.OwedCents-470 {
		t.Fatalf("typed: %+v (base owed %d)", rep, base.OwedCents)
	}
	if l := rep.Lines[0]; l.TipCents != 500 || l.OrderTipCents != 500 || l.SlipTotalCents != 2500 || l.TipState != TipStateTipped {
		t.Fatalf("card line: %+v", l)
	}
	if base.TipsToType != 2 {
		t.Fatalf("pile before: %d", base.TipsToType)
	}
	// Applied at the press: the order carries it now (its own tip), nothing extra.
	applied := DriverReport{Lines: []DriverReportLine{{OrderID: "card", Tippable: true, TipCents: 500, OrderTipCents: 500, SlipTotalCents: 2500}}}
	ApplyTypedTips(&applied, map[string]DriverTip{"card": {OrderID: "card", State: TipStateTipped, TipCents: 500, AppliedCents: 500}})
	applied.Finish(600, 300, false)
	if applied.TipCents != 500 {
		t.Fatalf("counted twice: %+v", applied)
	}
	// Lowered after a press: the difference comes off.
	ApplyTypedTips(&applied, map[string]DriverTip{"card": {OrderID: "card", State: TipStateTipped, TipCents: 300, AppliedCents: 500}})
	applied.Finish(600, 300, false)
	if applied.TipCents != 300 {
		t.Fatalf("lowered: %+v", applied)
	}
}

// #27: old = still not delivered past the setting, whoever has it; off at 0.
func TestOldDelivery(t *testing.T) {
	t.Parallel()
	acc := time.Date(2026, 11, 14, 18, 0, 0, 0, time.UTC)
	so := StoredOrder{Order: Snapshot{Fulfillment: Fulfillment{Type: "delivery"}},
		Status: Status{State: StateOnDelivery, BatchID: 7, DriverID: "d-1", AcceptedAt: &acc}}
	hour := time.Hour
	if OldDelivery(so, 7, acc.Add(59*time.Minute), hour) || !OldDelivery(so, 7, acc.Add(hour), hour) {
		t.Fatal("the 60-minute line")
	}
	if OldDelivery(so, 7, acc.Add(5*hour), 0) || OldDelivery(so, 8, acc.Add(5*hour), hour) {
		t.Fatal("off / another batch")
	}
	for _, st := range []State{StateDelivered, StateCancelled, StateScheduled, StateArrived} {
		so.Status.State = st
		if OldDelivery(so, 7, acc.Add(5*hour), hour) {
			t.Fatalf("%s is never old", st)
		}
	}
}
