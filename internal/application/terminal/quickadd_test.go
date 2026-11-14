package terminal

import (
	"encoding/json"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/pricing"
	"quiccpos/agent/internal/domain/terminal"
)

// A Quicc Add (custom) line prints where main's Quick add item routes (main
// 4.0.5): its printer, receipt only, or every station when it has no printer
// or main names no Quick add item (an older main).
func TestQuickAddLineRoutes(t *testing.T) {
	price := int64(450)
	cart := Cart{Cart: pricing.Cart{Lines: []pricing.Line{
		{Custom: true, Name: "Side of ranch x3", Qty: 2, Selection: pricing.Selection{PriceCents: &price}},
	}}}
	line := func(m *terminal.CounterMenu) order.LineItem {
		t.Helper()
		p, err := priceCart(m, time.UTC, terminal.TypeCounter, cart, time.Now(), 3)
		if err != nil || len(p.quote.Errors) > 0 {
			t.Fatalf("price: %v %+v", err, p.quote.Errors)
		}
		items, _, _, err := buildItems(m, p)
		if err != nil || len(items) != 1 {
			t.Fatalf("build: %v %+v", err, items)
		}
		it := items[0]
		if it.SKU != order.SKUCustom || it.Name != "Side of ranch x3" || it.UnitPrice != 450 || it.Quantity != 2 {
			t.Fatalf("line: %+v", it)
		}
		return it
	}
	cases := []struct {
		name, quickAdd   string
		route            *terminal.Route
		routing, printer string
	}{
		{"older main: no Quick add item", "", nil, order.RoutingUnmatched, ""},
		{"Quick add without a printer", "qa-item", nil, order.RoutingUnassigned, ""},
		{"Quick add on Wings", "qa-item", &terminal.Route{PrinterID: "p-wings", Name: "Wings"}, order.RoutingMatched, "p-wings"},
		{"Quick add receipt only", "qa-item", &terminal.Route{NoKitchen: true}, order.RoutingNone, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := driverMenu(t, true)
			m.QuickAddItemID = c.quickAdd
			m.Routes = map[string]terminal.Route{}
			if c.route != nil {
				m.Routes[c.quickAdd] = *c.route
			}
			it := line(m)
			if it.Routing != c.routing || it.PrinterID != c.printer {
				t.Errorf("routing %q printer %q, want %q %q", it.Routing, it.PrinterID, c.routing, c.printer)
			}
			if want := c.quickAdd; it.MenuItemID != want {
				t.Errorf("menu item %q, want %q", it.MenuItemID, want)
			}
		})
	}
}

// The counter menu body names the Quick add item; an older body has none.
func TestParseCounterMenuQuickAdd(t *testing.T) {
	m, err := terminal.ParseCounterMenu([]byte(`{"menu_version":3,"build":{"categories":[{"key":"a","name":"A","items":[]}]},"routes":{},"quick_add_item_id":"qa-1"}`))
	if err != nil || m.QuickAddItemID != "qa-1" {
		t.Fatalf("%v %+v", err, m)
	}
	m, err = terminal.ParseCounterMenu([]byte(`{"menu_version":3,"build":{"categories":[{"key":"a","name":"A","items":[]}]},"routes":{}}`))
	if err != nil || m.QuickAddItemID != "" {
		t.Fatalf("%v %+v", err, m)
	}
}

// The owner, 2026-11-14 (#17; contract §2.1i / §2.10an): a Quicc Add from a
// terminal that sends printer_id prints only where the cashier sent it:
// null = no kitchen paper anywhere (routing none, even when the store's Quick
// add item has a printer), an id = that printer alone. The Quick add item's
// route no longer decides, and renaming the line changes nothing.
func TestQuickAddPrinterIDDecides(t *testing.T) {
	for _, c := range []struct {
		name, body       string
		routing, printer string
	}{
		{"null: nowhere", `null`, order.RoutingNone, ""},
		{"a printer: that one", `"p-pizza"`, order.RoutingMatched, "p-pizza"},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, label := range []string{"Side of ranch", "Pizza dough"} { // a rename never moves it
				var l pricing.Line
				if err := json.Unmarshal([]byte(`{"custom":true,"name":"`+label+`","qty":1,"price_cents":450,"printer_id":`+c.body+`}`), &l); err != nil {
					t.Fatal(err)
				}
				m := driverMenu(t, true)
				m.QuickAddItemID = "qa-item"
				m.Routes = map[string]terminal.Route{"qa-item": {PrinterID: "p-wings", Name: "Wings"}}
				p, err := priceCart(m, time.UTC, terminal.TypeCounter, Cart{Cart: pricing.Cart{Lines: []pricing.Line{l}}}, time.Now(), 3)
				if err != nil || len(p.quote.Errors) > 0 {
					t.Fatalf("price: %v %+v", err, p.quote.Errors)
				}
				items, _, _, err := buildItems(m, p)
				if err != nil || len(items) != 1 {
					t.Fatalf("build: %v %+v", err, items)
				}
				if it := items[0]; it.Routing != c.routing || it.PrinterID != c.printer || it.MenuItemID != "qa-item" {
					t.Errorf("%s: routing %q printer %q item %q, want %q %q qa-item", label, it.Routing, it.PrinterID, it.MenuItemID, c.routing, c.printer)
				}
			}
		})
	}
}
