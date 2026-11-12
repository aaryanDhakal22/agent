package terminal

import (
	"encoding/json"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/menuview"
	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/pricing"
	"quiccpos/agent/internal/domain/terminal"

	"github.com/google/uuid"
)

// show_on_driver_ticket stamping (the canonical-menu merge, 2026-11-12):
// counter lines (buildItems) and channel orders from main (StampDriverTicket)
// get each modifier child stamped from the counter-menu build's option flag.

const (
	sub     = "00000000-0000-0000-0000-0000000000f1"
	drinks  = "00000000-0000-0000-0000-0000000000f2"
	cook    = "00000000-0000-0000-0000-0000000000f3"
	pepsi   = "00000000-0000-0000-0000-0000000000f4"
	welldon = "00000000-0000-0000-0000-0000000000f5"
	lettuce = "00000000-0000-0000-0000-0000000000f6"
)

// driverMenu is case 33's menu plus a sub with a Drinks group (Can Pepsi,
// flagged) and a Cook group (Well Done; Lettuce, a default), each $0; the
// build carries show_on_driver_ticket as main sends it (omitted when false).
func driverMenu(t *testing.T, flagged bool) *terminal.CounterMenu {
	t.Helper()
	ctx := caseContext(t)
	ctx.Menu.ModifierGroups = append(ctx.Menu.ModifierGroups,
		menuview.BuildGroup{ID: uuid.MustParse(drinks), Name: "Drinks", MaxSelect: 1,
			Options: []menuview.BuildOption{{ID: uuid.MustParse(pepsi), Name: "Can Pepsi"}}},
		menuview.BuildGroup{ID: uuid.MustParse(cook), Name: "Cook", MaxSelect: 3,
			Options: []menuview.BuildOption{{ID: uuid.MustParse(welldon), Name: "Well Done"}, {ID: uuid.MustParse(lettuce), Name: "Lettuce", IsDefault: true}}})
	ctx.Menu.Categories[0].Items = append(ctx.Menu.Categories[0].Items, menuview.BuildItem{ID: sub, Name: "Cheesesteak Combo", PriceCents: 1299,
		Anchor: menuview.Anchor{ItemIDs: []uuid.UUID{uuid.MustParse(sub)}}, ModifierGroupIDs: []uuid.UUID{uuid.MustParse(drinks), uuid.MustParse(cook)}})
	ctx.Items[sub] = pricing.ItemFacts{CategoryID: ctx.Items[cheese].CategoryID}
	raw, _ := json.Marshal(ctx.Menu)
	var build map[string]any
	if err := json.Unmarshal(raw, &build); err != nil {
		t.Fatal(err)
	}
	if flagged {
		for _, g := range build["modifier_groups"].([]any) {
			for _, o := range g.(map[string]any)["options"].([]any) {
				if om := o.(map[string]any); om["name"] == "Can Pepsi" {
					om["show_on_driver_ticket"] = true
				}
			}
		}
	}
	b, _ := json.Marshal(build)
	return &terminal.CounterMenu{MenuVersion: 1, Build: b, Pricing: ctx, Routes: map[string]terminal.Route{}}
}

func stampOf(t *testing.T, it order.LineItem, name string) *bool {
	t.Helper()
	for _, c := range it.Children {
		if c.Name == name {
			return c.ShowOnDriverTicket
		}
	}
	t.Fatalf("no child %q in %+v", name, it.Children)
	return nil
}

func want(t *testing.T, got *bool, w *bool, what string) {
	t.Helper()
	switch {
	case w == nil && got != nil:
		t.Errorf("%s: stamped %v, want unset", what, *got)
	case w != nil && got == nil:
		t.Errorf("%s: unset, want %v", what, *w)
	case w != nil && *got != *w:
		t.Errorf("%s: %v, want %v", what, *got, *w)
	}
}

func bp(v bool) *bool { return &v }

func TestDriverTicket_CounterStamps(t *testing.T) {
	menu := driverMenu(t, true)
	cart := Cart{Cart: pricing.Cart{Lines: []pricing.Line{
		{Qty: 1, Selection: pricing.Selection{ItemID: sub, Options: []pricing.Pick{
			{GroupID: drinks, OptionID: pepsi}, {GroupID: cook, OptionID: welldon}, {GroupID: cook, OptionID: lettuce, Mode: "no"}}}},
		pizzaLine(),
	}}}
	p, err := priceCart(menu, time.UTC, terminal.TypeDelivery, cart, time.Now(), 3)
	if err != nil || len(p.quote.Errors) > 0 {
		t.Fatalf("price: %v %+v", err, p.quote.Errors)
	}
	items, _, _, err := buildItems(menu, p)
	if err != nil {
		t.Fatal(err)
	}
	combo, pizza := items[0], items[1]
	want(t, stampOf(t, combo, "Can Pepsi"), bp(true), "drink")
	want(t, stampOf(t, combo, "Well Done"), bp(false), "note")
	want(t, stampOf(t, combo, "NO Lettuce"), bp(false), "NO")
	want(t, stampOf(t, pizza, "Pepperoni (1st half)"), bp(false), "topping on a half (option name, not the label)")
	for _, c := range pizza.Children {
		if c.Mode == order.ModeSpecialty {
			want(t, c.ShowOnDriverTicket, nil, "half specialty (no such option)")
		}
	}

	// A build without any flag (a main before show_on_driver_ticket): nothing stamped.
	old := driverMenu(t, false)
	p, _ = priceCart(old, time.UTC, terminal.TypeDelivery, cart, time.Now(), 3)
	items, _, _, _ = buildItems(old, p)
	for _, it := range items {
		for _, c := range it.Children {
			want(t, c.ShowOnDriverTicket, nil, "no flag in the build: "+c.Name)
		}
	}
}

