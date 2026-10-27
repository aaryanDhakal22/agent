package order

import (
	"encoding/json"
	"testing"
)

// The driver's owed total rounds to whole dollars on its absolute value:
// 50 cents or more up, 49 or less down.
func TestRoundOwed(t *testing.T) {
	for in, want := range map[int64]int64{
		0: 0, 1: 0, 49: 0, 50: 100, 99: 100, 100: 100,
		3249: 3200, 3250: 3300, 3253: 3300, 3299: 3300, 3300: 3300,
		-1: 0, -49: 0, -50: -100, -749: -700, -750: -800, -940: -900, -3253: -3300,
	} {
		if got := RoundOwed(in); got != want {
			t.Errorf("RoundOwed(%d) = %d, want %d", in, got, want)
		}
	}
	r := DriverReport{Lines: []DriverReportLine{{Cash: true, CashCents: 3249}}}
	r.Finish(0)
	if r.OwedCents != 3249 || r.OwedRoundedCents != 3200 {
		t.Fatalf("Finish: %d %d", r.OwedCents, r.OwedRoundedCents)
	}
	raw, _ := json.Marshal(r)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if m["owed_cents"] != float64(3249) || m["owed_rounded_cents"] != float64(3200) {
		t.Fatalf("json: %s", raw)
	}
}

func TestReceiptPrefsDriverReportSizeAndSpacing(t *testing.T) {
	d := DefaultReceiptPrefs()
	if d.DriverReportSize != SizeNormal || d.Spacing != (LineSpacing{}) {
		t.Fatalf("defaults = %+v", d)
	}
	// Stored before these settings existed: their defaults.
	p, err := DecodeReceiptPrefs([]byte(`{"copies":2}`))
	if err != nil || p.DriverReportSize != SizeNormal || p.Spacing != (LineSpacing{}) || p.Validate() != nil {
		t.Fatalf("old stored prefs: %+v %v", p, err)
	}
	p, _ = DecodeReceiptPrefs([]byte(`{"driver_report_size":"small","spacing":{"item_mod":8,"mod_mod":4,"item_item":12}}`))
	if p.DriverReportSize != SizeSmall || p.Spacing != (LineSpacing{8, 4, 12}) || p.Validate() != nil {
		t.Fatalf("set: %+v", p)
	}
	for _, bad := range []string{`{"driver_report_size":"huge"}`, `{"spacing":{"item_mod":49}}`, `{"spacing":{"mod_mod":-1}}`} {
		if p, _ := DecodeReceiptPrefs([]byte(bad)); p.Validate() == nil {
			t.Errorf("%s must be refused", bad)
		}
	}
}

func TestStreetOnly(t *testing.T) {
	for in, want := range map[string]string{
		"12 Main St":              "12 Main St",
		"12 Main St, Apt 3":       "12 Main St",
		"931 Taylor Ave Apt 2B":   "931 Taylor Ave",
		"931 Taylor Ave #4":       "931 Taylor Ave",
		"40 York Rd Suite 100":    "40 York Rd",
		"  7 Unit Rd  ":           "7 Unit Rd",
		"1 Charles St Fl 3, rear": "1 Charles St",
		"":                        "",
	} {
		if got := StreetOnly(in); got != want {
			t.Errorf("StreetOnly(%q) = %q, want %q", in, got, want)
		}
	}
}

// The slip's Tip / Total: card = the total + the tip, cash = just the total
// (an online total includes the tip; a POS total doesn't).
func TestDriverReportLineSlipMoney(t *testing.T) {
	online := func(tip, total int64, card bool) StoredOrder {
		so := StoredOrder{Order: Snapshot{Source: "online", Customer: Customer{DisplayName: "Erik Davis"},
			Money: Money{Tip: tip, Total: total}, Fulfillment: Fulfillment{Address: &Address{Line1: "12 Main St Apt 3", Line2: "rear"}}}}
		if card {
			so.Order.Payments = []PaymentSummary{{Method: "card", AmountCents: total}}
		}
		return so
	}
	if l := NewDriverReportLine(online(300, 2700, true)); l.Street != "12 Main St" || l.OrderTipCents != 300 || l.SlipTotalCents != 2700 {
		t.Errorf("online card: %+v", l)
	}
	if l := NewDriverReportLine(online(200, 2449, false)); !l.Cash || l.OrderTipCents != 200 || l.SlipTotalCents != 2249 || l.CashCents != 2249 {
		t.Errorf("online cash: %+v", l)
	}
	pos := StoredOrder{Order: Snapshot{Source: SourcePOS, Money: Money{Total: 2400}},
		Status: Status{ExtSource: ExtSourceSoftTouch, Paid: true, TipCents: 300}}
	if l := NewDriverReportLine(pos); l.Cash || l.OrderTipCents != 300 || l.SlipTotalCents != 2700 {
		t.Errorf("POS card: %+v", l)
	}
	pos.Status.Paid, pos.Status.CashDueCents = false, 2400
	if l := NewDriverReportLine(pos); !l.Cash || l.SlipTotalCents != 2400 {
		t.Errorf("POS cash: %+v", l)
	}
	l := DriverReportLine{}.withPOSMoney(LedgerCheck{Paid: true, TipCents: 300, TotalCents: 2400})
	if l.SlipTotalCents != 2700 || l.OrderTipCents != 300 {
		t.Errorf("ledger card: %+v", l)
	}
}
