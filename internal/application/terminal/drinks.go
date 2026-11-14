package terminal

import (
	"encoding/json"
	"strings"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/terminal"
)

// Drink marks (the owner 2026-11-14, contract §2.1k): SoftTouch ended every
// drink line of a delivery ticket in "#####" and the drivers scan a long
// ticket for them to know what to carry. The owner's condition: "as long as
// our code for the drinks are completely solid and the filters are solid".
// So the menu decides, never a guess on words in an item's name:
//
//   - an item is a drink when its counter-menu category is one of the
//     store's drink categories (receipt_prefs.drink_categories; Lombardi's
//     "Beverages");
//   - a choice is a drink when its modifier group's name holds one of the
//     store's drink group words (receipt_prefs.drink_groups; Lombardi's
//     "Whole Wings Soda (Counter)") and its own name holds none of the skip
//     words (receipt_prefs.drink_skip_words: that group also sells "Extra
//     BBq Sauce", "BBQ on Tender", "SP & K on Fries"), unless its item is a
//     drink already (the flavour of a drink is the same drink: one mark a
//     drink);
//   - a line with no bank item the build knows (an online vendor order) is
//     a drink only when its name equals the name of a drink category's item
//     (DrinkNameKey: case, spaces and punctuation ignored), a choice when
//     its name equals an option of a drink group. Nothing fuzzier.
//
// The Pi stamps each line (order.LineItem.Drink) when it stores the order,
// where it stamps show_on_driver_ticket (driverticket.go): counter lines
// (Service.orderLines), channel, call-center and vendor orders from main
// (Store.StampArrival). Print only: never money, never routing. The build
// is parsed here, never through the pricing engine copy.

// drinkBuild is the part of the counter-menu build the drink rule reads.
type drinkBuild struct {
	Categories []struct {
		Name  string `json:"name"`
		Items []struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Anchor struct {
				ItemIDs []string `json:"item_ids"`
			} `json:"anchor"`
			ModifierGroupIDs []string `json:"modifier_group_ids"`
			Choices          []struct {
				ItemID string `json:"item_id"`
			} `json:"choices"`
		} `json:"items"`
	} `json:"categories"`
	ModifierGroups []struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Options []struct {
			Name string `json:"name"`
		} `json:"options"`
	} `json:"modifier_groups"`
}

// drinkItem is one bank item: a drink itself, and the option names (by
// DrinkNameKey) of its drink groups.
type drinkItem struct {
	drink bool
	opts  map[string]bool
}

// drinkIndex is the drink rule applied to one build. nil = nothing is
// stamped (no build, it doesn't parse).
type drinkIndex struct {
	items map[string]*drinkItem // bank item id (lower case)
	// names: the names of the drink categories' items; optNames: the
	// options of every drink group (both by DrinkNameKey), for lines with
	// no bank item the build knows.
	names, optNames map[string]bool
}

func drinkIndexOf(build json.RawMessage, rule order.DrinkRule) *drinkIndex {
	if len(build) == 0 {
		return nil
	}
	var b drinkBuild
	if err := json.Unmarshal(build, &b); err != nil {
		return nil
	}
	idx := &drinkIndex{items: map[string]*drinkItem{}, names: map[string]bool{}, optNames: map[string]bool{}}
	groups := map[string]map[string]bool{} // drink group id (lower case) -> its option keys
	for _, g := range b.ModifierGroups {
		if !rule.GroupIsDrink(g.Name) {
			continue
		}
		opts := map[string]bool{}
		for _, o := range g.Options {
			if !rule.OptionIsDrink(o.Name) {
				continue // a sauce sold in the soda group (drink_skip_words)
			}
			if k := order.DrinkNameKey(o.Name); k != "" {
				opts[k] = true
				idx.optNames[k] = true
			}
		}
		groups[strings.ToLower(strings.TrimSpace(g.ID))] = opts
	}
	for _, c := range b.Categories {
		drinks := rule.CategoryIsDrink(c.Name)
		for _, it := range c.Items {
			if k := order.DrinkNameKey(it.Name); drinks && k != "" {
				idx.names[k] = true
			}
			ids := append([]string{it.ID}, it.Anchor.ItemIDs...)
			for _, ch := range it.Choices {
				ids = append(ids, ch.ItemID)
			}
			for _, id := range ids {
				id = strings.ToLower(strings.TrimSpace(id))
				if id == "" {
					continue
				}
				d := idx.items[id]
				if d == nil {
					d = &drinkItem{opts: map[string]bool{}}
					idx.items[id] = d
				}
				d.drink = d.drink || drinks
				for _, gid := range it.ModifierGroupIDs {
					for k := range groups[strings.ToLower(strings.TrimSpace(gid))] {
						d.opts[k] = true
					}
				}
			}
		}
	}
	return idx
}