func TestDriverTicket_ChannelOrderStamps(t *testing.T) {
	r := newRig(t)
	st := r.svc.Store()
	menu := driverMenu(t, true)
	body, _ := json.Marshal(map[string]any{"menu_version": 99, "build": json.RawMessage(menu.Build), "pricing": menu.Pricing, "routes": map[string]any{}})
	r.main.set(PathCounterMenu, json.RawMessage(body))
	if changed, err := st.Refresh(r.ctx, terminal.KindCounterMenu); err != nil || !changed {
		t.Fatalf("refresh: %v %v", changed, err)
	}
	line := func() order.LineItem {
		return order.LineItem{Name: "Cheesesteak Combo", Quantity: 1, UnitPrice: 1299, MenuItemID: sub, Children: []order.LineItem{
			{Name: "Can Pepsi", Quantity: 1}, {Name: "Well Done", Quantity: 1}, {Name: "NO Lettuce", Mode: order.ModeNo, Quantity: 1},
			{Name: "Mystery", Quantity: 1}, {Name: "Can Pepsi", Quantity: 1, ShowOnDriverTicket: bp(false)}}}
	}
	pizza := order.LineItem{Name: "Cheese Pizza", Quantity: 1, MenuItemID: cheese, Children: []order.LineItem{
		{Name: "Pepperoni (2nd half)", Portion: order.PortionSecond, Quantity: 1}, {Name: "2x Mushrooms", Quantity: 1}}}
	web := order.Snapshot{Source: "web", Items: []order.LineItem{line(), pizza, {Name: "Soda", Quantity: 1}}}
	in := web.Items[0].Children
	st.StampDriverTicket(&web)
	if in[0].ShowOnDriverTicket != nil {
		t.Fatal("the caller's children were written")
	}
	c := web.Items[0]
	want(t, c.Children[0].ShowOnDriverTicket, bp(true), "drink")
	want(t, c.Children[1].ShowOnDriverTicket, bp(false), "note")
	want(t, c.Children[2].ShowOnDriverTicket, bp(false), "NO")
	want(t, c.Children[3].ShowOnDriverTicket, nil, "not on the menu")
	want(t, c.Children[4].ShowOnDriverTicket, bp(false), "already stamped keeps its stamp")
	want(t, web.Items[1].Children[0].ShowOnDriverTicket, bp(false), "half suffix stripped")
	want(t, web.Items[1].Children[1].ShowOnDriverTicket, bp(false), "repeat prefix stripped")

	// No bank item on the line, a vendor order, a POS order: left as they are.
	noID := order.Snapshot{Source: "web", Items: []order.LineItem{line()}}
	noID.Items[0].MenuItemID = ""
	st.StampDriverTicket(&noID)
	want(t, noID.Items[0].Children[0].ShowOnDriverTicket, nil, "line without menu_item_id")
	for _, src := range []string{order.SourceOnline, "pos", ""} {
		v := order.Snapshot{Source: src, Items: []order.LineItem{line()}}
		st.StampDriverTicket(&v)
		want(t, v.Items[0].Children[0].ShowOnDriverTicket, nil, "source "+src)
	}
	// No counter menu: nothing stamped.
	empty := NewStore(r.repo, r.main.fetch, nil, r.svc.store.logger)
	w2 := order.Snapshot{Source: "web", Items: []order.LineItem{line()}}
	empty.StampDriverTicket(&w2)
	want(t, w2.Items[0].Children[0].ShowOnDriverTicket, nil, "no counter menu")
}

func TestDriverTicket_JSON(t *testing.T) {
	b, _ := json.Marshal(order.LineItem{Name: "x", ShowOnDriverTicket: bp(false)})
	var back order.LineItem
	if err := json.Unmarshal(b, &back); err != nil || back.ShowOnDriverTicket == nil || *back.ShowOnDriverTicket {
		t.Fatalf("false must survive the snapshot: %s", b)
	}
	if b, _ := json.Marshal(order.LineItem{Name: "x"}); string(b) != `{"id":"","position":0,"name":"x","quantity":0,"unit_price_cents":0,"line_total_cents":0}` {
		t.Fatalf("unset must be omitted: %s", b)
	}
}
