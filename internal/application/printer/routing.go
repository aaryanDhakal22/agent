package printer

import (
	"context"

	"quiccpos/agent/internal/domain/printroute"
)

// RoutingStore keeps the store's routing + emergency reroute setting
// (settings.print_routing; repositories.OrderRepository).
type RoutingStore interface {
	PrintRouting(ctx context.Context) (printroute.Routing, error)
	SetPrintRouting(ctx context.Context, r printroute.Routing) error
}

// SetRoutingStore wires where the routing setting is saved and changed, a
// callback for after a save (cmd/main.go wakes state sync: the setting rides
// in agent.state so a re-imaged Pi gets it back at link).
func (d *Dispatcher) SetRoutingStore(s RoutingStore, changed func()) {
	d.mu.Lock()
	d.routingStore, d.routingChanged = s, changed
	d.mu.Unlock()
}

// LoadRouting reads the setting into the print path's copy (startup, and
// after a restore brought the old Pi's back). A read error keeps mode none.
func (d *Dispatcher) LoadRouting(ctx context.Context) {
	d.mu.RLock()
	st := d.routingStore
	d.mu.RUnlock()
	if st == nil {
		return
	}
	r, err := st.PrintRouting(ctx)
	if err != nil {
		d.logger.Warn().Ctx(ctx).Err(err).Msg("read printer routing failed; automatic failover only")
		r = printroute.Default()
	}
	d.SetRouting(r)
}

// SetRouting swaps the print path's copy (no save; tests and LoadRouting).
func (d *Dispatcher) SetRouting(r printroute.Routing) {
	r = r.Normalize()
	d.routing.Store(&r)
}

// Routing is the setting tickets route by right now (mode none when unset).
func (d *Dispatcher) Routing() printroute.Routing {
	if r := d.routing.Load(); r != nil {
		return *r
	}
	return printroute.Default()
}

// SaveRouting validates r against the printers the Pi has now, saves it and
// switches printing over to it. A *printroute.ValidationError (errors.Is
// printroute.ErrInvalid) is the caller's 400.
func (d *Dispatcher) SaveRouting(ctx context.Context, r printroute.Routing) (printroute.Routing, error) {
	r = r.Normalize()
	known := map[string]bool{}
	for _, e := range d.Roster() {
		known[e.ID] = true
	}
	if err := r.Validate(known); err != nil {
		return r, err
	}
	d.mu.RLock()
	st, changed := d.routingStore, d.routingChanged
	d.mu.RUnlock()
	if st != nil {
		if err := st.SetPrintRouting(ctx, r); err != nil {
			return r, err
		}
	}
	d.routing.Store(&r)
	if changed != nil {
		changed()
	}
	return r, nil
}

// RoutingPrinter is one printer on the Routing & emergency reroute page:
// the roster entry with its live status.
type RoutingPrinter struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	IsReceipt bool   `json:"is_receipt"`
	// Status: up | down | unknown (not probed yet).
	Status string `json:"status"`
	// Fallbacks: the full order a ticket for this printer tries after it,
	// by printer name, with today's setting and live status (what would
	// happen now).
	Fallbacks []RoutingFallback `json:"fallbacks"`
}

// RoutingFallback is one step of RoutingPrinter.Fallbacks.
type RoutingFallback struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// By: rule | auto.
	By string `json:"by"`
	// Status: up | down | unknown.
	Status string `json:"status"`
}

// RoutingView lists the roster with live status and, per printer, the order
// its tickets would fail over in right now.
func (d *Dispatcher) RoutingView() []RoutingPrinter {
	d.mu.RLock()
	roster := append([]rosterSlot(nil), d.roster...)
	d.mu.RUnlock()
	status := func(name string) string {
		st, ok := d.registry.Get(name)
		switch {
		case !ok || st.LastChecked == nil:
			return "unknown"
		case st.Up:
			return "up"
		}
		return "down"
	}
	out := make([]RoutingPrinter, 0, len(roster))
	for _, sl := range roster {
		cands, rule := d.candidates(sl, roster)
		p := RoutingPrinter{ID: sl.entry.ID, Name: sl.entry.Name, IsReceipt: sl.entry.IsReceipt, Status: status(sl.entry.Name),
			Fallbacks: make([]RoutingFallback, 0, len(cands)-1)}
		for _, c := range cands[1:] {
			p.Fallbacks = append(p.Fallbacks, RoutingFallback{ID: c.entry.ID, Name: c.entry.Name, By: rerouteBy(rule, c.entry.ID), Status: status(c.entry.Name)})
		}
		out = append(out, p)
	}
	return out
}
