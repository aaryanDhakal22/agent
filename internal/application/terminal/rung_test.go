package terminal

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/pricing"
	"quiccpos/agent/internal/domain/terminal"
)

// Prices don't move under an order being taken (contract §2.10u).

// editMenu sends a new counter menu: fn changes main's pricing context
// (as JSON) first.
func (r *rig) editMenu(t *testing.T, version int64, fn func(menu map[string]any)) {
	t.Helper()
	raw, _ := json.Marshal(caseContext(t))
	var ctxMap map[string]any
	if err := json.Unmarshal(raw, &ctxMap); err != nil {
		t.Fatal(err)
	}
	fn(ctxMap["menu"].(map[string]any))
	r.main.set(PathCounterMenu, map[string]any{"menu_version": version, "build": map[string]any{"view": map[string]any{"kind": "counter"}},
		"pricing": ctxMap, "routes": map[string]any{cheese: map[string]any{"printer_id": pizzaPrn, "name": "Pizza"}}})
	if changed, err := r.svc.Store().Refresh(r.ctx, terminal.KindCounterMenu); err != nil || !changed {
		t.Fatalf("menu refresh: %v %v", changed, err)
	}
}

// raise adds cents to every price of item id (its sizes too) and to every
// topping.
func raise(id string, cents float64) func(map[string]any) {
	return func(menu map[string]any) {
		for _, c := range menu["categories"].([]any) {
			for _, it := range c.(map[string]any)["items"].([]any) {
				m := it.(map[string]any)
				if m["id"] != id {
					continue
				}
				m["price_cents"] = m["price_cents"].(float64) + cents
				for _, sz := range asList(m["sizes"]) {
					sz.(map[string]any)["price_cents"] = sz.(map[string]any)["price_cents"].(float64) + cents
				}
			}
		}
		for _, g := range menu["modifier_groups"].([]any) {
			for _, o := range g.(map[string]any)["options"].([]any) {
				om := o.(map[string]any)
				om["price_cents"] = om["price_cents"].(float64) + cents
				for _, p := range asList(om["prices"]) {
					p.(map[string]any)["price_cents"] = p.(map[string]any)["price_cents"].(float64) + cents
				}
			}
		}
	}
}

