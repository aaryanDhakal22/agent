package terminal

import (
	"encoding/json"
	"strings"
	"sync/atomic"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/pricing"
	"quiccpos/agent/internal/domain/terminal"
)

// The driver's copy (the owner, 2026-11-12, the canonical-menu merge):
// SoftTouch rang a drink / dessert / side picked inside an item at 1¢ only
// so the driver saw it on the delivery ticket. In our menu those choices are
// $0 with main's per-option flag show_on_driver_ticket (counter-menu build:
// modifier_groups[].options[].show_on_driver_ticket, omitted when false).
// When the Pi stores an order it stamps each modifier child of a line whose
// bank item it knows with the flag (order.LineItem.ShowOnDriverTicket); the
// delivery receipt leaves a $0 child stamped false off (receipt package).
// The build is parsed here, never through the pricing engine copy.

// driverIndex: bank item id (lower case) → option name key → the flag (true
// when any of the item's options of that name has it on).
type driverIndex map[string]map[string]bool

// driverBuild is the part of the counter-menu build the stamping reads.
type driverBuild struct {
	Categories []struct {
		Items []struct {
			ID     string `json:"id"`
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
		Options []struct {
			Name string `json:"name"`
			Show bool   `json:"show_on_driver_ticket"`
		} `json:"options"`
	} `json:"modifier_groups"`
}

// driverIndexOf reads the flags off a counter-menu build. nil (nothing is
// stamped, receipts print as before) when there is no build, it doesn't
// parse, or no option has the flag on: a main that sends no flag yet would
// otherwise read as "every $0 choice hidden".
func driverIndexOf(build json.RawMessage) driverIndex {
	if len(build) == 0 {
		return nil
	}
	var b driverBuild
	if err := json.Unmarshal(build, &b); err != nil {
		return nil
	}
	groups := make(map[string]map[string]bool, len(b.ModifierGroups))
	anyOn := false
	for _, g := range b.ModifierGroups {
		opts := make(map[string]bool, len(g.Options))
		for _, o := range g.Options {
			k := optionKey(o.Name)
			if k == "" {
				continue
			}
			opts[k] = opts[k] || o.Show
			anyOn = anyOn || o.Show
		}
		groups[strings.ToLower(g.ID)] = opts
	}
	if !anyOn {
		return nil
	}
	idx := driverIndex{}
	for _, c := range b.Categories {
		for _, it := range c.Items {
			ids := append([]string{it.ID}, it.Anchor.ItemIDs...)
			for _, ch := range it.Choices {
				ids = append(ids, ch.ItemID)
			}
			for _, id := range ids {
				id = strings.ToLower(strings.TrimSpace(id))
				if id == "" {
					continue
				}
				m := idx[id]
				if m == nil {
					m = map[string]bool{}
					idx[id] = m
				}
				for _, gid := range it.ModifierGroupIDs {
					for k, v := range groups[strings.ToLower(gid)] {
						m[k] = m[k] || v
					}
				}
			}
		}
	}
	return idx
}

func optionKey(name string) string { return strings.ToLower(strings.Join(strings.Fields(name), " ")) }

// show is the stamp for one child of a line on bank item bankID: nil = not
// known (no index, an unknown item, no option of that name); a NO child is
// false. names are tried in order.
func (d driverIndex) show(bankID, mode string, names ...string) *bool {
	opts, ok := d[strings.ToLower(strings.TrimSpace(bankID))]
	if d == nil || bankID == "" || !ok {
		return nil
	}
	if mode == order.ModeNo {
		v := false
		return &v
	}
	for _, n := range names {
		if v, ok := opts[optionKey(n)]; ok {
			return &v
		}
	}
	return nil
}

// childNames are the option names a stored child may stand for: its name,
// without " (1st half)" / " (2nd half)", without an ONLY word or a "2x "
// repeat.
func childNames(c order.LineItem) []string {
	n := strings.TrimSpace(c.Name)
	out := []string{n}
	add := func(s string) {
		if s = strings.TrimSpace(s); s != "" && s != out[len(out)-1] {
			out = append(out, s)
		}
	}
	n = strings.TrimSuffix(strings.TrimSuffix(n, " (1st half)"), " (2nd half)")
	add(n)
	if c.Mode == order.ModeOnly {
		n = strings.TrimPrefix(n, "ONLY ")
		add(n)
	}
	if i := strings.Index(n, "x "); i > 0 && allDigits(n[:i]) {
		add(n[i+2:])
	}
	return out
}

func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// stampItems stamps the children of each line that names its bank item
// (menu_item_id); a child already stamped keeps its stamp. A copy: the
// caller's slices are not written.
func (d driverIndex) stampItems(items []order.LineItem) []order.LineItem {
	if d == nil || len(items) == 0 {
		return items
	}
	out := make([]order.LineItem, len(items))
	for i, it := range items {
		if it.MenuItemID != "" && len(it.Children) > 0 {
			kids := make([]order.LineItem, len(it.Children))
			copy(kids, it.Children)
			for j := range kids {
				if kids[j].ShowOnDriverTicket == nil {
					kids[j].ShowOnDriverTicket = d.show(it.MenuItemID, kids[j].Mode, childNames(kids[j])...)
				}
			}
			it.Children = kids
		}
		out[i] = it
	}
	return out
}

// optionShow is the stamp for a resolved option of a counter line (the
// engine's option name; a default left off reads as a NO).
func (d driverIndex) optionShow(bankID string, o pricing.ResolvedOption) *bool {
	mode := o.Mode
	if o.RemovedDefault {
		mode = order.ModeNo
	}
	return d.show(bankID, mode, o.Name, o.Label)
}

// driverCache keeps the index of the counter menu in use (a new menu is a
// new pointer: the index is rebuilt once per menu).
type driverCache struct {
	menu *terminal.CounterMenu
	idx  driverIndex
}

var lastDriverIndex atomic.Pointer[driverCache]

func driverIndexFor(m *terminal.CounterMenu) driverIndex {
	if m == nil {
		return nil
	}
	if c := lastDriverIndex.Load(); c != nil && c.menu == m {
		return c.idx
	}
	idx := driverIndexOf(m.Build)
	lastDriverIndex.Store(&driverCache{menu: m, idx: idx})
	return idx
}

// StampDriverTicket stamps a channel order arriving from main (SSE, catch-up,
// resend) with the counter menu's show_on_driver_ticket flags before the Pi
// stores it. Vendor (online) and POS orders, and every order while the Pi
// has no counter menu, are left as they are.
func (s *Store) StampDriverTicket(snap *order.Snapshot) {
	if snap == nil || !snap.IsChannel() {
		return
	}
	m, _ := s.Menu()
	snap.Items = driverIndexFor(m).stampItems(snap.Items)
}
