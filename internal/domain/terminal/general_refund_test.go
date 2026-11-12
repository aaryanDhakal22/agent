package terminal

import (
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
)

func TestGeneralRefundNeed(t *testing.T) {
	cashier := Staff{ID: "c", Role: RoleCashier, Limits: &Limits{RefundCents: 2000}}
	if n := GeneralRefundNeed(nil, cashier, 2000); n != nil {
		t.Fatalf("$20 needs %+v", n)
	}
	n := GeneralRefundNeed(nil, cashier, 2001)
	if n == nil || n.Action != ActionRefund || n.Limit != 2000 || n.Requested != 2001 {
		t.Fatalf("$20.01: %+v", n)
	}
	if n := GeneralRefundNeed(nil, Staff{ID: "m", Role: RoleManager}, 1_000_000); n != nil {
		t.Fatalf("a manager needs %+v", n)
	}
	if n := GeneralRefundNeed(nil, Staff{ID: "d", Role: RoleDriver, Limits: &Limits{}}, 1); n == nil {
		t.Fatal("a driver gave a general refund alone")
	}
}

func TestZGeneralRefund(t *testing.T) {
	t0 := time.Date(2026, 11, 12, 15, 0, 0, 0, time.UTC)
	so := tillOrder("o1", order.SourceCounter, "dine_in", order.StateAccepted, []order.LineItem{tline("x", 1, 1000)}, 0, 0, 60, 0, nil)
	pays := []Payment{
		{ID: "p1", OrderID: "o1", At: t0, Tender: TenderCash, AmountCents: 1060, DrawerID: "d1", DeviceID: "t1", StaffID: "s1"},
		{ID: "g1", At: t0, Tender: TenderCash, AmountCents: 500, Refund: true, RefundKind: RefundGeneral, DrawerID: "d1", DeviceID: "t1", StaffID: "s1"},
		{ID: "g2", At: t0, Tender: TenderCash, AmountCents: 250, Refund: true, RefundKind: RefundGeneral, DeviceID: "web", StaffID: "s1"},
	}
	closed := t0.Add(time.Hour)
	drawers := []Drawer{{ID: "d1", DeviceID: "t1", OpenedAt: t0, ClosedAt: &closed, StartCents: 10000, ExpectedCents: 10000 + 1060 - 500,
		DrawerTotals: DrawerTotals{CashSalesCents: 1060, CashRefundsCents: 500}}}
	z := BuildZ(ZInput{BatchID: 1, OpenedAt: t0, ClosedAt: closed, Now: closed, Orders: []order.StoredOrder{so}, Payments: pays, Drawers: drawers})
	if z.Reconcile.DifferenceCents != 0 || z.Reconcile.DrawersDifferences != 0 {
		t.Fatalf("unbalanced %+v", z.Reconcile)
	}
	if z.GeneralRefunds != (ZAmount{Count: 2, Cents: 750}) || z.Refunds != (ZAmount{Count: 2, Cents: 750}) {
		t.Fatalf("general %+v refunds %+v", z.GeneralRefunds, z.Refunds)
	}
	if z.Tenders[0].Tender != TenderCash || z.Tenders[0].NetCents != 1060-750 || z.CashOutside != -250 {
		t.Fatalf("cash %+v outside %d", z.Tenders[0], z.CashOutside)
	}
	if z.Sales.TotalCents != 1060 || z.Unpaid.Cents != 0 {
		t.Fatalf("sales %+v unpaid %+v", z.Sales, z.Unpaid)
	}
	till := BuildTill(TillInput{Z: ZInput{BatchID: 1, OpenedAt: t0, ClosedAt: closed, Now: closed, Orders: []order.StoredOrder{so},
		Payments: pays, Drawers: drawers}, Drawer: &drawers[0]})
	if till.Detail.GeneralRefunds != (TillCount{Count: 1, Amount: -500}) || till.Summary.Refunds != -500 || till.Summary.TotalCash != 560 {
		t.Fatalf("drawer till %+v %+v", till.Detail.GeneralRefunds, till.Summary)
	}
	if till.Summary.ExpectedInDrawer != 10560 {
		t.Fatalf("expected in drawer %d", till.Summary.ExpectedInDrawer)
	}
	checkTill(t, "general refund drawer", till)
}
