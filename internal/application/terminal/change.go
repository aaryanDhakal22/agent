package terminal

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/terminal"

	"github.com/google/uuid"
)

// Kitchen changes after SEND (TERMINAL-PLAN "After SEND", contract §2.10d).

// EventOrderChanged is the SSE event a kitchen change sends to the terminals.
const EventOrderChanged = "terminal.order.changed"

// ChangeIn is PUT /api/terminal/orders/{id}/cart: the order's whole new cart.
// Type (with Customer) moves the order takeout <-> delivery (terminal owner
// pass 2026-11-12): re-quoted for the new type, a delivery needs the
// customer's address, the kitchen gets a change ticket saying the new type.
type ChangeIn struct {
	Cart       Cart      `json:"cart"`
	ClientKey  string    `json:"client_key"`
	ManagerPIN string    `json:"manager_pin"`
	Type       string    `json:"type"`
	Customer   *Customer `json:"customer"`
	// PrepMinutes: accepted for the shape of POST /orders; the order keeps
	// its ready time (the kitchen's clock does not change with the type).
	PrepMinutes *int `json:"prep_minutes"`
	// Approvals (X-5) and DraftID (deleted in the change's transaction).
	Approvals []string `json:"approvals,omitempty"`
	DraftID   string   `json:"draft_id,omitempty"`
}

// typeChange checks a takeout <-> delivery move: "" = the type stays.
func typeChange(cur string, in ChangeIn) (string, error) {
	to := strings.TrimSpace(in.Type)
	if to == "" || to == cur {
		return "", nil
	}
	if (to != terminal.TypePickup && to != terminal.TypeDelivery) || (cur != terminal.TypePickup && cur != terminal.TypeDelivery) {
		return "", badRequest("only a pickup and a delivery change into each other")
	}
	if to == terminal.TypeDelivery {
		c := in.Customer
		if c == nil || c.Address == nil || strings.TrimSpace(c.Address.Line1) == "" || strings.TrimSpace(c.Address.City) == "" {
			return "", badRequest("a delivery needs the customer's address (line1 and city)")
		}
	}
	return to, nil
}

// ChangeResult is the changed order and what the kitchen was told.
type ChangeResult struct {
	Order        order.StoredOrder `json:"order"`
	Added        []string          `json:"added"`
	Removed      []string          `json:"removed"`
	Changed      bool              `json:"changed"`
	Duplicate    bool              `json:"duplicate"`
	BalanceCents int64             `json:"balance_cents"`
	// TypeChanged: the order moved takeout <-> delivery (ChangeIn.Type).
	TypeChanged bool `json:"type_changed"`
	// RefundDue (an online order, §2.10aa): this change's refund owed to its
	// customer (the terminal prints the REFUND DUE slip); nil = none.
	RefundDue *terminal.RefundDue `json:"refund_due"`
}

// orderAddress is a terminal address as the order keeps it (nil = none given:
// line1 or city missing).
func orderAddress(c *Customer) *order.Address {
	if c == nil || c.Address == nil || strings.TrimSpace(c.Address.Line1) == "" || strings.TrimSpace(c.Address.City) == "" {
		return nil
	}
	ad := c.Address
	return &order.Address{Line1: strings.TrimSpace(ad.Line1), Line2: strings.TrimSpace(ad.Line2), City: strings.TrimSpace(ad.City),
		State: strings.TrimSpace(ad.State), PostalCode: strings.TrimSpace(ad.Zip), Country: "US", Notes: strings.TrimSpace(ad.Notes)}
}

// customerAt is a sent order's delivery address as a terminal Customer
// (for the Pi's area / distance lookup).
func customerAt(ad *order.Address) *Customer {
	if ad == nil {
		return nil
	}
	return &Customer{Address: &Address{Line1: ad.Line1, Line2: ad.Line2, City: ad.City, State: ad.State, Zip: ad.PostalCode}}
}

