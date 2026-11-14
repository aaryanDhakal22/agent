package terminal

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/pricing"
	"quiccpos/agent/internal/domain/terminal"
	mr "quiccpos/agent/internal/domain/terminal/moneyrules"
)

// Order lines (batch A, the owner 2026-11-14; contract §2.10an): mods never
// stack (#20), a mod's price by any cashier (#11), priced notes (#19), BYO's
// included toppings at the counter (#23).

// linesRig is a Pi whose counter menu is one shared pricing case's context.
func linesRig(t *testing.T, caseFile string, routes map[string]any, extra ...terminal.Staff) *rig {
	t.Helper()
	r := newRig(t)
	raw, err := os.ReadFile("../../domain/pricing/testdata/cases/" + caseFile)
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Context pricing.Context `json:"context"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	c.Context.Counter, c.Context.MaxRepeat, c.Context.PriceOverrides = false, 0, false // the Pi sets them itself
	r.main.set(PathCounterMenu, map[string]any{"menu_version": 14, "build": map[string]any{"view": map[string]any{"kind": "counter"}},
		"pricing": c.Context, "routes": routes})
	r.main.set(PathTerminalConfig, staffConfig(t, 2, extra...))
	for _, k := range []string{terminal.KindCounterMenu, terminal.KindTerminalConfig} {
		if _, err := r.svc.store.Refresh(r.ctx, k); err != nil {
			t.Fatal(err)
		}
	}
	r.login(t, till, "1234")
	return r
}

const (
	subItem = "00000000-0000-0000-0000-0000000002a1"
	sub8    = "00000000-0000-0000-0000-0000000002b1"
	subMods = "00000000-0000-0000-0000-0000000002c1"
	subLite = "00000000-0000-0000-0000-0000000002d1"
	subFry  = "00000000-0000-0000-0000-0000000002d3"
	subMayo = "00000000-0000-0000-0000-0000000002d4"
)

func modLineNames(it order.LineItem) string {
	var n []string
	for _, c := range it.Children {
		n = append(n, c.Name)
	}
	return strings.Join(n, ", ")
}

// #20: "Lite Fried Onions" + "Lite Mayo" are two children on the stored order
// (the owner saw "2 x Lite Fried Onions, Mayo": the two Lite picks folded
// into one "2x Lite" and the second Lite was lost from Mayo). Sub Mods is a
// pick-once group: the second Lite used to be refused ("Lite only once").
func TestLiteFriedOnionsAndLiteMayoStayTwoLines(t *testing.T) {
	r := linesRig(t, "78_mods_never_stack.json", map[string]any{subItem: map[string]any{"printer_id": pizzaPrn}})
	mods := func(ids ...string) []pricing.Pick {
		var out []pricing.Pick
		for _, id := range ids {
			out = append(out, pricing.Pick{GroupID: subMods, OptionID: id})
		}
		return out
	}
	line := pricing.Line{Qty: 1, Selection: pricing.Selection{ItemID: subItem, SizeID: sub8, Options: mods(subLite, subFry, subLite, subMayo)}}
	res, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: "k-lite-0001", Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{line}}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := modLineNames(res.Order.Order.Items[0]); got != "Lite Add Fried Onions, Lite Add Mayo" {
		t.Fatalf("children: %q", got)
	}
}

// #11: any cashier sets a mod's price (no PIN); the child carries it, the
// item's own price doesn't move, and it is journaled once (a later kitchen
// change with the same price doesn't journal it again).
func TestModPriceAnyCashierJournaledOnce(t *testing.T) {
	r := gapsRig(t, nil)
	fifty := int64(50)
	pizza := pricing.Line{Qty: 1, Selection: pricing.Selection{ItemID: gapPizza, SizeID: gapXL,
		Options: []pricing.Pick{{GroupID: gapToppings, OptionID: gapPep, PriceCents: &fifty}, {GroupID: gapToppings, OptionID: gapMush}}}}
	v, err := r.svc.Quote(r.ctx, till, QuoteIn{Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizza}}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(v.ModPriceChanges) != 1 || v.ModPriceChanges[0].FromCents != 225 || v.ModPriceChanges[0].ToCents != 50 || v.ModPriceChanges[0].Pick != 0 ||
		v.ModPriceChanges[0].NeedsManager || v.NeedsManager {
		t.Fatalf("quote: %+v needs %v", v.ModPriceChanges, v.NeedsManager)
	}
	ql := v.Quote.(pricing.Quote).Lines[0]
	if *ql.BaseCents != 1299 || ql.UnitCents != 1299+50+225 || ql.ModPrices[0].PriceCents != 50 {
		t.Fatalf("quote line: %+v", ql)
	}
	cart := Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizza}}}
	res, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: "k-modprice-1", Cart: cart})
	if err != nil {
		t.Fatal(err)
	}
	it := res.Order.Order.Items[0]
	if it.UnitPrice != 1299 || it.Children[0].Name != "Pepperoni" || it.Children[0].UnitPrice != 50 || it.Children[1].UnitPrice != 225 {
		t.Fatalf("line: %+v", it)
	}
	if j := journalOf(t, r, terminal.ActionModPriceChange); len(j) != 1 || j[0]["from_cents"] != float64(225) || j[0]["to_cents"] != float64(50) ||
		j[0]["_staff"] != "cash-1" || j[0]["name"] != "Pepperoni" {
		t.Fatalf("journal: %+v", j)
	}
	cart.Lines = append(cart.Lines, pricing.Line{Qty: 1, Selection: pricing.Selection{ItemID: gapPizza, SizeID: gapXL}})
	if _, err := r.svc.ChangeCart(r.ctx, till, res.Order.Order.ID, ChangeIn{Cart: cart, ClientKey: "k-modprice-2"}); err != nil {
		t.Fatal(err)
	}
	if j := journalOf(t, r, terminal.ActionModPriceChange); len(j) != 1 {
		t.Fatalf("journaled again: %+v", j)
	}
}

// #11: a driver lowering a mod needs a manager (money-rules M06); with the
// manager's PIN it goes, journaled with approved_by.
func TestModPriceDriverNeedsAManager(t *testing.T) {
	drv := terminal.Staff{ID: "drv-1", FirstName: "Dev", Role: terminal.RoleDriver, PinHash: hash(t, "4321")}
	r := linesRig(t, "45_doubles.json", map[string]any{}, drv)
	driverSeat := Actor{DeviceID: "dev-9", DeviceName: "Till 9"}
	r.login(t, driverSeat, "4321")
	fifty := int64(50)
	cart := Cart{Cart: pricing.Cart{Lines: []pricing.Line{{Qty: 1, Selection: pricing.Selection{ItemID: gapPizza, SizeID: gapXL,
		Options: []pricing.Pick{{GroupID: gapToppings, OptionID: gapPep, PriceCents: &fifty}}}}}}}
	_, err := r.svc.PlaceOrder(r.ctx, driverSeat, OrderIn{ClientKey: "k-modprice-d1", Cart: cart})
	var need *terminal.ManagerRequired
	if !errors.As(err, &need) || need.Action != terminal.ActionModPrice || need.Requested != 175 {
		t.Fatalf("driver: %v", err)
	}
	if _, err := r.svc.PlaceOrder(r.ctx, driverSeat, OrderIn{ClientKey: "k-modprice-d2", Cart: cart, ManagerPIN: "9876"}); err != nil {
		t.Fatal(err)
	}
	if j := journalOf(t, r, terminal.ActionModPriceChange); len(j) != 1 || j[0]["approved_by"] != "mgr-1" {
		t.Fatalf("journal: %+v", j)
	}
}

// The shared rule table's mod_prices rows through the Pi's check.
func TestMoneyRulesModPrices(t *testing.T) {
	raw, err := os.ReadFile("../../domain/terminal/testdata/money-rules.json")
	if err != nil {
		t.Fatal(err)
	}
	var tab struct {
		ModPrices []mr.RuleCase `json:"mod_prices"`
	}
	if err := json.Unmarshal(raw, &tab); err != nil {
		t.Fatal(err)
	}
	if len(tab.ModPrices) < 7 {
		t.Fatalf("only %d mod_prices rows", len(tab.ModPrices))
	}
	rules := mr.LoadRules(t, "../../domain/terminal/testdata/money-rules.json")
	for _, c := range tab.ModPrices {
		cfg := rules.ConfigFor(t, c.Settings)
		ch := []ModPriceChange{{FromCents: c.From, ToCents: c.To}}
		need := checkModPrices(cfg, rules.Staff(cfg, c.By), pricing.Cart{}, ch, nil)
		if got, want := mr.Answer(need, nil), mr.Want(t, c.Pi); got != want {
			t.Errorf("%s (%s): the Pi answers %s, the table says %s", c.ID, c.Note, got, want)
		}
	}
}

// #19: a priced note is its own line under its item: sku note, the item's
// line id as parent_line_id, the item's printer; on the receipt with its
// price. A note_of that doesn't point back at an item is refused.
func TestPricedNoteTravelsWithItsItem(t *testing.T) {
	r := gapsRig(t, nil)
	at, dollar := 0, int64(100)
	pizza := pricing.Line{Qty: 2, Selection: pricing.Selection{ItemID: gapPizza, SizeID: gapXL}}
	note := pricing.Line{Qty: 2, Custom: true, Name: "extra crispy", NoteOf: &at, Selection: pricing.Selection{PriceCents: &dollar}}
	res, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: "k-note-0001", Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizza, note}}}})
	if err != nil {
		t.Fatal(err)
	}
	items := res.Order.Order.Items
	if len(items) != 2 || items[1].SKU != order.SKUNote || items[1].ParentLineID != items[0].ID || items[1].Name != "extra crispy" ||
		items[1].PrinterID != pizzaPrn || items[1].PrinterID != items[0].PrinterID || items[1].UnitPrice != 100 || items[1].LineTotal != 200 ||
		res.Order.Order.Money.Subtotal != items[0].LineTotal+200 {
		t.Fatalf("items: %+v", items)
	}
	bad := note
	ahead := 1
	bad.NoteOf = &ahead
	if q := quoteOf(t, r, bad, pizza); len(q.Errors) != 1 || q.Errors[0].Code != pricing.IssueCustomLine || *q.Errors[0].Line != 0 {
		t.Fatalf("a note before its item: %+v", q.Errors)
	}
}

// #23: BYO's first 5 toppings are free at the counter, the 6th charged at
// the line's size price (case 79's menu: 12" toppings $1.75).
func TestBYOSixthToppingAtItsSizePriceOnThePi(t *testing.T) {
	r := linesRig(t, "79_byo_sixth_topping_at_its_size_price.json", map[string]any{})
	tp := func(ids ...int) []pricing.Pick {
		var out []pricing.Pick
		for _, n := range ids {
			out = append(out, pricing.Pick{GroupID: "00000000-0000-0000-0000-0000000000c1", OptionID: "00000000-0000-0000-0000-0000000000d" + string(rune('0'+n))})
		}
		return out
	}
	byo := func(picks []pricing.Pick) pricing.Line {
		return pricing.Line{Qty: 1, Selection: pricing.Selection{ItemID: "00000000-0000-0000-0000-0000000000a1", SizeID: "00000000-0000-0000-0000-0000000000b1",
			Options: picks}}
	}
	if q := quoteOf(t, r, byo(tp(1, 2, 3, 4, 1))); q.Lines[0].UnitCents != 1599 {
		t.Fatalf("5 toppings: %+v", q.Lines[0])
	}
	q := quoteOf(t, r, byo(tp(1, 2, 3, 4, 1, 2)))
	if q.Lines[0].UnitCents != 1599+175 || len(q.Lines[0].ModPrices) != 6 || !q.Lines[0].ModPrices[0].Included || q.Lines[0].ModPrices[5].Included {
		t.Fatalf("6 toppings: %+v", q.Lines[0])
	}
}

// A priced note under a Quicc Add follows that line's printer_id: null =
// nowhere (no kitchen paper), an id = that printer alone; never the item route.
func TestPricedNoteUnderAQuiccAddFollowsItsPrinter(t *testing.T) {
	for _, c := range []struct {
		name, body       string
		routing, printer string
	}{
		{"null: nowhere", `null`, order.RoutingNone, ""},
		{"a printer: that one", `"p-pizza"`, order.RoutingMatched, "p-pizza"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var cart pricing.Cart
			raw := `{"lines":[{"custom":true,"name":"Side of ranch","qty":2,"price_cents":75,"printer_id":` + c.body + `},` +
				`{"custom":true,"name":"in a cup","qty":2,"price_cents":25,"note_of":0}]}`
			if err := json.Unmarshal([]byte(raw), &cart); err != nil {
				t.Fatal(err)
			}
			m := driverMenu(t, true)
			m.QuickAddItemID = "qa-item"
			m.Routes = map[string]terminal.Route{"qa-item": {PrinterID: "p-wings", Name: "Wings"}}
			p, err := priceCart(m, time.UTC, terminal.TypeCounter, Cart{Cart: cart}, time.Now(), 3)
			if err != nil || len(p.quote.Errors) > 0 || len(noteIssues(cart)) > 0 {
				t.Fatalf("price: %v %+v %+v", err, p.quote.Errors, noteIssues(cart))
			}
			items, _, _, err := buildItems(m, p)
			if err != nil || len(items) != 2 {
				t.Fatalf("build: %v %+v", err, items)
			}
			note := items[1]
			if note.SKU != order.SKUNote || note.ParentLineID != items[0].ID || note.Routing != c.routing || note.PrinterID != c.printer ||
				note.LineTotal != 50 {
				t.Errorf("note: %+v", note)
			}
		})
	}
}
