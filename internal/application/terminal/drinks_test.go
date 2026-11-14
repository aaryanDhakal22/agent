package terminal

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/pricing"
	"quiccpos/agent/internal/domain/terminal"
)

// The drink stamps (the owner 2026-11-14, contract §2.1k) against Lombardi's
// real counter-menu build: testdata/lombardis_build.json is the terminal's
// test/fixtures/lombardis_counter_menu.json build cut to what the drink rule
// reads (categories with their items' ids, names and groups; groups with
// their options' names).

const (
	lbTwoLtPepsi = "7ce2b7ea-65de-5ca7-9277-a07c0b824745" // Beverages
	lbCanTo2L    = "6ca36a65-70b1-52a8-a30b-fc7bd2b728f3" // Upcharges "Can -> 2L"
	lbWings      = "ed773070-0df1-5eb8-a1ef-9788c45a7a1c" // Party Wings "8 Wings & Fries" (the soda group)
	lbTender     = "c2fc5f38-a94f-5a8c-b172-c9904703a133" // Side Orders "Chicken Tender" (the soda group)
	lbShake      = "99bb76ce-beaa-50c2-ae04-4afe545150e3" // Beverages "Med 20 oz Shake" (Milk Shake Mods)
	lbFries      = "6fa02cca-48b4-5d92-b6cd-f22bd57190d5" // Side Orders "French Fries"
	lbIceCup     = "a2af7187-97f0-5288-a5ee-ac7e9c85cc6a" // Beverages "Cup of Ice"
)

