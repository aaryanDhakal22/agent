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
