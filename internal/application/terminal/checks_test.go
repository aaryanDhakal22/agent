package terminal

import (
	"context"
	"testing"

	orderApp "quiccpos/agent/internal/application/order"
	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/pricing"
	"quiccpos/agent/internal/domain/terminal"
)

// fakeReports: the Manager tab's floor over the rig's real repo (batches);
// nothing prints.
type fakeReports struct{ r *rig }

func (f fakeReports) ListBatches(ctx context.Context) ([]order.Batch, error) {
	return f.r.repo.ListBatches(ctx, 10)
}
func (f fakeReports) DriverReports(context.Context, int64) ([]order.DriverReport, error) {
	return nil, nil
}
func (f fakeReports) PrintDriverReports(context.Context, int64) ([]orderApp.ReportPrint, error) {
	return nil, orderApp.ErrPrintingUnavailable
}
func (f fakeReports) PrintBatchDriverReport(context.Context, int64, string) (*orderApp.ReportPrint, error) {
	return nil, orderApp.ErrPrintingUnavailable
}
func (f fakeReports) CloseBatch(context.Context, bool) (*order.Batch, []order.StoredOrder, error) {
	return nil, nil, nil
}

// The All Checks Overview (contract 2.10o): filters, text search, paging,
// one order in full.
func TestChecksOverview(t *testing.T) {
	r := newRig(t)
	r.svc.SetReports(fakeReports{r})
	r.login(t, till, "1234")
	place := func(key, typ string, pays ...PaymentIn) string {
		in := OrderIn{ClientKey: key, Type: typ, Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine()}}}, Payments: pays}
		if typ == "delivery" {
			in.Customer = &Customer{Name: "Erik Davis", Phone: "4105550101", Address: &Address{Line1: "12 Main St", City: "Towson"}}
		}
		res, err := r.svc.PlaceOrder(r.ctx, till, in)
		if err != nil {
			t.Fatal(err)
		}
		return res.Order.Order.ID
	}
	a := place("k-chk-00001", "counter", PaymentIn{Tender: "cash", AmountCents: 2357, TenderedCents: 3000})
	b := place("k-chk-00002", "delivery")
	c := place("k-chk-00003", "pickup", PaymentIn{Tender: "card", AmountCents: 2357, Brand: "visa"})
	refundAll(t, r, till, a)
	if _, err := r.svc.Void(r.ctx, till, a, VoidIn{Reason: "test", ManagerPIN: "9876"}); err != nil {
		t.Fatal(err)
	}
	get := func(raw map[string]string) *ChecksPage {
		t.Helper()
		q, err := ParseChecksQuery(func(k string) string { return raw[k] }, nil)
		if err != nil {
			t.Fatal(err)
		}
		p, err := r.svc.Checks(r.ctx, till, q)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	if p := get(nil); p.Total != 3 || len(p.Checks) != 3 || len(p.BatchIDs) != 1 {
		t.Fatalf("all: %+v", p)
	}
	if p := get(map[string]string{"type": "delivery"}); p.Total != 1 || p.Checks[0].OrderID != b || p.Checks[0].Street != "12 Main St" || p.Checks[0].Paid {
		t.Fatalf("delivery: %+v", p.Checks)
	}
	if p := get(map[string]string{"tender": "card"}); p.Total != 1 || p.Checks[0].OrderID != c {
		t.Fatalf("card: %+v", p.Checks)
	}
	if p := get(map[string]string{"paid": "false"}); p.Total != 1 || p.Checks[0].OrderID != b {
		t.Fatalf("unpaid: %+v", p.Checks)
	}
	if p := get(map[string]string{"q": "erik"}); p.Total != 1 {
		t.Fatalf("q name: %+v", p.Checks)
	}
	if p := get(map[string]string{"q": "0101"}); p.Total != 1 {
		t.Fatalf("q phone: %+v", p.Checks)
	}
	if p := get(map[string]string{"state": "voided"}); p.Total != 1 || p.Checks[0].OrderID != a || !p.Checks[0].Voided {
		t.Fatalf("voided: %+v", p.Checks)
	}
	if p := get(map[string]string{"staff_id": "cash-1", "device_id": till.DeviceID}); p.Total != 3 || p.Checks[0].StaffName != "Cara S." {
		t.Fatalf("staff/device: %+v", p.Checks)
	}
	if p := get(map[string]string{"limit": "2", "offset": "2"}); p.Total != 3 || len(p.Checks) != 1 {
		t.Fatalf("paging: %+v", p)
	}
	if _, err := ParseChecksQuery(func(k string) string { return map[string]string{"type": "boat"}[k] }, nil); err == nil {
		t.Fatal("bad type accepted")
	}
	d, err := r.svc.CheckByID(r.ctx, till, c)
	if err != nil || d.Order.Order.ID != c || len(d.Payments) != 1 || d.Payments[0].Brand != terminal.BrandVisa || len(d.Journal) == 0 {
		t.Fatalf("detail: %+v %v", d, err)
	}
	if _, err := r.svc.CheckByID(r.ctx, till, "00000000-0000-0000-0000-000000000000"); err != ErrReportNotFound {
		t.Fatalf("missing: %v", err)
	}
}