// drop takes item id off the menu.
func drop(id string) func(map[string]any) {
	return func(menu map[string]any) {
		for _, c := range menu["categories"].([]any) {
			cm := c.(map[string]any)
			var keep []any
			for _, it := range cm["items"].([]any) {
				if it.(map[string]any)["id"] != id {
					keep = append(keep, it)
				}
			}
			cm["items"] = keep
		}
	}
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

func rungLine(l pricing.Line, q pricing.QuoteLine) (pricing.Line, Rung) {
	u := q.UnitCents
	return l, Rung{Cents: &u, Name: q.Name, Mods: q.Mods}
}

func (r *rig) quote(t *testing.T, c Cart) *QuoteView {
	t.Helper()
	v, err := r.svc.Quote(r.ctx, till, QuoteIn{Type: "counter", Cart: c})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func rq(v *QuoteView) pricing.Quote { return v.Quote.(pricing.Quote) }

// The security round (M3): a new order has nothing to keep, so a rung is
// never a price by itself. A rung that isn't today's menu price (the menu
// moved since the quote, or a made-up number) is a price change to it under
// the price-drop rule: over the cashier's limit a manager, journaled
// price_change, never price_kept. A rung at today's price is nothing.
func TestRungOnANewOrderIsAPriceChange(t *testing.T) {
	r := newRig(t)
	r.login(t, till, "1234")
	first := rq(r.quote(t, Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine()}}}))
	was := first.Lines[0].UnitCents
	// A made-up rung on an untouched menu: the 2224-cent pizza at 1000 (at
	// 225, the price of its one topping, the pizza itself would be $0: a
	// comp, journaled as one, which a cashier may give).
	forged := int64(1000)
	fc := Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine()}}, Rungs: []Rung{{Cents: &forged, Name: "Cheese Pizza"}}}
	v := r.quote(t, fc)
	if !v.NeedsManager || len(v.PriceChanges) != 1 || !v.PriceChanges[0].NeedsManager || len(v.KeptPrices) != 0 {
		t.Fatalf("a forged rung is a price drop for a manager: %+v", v)
	}
	var mr *terminal.ManagerRequired
	if _, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: tk(), Type: "counter", Cart: fc}); !errors.As(err, &mr) || mr.Action != terminal.ActionPriceChange {
		t.Fatalf("a forged rung placed without a manager: %v", err)
	}
	// The menu moves up while the order is taken.
	r.editMenu(t, 20, raise(cheese, 100))
	r.editMenu(t, 21, func(m map[string]any) { raise(cheese, 100)(m); raise(hawaiian, 300)(m) })
	plain := rq(r.quote(t, Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine()}}}))
	if plain.Lines[0].UnitCents == was {
		t.Fatalf("the menu didn't move: %d", was)
	}
	l, rg := rungLine(pizzaLine(), first.Lines[0])
	c := Cart{Cart: pricing.Cart{Lines: []pricing.Line{l}}, Rungs: []Rung{rg}}
	v = r.quote(t, c)
	q := rq(v)
	if q.Lines[0].UnitCents != was || len(q.Errors) != 0 || len(v.PriceChanges) != 1 || len(v.KeptPrices) != 0 {
		t.Fatalf("the old price is a price change now: %+v %+v", q.Lines[0], v)
	}
	if !v.NeedsManager { // 31 % under today's price, the cashier's limit is 20 %
		t.Fatalf("over the cashier's limit: %+v", v.PriceChanges)
	}
	if _, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: tk(), Type: "counter", Cart: c}); !errors.As(err, &mr) || mr.Action != terminal.ActionPriceChange {
		t.Fatalf("the old price without a manager: %v", err)
	}
	res, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: tk(), Type: "counter", Cart: c, ManagerPIN: "9876"})
	if err != nil {
		t.Fatal(err)
	}
	if it := res.Order.Order.Items[0]; it.LineTotal != was || it.SKU == order.SKUCustom || it.PrinterID != pizzaPrn {
		t.Fatalf("placed line %+v", it)
	}
	if j := journalOf(t, r, terminal.ActionPriceKept); len(j) != 0 {
		t.Fatalf("a new order keeps no price: %v", j)
	}
	if j := journalOf(t, r, terminal.ActionPriceChange); len(j) != 1 || j[0]["approved_by"] != "mgr-1" {
		t.Fatalf("price_change %v", j)
	}
	// The stored cart keeps the rung fields (a reopened order reads them).
	sc, ok := storedCart(res.Order.Order)
	if !ok || len(sc.Rungs) != 1 || sc.Rungs[0].Cents == nil || *sc.Rungs[0].Cents != was {
		t.Fatalf("stored cart %+v", sc)
	}
	// A rung at today's price: no change, no manager.
	l2, rg2 := rungLine(pizzaLine(), plain.Lines[0])
	v = r.quote(t, Cart{Cart: pricing.Cart{Lines: []pricing.Line{l2}}, Rungs: []Rung{rg2}})
	if v.NeedsManager || len(v.PriceChanges) != 0 || len(v.KeptPrices) != 0 {
		t.Fatalf("today's price as the rung: %+v", v)
	}
	// An edit of the sent order: its own line keeps the price stored with
	// it whatever the terminal sends (a lower rung is ignored); a new line
	// with a made-up rung is a price change for a manager.
	low := int64(100)
	own := Cart{Cart: pricing.Cart{Lines: []pricing.Line{l}}, Rungs: []Rung{{Cents: &low, Name: "Cheese Pizza"}}}
	ch, err := r.svc.ChangeCart(r.ctx, till, res.Order.Order.ID, ChangeIn{ClientKey: tk(), Cart: own})
	if err != nil || ch.Order.Order.Items[0].LineTotal != was {
		t.Fatalf("the order's own line keeps its stored price: %+v %v", ch, err)
	}
	soda1 := pricing.Line{Qty: 1, Selection: pricing.Selection{ItemID: soda}}
	one := int64(1)
	added := Cart{Cart: pricing.Cart{Lines: []pricing.Line{l, soda1}}, Rungs: []Rung{rg, {Cents: &one, Name: "Soda"}}}
	if _, err := r.svc.ChangeCart(r.ctx, till, res.Order.Order.ID, ChangeIn{ClientKey: tk(), Cart: added}); !errors.As(err, &mr) || mr.Action != terminal.ActionPriceChange {
		t.Fatalf("a made-up rung on an added line: %v", err)
	}
}

