package terminal

import (
	"fmt"
	"strconv"
	"testing"

	"quiccpos/agent/internal/domain/pricing"
	"quiccpos/agent/internal/domain/terminal"
)

// The "missing checks" (2026-11-14, contract 2.10ak): a day with more checks
// than one page (the store's had 310). Paging by offset reaches every one,
// and the summary counts them all, whatever page is read.
func TestChecksPagingReachesEveryCheckOfABusyDay(t *testing.T) {
	r := newRig(t)
	r.svc.SetReports(fakeReports{r})
	r.login(t, till, "1234")
	const n = 310
	var paidTotal int64
	for i := range n {
		in := OrderIn{ClientKey: fmt.Sprintf("k-busy-%05d", i), Type: terminal.TypeCounter,
			Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{{Qty: 1, Selection: pricing.Selection{ItemID: soda}}}}}}
		res, err := r.svc.PlaceOrder(r.ctx, till, in)
		if err != nil {
			t.Fatal(err)
		}
		paidTotal += res.Order.Order.Money.Total
	}
	seen := map[string]bool{}
	for offset := 0; ; offset += 200 {
		q, err := ParseChecksQuery(func(k string) string {
			return map[string]string{"limit": "200", "offset": strconv.Itoa(offset)}[k]
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		p, err := r.svc.Checks(r.ctx, till, q)
		if err != nil {
			t.Fatal(err)
		}
		if p.Total != n || p.Summary.Count != n || p.Summary.TotalCents != paidTotal || p.Summary.DueCents != paidTotal {
			t.Fatalf("page %d: total %d, summary %+v (want %d checks, %d cents)", offset, p.Total, p.Summary, n, paidTotal)
		}
		for _, c := range p.Checks {
			seen[c.OrderID] = true
		}
		if offset+200 >= p.Total {
			break
		}
	}
	if len(seen) != n {
		t.Fatalf("reached %d of %d checks", len(seen), n)
	}
}
