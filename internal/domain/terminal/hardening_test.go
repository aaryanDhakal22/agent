package terminal

import (
	"errors"
	"testing"

	"quiccpos/agent/internal/domain/order"
)

func TestCheckMoney(t *testing.T) {
	items := []order.LineItem{{LineTotal: 1500}, {LineTotal: 500}}
	ok := order.Money{Subtotal: 2000, Tax: 120, Tip: 0, Total: 2120 + 400 - 200,
		Fees: []order.Fee{{Amount: 400}}, Discounts: []order.Discount{{Amount: 200}}}
	if err := CheckMoney(ok, items); err != nil {
		t.Fatal(err)
	}
	var me *ErrMoneyIdentity
	off := ok
	off.Total++
	if err := CheckMoney(off, items); !errors.As(err, &me) || me.Diff != -1 {
		t.Fatalf("off by one: %v", err)
	}
	if err := CheckMoney(ok, items[:1]); !errors.As(err, &me) {
		t.Fatalf("lines != subtotal: %v", err)
	}
	neg := ok
	neg.Discounts = []order.Discount{{Amount: -5}}
	if err := CheckMoney(neg, items); !errors.As(err, &me) {
		t.Fatalf("negative discount: %v", err)
	}
}

func TestFingerprintAndWasteReasons(t *testing.T) {
	if Fingerprint(map[string]any{"a": 1, "b": 2}) != Fingerprint(map[string]any{"b": 2, "a": 1}) {
		t.Fatal("key order must not matter")
	}
	if !SameRequest("", "x") || SameRequest("a", "b") {
		t.Fatal("SameRequest")
	}
	var c *Config
	if len(c.WasteReasons()) != 4 || c.WasteReasonOf(" Never picked up ") != "Never picked up" || c.WasteReasonOf("x") != "" {
		t.Fatal("defaults")
	}
	cfg, err := ParseConfig([]byte(`{"version":1,"staff":[],"settings":{"waste_reasons":["Dropped","", "Expired"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if r := cfg.WasteReasons(); len(r) != 2 || r[0] != "Dropped" {
		t.Fatalf("main's list: %v", r)
	}
}

// counter_start is the store's ONE sequence's start (contract 2.10ae,
// 2026-11-14): default 100, kept below the 5-digit future placeholders, no
// longer tied to kitchen_start (retired).
func TestCounterStartStaysBelowThePlaceholders(t *testing.T) {
	if DefaultCounterStart != 100 {
		t.Fatalf("default start %d, want 100", DefaultCounterStart)
	}
	for raw, want := range map[string]int64{`{"counter_start": 500}`: 500, `{"counter_start": 1100}`: 1100,
		`{"counter_start": 1100, "kitchen_start": 1100}`: 1100, `{"counter_start": 9999}`: 9999,
		`{"counter_start": 10000}`: DefaultCounterStart, `{"counter_start": 0}`: DefaultCounterStart, `{}`: DefaultCounterStart} {
		cfg, err := ParseConfig([]byte(`{"version":1,"staff":[],"settings":` + raw + `}`))
		if err != nil {
			t.Fatal(err)
		}
		if got := cfg.CounterStart(); got != want {
			t.Errorf("%s: %d, want %d", raw, got, want)
		}
	}
}

// main's driver_pay_from_cash (2.5.8): the delivery pay comes off the driver's
// cash only when main sends it true; missing / no config = off (paid weekly).
func TestDriverPayFromCash(t *testing.T) {
	var none *Config
	if none.DriverPayFromCash() {
		t.Fatal("no config yet: off")
	}
	for raw, want := range map[string]bool{`{}`: false, `{"driver_pay_from_cash":true}`: true, `{"driver_pay_from_cash":false}`: false,
		`{"driver_pay_from_cash":null}`: false} {
		cfg, err := ParseConfig([]byte(`{"version":1,"staff":[],"settings":` + raw + `}`))
		if err != nil {
			t.Fatal(err)
		}
		if got := cfg.DriverPayFromCash(); got != want {
			t.Errorf("%s: %v, want %v", raw, got, want)
		}
	}
}

// The owner's X of 2026-11-13: what a forced close does with an unpaid
// counter check (main's close_unpaid_counter_action; missing = waste).
func TestCloseUnpaidCounterAction(t *testing.T) {
	var none *Config
	if none.CloseUnpaidCounterAction() != "waste" {
		t.Fatal("no config yet: waste")
	}
	for raw, want := range map[string]string{`{}`: "waste", `{"close_unpaid_counter_action":"carry"}`: "carry",
		`{"close_unpaid_counter_action":"waste"}`: "waste", `{"close_unpaid_counter_action":"keep"}`: "waste"} {
		cfg, err := ParseConfig([]byte(`{"version":1,"staff":[],"settings":` + raw + `}`))
		if err != nil {
			t.Fatal(err)
		}
		if got := cfg.CloseUnpaidCounterAction(); got != want {
			t.Errorf("%s: %s, want %s", raw, got, want)
		}
	}
}
