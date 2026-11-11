package terminal

import "testing"

func TestLimitsOf(t *testing.T) {
	cfg, err := ParseConfig([]byte(`{"version":3,"settings":{"cashier_refund_cents":500,"cashier_discount_pct":15,"counter_start":40},
	 "staff":[{"id":"a","role":"cashier"},{"id":"b","role":"cashier","effective_limits":{"refund_cents":9,"discount_pct":1}},
	          {"id":"c","role":"driver","effective_limits":null},{"id":"d","role":"manager"},{"id":"e","role":"driver"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Limits{"a": {RefundCents: 500, DiscountPct: 15}, "b": {RefundCents: 9, DiscountPct: 1},
		"c": {Unlimited: true}, "d": {Unlimited: true}, "e": {}}
	for id, w := range want {
		s, _ := cfg.Find(id)
		if got := cfg.LimitsOf(s); got != w {
			t.Errorf("%s: %+v want %+v", id, got, w)
		}
	}
	if cfg.CounterStart() != 40 {
		t.Fatal(cfg.CounterStart())
	}
}
