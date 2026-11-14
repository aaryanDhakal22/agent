package terminal

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/pricing"
	"quiccpos/agent/internal/domain/terminal"
)

// The order screen's gaps (2026-11-11): double / triple toppings (repeated
// picks in an allow_multiple group, capped by main's topping_max_count),
// Quicc Add custom lines, and a missed required group as a warning on the
// counter. The menu is the shared pricing case 45 (Toppings allow_multiple
// with portions, a Club Sandwich with a required Side and min 2 Dips).

const (
	gapPizza    = "00000000-0000-0000-0000-0000000000a1"
	gapClub     = "00000000-0000-0000-0000-0000000000a3"
	gapToppings = "00000000-0000-0000-0000-0000000000c1"
	gapPep      = "00000000-0000-0000-0000-0000000000d1"
	gapMush     = "00000000-0000-0000-0000-0000000000d2"
	gapXL       = "00000000-0000-0000-0000-0000000000b2"
)

func gapsRig(t *testing.T, settings map[string]any) *rig {
	t.Helper()
	r := newRig(t)
	raw, err := os.ReadFile("../../domain/pricing/testdata/cases/45_doubles.json")
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Context pricing.Context `json:"context"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	c.Context.Counter, c.Context.MaxRepeat = false, 0 // the Pi sets both itself
	r.main.set(PathCounterMenu, map[string]any{"menu_version": 13, "build": map[string]any{"view": map[string]any{"kind": "counter"}},
		"pricing": c.Context, "routes": map[string]any{gapPizza: map[string]any{"printer_id": pizzaPrn, "name": "Pizza"}}})
	cfg := staffConfig(t, 2)
	for k, v := range settings {
		cfg["settings"].(map[string]any)[k] = v
	}
	r.main.set(PathTerminalConfig, cfg)
	for _, k := range []string{terminal.KindCounterMenu, terminal.KindTerminalConfig} {
		if _, err := r.svc.store.Refresh(r.ctx, k); err != nil {
			t.Fatal(err)
		}
	}
	r.login(t, till, "1234")
	return r
}

func toppingPicks(pairs ...string) []pricing.Pick {
	var out []pricing.Pick
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, pricing.Pick{GroupID: gapToppings, OptionID: pairs[i], Portion: pairs[i+1]})
	}
	return out
}

func quoteOf(t *testing.T, r *rig, lines ...pricing.Line) pricing.Quote {
	t.Helper()
	q, err := r.svc.Quote(r.ctx, till, QuoteIn{Cart: Cart{Cart: pricing.Cart{Lines: lines}}})
	if err != nil {
		t.Fatal(err)
	}
	return q.Quote.(pricing.Quote)
}

func TestDoubleToppingsCapAndEachPickItsOwnLine(t *testing.T) {
	r := gapsRig(t, nil)
	pizza := func(picks []pricing.Pick) pricing.Line {
		return pricing.Line{Qty: 1, Selection: pricing.Selection{ItemID: gapPizza, SizeID: gapXL, Options: picks}}
	}
	// topping_max_count missing = 3: a 4th Mushrooms on the 1st half is refused.
	four := pizza(toppingPicks(gapMush, "first", gapMush, "first", gapMush, "first", gapMush, "first"))
	if q := quoteOf(t, r, four); len(q.Errors) != 1 || q.Errors[0].Message != "Cheese Pizza: up to 3× Mushrooms." {
		t.Fatalf("cap 3: %+v", q.Errors)
	}
	r2 := gapsRig(t, map[string]any{"topping_max_count": 4})
	if q := quoteOf(t, r2, four); len(q.Errors) != 0 || q.Lines[0].UnitCents != 1299+4*113 {
		t.Fatalf("cap 4: %+v %+v", q.Errors, q.Lines)
	}
	// Mods never stack (the owner, 2026-11-14, #20): 2× Pepperoni (whole) +
	// 2× Mushrooms (1st half) are four children in pick order, each at its own
	// price (Cheese, the group's default, is left off: NO Cheese). Before:
	// one "2x Pepperoni" child at twice the price.
	res, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: "k-doubles-01",
		Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizza(toppingPicks(gapPep, "", gapMush, "first", gapPep, "", gapMush, "first"))}}}})
	if err != nil {
		t.Fatal(err)
	}
	it := res.Order.Order.Items[0]
	var got []string
	for _, c := range it.Children {
		got = append(got, c.Name)
	}
	if strings.Join(got, ", ") != "Pepperoni, Mushrooms (1st half), Pepperoni, Mushrooms (1st half), NO Cheese" || it.Children[0].UnitPrice != 225 ||
		it.Children[1].UnitPrice != 113 || it.Children[2].UnitPrice != 225 || it.Children[1].Portion != "first" ||
		it.LineTotal != 1299+450+226 || res.Order.Order.Money.Subtotal != it.LineTotal {
		t.Fatalf("children %v: %+v", got, it)
	}
}

func TestCustomLineIsPlacedAndJournaled(t *testing.T) {
	r := gapsRig(t, nil)
	custom := func(name string, cents int64, qty int) pricing.Line {
		return pricing.Line{Qty: qty, Custom: true, Name: name, Selection: pricing.Selection{PriceCents: &cents}}
	}
	if q := quoteOf(t, r, custom(strings.Repeat("x", 61), 100, 1)); len(q.Errors) != 1 || q.Errors[0].Code != pricing.IssueCustomLine {
		t.Fatalf("a long name: %+v", q.Errors)
	}
	cart := Cart{Cart: pricing.Cart{Lines: []pricing.Line{custom("Extra dough ball", 250, 2)}}}
	cart.Lines[0].Note = "on the side"
	res, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: "k-custom-001", Cart: cart})
	if err != nil {
		t.Fatal(err)
	}
	it := res.Order.Order.Items[0]
	if it.Name != "Extra dough ball" || it.SKU != order.SKUCustom || it.MenuItemID != "" || it.Routing != order.RoutingUnmatched ||
		it.UnitPrice != 250 || it.Quantity != 2 || it.LineTotal != 500 || it.Notes != "on the side" || res.Order.Order.Money.Tax != 30 {
		t.Fatalf("custom line: %+v money %+v", it, res.Order.Order.Money)
	}
	if j := journalOf(t, r, terminal.ActionCustomLine); len(j) != 1 || j[0]["name"] != "Extra dough ball" || j[0]["price_cents"] != float64(250) ||
		j[0]["_staff"] != "cash-1" {
		t.Fatalf("journal: %+v", j)
	}
	// A kitchen change that adds a second one journals only the new one.
	cart.Lines = append(cart.Lines, custom("", 0, 1))
	if _, err := r.svc.ChangeCart(r.ctx, till, res.Order.Order.ID, ChangeIn{Cart: cart, ClientKey: "k-custom-002"}); err != nil {
		t.Fatal(err)
	}
	if j := journalOf(t, r, terminal.ActionCustomLine); len(j) != 2 || j[0]["name"] != "Open item" && j[1]["name"] != "Open item" {
		t.Fatalf("journal after the change: %+v", j)
	}
}

func TestRequiredGroupIsAWarningOnTheCounter(t *testing.T) {
	r := gapsRig(t, nil)
	club := pricing.Line{Qty: 1, Selection: pricing.Selection{ItemID: gapClub}}
	q := quoteOf(t, r, club)
	if len(q.Errors) != 0 || len(q.LineWarnings) != 2 || q.LineWarnings[0].Code != pricing.IssueOptionRules || *q.LineWarnings[0].Line != 0 ||
		q.Warnings[0] != "Club Sandwich: choose a side." {
		t.Fatalf("warnings: %+v %+v %+v", q.Errors, q.Warnings, q.LineWarnings)
	}
	res, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: "k-required-1", Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{club}}}})
	if err != nil || res.Order.Order.Items[0].Name != "Club Sandwich" {
		t.Fatalf("placed: %+v %v", res, err)
	}
}