// A line whose item left the menu: without rung_cents still 422; with it,
// an open-price custom line at the rung price named rung_name, its mods in
// the note, journaled menu_gone_line (not custom_line).
func TestRungGoneItemIsACustomLine(t *testing.T) {
	r := newRig(t)
	r.login(t, till, "1234")
	soda1 := pricing.Line{Qty: 2, Selection: pricing.Selection{ItemID: soda}}
	first := rq(r.quote(t, Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine(), soda1}}}))
	r.editMenu(t, 30, drop(cheese))
	bare := rq(r.quote(t, Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine(), soda1}}}))
	if len(bare.Errors) == 0 || bare.Errors[0].Code != pricing.IssueUnknownItem {
		t.Fatalf("no rung: %+v", bare.Errors)
	}
	l, rg := rungLine(pizzaLine(), first.Lines[0])
	l.Note = "well done"
	c := Cart{Cart: pricing.Cart{Lines: []pricing.Line{l, soda1}}, Rungs: []Rung{rg}}
	v := r.quote(t, c)
	q := rq(v)
	if len(q.Errors) != 0 || !q.Lines[0].Custom || q.Lines[0].UnitCents != first.Lines[0].UnitCents || q.Lines[0].Name != "Cheese Pizza" {
		t.Fatalf("gone quote: %+v %+v", q.Errors, q.Lines[0])
	}
	if len(v.MenuGone) != 1 || v.MenuGone[0].Reason != pricing.IssueUnknownItem || v.MenuGone[0].ItemID != cheese || v.NeedsManager {
		t.Fatalf("menu_gone %+v", v.MenuGone)
	}
	res, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: tk(), Type: "counter", Cart: c})
	if err != nil {
		t.Fatal(err)
	}
	it := res.Order.Order.Items[0]
	if it.SKU != order.SKUCustom || it.Name != "Cheese Pizza" || it.UnitPrice != first.Lines[0].UnitCents ||
		!strings.Contains(it.Notes, "Pepperoni") || !strings.HasSuffix(it.Notes, "well done") {
		t.Fatalf("gone line stored as %+v", it)
	}
	if j := journalOf(t, r, terminal.ActionMenuGoneLine); len(j) != 1 || j[0]["reason"] != pricing.IssueUnknownItem {
		t.Fatalf("menu_gone_line %v", j)
	}
	if j := journalOf(t, r, terminal.ActionCustomLine); len(j) != 0 {
		t.Fatalf("custom_line %v", j)
	}
	// A rung price out of range is a bad request.
	bad := int64(-1)
	if _, err := r.svc.Quote(r.ctx, till, QuoteIn{Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{l}}, Rungs: []Rung{{Cents: &bad}}}}); err == nil {
		t.Fatal("negative rung_cents taken")
	}
}