// recustomer applies a customer edit to a sent order whose type stays
// (TERMINAL-FLOWS CHG-14: "the customer gave the wrong address"): a
// delivery's new address (line1 + city given), the name, the phone. A field
// left empty keeps its value. false = nothing differs.
func recustomer(snap *order.Snapshot, typ string, c *Customer) bool {
	if c == nil {
		return false
	}
	changed := false
	if na := orderAddress(c); na != nil && typ == terminal.TypeDelivery {
		if old := snap.Fulfillment.Address; old == nil || *old != *na {
			snap.Fulfillment.Address = na
			changed = true
		}
	}
	if n := strings.TrimSpace(c.Name); n != "" && n != snap.Customer.DisplayName {
		snap.Customer.DisplayName = n
		changed = true
	}
	if ph := strings.TrimSpace(c.Phone); ph != "" && ph != snap.Customer.Phone {
		snap.Customer.Phone = ph
		changed = true
	}
	return changed
}

// retype moves snap to the terminal type [to]: the fulfillment (a
// delivery's address from c), the customer's name / phone when given, the
// terminal type and customer id in source_metadata.
func retype(snap *order.Snapshot, to string, c *Customer) {
	ful := order.Fulfillment{Type: terminal.FulfillmentOf(to), PromisedAt: snap.Fulfillment.PromisedAt}
	if to == terminal.TypeDelivery && c != nil && c.Address != nil {
		ad := c.Address
		ful.Address = &order.Address{Line1: strings.TrimSpace(ad.Line1), Line2: strings.TrimSpace(ad.Line2), City: strings.TrimSpace(ad.City),
			State: strings.TrimSpace(ad.State), PostalCode: strings.TrimSpace(ad.Zip), Country: "US", Notes: strings.TrimSpace(ad.Notes)}
	}
	snap.Fulfillment = ful
	if c != nil {
		if n := strings.TrimSpace(c.Name); n != "" {
			snap.Customer.DisplayName = n
		}
		if ph := strings.TrimSpace(c.Phone); ph != "" {
			snap.Customer.Phone = ph
		}
	}
	meta := make(map[string]any, len(snap.SourceMeta))
	for k, v := range snap.SourceMeta {
		meta[k] = v
	}
	tm := map[string]any{}
	if old, ok := meta["terminal"].(map[string]any); ok {
		for k, v := range old {
			tm[k] = v
		}
	}
	tm["type"] = to
	if c != nil && strings.TrimSpace(c.ID) != "" {
		tm["customer_id"] = strings.TrimSpace(c.ID)
	}
	meta["terminal"] = tm
	snap.SourceMeta = meta
}

// orderType is the terminal type a counter order was placed as (its
// source_metadata), else read from the fulfillment.
func orderType(snap order.Snapshot) string {
	if tm, ok := snap.SourceMeta["terminal"].(map[string]any); ok {
		if t, _ := tm["type"].(string); t != "" {
			return t
		}
	}
	switch snap.Fulfillment.Type {
	case terminal.TypeDelivery:
		return terminal.TypeDelivery
	case terminal.TypePickup:
		return terminal.TypePickup
	}
	return terminal.TypeCounter
}

// editRecord is one kitchen change remembered on the order (by client_key).
type editRecord struct {
	ClientKey string `json:"client_key"`
	// Hash: the change request's fingerprint (contract 2.10s: the same key
	// with another body is 409 key_reused; "" on an older edit).
	Hash    string    `json:"hash,omitempty"`
	At      time.Time `json:"at"`
	StaffID string    `json:"staff_id"`
	Added   []string  `json:"added"`
	Removed []string  `json:"removed"`
}

// edits are the changes the order remembers (newest last).
func edits(snap order.Snapshot) []editRecord {
	tm, ok := snap.SourceMeta["terminal"].(map[string]any)
	if !ok {
		return nil
	}
	raw, err := json.Marshal(tm["edits"])
	if err != nil {
		return nil
	}
	var out []editRecord
	_ = json.Unmarshal(raw, &out)
	return out
}

