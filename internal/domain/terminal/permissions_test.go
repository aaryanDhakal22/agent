package terminal

import "testing"

// The owner's permissions of 2026-11-12: cashiers refund cash only, up to the
// cashier limit for the whole order; card refunds and cash back for a card
// sale need a manager; a manager (or anyone unlimited) never does.
func TestRefundNeed(t *testing.T) {
	cfg, err := ParseConfig([]byte(`{"version":1,"settings":{"cashier_refund_cents":2000},
	 "staff":[{"id":"c1","role":"cashier"},{"id":"c2","role":"cashier"},{"id":"m","role":"manager"},{"id":"d","role":"driver"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	c1, _ := cfg.Find("c1")
	m, _ := cfg.Find("m")
	pay := func(tender string, cents int64) Payment { return Payment{Tender: tender, AmountCents: cents} }
	ref := func(tender string, cents int64, by, approved string) Payment {
		return Payment{Tender: tender, AmountCents: cents, Refund: true, StaffID: by, ApprovedBy: approved}
	}
	cash3000 := []Payment{pay(TenderCash, 3000)}
	cases := []struct {
		name    string
		st      Staff
		paid    []Payment
		refunds []Payment
		want    string // "" = no manager
		limit   int64
		req     int64
	}{
		{"small cash refund", c1, cash3000, []Payment{ref(TenderCash, 1500, "c1", "")}, "", 0, 0},
		{"cash refund at the limit", c1, cash3000, []Payment{ref(TenderCash, 2000, "c1", "")}, "", 0, 0},
		{"cash refund over the limit", c1, cash3000, []Payment{ref(TenderCash, 2001, "c1", "")}, ActionRefund, 2000, 2001},
		{"small refunds add up", c1, append(cash3000, ref(TenderCash, 1500, "c2", "")), []Payment{ref(TenderCash, 600, "c1", "")}, ActionRefund, 2000, 2100},
		{"a manager-approved refund doesn't count", c1, append(cash3000, ref(TenderCash, 1500, "c2", "m")), []Payment{ref(TenderCash, 600, "c1", "")}, "", 0, 0},
		{"a manager's own refund doesn't count", c1, append(cash3000, ref(TenderCash, 1500, "m", "")), []Payment{ref(TenderCash, 600, "c1", "")}, "", 0, 0},
		{"card refund, any amount", c1, []Payment{pay(TenderCard, 3000)}, []Payment{ref(TenderCard, 1, "c1", "")}, NeedCardRefund, 0, 1},
		{"square app refund", c1, []Payment{pay(TenderSquareApp, 3000)}, []Payment{ref(TenderSquareApp, 100, "c1", "")}, NeedCardRefund, 0, 100},
		{"cash back for a card sale", c1, []Payment{pay(TenderCard, 3000), pay(TenderCash, 500)}, []Payment{ref(TenderCash, 800, "c1", "")}, NeedCashForCard, 500, 800},
		{"comp refund is free", c1, []Payment{pay(TenderComp, 3000)}, []Payment{ref(TenderComp, 3000, "c1", "")}, "", 0, 0},
		{"manager: no one", m, []Payment{pay(TenderCard, 3000)}, []Payment{ref(TenderCard, 3000, "m", ""), ref(TenderCash, 9000, "m", "")}, "", 0, 0},
		{"void over the limit names void", c1, cash3000, []Payment{ref(TenderCash, 3000, "c1", "")}, ActionVoid, 2000, 3000},
	}
	for _, tc := range cases {
		over := ActionRefund
		if tc.want == ActionVoid {
			over = ActionVoid
		}
		got := RefundNeed(cfg, tc.st, tc.paid, tc.refunds, over)
		switch {
		case tc.want == "" && got != nil:
			t.Errorf("%s: want none, got %+v", tc.name, got)
		case tc.want != "" && (got == nil || got.Action != tc.want || got.Limit != tc.limit || got.Requested != tc.req):
			t.Errorf("%s: got %+v want %s limit %d requested %d", tc.name, got, tc.want, tc.limit, tc.req)
		}
	}
}

func TestCashierDefaultsAndComp(t *testing.T) {
	// main sends nothing about cashiers: the owner's defaults ($20, 20 %).
	cfg, err := ParseConfig([]byte(`{"version":1,"staff":[{"id":"c","role":"cashier"},{"id":"m","role":"manager"},{"id":"d","role":"driver"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	c, _ := cfg.Find("c")
	if l := cfg.LimitsOf(c); l.RefundCents != 2000 || l.DiscountPct != 20 {
		t.Fatalf("cashier defaults %+v", l)
	}
	if cfg.EmployeeMealPct() != 20 {
		t.Fatalf("employee meal default %d", cfg.EmployeeMealPct())
	}
	// Only one of the two cashier settings: the other keeps its default.
	cfg2, _ := ParseConfig([]byte(`{"version":1,"settings":{"cashier_discount_pct":5},"staff":[{"id":"c","role":"cashier"}]}`))
	if l := cfg2.LimitsOf(c); l.RefundCents != 2000 || l.DiscountPct != 5 {
		t.Fatalf("half set %+v", l)
	}
	m, _ := cfg.Find("m")
	d, _ := cfg.Find("d")
	// A comp and a void of a sent order are a manager's (the owner,
	// 2026-11-12 evening).
	if cfg.MayComp(c) || !cfg.MayComp(m) || cfg.MayComp(d) {
		t.Fatal("comp: a manager yes, a cashier and a driver no")
	}
	if cfg.MayVoid(c) || !cfg.MayVoid(m) || cfg.MayVoid(d) || cfg.VoidNeed(c, 500) == nil || cfg.VoidNeed(m, 500) != nil {
		t.Fatal("void of a sent order: a manager yes, a cashier and a driver no")
	}
}

func TestNormBrand(t *testing.T) {
	for in, want := range map[string]string{"": "", "VISA": BrandVisa, "Mastercard": BrandMastercard, "MASTER_CARD": BrandMastercard,
		"AMERICAN_EXPRESS": BrandAmex, "amex": BrandAmex, "DISCOVER_DINERS": BrandDiscover, "Discover": BrandDiscover,
		"JCB": BrandOther, "SQUARE_GIFT_CARD": BrandOther, "other": BrandOther} {
		if got := NormBrand(in); got != want {
			t.Errorf("%q: %q want %q", in, got, want)
		}
	}
}
