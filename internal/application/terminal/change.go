package terminal

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/terminal"

	"github.com/google/uuid"
)

// Kitchen changes after SEND (TERMINAL-PLAN "After SEND", contract §2.10d).

// EventOrderChanged is the SSE event a kitchen change sends to the terminals.
const EventOrderChanged = "terminal.order.changed"

// ChangeIn is PUT /api/terminal/orders/{id}/cart: the order's whole new cart.
type ChangeIn struct {
	Cart       Cart   `json:"cart"`
	ClientKey  string `json:"client_key"`
	ManagerPIN string `json:"manager_pin"`
}

// ChangeResult is the changed order and what the kitchen was told.
type ChangeResult struct {
	Order        order.StoredOrder `json:"order"`
	Added        []string          `json:"added"`
	Removed      []string          `json:"removed"`
	Changed      bool              `json:"changed"`
	Duplicate    bool              `json:"duplicate"`
	BalanceCents int64             `json:"balance_cents"`
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
	ClientKey string    `json:"client_key"`
	At        time.Time `json:"at"`
	StaffID   string    `json:"staff_id"`
	Added     []string  `json:"added"`
	Removed   []string  `json:"removed"`
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
	st, cfg, err := s.staffOf(a)
	if err != nil {
		return nil, err
	}
	if _, err := uuid.Parse(id); err != nil {
		return nil, order.ErrNotFound
	}
	if in.ClientKey != "" && !keyRe.MatchString(in.ClientKey) {
		return nil, badRequest("client_key is 8-64 letters, digits, _ . : -")
	}
	cur, err := s.Order(ctx, id)
	if err != nil {
		return nil, err
	}
	if e := findEdit(cur.Order.Order, in.ClientKey); e != nil {
		return &ChangeResult{Order: cur.Order, Added: nonNil(e.Added), Removed: nonNil(e.Removed), Changed: true, Duplicate: true,
			BalanceCents: cur.Order.Status.CashDueCents}, nil
	}
	m, ms := s.store.Menu()
	if m == nil {
		return nil, terminal.ErrNoMenu
	}
	typ := orderType(cur.Order.Order)
	now := s.now()
	p, err := priceCart(m, s.orders.StoreLocation(), typ, in.Cart, now)
	if err != nil {
		return nil, badRequest(err.Error())
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
	approver, overrides, err := s.approve(a, st, cfg, in.ManagerPIN, id, need, priceNeed, compNeed)
	if err != nil {
		return nil, err
	}
	items, off, fees, err := buildItems(m, p)
	if err != nil {
		return nil, err
	}
	money := moneyOf(items, p, off, fees)

	var added, removed []terminal.LineDelta
	var dup *editRecord
	so, _, changed, err := s.repo.ChangeCounterOrder(ctx, id, st.ID, a.DeviceID, func(cur order.StoredOrder, paid []terminal.Payment) (*order.Snapshot, []terminal.Entry, error) {
		if e := findEdit(cur.Order, in.ClientKey); e != nil { // a retry raced the first one
			dup = e
			return nil, nil, nil
		}
		amount, _ := netPaid(paid)
		if money.Total < amount {
			return nil, nil, fmt.Errorf("%w (%d paid, the new total is %d)", terminal.ErrRefundFirst, amount, money.Total)
		}
		lines := append([]order.LineItem(nil), items...)
		terminal.KeepLineIDs(cur.Order.Items, lines)
		added, removed = terminal.DiffLines(cur.Order.Items, lines)
		if len(added) == 0 && len(removed) == 0 && sameMoney(cur.Order.Money, money) {
			return nil, nil, nil
		}
		snap := cur.Order
		snap.Items, snap.Money = lines, money
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
		rec := editRecord{ClientKey: in.ClientKey, At: now.UTC().Truncate(time.Millisecond), StaffID: st.ID,
			Added: nonNil(terminal.Labels(added)), Removed: nonNil(terminal.Labels(removed))}
		hist := append(edits(cur.Order), rec)
		if len(hist) > terminal.MaxEditKeys {
			hist = hist[len(hist)-terminal.MaxEditKeys:]
		}
		tm["cart"], tm["edits"], tm["menu_version"] = in.Cart, hist, ms.Version
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
		entries := []terminal.Entry{{StaffID: st.ID, DeviceID: a.DeviceID, Action: terminal.ActionOrderChanged, OrderID: id}}
		if approver != nil {
			d["approved_by"] = approver.ID
		}
		entries = append(entries, overrides...)
		entries = append(entries, priceEntries(a, st, p.changes, approver, id)...)
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
	if !changed {
		return res, nil
	}
	s.orders.PrintChange(ctx, so, terminal.Lines(added), terminal.Lines(removed))
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
