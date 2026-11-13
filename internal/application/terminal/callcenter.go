package terminal

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"quiccpos/agent/internal/domain/device"
	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/pricing"
	"quiccpos/agent/internal/domain/terminal"
)

// The call center: a remote terminal over main (contract/agent.md §2.10ab,
// CALLCENTER-PLAN.md). main relays the call-center app's calls (RPC
// terminal.relay, transport/server_callcenter.go) and the Pi runs each one
// through the terminal handlers with a call-center Actor: a seat
// (callcenter:<main's device id>) and the agent main signed in. Here: that
// actor's session, the allowlist, what a call may not carry, paid lines
// locked, the store wins a draft / an order's edit lock, "Call center -
// Name" on the orders it places.

// CallcenterActor is the Actor of one relayed call: seat is main's
// callcenter device id (one browser), staffID the agent main signed in.
func CallcenterActor(seat, seatName, staffID string) Actor {
	name := strings.TrimSpace(seatName)
	if name == "" {
		name = device.CallcenterName
	}
	return Actor{DeviceID: device.CallcenterSeatPrefix + strings.TrimSpace(seat), DeviceName: name, Callcenter: true, StaffID: strings.TrimSpace(staffID)}
}

// callcenterStaff is the agent of a call-center call: an active staff
// member of role callcenter in the config snapshot (no PIN travels: main
// checked it), else login_required.
func (s *Service) callcenterStaff(a Actor) (terminal.Staff, *terminal.Config, error) {
	cfg, _ := s.store.Config()
	if cfg == nil || a.StaffID == "" {
		return terminal.Staff{}, cfg, terminal.ErrLoginRequired
	}
	st, ok := cfg.Find(a.StaffID)
	if !ok || st.Role != terminal.RoleCallcenter {
		return terminal.Staff{}, cfg, terminal.ErrLoginRequired
	}
	return st, cfg, nil
}

// callcenterRefreshEvery: an unknown agent id pulls the config again at
// most this often (a person made a minute ago; a wrong id never hammers
// main).
const callcenterRefreshEvery = 5 * time.Second

type callcenterState struct {
	mu        sync.Mutex
	refreshed time.Time
}

// CallcenterSession checks the relay's staff id before its call runs: an
// active staff member of role callcenter. An id the snapshot doesn't know
// pulls the snapshot again once first.
func (s *Service) CallcenterSession(ctx context.Context, a Actor) error {
	if _, _, err := s.callcenterStaff(a); err == nil {
		return nil
	}
	cfg, _ := s.store.Config()
	if a.StaffID == "" {
		return terminal.ErrLoginRequired
	}
	if cfg != nil {
		for _, st := range cfg.Staff {
			if st.ID == a.StaffID { // known: switched off, or another role
				return terminal.ErrLoginRequired
			}
		}
	}
	s.cc.mu.Lock()
	due := s.now().Sub(s.cc.refreshed) >= callcenterRefreshEvery
	if due {
		s.cc.refreshed = s.now()
	}
	s.cc.mu.Unlock()
	if due {
		if _, err := s.store.Refresh(ctx, terminal.KindTerminalConfig); err != nil {
			s.logger.Warn().Ctx(ctx).Err(err).Str("staff_id", a.StaffID).Msg("Call center: config refresh for an unknown agent failed")
		}
	}
	_, _, err := s.callcenterStaff(a)
	return err
}

// --- the allowlist (main §2.5zh, checked again here) --------------------------------

// callcenterRoutes: method + path under /api/terminal/ ("floor" = GET
// /api/floor); * is one path segment.
var callcenterRoutes = [][2]string{
	{"GET", "menu"}, {"GET", "status"}, {"GET", "session"},
	{"POST", "quote"},
	{"POST", "orders"}, {"GET", "orders"}, {"GET", "orders/*"},
	{"PUT", "orders/*/cart"},
	{"POST", "orders/*/payments"},
	{"POST", "square/keyed-payments"},
	{"POST", "orders/*/void"},
	{"POST", "orders/*/time"},
	{"POST", "orders/*/reprint"},
	{"GET", "pickups"},
	{"GET", "checks"}, {"GET", "checks/*"},
	{"GET", "drivers"},
	{"GET", "floor"},
	{"GET", "drafts"}, {"PUT", "drafts/*"}, {"DELETE", "drafts/*"},
	{"GET", "customers"}, {"POST", "customers"},
	{"PATCH", "customers/*"},
	{"POST", "customers/*/addresses"},
	{"GET", "customers/*/orders"},
	{"GET", "addresses"},
	{"POST", "journal"},
}