// A sent order's lines keep what they were charged on an edit, halves and
// options included (M-15 extended), and a line whose item then left the
// menu becomes a custom line at that price (journaled once).
func TestSentOrderKeepsFullPricesAndGoneLines(t *testing.T) {
	r := newRig(t)
	r.login(t, till, "1234")
	res, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: tk(), Type: "counter", Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine()}}}})
	if err != nil {
		t.Fatal(err)
	}
	id, was := res.Order.Order.ID, res.Order.Order.Items[0].LineTotal
	r.editMenu(t, 40, func(m map[string]any) { raise(cheese, 100)(m); raise(hawaiian, 200)(m) })
	soda1 := pricing.Line{Qty: 1, Selection: pricing.Selection{ItemID: soda}}
	cart := Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine(), soda1}}}
	v, err := r.svc.Quote(r.ctx, till, QuoteIn{Cart: cart, OrderID: id})
	if err != nil || rq(v).Lines[0].UnitCents != was || len(v.KeptPrices) != 1 || v.KeptPrices[0].From != terminal.RungFromOrder {
		t.Fatalf("quote with order_id: %+v %v", v, err)
	}
	ch, err := r.svc.ChangeCart(r.ctx, till, id, ChangeIn{ClientKey: tk(), Cart: cart})
	if err != nil {
		t.Fatal(err)
	}
	if got := ch.Order.Order.Items[0].LineTotal; got != was {
		t.Fatalf("half and half + topping repriced: %d, was %d", got, was)
	}
	if j := journalOf(t, r, terminal.ActionPriceKept); len(j) != 0 {
		t.Fatalf("an order's own kept price isn't journaled: %v", j)
	}
	// The pizza leaves the menu: the next edit keeps it as a custom line.
	r.editMenu(t, 41, drop(cheese))
	cart.Lines = append(cart.Lines, soda1)
	ch, err = r.svc.ChangeCart(r.ctx, till, id, ChangeIn{ClientKey: tk(), Cart: cart})
	if err != nil {
		t.Fatal(err)
	}
	it := ch.Order.Order.Items[0]
	if it.SKU != order.SKUCustom || it.LineTotal != was || it.Name != "Cheese Pizza" || !strings.Contains(it.Notes, "Hawaiian") {
		t.Fatalf("gone pizza %+v", it)
	}
	// And again (one more soda): the gone line isn't journaled twice.
	cart.Lines = append(cart.Lines, soda1)
	if _, err := r.svc.ChangeCart(r.ctx, till, id, ChangeIn{ClientKey: tk(), Cart: cart}); err != nil {
		t.Fatal(err)
	}
	if j := journalOf(t, r, terminal.ActionMenuGoneLine); len(j) != 1 || j[0]["from"] != terminal.RungFromOrder {
		t.Fatalf("menu_gone_line %v", j)
	}
}

// The cart's JSON: rung_* ride on the lines; a cart without them encodes
// exactly as before (stored fingerprints keep matching).
func TestCartRungJSON(t *testing.T) {
	in := `{"type":"","payment":"","lines":[{"item_id":"a","qty":1,"rung_cents":1299,"rung_name":"Cheese Pizza","rung_mods":["16 XL"]},{"item_id":"b","qty":2}],"discount":{"pct":10,"reason":"x"}}`
	var c Cart
	if err := json.Unmarshal([]byte(in), &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Rungs) != 2 || *c.Rungs[0].Cents != 1299 || c.Rungs[0].Name != "Cheese Pizza" || c.Rungs[1].Cents != nil || c.Discount.Pct != 10 {
		t.Fatalf("decoded %+v", c)
	}
	out, _ := json.Marshal(c)
	var back Cart
	if err := json.Unmarshal(out, &back); err != nil || *back.Rungs[0].Cents != 1299 || back.Lines[1].Qty != 2 {
		t.Fatalf("round trip %s", out)
	}
	plain := Cart{Cart: pricing.Cart{Lines: []pricing.Line{{Qty: 1}}}}
	a, _ := json.Marshal(plain)
	b, _ := json.Marshal(cartWire{Cart: plain.Cart})
	if string(a) != string(b) {
		t.Fatalf("plain cart changed: %s vs %s", a, b)
	}
}