// lineIsDrink: the line's bank item is a drink; with no bank item the build
// knows, its name (or size + name) is a drink category item's name.
func (d *drinkIndex) lineIsDrink(it order.LineItem) bool {
	if it.SKU == order.SKUNote {
		return false
	}
	if known, ok := d.items[strings.ToLower(strings.TrimSpace(it.MenuItemID))]; ok && it.MenuItemID != "" {
		return known.drink
	}
	if d.names[order.DrinkNameKey(it.Name)] {
		return true
	}
	return it.Size != "" && d.names[order.DrinkNameKey(it.Size+" "+it.Name)]
}

// choiceIsDrink: a choice (not a NO) of a drink group: its item's drink
// groups when the build knows the item, else any drink group's option.
func (d *drinkIndex) choiceIsDrink(parent, c order.LineItem) bool {
	if isNoChild(c) {
		return false
	}
	opts := d.optNames
	if known, ok := d.items[strings.ToLower(strings.TrimSpace(parent.MenuItemID))]; ok && parent.MenuItemID != "" {
		opts = known.opts
	}
	for _, n := range childNames(c) {
		if opts[order.DrinkNameKey(n)] {
			return true
		}
	}
	return false
}

func isNoChild(c order.LineItem) bool {
	n := strings.ToLower(strings.TrimSpace(c.Name))
	return c.Mode == order.ModeNo || strings.EqualFold(strings.TrimSpace(c.Notes), "remove") ||
		n == "no" || strings.HasPrefix(n, "no ")
}

// stampItems stamps every drink line and drink choice (Drink true); a line
// already stamped keeps its stamp; the rest stay nil. A copy: the caller's
// slices are not written.
func (d *drinkIndex) stampItems(items []order.LineItem) []order.LineItem {
	if d == nil || len(items) == 0 {
		return items
	}
	yes := true
	out := make([]order.LineItem, len(items))
	for i, it := range items {
		if it.Drink == nil && d.lineIsDrink(it) {
			it.Drink = &yes
		}
		if len(it.Children) > 0 {
			kids := make([]order.LineItem, len(it.Children))
			copy(kids, it.Children)
			if !it.IsDrink() { // a drink's own choices are that drink: no second mark
				for j := range kids {
					if kids[j].Drink == nil && d.choiceIsDrink(it, kids[j]) {
						kids[j].Drink = &yes
					}
				}
			}
			it.Children = kids
		}
		out[i] = it
	}
	return out
}

// StampDrinksWith stamps items with the drink rule over a counter-menu
// build (the Pi's own paths use the store's cached index; the receipt
// samples and tests use this).
func StampDrinksWith(build json.RawMessage, rule order.DrinkRule, items []order.LineItem) []order.LineItem {
	return drinkIndexOf(build, rule).stampItems(items)
}

// drinkCache keeps the index of the counter menu and rule in use.
type drinkCache struct {
	menu *terminal.CounterMenu
	rule order.DrinkRule
	idx  *drinkIndex
}

// SetDrinkRule wires the store's drink rule (cmd/main.go: the receipt
// settings' drink_categories / drink_groups). Unset = the defaults.
func (s *Store) SetDrinkRule(fn func() order.DrinkRule) { s.drinkRule.Store(&fn) }

func (s *Store) drinkRuleNow() order.DrinkRule {
	if fn := s.drinkRule.Load(); fn != nil && *fn != nil {
		return (*fn)()
	}
	return order.DefaultReceiptPrefs().DrinkRule()
}

// drinkIndexFor is the index of menu m under the store's drink rule (built
// once per menu and rule).
func (s *Store) drinkIndexFor(m *terminal.CounterMenu) *drinkIndex {
	if s == nil || m == nil {
		return nil
	}
	rule := s.drinkRuleNow()
	if c := s.drinks.Load(); c != nil && c.menu == m && c.rule == rule {
		return c.idx
	}
	idx := drinkIndexOf(m.Build, rule)
	s.drinks.Store(&drinkCache{menu: m, rule: rule, idx: idx})
	return idx
}

// StampDrinks stamps the drinks of an order arriving from main, from any
// source (counter, website, call center, the online vendor), before the Pi
// stores it. Nothing while the Pi has no counter menu.
func (s *Store) StampDrinks(snap *order.Snapshot) {
	if snap == nil {
		return
	}
	m, _ := s.Menu()
	snap.Items = s.drinkIndexFor(m).stampItems(snap.Items)
}

// StampArrival is the order service's arrival stamp (cmd/main.go): the
// drinks (every source), then show_on_driver_ticket (channel orders).
func (s *Store) StampArrival(snap *order.Snapshot) {
	s.StampDrinks(snap)
	s.StampDriverTicket(snap)
}

// orderLines is buildItems with the drinks stamped (every counter, call
// center and edit path builds its lines here).
func (s *Service) orderLines(m *terminal.CounterMenu, p priced) ([]order.LineItem, []order.Discount, []order.Fee, error) {
	items, off, fees, err := buildItems(m, p)
	if err != nil {
		return items, off, fees, err
	}
	return s.store.drinkIndexFor(m).stampItems(items), off, fees, nil
}