// CallcenterAllowed: may the call center call this terminal route? path is
// relative to /api/terminal/ (no leading slash, no query).
func CallcenterAllowed(method, path string) bool {
	segs := strings.Split(path, "/")
	for _, sg := range segs {
		if sg == "" || sg == "." || sg == ".." || strings.ContainsAny(sg, "?#%\\ ") {
			return false
		}
	}
	for _, rt := range callcenterRoutes {
		if rt[0] != method {
			continue
		}
		want := strings.Split(rt[1], "/")
		if len(want) != len(segs) {
			continue
		}
		ok := true
		for i := range want {
			if want[i] != "*" && want[i] != segs[i] {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// --- what a call may not carry -------------------------------------------------------

func notAllowed(what string) error { return &terminal.NotAllowedForCallcenter{What: what} }

// callcenterQuoteGuard: a quote changes nothing, so only a typed discount
// (an employee meal is one) is refused; custom lines and kept prices of an
// order being edited price as they are.
func callcenterQuoteGuard(a Actor, in Cart) error {
	if a.Callcenter && in.Discount != nil {
		return notAllowed(terminal.NotAllowedDiscount)
	}
	return nil
}

// callcenterPaymentsGuard: a typed card only, never a refund or a comp.
func callcenterPaymentsGuard(a Actor, pays ...PaymentIn) error {
	if !a.Callcenter {
		return nil
	}
	for _, p := range pays {
		switch {
		case p.Refund:
			return notAllowed(terminal.NotAllowedRefund)
		case p.Tender == terminal.TenderComp:
			return notAllowed(terminal.NotAllowedComp)
		case p.Tender != terminal.TenderManualCard:
			return notAllowed(terminal.NotAllowedTender)
		}
	}
	return nil
}

// callcenterCartGuard: what a call-center cart may not carry when it is
// placed (stored nil) or changes a placed order (stored = the order's cart
// as the terminals read it): a typed discount, a custom line, a price on a
// line (an open price, a price change, a $0 comp line) or on one of its
// options, a kept price (rung_cents) that isn't today's price of its line. A line the stored order already holds as it is (the same line
// but for its quantity and note: a cashier's Quicc Add, a kept price, an
// online order's own lines) rides along.
func callcenterCartGuard(a Actor, ctx pricing.Context, in Cart, stored *Cart) error {
	if !a.Callcenter {
		return nil
	}
	if in.Discount != nil {
		return notAllowed(terminal.NotAllowedDiscount)
	}
	held := map[string]int{}
	heldNames := map[string]bool{} // the stored order's custom lines, by name
	if stored != nil {
		for _, l := range stored.Lines {
			held[heldKey(l)]++
			if l.Custom {
				heldNames[customName(l)] = true
			}
		}
	}
	priced := func(sel pricing.Selection) string {
		for _, o := range sel.Options {
			if o.PriceCents != nil {
				return terminal.NotAllowedPriceChange
			}
		}
		if sel.PriceCents == nil {
			return ""
		}
		if r := pricing.Resolve(ctx, sel, 0); r.BankItem != "" && ctx.Items[r.BankItem].OpenPrice {
			return terminal.NotAllowedOpenPrice
		}
		if *sel.PriceCents == 0 {
			return terminal.NotAllowedComp
		}
		return terminal.NotAllowedPriceChange
	}
	// A kept price (rung_cents) from a seat never sets a price: on a line
	// the stored order doesn't hold it must be today's price of that line
	// (its menu unit price, or what today's quote of this cart gives it),
	// and a line whose item is gone can't be sold at the seat's number.
	var today *pricing.Quote
	rungOK := func(i int, l pricing.Line) bool {
		if l.Custom || i >= len(in.Rungs) || in.Rungs[i].Cents == nil {
			return true
		}
		if goneReason(ctx, l) != "" {
			return false
		}
		if l.DealID != "" {
			return true // a bundle's price is the deal's: its rung is never read
		}
		want := *in.Rungs[i].Cents
		if r := pricing.Resolve(ctx, l.Selection, 0); len(r.Issues) == 0 && r.UnitCents == want {
			return true
		}
		if today == nil {
			pc := in.Cart
			pc.Type = terminal.TypePickup
			if pc.Payment == "" {
				pc.Payment = "cash"
			}
			q := pricing.Price(ctx, pc, time.Now())
			today = &q
		}
		return i < len(today.Lines) && today.Lines[i].UnitCents == want
	}
	for i, l := range in.Lines {
		if k := heldKey(l); held[k] > 0 {
			held[k]--
			continue
		}
		if l.Custom {
			// Quick add (owner 2026-11-13): the call center's typed line, like
			// the terminal's Quicc Add (engine-checked, journaled as
			// custom_line, routed by the store's Quick add item). One named
			// like a custom line the order holds, at another price, is that
			// line re-priced (an online order's own line): still refused.
			if heldNames[customName(l)] {
				return notAllowed(terminal.NotAllowedOpenPrice)
			}
			continue
		}
		if !rungOK(i, l) {
			return notAllowed(terminal.NotAllowedPriceChange)
		}
		if l.DealID != "" {
			for _, pk := range l.Picks {
				if what := priced(pk); what != "" {
					return notAllowed(what)
				}
			}
			continue
		}
		if what := priced(l.Selection); what != "" {
			return notAllowed(what)
		}
	}
	return nil
}

// customName is a custom line's name as the stored order compares it.
func customName(l pricing.Line) string {
	name := strings.TrimSpace(l.Name)
	if name == "" {
		name = pricing.CustomLineName
	}
	return strings.ToLower(name)
}

// heldKey tells whether a cart line is one the stored order holds: a custom
// line by its name and price, any other by what it is and what it is priced
// at (item, choice, size, options, halves, deal picks, every price on it).
// Quantity and note are left out, and how the sender wrote an empty list.
func heldKey(l pricing.Line) string {
	cents := func(p *int64) string {
		if p == nil {
			return "-"
		}
		return strconv.FormatInt(*p, 10)
	}
	if l.Custom {
		name := strings.TrimSpace(l.Name)
		if name == "" {
			name = pricing.CustomLineName
		}
		return "custom\x1f" + name + "\x1f" + cents(l.PriceCents)
	}
	var b strings.Builder
	sel := func(s pricing.Selection) {
		b.WriteString(s.ItemID + "\x1f" + s.ChoiceID + "\x1f" + s.SizeID + "\x1f" + cents(s.PriceCents))
		for _, o := range s.Options {
			b.WriteString("\x1eo" + o.GroupID + "\x1f" + o.OptionID + "\x1f" + o.Portion + "\x1f" + o.Mode + "\x1f" + cents(o.PriceCents))
		}
		for _, h := range s.Halves {
			b.WriteString("\x1eh" + h.ItemID + "\x1f" + strconv.Itoa(h.Half))
		}
	}
	b.WriteString("line\x1f" + l.DealID + "\x1f")
	if l.KeepPrice {
		b.WriteString("keep")
	}
	b.WriteString("\x1f")
	sel(l.Selection)
	for _, pk := range l.Picks {
		b.WriteString("\x1dp")
		sel(pk)
	}
	return b.String()
}

// callcenterPlaceGuard: a new call-center order is a pickup or a delivery,
// paid with a typed card or not at all.
func callcenterPlaceGuard(a Actor, in OrderIn) error {
	if !a.Callcenter {
		return nil
	}
	if t := strings.TrimSpace(in.Type); t != terminal.TypePickup && t != terminal.TypeDelivery {
		return notAllowed(terminal.NotAllowedType)
	}
	if strings.TrimSpace(in.ManagerPIN) != "" || len(in.Approvals) > 0 {
		return notAllowed(terminal.NotAllowedManager)
	}
	return callcenterPaymentsGuard(a, in.Payments...)
}

// --- paid lines locked ---------------------------------------------------------------

// paidLocked: the call center changes an order with money on it (a payment
// at a terminal, or an online / website order that arrived paid): what was
// there may not be removed, lowered or changed. removed = the change's
// removed lines (a changed line is its old self removed and its new self
// added).
func paidLocked(a Actor, cur order.StoredOrder, paid []terminal.Payment, removed []terminal.LineDelta) error {
	if !a.Callcenter || len(removed) == 0 {
		return nil
	}
	net, _ := netPaid(paid)
	if cur.Order.Source != order.SourceCounter {
		net += onlineBase(cur.Order).PaidCents
	}
	if net <= 0 {
		return nil
	}
	return &terminal.PaidLocked{Lines: terminal.Labels(removed)}
}

// --- the store wins ------------------------------------------------------------------

// storeHolds: a store terminal has the order open (a draft of it open
// there): the call center can't change or cancel it (409 open_elsewhere
// {holder}). A draft a call-center seat holds doesn't lock the order here
// (a PUT with its draft_id is checked in the order's transaction).
func (s *Service) storeHolds(ctx context.Context, a Actor, orderID string) error {
	if !a.Callcenter {
		return nil
	}
	list, err := s.repo.TerminalDrafts(ctx, s.location())
	if err != nil {
		return err
	}
	for _, d := range list {
		if d.OrderID == orderID && d.Holder != nil && !device.IsCallcenterSeat(d.Holder.DeviceID) {
			return &terminal.OpenElsewhere{Holder: *d.Holder}
		}
	}
	return nil
}

// StoreHolds: a store terminal has the order open (the relay asks before a
// seat's payment or time change; a cart change and a void ask themselves).
func (s *Service) StoreHolds(ctx context.Context, a Actor, orderID string) error {
	return s.storeHolds(ctx, a, orderID)
}

// --- out of the delivery area ----------------------------------------------------------

// AddressArea places a delivery address against the store's area the way
// the customers lookup does: the customer's own saved address (its point,
// else one learned from a located delivery to the same place), else a
// located delivery to that place. nil = no area check (the store has no
// pin or radius, or the customers aren't running); unknown = no point yet.
func (s *Service) AddressArea(ctx context.Context, cust *Customer) (*terminal.Area, *AreaInfo) {
	info := s.areaInfo()
	if info == nil || cust == nil || cust.Address == nil || strings.TrimSpace(cust.Address.Line1) == "" {
		return nil, nil
	}
	ad := cust.Address
	if id := strings.TrimSpace(cust.ID); id != "" {
		if c, err := s.cust.repo.Customer(ctx, id); err == nil {
			cs := []terminal.Customer{c}
			s.placeAll(ctx, cs)
			for _, a := range cs[0].Addresses {
				if terminal.SameAddress(a.Line1, a.Zip, ad.Line1, ad.Zip) && a.Area != nil && a.Area.State != terminal.AreaUnknown {
					return a.Area, info
				}
			}
		}
	}
	lat, lng, radius := s.cust.area()
	if p, ok := s.cust.located(ctx, ad.Line1, ad.Zip); ok {
		ar := terminal.AreaOf(terminal.DistanceM(*lat, *lng, p.Lat, p.Lng), radius, info.MarginMi)
		return &ar, info
	}
	return &terminal.Area{State: terminal.AreaUnknown}, info
}

// CallcenterAreaCheck is the call center's rule for a delivery address
// (the owner, 2026-11-12: allowed with a warning, no manager's PIN): out of
// the area without ack → *terminal.OutOfArea (409); with it → the check to
// mark the order with; in / near / unknown → as today. ok false = nothing
// to check.
func (s *Service) CallcenterAreaCheck(ctx context.Context, cust *Customer, ack bool) (check terminal.AreaCheck, ok bool, err error) {
	area, info := s.AddressArea(ctx, cust)
	if area == nil {
		return terminal.AreaCheck{}, false, nil
	}
	if area.State != terminal.AreaOut {
		return terminal.AreaCheck{Area: *area}, true, nil
	}
	if !ack {
		return terminal.AreaCheck{}, false, &terminal.OutOfArea{Area: *area, RadiusMi: info.RadiusMi, MarginMi: info.MarginMi}
	}
	return terminal.AreaCheck{Area: *area, Out: true}, true, nil
}

// areaMark applies the relay's area check to a changed order (inside the
// change's transaction): a delivery whose address changed takes the mark
// of its new address (out + acknowledged: marked and journaled; else the
// old mark comes off); an order that is no delivery any more loses it.
func areaMark(ctx context.Context, snap *order.Snapshot, before order.Snapshot, staffID, deviceID string) []terminal.Entry {
	if snap.Fulfillment.Address == nil || !strings.EqualFold(snap.Fulfillment.Type, terminal.TypeDelivery) {
		snap.OutOfArea = false
		return nil
	}
	old := before.Fulfillment.Address
	if old != nil && *old == *snap.Fulfillment.Address && strings.EqualFold(before.Fulfillment.Type, terminal.TypeDelivery) {
		return nil // the address stays: so does its mark
	}
	c := terminal.AreaCheckOf(ctx)
	if c == nil || !c.Out { // a new address nobody acknowledged as out of the area (a store terminal's change too)
		snap.OutOfArea = false
		return nil
	}
	snap.OutOfArea = true
	return []terminal.Entry{terminal.OutOfAreaEntry(*c, staffID, deviceID, snap.ID)}
}

// --- taken by ------------------------------------------------------------------------

// takenBy stamps an order the call center places: who took it and the
// device kind (the order's taken_by / device_kind: lists, tickets, main's
// per-agent report).
func takenBy(a Actor, st terminal.Staff, snap *order.Snapshot) {
	if !a.Callcenter {
		return
	}
	snap.TakenBy, snap.DeviceKind = terminal.TakenByCallcenter(st), terminal.DeviceKindCallcenter
	if tm, ok := snap.SourceMeta["terminal"].(map[string]any); ok {
		tm["seat_id"], tm["device_id"], tm["device_kind"] = a.DeviceID, device.CallcenterID, terminal.DeviceKindCallcenter
	}
}

// printCallcenterReceipt: a call-center order has no terminal printer, so
// the Pi prints its customer receipt / delivery ticket on the store's
// receipt printer (background; the station tickets printed as any SEND's).
func (s *Service) printCallcenterReceipt(ctx context.Context, so order.StoredOrder) {
	if p, ok := s.orders.(interface {
		PrintOrderReceipt(ctx context.Context, so order.StoredOrder)
	}); ok {
		p.PrintOrderReceipt(ctx, so)
	}
}

// --- reprint (every terminal kind) ---------------------------------------------------

// Reprinter prints an order's customer receipt again on the store's receipt
// printer with failover (the order service's ReprintOn).
type Reprinter interface {
	ReprintOn(ctx context.Context, id string) (*order.StoredOrder, string, error)
}

// ReprintResult is POST /api/terminal/orders/{id}/reprint's answer.
type ReprintResult struct {
	OK        bool   `json:"ok"`
	PrintedOn string `json:"printed_on"`
}

// Reprint prints the order's customer receipt on the location's receipt
// printer (the tablet's Reprint), journaled `reprint {number}`.
func (s *Service) Reprint(ctx context.Context, a Actor, id string) (*ReprintResult, error) {
	st, _, err := s.staffOf(a)
	if err != nil {
		return nil, err
	}
	rp, ok := s.orders.(Reprinter)
	if !ok {
		return nil, order.ErrNotFound
	}
	so, on, err := rp.ReprintOn(ctx, id)
	if err != nil {
		return nil, err
	}
	s.journal(ctx, terminal.Entry{StaffID: st.ID, DeviceID: a.DeviceID, Action: terminal.ActionReprint, OrderID: so.Order.ID,
		Detail: detail(map[string]any{"number": numberOf(*so), "printed_on": on})})
	s.logger.Info().Ctx(ctx).Str("order_id", so.Order.ID).Int64("number", numberOf(*so)).Str("staff_id", st.ID).
		Str("device_id", a.DeviceID).Str("printed_on", on).Msg("Receipt reprinted from a terminal")
	return &ReprintResult{OK: true, PrintedOn: on}, nil
}
