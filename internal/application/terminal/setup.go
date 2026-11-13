package terminal

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	"quiccpos/agent/internal/domain/terminal"
)

// Setup wizard (contract §2.10y): what the Pi already holds about the menu,
// the kitchen routing, the staff and the terminal settings, counted for
// Centurion's "Pi and store ready" step. Counts only: never a PIN, a hash or
// a name of a staff member.

// SetupMenu is the counter menu at a glance.
type SetupMenu struct {
	Version        int64      `json:"version"`
	FetchedAt      *time.Time `json:"fetched_at,omitempty"`
	Tabs           int        `json:"tabs"`
	Buttons        int        `json:"buttons"`
	Items          int        `json:"items"`
	NeedsAttention int        `json:"needs_attention"`
}

// SetupPrinterItems is how many menu items print on one kitchen printer.
type SetupPrinterItems struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Items int    `json:"items"`
}

// SetupRoutes is the kitchen routing: items per printer, items with no
// kitchen ticket, items with no printer (they print on every station) and
// printers a route names that the roster doesn't have.
type SetupRoutes struct {
	Printers       []SetupPrinterItems `json:"printers"`
	NoKitchen      int                 `json:"no_kitchen"`
	Unrouted       int                 `json:"unrouted"`
	UnknownPrinter []string            `json:"unknown_printer"`
}

// SetupStaff counts the active staff with a PIN by role.
type SetupStaff struct {
	Managers int `json:"managers"`
	Cashiers int `json:"cashiers"`
	Drivers  int `json:"drivers"`
	Others   int `json:"others"`
}

// SetupView is the terminal service's part of GET /api/terminal/setup.
type SetupView struct {
	Menu         *SetupMenu
	Routes       *SetupRoutes
	Staff        *SetupStaff
	TaxBps       *int
	Timezone     string
	CounterStart *int64
	KitchenStart *int64
	AutoCloseAt  *string
	Store        map[string]any
}

// RosterPrinter is one printer of the Pi's roster (id + name).
type RosterPrinter struct{ ID, Name string }

// SetupView counts what the Pi holds. roster = the printers of store_config.
func (s *Service) SetupView(roster []RosterPrinter) SetupView {
	var v SetupView
	if m, snap := s.store.Menu(); m != nil {
		v.Menu = menuStats(m, snap)
		v.Routes = routeStats(m, roster)
		tax := m.Pricing.Settings.TaxBps
		v.TaxBps = &tax
		v.Timezone = m.Pricing.Timezone
	}
	if cfg, _ := s.store.Config(); cfg != nil {
		v.Staff = staffStats(cfg)
		start, kitchen := cfg.CounterStart(), cfg.KitchenStart()
		at := cfg.AutoCloseAt()
		v.CounterStart, v.KitchenStart, v.AutoCloseAt = &start, &kitchen, &at
	}
	if s.storeInfo != nil {
		v.Store = s.storeInfo()
	}
	return v
}

func menuStats(m *terminal.CounterMenu, snap terminal.Snapshot) *SetupMenu {
	out := &SetupMenu{Version: m.MenuVersion, Items: len(m.Pricing.Items)}
	if !snap.FetchedAt.IsZero() {
		at := snap.FetchedAt.UTC()
		out.FetchedAt = &at
	}
	var b struct {
		Categories []struct {
			Items []json.RawMessage `json:"items"`
		} `json:"categories"`
		NeedsAttention []json.RawMessage `json:"needs_attention"`
		Layout         *struct {
			Tabs []struct {
				Buttons []json.RawMessage `json:"buttons"`
			} `json:"tabs"`
			Screens []struct {
				Buttons []json.RawMessage `json:"buttons"`
			} `json:"screens"`
		} `json:"layout"`
	}
	if json.Unmarshal(m.Build, &b) != nil {
		return out
	}
	if out.Items == 0 {
		for _, c := range b.Categories {
			out.Items += len(c.Items)
		}
	}
	out.NeedsAttention = len(b.NeedsAttention)
	if b.Layout != nil {
		out.Tabs = len(b.Layout.Tabs)
		for _, t := range b.Layout.Tabs {
			out.Buttons += len(t.Buttons)
		}
		for _, sc := range b.Layout.Screens {
			out.Buttons += len(sc.Buttons)
		}
	}
	return out
}

func routeStats(m *terminal.CounterMenu, roster []RosterPrinter) *SetupRoutes {
	out := &SetupRoutes{Printers: []SetupPrinterItems{}, UnknownPrinter: []string{}}
	idx := map[string]int{}
	for _, p := range roster {
		idx[p.ID] = len(out.Printers)
		out.Printers = append(out.Printers, SetupPrinterItems{ID: p.ID, Name: p.Name})
	}
	unknown := map[string]string{} // printer id -> its name (else the id)
	ids := make([]string, 0, len(m.Pricing.Items))
	for id := range m.Pricing.Items {
		ids = append(ids, id)
	}
	if len(ids) == 0 { // an older main sends no item facts: the routes alone
		for id := range m.Routes {
			ids = append(ids, id)
		}
	}
	for _, id := range ids {
		r, ok := m.Routes[id]
		switch {
		case ok && r.NoKitchen:
			out.NoKitchen++
		case !ok || strings.TrimSpace(r.PrinterID) == "":
			out.Unrouted++
		default:
			if i, known := idx[r.PrinterID]; known {
				out.Printers[i].Items++
				continue
			}
			if name := strings.TrimSpace(r.Name); name != "" || unknown[r.PrinterID] == "" {
				if name == "" {
					name = r.PrinterID
				}
				unknown[r.PrinterID] = name
			}
		}
	}
	for _, n := range unknown {
		out.UnknownPrinter = append(out.UnknownPrinter, n)
	}
	sort.Strings(out.UnknownPrinter)
	return out
}

func staffStats(cfg *terminal.Config) *SetupStaff {
	out := &SetupStaff{}
	for _, st := range cfg.Staff {
		if !st.IsActive() || strings.TrimSpace(st.PinHash) == "" {
			continue
		}
		switch st.Role {
		case terminal.RoleManager:
			out.Managers++
		case terminal.RoleCashier:
			out.Cashiers++
		case terminal.RoleDriver:
			out.Drivers++
		default:
			out.Others++
		}
	}
	return out
}