func lombardisBuild(t *testing.T) json.RawMessage {
	t.Helper()
	b, err := os.ReadFile("testdata/lombardis_build.json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func defaultDrinkRule() order.DrinkRule { return order.DefaultReceiptPrefs().DrinkRule() }

func isDrink(it order.LineItem) bool { return it.Drink != nil && *it.Drink }

func childNamed(t *testing.T, it order.LineItem, name string) order.LineItem {
	t.Helper()
	for _, c := range it.Children {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no child %q under %q", name, it.Name)
	return order.LineItem{}
}

func kids(names ...string) []order.LineItem {
	var out []order.LineItem
	for _, n := range names {
		out = append(out, order.LineItem{Name: n, Quantity: 1})
	}
	return out
}

func TestDrinks_LombardisBuild(t *testing.T) {
	idx := drinkIndexOf(lombardisBuild(t), defaultDrinkRule())
	if idx == nil {
		t.Fatal("no index")
	}
	no := order.LineItem{Name: "NO W Can Pepsi", Mode: order.ModeNo, Quantity: 1}
	items := []order.LineItem{
		{Name: "2Lt Pepsi", MenuItemID: lbTwoLtPepsi, Quantity: 1},
		{Name: "Can -> 2L", MenuItemID: lbCanTo2L, Quantity: 1},
		{Name: "8 Wings & Fries", MenuItemID: lbWings, Quantity: 2,
			Children: append(kids("Regular", "Hot Sauce", "Ranch", "W Can Coke", "Extra BBq Sauce", "SP & K on Fries", "Med Lemonade"), no)},
		{Name: "Chicken Tender", MenuItemID: lbTender, Quantity: 1, Children: kids("W Can Sprite", "Honey Must on Tender")},
		{Name: "Med 20 oz Shake", MenuItemID: lbShake, Quantity: 1, Children: kids("Mango Shake")},
		{Name: "French Fries", MenuItemID: lbFries, Quantity: 1, Children: kids("Ranch")},
		{Name: "Cup of Ice", MenuItemID: lbIceCup, Quantity: 1},
		{Name: "Note", SKU: order.SKUNote, Quantity: 1},
	}
	in := items[2].Children
	got := idx.stampItems(items)
	if in[3].Drink != nil {
		t.Fatal("the caller's children were written")
	}
	for _, c := range []struct {
		it   order.LineItem
		want bool
	}{
		{got[0], true}, {got[1], false}, {got[2], false}, {got[3], false}, {got[4], true}, {got[5], false}, {got[6], true}, {got[7], false},
	} {
		if isDrink(c.it) != c.want {
			t.Errorf("%s: drink %v, want %v", c.it.Name, isDrink(c.it), c.want)
		}
	}
	wings := got[2]
	for name, w := range map[string]bool{"Regular": false, "Hot Sauce": false, "Ranch": false, "W Can Coke": true,
		"Extra BBq Sauce": false, "SP & K on Fries": false, "Med Lemonade": true, "NO W Can Pepsi": false} {
		if d := isDrink(childNamed(t, wings, name)); d != w {
			t.Errorf("wings choice %q: drink %v, want %v", name, d, w)
		}
	}
	if !isDrink(childNamed(t, got[3], "W Can Sprite")) || isDrink(childNamed(t, got[3], "Honey Must on Tender")) {
		t.Errorf("tender choices: %+v", got[3].Children)
	}
	if isDrink(childNamed(t, got[4], "Mango Shake")) {
		t.Error("a drink's own choice is the same drink: no second mark")
	}
	if isDrink(childNamed(t, got[5], "Ranch")) {
		t.Error("a sauce is no drink")
	}
	// A stamp already there is kept.
	f := false
	kept := idx.stampItems([]order.LineItem{{Name: "2Lt Pepsi", MenuItemID: lbTwoLtPepsi, Drink: &f}})
	if kept[0].Drink == nil || *kept[0].Drink {
		t.Error("an existing stamp must be kept")
	}
}

// An online vendor order has no bank item the build knows: only exact names
// (case, spaces and punctuation aside) count.
func TestDrinks_VendorNames(t *testing.T) {
	idx := drinkIndexOf(lombardisBuild(t), defaultDrinkRule())
	got := idx.stampItems([]order.LineItem{
		{Name: "2 LT. PEPSI", Quantity: 1},
		{Name: "2 Liter Pepsi", Quantity: 1},
		{Name: "Pepsi", Quantity: 1},
		{Name: "Can Coke", Quantity: 1},
		{Name: "15 pc Chicken Tender", Quantity: 1, Children: kids("W Can Coke", "Can Coke", "Extra Hot Sauce", "w can  dr. pepper")},
		{Name: "Pepsi", Size: "2Lt", Quantity: 1},
	})
	for i, w := range []bool{true, false, false, true, false, true} {
		if isDrink(got[i]) != w {
			t.Errorf("vendor line %q (size %q): drink %v, want %v", got[i].Name, got[i].Size, isDrink(got[i]), w)
		}
	}
	for name, w := range map[string]bool{"W Can Coke": true, "Can Coke": false, "Extra Hot Sauce": false, "w can  dr. pepper": true} {
		if d := isDrink(childNamed(t, got[4], name)); d != w {
			t.Errorf("vendor choice %q: drink %v, want %v", name, d, w)
		}
	}
}

// The settings decide: another category, no groups, another skip word.
func TestDrinks_SettingsChangeTheResult(t *testing.T) {
	build := lombardisBuild(t)
	lines := func() []order.LineItem {
		return []order.LineItem{
			{Name: "2Lt Pepsi", MenuItemID: lbTwoLtPepsi, Quantity: 1},
			{Name: "French Fries", MenuItemID: lbFries, Quantity: 1},
			{Name: "8 Wings & Fries", MenuItemID: lbWings, Quantity: 1, Children: kids("W Can Coke", "Extra BBq Sauce")},
		}
	}
	rule := order.DrinkRule{Categories: order.NewWordList("Side Orders"), Groups: order.NewWordList(), Skip: order.NewWordList()}
	got := drinkIndexOf(build, rule).stampItems(lines())
	if isDrink(got[0]) || !isDrink(got[1]) || isDrink(got[2].Children[0]) {
		t.Errorf("categories [Side Orders], no groups: %v %v %v", isDrink(got[0]), isDrink(got[1]), isDrink(got[2].Children[0]))
	}
	rule = order.DrinkRule{Categories: order.NewWordList(), Groups: order.NewWordList("soda"), Skip: order.NewWordList()}
	got = drinkIndexOf(build, rule).stampItems(lines())
	if isDrink(got[0]) || !isDrink(got[2].Children[0]) || !isDrink(got[2].Children[1]) {
		t.Errorf("no skip words: the sauce in the soda group counts: %+v", got[2].Children)
	}
	// A "(Counter)" ending and the case don't matter for a category.
	rule = order.DrinkRule{Categories: order.NewWordList("PARTY WINGS")}
	got = drinkIndexOf(build, rule).stampItems(lines())
	if !isDrink(got[2]) {
		t.Error("category names compare without case")
	}
}

// Every Lombardi's item in "Beverages" is a drink and nothing else is (the
// default categories); the soda group catches its drinks and none of its
// sauces.
func TestDrinks_LombardisDefaultsSurvey(t *testing.T) {
	build := lombardisBuild(t)
	var b drinkBuild
	if err := json.Unmarshal(build, &b); err != nil {
		t.Fatal(err)
	}
	rule := defaultDrinkRule()
	var drinkCats []string
	for _, c := range b.Categories {
		if rule.CategoryIsDrink(c.Name) {
			drinkCats = append(drinkCats, c.Name)
		}
	}
	if len(drinkCats) != 1 || drinkCats[0] != "Beverages" {
		t.Errorf("drink categories: %v", drinkCats)
	}
	var groups []string
	for _, g := range b.ModifierGroups {
		if rule.GroupIsDrink(g.Name) {
			groups = append(groups, g.Name)
		}
		if g.Name == "Whole Wings Soda (Counter)" {
			n := 0
			for _, o := range g.Options {
				if rule.OptionIsDrink(o.Name) {
					n++
				}
			}
			if n != 26 {
				t.Errorf("the soda group's drinks: %d, want 26 (39 options less 13 sauces / extras)", n)
			}
		}
	}
	want := []string{"Whole Wings Soda (Counter)", "Milk Shake Mods (Counter)", "Slurpy Mods (Counter)"}
	if len(groups) != len(want) {
		t.Fatalf("drink groups: %v, want %v", groups, want)
	}
	for i := range want {
		if groups[i] != want[i] {
			t.Errorf("drink groups: %v, want %v", groups, want)
		}
	}
}

// Counter lines (Service.orderLines) and orders from main (StampArrival, any
// source) are stamped with the store's rule; a rule change is read at once.
func TestDrinks_CounterAndArrival(t *testing.T) {
	r := newRig(t)
	st := r.svc.Store()
	menu := driverMenu(t, true) // a "Drinks" group with Can Pepsi under the Cheesesteak Combo
	body, _ := json.Marshal(map[string]any{"menu_version": 99, "build": json.RawMessage(menu.Build), "pricing": menu.Pricing, "routes": map[string]any{}})
	r.main.set(PathCounterMenu, json.RawMessage(body))
	if changed, err := st.Refresh(r.ctx, terminal.KindCounterMenu); err != nil || !changed {
		t.Fatalf("refresh: %v %v", changed, err)
	}
	m, _ := st.Menu()
	cart := Cart{Cart: pricing.Cart{Lines: []pricing.Line{
		{Qty: 1, Selection: pricing.Selection{ItemID: sub, Options: []pricing.Pick{{GroupID: drinks, OptionID: pepsi}, {GroupID: cook, OptionID: welldon}}}},
	}}}
	p, err := priceCart(m, time.UTC, terminal.TypeDelivery, cart, time.Now(), 3)
	if err != nil || len(p.quote.Errors) > 0 {
		t.Fatalf("price: %v %+v", err, p.quote.Errors)
	}
	items, _, _, err := r.svc.orderLines(m, p)
	if err != nil {
		t.Fatal(err)
	}
	if !isDrink(childNamed(t, items[0], "Can Pepsi")) || isDrink(childNamed(t, items[0], "Well Done")) || isDrink(items[0]) {
		t.Errorf("counter line: %+v", items[0])
	}

	line := order.LineItem{Name: "Cheesesteak Combo", MenuItemID: sub, Quantity: 1, Children: kids("Can Pepsi", "Well Done")}
	for _, src := range []string{order.SourceOnline, "web", "counter", ""} {
		snap := order.Snapshot{Source: src, Items: []order.LineItem{line}}
		st.StampArrival(&snap)
		if !isDrink(childNamed(t, snap.Items[0], "Can Pepsi")) {
			t.Errorf("source %q: the can is not stamped", src)
		}
	}
	// The rule is the store's setting: no drink groups = no stamp.
	st.SetDrinkRule(func() order.DrinkRule { return order.DrinkRule{} })
	snap := order.Snapshot{Source: order.SourceOnline, Items: []order.LineItem{line}}
	st.StampArrival(&snap)
	if isDrink(snap.Items[0].Children[0]) {
		t.Error("a rule change must be read at once")
	}
	// No counter menu: nothing stamped.
	empty := NewStore(r.repo, r.main.fetch, nil, r.svc.store.logger)
	w := order.Snapshot{Source: "web", Items: []order.LineItem{line}}
	empty.StampArrival(&w)
	if w.Items[0].Children[0].Drink != nil {
		t.Error("no counter menu: nothing stamped")
	}
}