func findEdit(snap order.Snapshot, key string) *editRecord {
	if key == "" {
		return nil
	}
	for _, e := range edits(snap) {
		if e.ClientKey == key {
			e := e
			return &e
		}
	}
	return nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// ChangeCart re-prices a placed counter order's whole cart and, when its
// lines or money moved, stores the new lines + money (payments stay), prints
// an ADD-ON ticket for the added lines and a VOID ticket for the removed
// ones, journals it and tells main (order.changed) and the terminals
// (terminal.order.changed). Only an order of the open batch that is accepted
// or ready; a total below what was paid needs a refund first. Idempotent by
// client_key.
func (s *Service) ChangeCart(ctx context.Context, a Actor, id string, in ChangeIn) (*ChangeResult, error) {
	ctx = terminal.WithDraftID(WithApprovals(ctx, in.Approvals), strings.TrimSpace(in.DraftID))
	res, err := s.changeCart(ctx, a, id, in)
	if err == nil && !res.Duplicate && in.DraftID != "" {
		s.publishDraft(a.Callcenter, terminal.EventDraftRemoved, map[string]any{"id": in.DraftID})
	}
	return res, err
}

func (s *Service) changeCart(ctx context.Context, a Actor, id string, in ChangeIn) (*ChangeResult, error) {
	st, cfg, err := s.staffOf(a)
	if err != nil {
		return nil, err
	}
	if _, err := uuid.Parse(id); err != nil {
		return nil, order.ErrNotFound
	}
	if err := requireKey(in.ClientKey); err != nil {
		return nil, err
	}
	hash := changeFingerprint(in)
	cur, err := s.Order(ctx, id)
	if err != nil {
		return nil, err
	}
	if e := findEdit(cur.Order.Order, in.ClientKey); e != nil {
		if !terminal.SameRequest(e.Hash, hash) {
			return nil, &terminal.KeyReused{ClientKey: in.ClientKey, OrderID: id}
		}
		return &ChangeResult{Order: cur.Order, Added: nonNil(e.Added), Removed: nonNil(e.Removed), Changed: true, Duplicate: true,
			BalanceCents: cur.Order.Status.CashDueCents}, nil
	}
	if a.Callcenter { // §2.10ab: no manager; the store wins an order it has open
		if strings.TrimSpace(in.ManagerPIN) != "" || len(in.Approvals) > 0 {
			return nil, notAllowed(terminal.NotAllowedManager)
		}
		if err := s.storeHolds(ctx, a, id); err != nil {
			return nil, err
		}
	}
	if cur.Order.Order.Source != order.SourceCounter {
		return s.changeOnline(ctx, a, st, cfg, cur.Order, in, hash) // §2.10aa
	}
	m, ms := s.store.Menu()
	if m == nil {
		return nil, terminal.ErrNoMenu
	}
	if a.Callcenter {
		var held *Cart
		if c, ok := storedCart(cur.Order.Order); ok {
			held = &c
		}
		if err := callcenterCartGuard(a, counterCtx(m, s.orders.StoreLocation(), s.toppingMax()), in.Cart, held); err != nil {
			return nil, err
		}
	}
	was := orderType(cur.Order.Order)
	to, err := typeChange(was, in)
	if err != nil {
		return nil, err
	}
	typ := was
	if to != "" {
		typ = to
	}
	now := s.now()
	// The delivery fee by distance (2.10ae): the new address's, else the
	// order's own.
	if c := in.Customer; c != nil && orderAddress(c) != nil {
		in.Cart = s.withDistance(ctx, typ, c, in.Cart)
	} else if ad := cur.Order.Order.Fulfillment.Address; ad != nil {
		in.Cart = s.withDistance(ctx, typ, customerAt(ad), in.Cart)
	} else {
		in.Cart = s.withDistance(ctx, typ, nil, in.Cart)
	}
	// M-15 / §2.10u: kept lines keep their rung prices, gone ones are sold
	// as custom lines (rung.go).
	p, _, err := priceRung(m, s.orders.StoreLocation(), typ, &cur.Order.Order, in.Cart, now, s.toppingMax())
	if err != nil {
		return nil, asBadRequest(err)
	}
	if len(p.quote.Errors) > 0 {
		return nil, &terminal.CartInvalid{Issues: p.quote.Errors}
	}
	if len(p.cart.Lines) == 0 {
		return nil, badRequest("an order keeps at least one line (void it instead)")
	}
	need := discountNeed(cfg, st, in.Cart.Discount, p)
	// A price change (or comp line) the order had already (same item, size
	// and price) was approved then: only new ones are checked and journaled.
	priceNeed, compNeed := checkPrices(cfg, st, p.changes, keptPrices(cur.Order.Order))
	items, off, fees, err := buildItems(m, p)
	if err != nil {
		return nil, err
	}
	money := moneyOf(items, p, off, fees)
	// A sent line taken off so that the check gets smaller is a void of what
	// the kitchen got: a manager's (the owner, 2026-11-12 evening).
	voidNeed := removalNeed(a, cfg, st, cur.Order.Order, items, money.Total)
	if voidNeed != nil {
		// The money comes back first (refund_first), before a manager is
		// asked for anything: checked again in the transaction.
		if paid, err := s.repo.OrderPayments(ctx, id); err != nil {
			return nil, err
		} else if amount, _ := netPaid(paid); money.Total < amount {
			return nil, fmt.Errorf("%w (%d paid, the new total is %d)", terminal.ErrRefundFirst, amount, money.Total)
		}
	}
	modNeed := checkModPrices(cfg, st, p.cart, p.modChanges, keptModPrices(cur.Order.Order)) // §2.10an
	approver, overrides, err := s.approve(ctx, a, st, cfg, in.ManagerPIN, id, need, priceNeed, compNeed, voidNeed, modNeed)
	if err != nil {
		return nil, err
	}
	if err := s.checkMoney(ctx, money, items); err != nil {
		return nil, err
	}

	var added, removed []terminal.LineDelta
	var dup *editRecord
	so, _, changed, err := s.repo.ChangeCounterOrder(ctx, id, st.ID, a.DeviceID, func(cur order.StoredOrder, paid []terminal.Payment) (*order.Snapshot, []terminal.Entry, error) {
		if e := findEdit(cur.Order, in.ClientKey); e != nil { // a retry raced the first one
			if !terminal.SameRequest(e.Hash, hash) {
				return nil, nil, &terminal.KeyReused{ClientKey: in.ClientKey, OrderID: id}
			}
			dup = e
			return nil, nil, nil
		}
		amount, _ := netPaid(paid)
		lines := append([]order.LineItem(nil), items...)
		terminal.KeepLineIDs(cur.Order.Items, lines)
		added, removed = terminal.DiffLines(cur.Order.Items, lines)
		if err := paidLocked(a, cur, paid, removed); err != nil { // §2.10ab: before refund_first
			return nil, nil, err
		}
		if money.Total < amount {
			return nil, nil, fmt.Errorf("%w (%d paid, the new total is %d)", terminal.ErrRefundFirst, amount, money.Total)
		}
		if to != "" && (cur.Status.DriverID != "" || cur.Status.PoolDriverID != "") {
			return nil, nil, terminal.ErrTypeLocked
		}
		snap := cur.Order
		recust := to == "" && recustomer(&snap, typ, in.Customer)
		if to == "" && !recust && len(added) == 0 && len(removed) == 0 && sameMoney(cur.Order.Money, money) {
			return nil, nil, nil
		}
		snap.Items, snap.Money = lines, money
		if to != "" {
			retype(&snap, to, in.Customer)
		}
		area := areaMark(ctx, &snap, cur.Order, st.ID, a.DeviceID) // §2.10ab: a call-center address out of the area
		meta := make(map[string]any, len(snap.SourceMeta)+1)
		for k, v := range snap.SourceMeta {
			meta[k] = v
		}
		tm := map[string]any{}
		if old, ok := meta["terminal"].(map[string]any); ok {
			for k, v := range old {
				tm[k] = v
			}
		}
		rec := editRecord{ClientKey: in.ClientKey, Hash: hash, At: now.UTC().Truncate(time.Millisecond), StaffID: st.ID,
			Added: nonNil(terminal.Labels(added)), Removed: nonNil(terminal.Labels(removed))}
		hist := append(edits(cur.Order), rec)
		if len(hist) > terminal.MaxEditKeys {
			hist = hist[len(hist)-terminal.MaxEditKeys:]
		}
		tm["cart"], tm["edits"], tm["menu_version"], tm["pick_cents"] = in.Cart, hist, ms.Version, pickCents(p)
		meta["terminal"] = tm
		snap.SourceMeta = meta
		d := map[string]any{"added": rec.Added, "removed": rec.Removed, "total_before_cents": cur.Order.Money.Total,
			"total_after_cents": money.Total, "number": numberOf(cur)}
		if in.ClientKey != "" {
			d["client_key"] = in.ClientKey
		}
		if p.manualCents > 0 {
			d["discount_cents"] = p.manualCents
		}
		if to != "" {
			d["type_from"], d["type_to"] = was, to
		}
		if recust {
			d["customer_changed"] = true
		}
		entries := []terminal.Entry{{StaffID: st.ID, DeviceID: a.DeviceID, Action: terminal.ActionOrderChanged, OrderID: id}}
		if approver != nil {
			d["approved_by"] = approver.ID
		}
		entries = append(entries, overrides...)
		entries = append(entries, priceEntries(a, st, p.changes, approver, id)...)
		entries = append(entries, modPriceEntries(a, st, p.modChanges, approver, id)...)
		entries = append(entries, customEntries(a, st, p, id, keptCustom(cur.Order))...)
		entries = append(entries, rungEntries(a, st, p, id, keptCustom(cur.Order))...)
		entries = append(entries, couponEntries(a, st, p, id, cartCoupons(cur.Order))...)
		entries = append(entries, area...)
		entries[0].Detail = detail(d)
		return &snap, entries, nil
	})
	if err != nil {
		return nil, err
	}
	if dup != nil {
		return &ChangeResult{Order: so, Added: nonNil(dup.Added), Removed: nonNil(dup.Removed), Changed: true, Duplicate: true,
			BalanceCents: so.Status.CashDueCents}, nil
	}
	res := &ChangeResult{Order: so, Added: nonNil(terminal.Labels(added)), Removed: nonNil(terminal.Labels(removed)), Changed: changed,
		BalanceCents: so.Status.CashDueCents}
	res.TypeChanged = to != ""
	if !changed {
		return res, nil
	}
	printed := so
	if to != "" {
		printed.Order.TypeChangedFrom = was
	}
	s.orders.PrintChange(ctx, printed, terminal.Lines(added), terminal.Lines(removed))
	s.orders.CounterOrderChanged(ctx, so, nil, false)
	s.publish(EventOrderChanged, map[string]any{"order": so, "added": res.Added, "removed": res.Removed, "device_id": a.DeviceID})
	s.orders.DevEvent(fmt.Sprintf("Counter order #%d changed (+%d / -%d lines)", numberOf(so), len(added), len(removed)))
	s.logger.Info().Ctx(ctx).Str("order_id", id).Int64("number", numberOf(so)).Int("added", len(added)).Int("removed", len(removed)).
		Str("staff_id", st.ID).Str("device_id", a.DeviceID).Msg("Counter order changed")
	return res, nil
}

// keptPrices are the price changes on the order's stored cart (Key → count).
func keptPrices(snap order.Snapshot) map[string]int {
	tm, ok := snap.SourceMeta["terminal"].(map[string]any)
	if !ok {
		return nil
	}
	raw, err := json.Marshal(tm["cart"])
	if err != nil {
		return nil
	}
	var c Cart
	if json.Unmarshal(raw, &c) != nil {
		return nil
	}
	out := map[string]int{}
	for _, l := range c.Lines {
		if l.DealID == "" && l.PriceCents != nil {
			out[terminal.PriceChange{ItemID: l.ItemID, SizeID: l.SizeID, ChoiceID: l.ChoiceID, ToCents: *l.PriceCents}.Key()]++
		}
	}
	return out
}

// sameMoney compares two orders' money as they go over the wire (an empty
// list and none are the same).
func sameMoney(a, b order.Money) bool {
	ja, err1 := json.Marshal(a)
	jb, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(ja) == string(jb)
}

// removalNeed: a kitchen change that takes a sent line off the order (or
// lowers its quantity) and leaves the check smaller than it was is a void of
// what was sent: a manager's PIN / approval for "void" unless a manager (or
// anyone main made unlimited) does it. Adding lines, or swapping a line for
// one that costs as much or more, is free; the call center has no manager
// (its paid lines are locked instead, contract 2.10ab). Requested = how much
// the check goes down by.
func removalNeed(a Actor, cfg *terminal.Config, st terminal.Staff, cur order.Snapshot, items []order.LineItem, total int64) *terminal.ManagerRequired {
	if a.Callcenter || total >= cur.Money.Total || cfg.VoidNeed(st, 1) == nil {
		return nil
	}
	lines := append([]order.LineItem(nil), items...)
	terminal.KeepLineIDs(cur.Items, lines)
	if _, removed := terminal.DiffLines(cur.Items, lines); len(removed) == 0 {
		return nil
	}
	return cfg.VoidNeed(st, cur.Money.Total-total)
}
