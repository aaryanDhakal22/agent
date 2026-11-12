package terminal

import (
	"context"
	"fmt"
	"strings"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/pricing"
	"quiccpos/agent/internal/domain/terminal"
)

// Online orders = called-in orders (agent contract §2.10aa): an online /
// website order is edited at a terminal like ours (add / remove lines, the
// address, the time). Its own lines are custom lines on the terminal
// (terminal.OnlineCartLines); its money is its own, less what was removed,
// plus what was added at our price; removing below what it arrived paid with
// owes the customer a refund (no money moves: the manager refunds it in the
// vendor's site / Square), the REFUND DUE slip prints on the terminal.

// onlineRepo is the online-order part of the storage
// (repositories.OrderRepository).
type onlineRepo interface {
	ChangeOnlineOrder(ctx context.Context, id, staffID, deviceID string, check terminal.ChangeCheck,
		extra func() map[string]any) (so, prev order.StoredOrder, changed bool, err error)
	OnlineOrder(ctx context.Context, id string) (order.StoredOrder, error)
}

// withOnlineCart is the order as the terminals read it: an online order
// never edited gets its cart (its own lines as custom lines) and its type in
// source_metadata.terminal (a copy; the stored snapshot is untouched).
func withOnlineCart(so order.StoredOrder) order.StoredOrder {
	if so.Order.Source == order.SourceCounter {
		return so
	}
	tm, _ := so.Order.SourceMeta["terminal"].(map[string]any)
	if tm != nil && tm["cart"] != nil {
		return so
	}
	meta := make(map[string]any, len(so.Order.SourceMeta)+1)
	for k, v := range so.Order.SourceMeta {
		meta[k] = v
	}
	nt := map[string]any{}
	for k, v := range tm {
		nt[k] = v
	}
	nt["type"] = onlineType(so.Order)
	nt["cart"] = Cart{Cart: pricing.Cart{Lines: terminal.OnlineCartLines(so.Order.Items)}}
	meta["terminal"] = nt
	so.Order.SourceMeta = meta
	return so
}

// onlineType is an online order's terminal type (pickup | delivery).
func onlineType(snap order.Snapshot) string {
	if strings.EqualFold(snap.Fulfillment.Type, terminal.TypeDelivery) {
		return terminal.TypeDelivery
	}
	return terminal.TypePickup
}

// onlineBase is the order as it arrived: its kept base, else the order now
// (never edited yet) with what it arrived paid with.
func onlineBase(snap order.Snapshot) order.OnlineBase {
	if b := snap.OnlineBase(); b != nil {
		return *b
	}
	return order.OnlineBase{Items: snap.Items, Money: snap.Money, PaidCents: max(snap.Money.Total-snap.CashDue(), 0)}
}

// onlinePriced is an online order priced for a cart: its new lines and money.
type onlinePriced struct {
	base    order.OnlineBase
	items   []order.LineItem
	money   order.Money
	added   *priced // the added lines priced as a pickup (nil = none)
	removed int64
}

// priceOnline prices cart against the online order snap (pure but for the
// menu): the order's own lines it keeps, the rest priced as a pickup at our
// prices (no delivery fee; the order has its own).
func (s *Service) priceOnline(menu *terminal.CounterMenu, snap order.Snapshot, cart Cart, now time.Time) (*onlinePriced, error) {
	base := onlineBase(snap)
	own := terminal.OnlineLines(base.Items)
	match := terminal.MatchOnline(own, cart.Lines)
	out := &onlinePriced{base: base}
	var addedItems []order.LineItem
	var added order.Money
	if len(match.Added) > 0 {
		ac := Cart{Cart: cart.Cart, Discount: cart.Discount}
		ac.Lines = match.Added
		for _, i := range match.AddedFrom {
			if i < len(cart.Rungs) {
				ac.Rungs = append(ac.Rungs, cart.Rungs[i])
			} else {
				ac.Rungs = append(ac.Rungs, Rung{})
			}
		}
		p, _, err := priceRung(menu, s.orders.StoreLocation(), terminal.TypePickup, nil, ac, now, s.toppingMax())
		if err != nil {
			return nil, asBadRequest(err)
		}
		if len(p.quote.Errors) > 0 {
			return nil, &terminal.CartInvalid{Issues: p.quote.Errors}
		}
		items, off, fees, err := buildItems(menu, p)
		if err != nil {
			return nil, err
		}
		addedItems, added = items, moneyOf(items, p, off, fees)
		out.added = &p
	}
	m := terminal.OnlineMoney(terminal.OnlineMoneyIn{Base: base, Own: own, Kept: match.Kept, Added: added})
	out.items = append(append([]order.LineItem(nil), m.Items...), addedItems...)
	for i := range out.items {
		out.items[i].Position = i
	}
	out.money, out.removed = m.Money, m.RemovedCents
	return out, nil
}

// onlineQuote is POST /api/terminal/quote for an online order: the cart's
// lines as the engine prices them, its totals the order's after that change.
func (s *Service) onlineQuote(menu *terminal.CounterMenu, snap order.Snapshot, cart Cart) (*QuoteView, error) {
	op, err := s.priceOnline(menu, snap, cart, s.now())
	if err != nil {
		return nil, err
	}
	q := pricing.Quote{SubtotalCents: op.money.Subtotal, TaxCents: op.money.Tax, TipCents: op.money.Tip, TotalCents: op.money.Total,
		Errors: []pricing.Issue{}, Warnings: []string{}, Nudges: []string{}}
	for _, f := range op.money.Fees {
		if f.Type == "delivery" {
			q.DeliveryFeeCents += f.Amount
		} else {
			q.ServiceFeeCents += f.Amount
		}
	}
	for _, d := range op.money.Discounts {
		q.Discounts = append(q.Discounts, pricing.Discount{Type: d.Type, Label: d.Label, AmountCents: d.Amount})
	}
	// The lines as the engine prices the whole cart (the order's own lines are
	// custom lines at their price): the per-line view the terminal shows.
	if all, _, err := priceRung(menu, s.orders.StoreLocation(), terminal.TypePickup, nil, cart, s.now(), s.toppingMax()); err == nil {
		q.Lines, q.Warnings, q.LineWarnings = all.quote.Lines, all.quote.Warnings, all.quote.LineWarnings
		if q.Warnings == nil {
			q.Warnings = []string{}
		}
	}
	v := &QuoteView{Quote: q}
	if op.added != nil {
		v.Quote, v.DiscountPct, v.PriceChanges, v.MenuGone, v.KeptPrices = q, op.added.manualPct, op.added.changes, op.added.gone, op.added.kept
	}
	return v, nil
}

// changeOnline is ChangeCart for an online / website order.
func (s *Service) changeOnline(ctx context.Context, a Actor, st terminal.Staff, cfg *terminal.Config, cur order.StoredOrder, in ChangeIn,
	hash string) (*ChangeResult, error) {
	orp, ok := s.repo.(onlineRepo)
	if !ok {
		return nil, terminal.ErrNotCounterOrder
	}
	id := cur.Order.ID
	typ := onlineType(cur.Order)
	if t := strings.TrimSpace(in.Type); t != "" && t != typ {
		return nil, badRequest("an online order keeps its type")
	}
	menu, ms := s.store.Menu()
	if menu == nil {
		return nil, terminal.ErrNoMenu
	}
	now := s.now()
	// The manager's PIN is a bcrypt check: decided before the transaction on
	// the order as read now, priced again inside it.
	pre, err := s.priceOnline(menu, cur.Order, in.Cart, now)
	if err != nil {
		return nil, err
	}
	var need, priceNeed, compNeed *terminal.ManagerRequired
	if pre.added != nil {
		need = discountNeed(cfg, st, in.Cart.Discount, *pre.added)
		priceNeed, compNeed = checkPrices(cfg, st, pre.added.changes, keptPrices(cur.Order))
	}
	approver, overrides, err := s.approve(ctx, a, st, cfg, in.ManagerPIN, id, need, priceNeed, compNeed)
	if err != nil {
		return nil, err
	}
	if err := s.checkMoney(ctx, pre.money, pre.items); err != nil {
		return nil, err
	}

	var added, removed []terminal.LineDelta
	var dup *editRecord
	var due *terminal.RefundDue
	var owed int64
	so, _, changed, err := orp.ChangeOnlineOrder(ctx, id, st.ID, a.DeviceID, func(cur order.StoredOrder, paid []terminal.Payment) (*order.Snapshot, []terminal.Entry, error) {
		if e := findEdit(cur.Order, in.ClientKey); e != nil {
			if !terminal.SameRequest(e.Hash, hash) {
				return nil, nil, &terminal.KeyReused{ClientKey: in.ClientKey, OrderID: id}
			}
			dup = e
			return nil, nil, nil
		}
		op, err := s.priceOnline(menu, cur.Order, in.Cart, now)
		if err != nil {
			return nil, nil, err
		}
		if len(op.items) == 0 {
			return nil, nil, badRequest("an order keeps at least one line (cancel it instead)")
		}
		net, _ := netPaid(paid)
		if op.money.Total < net {
			return nil, nil, fmt.Errorf("%w (%d taken at a terminal, the new total is %d)", terminal.ErrRefundFirst, net, op.money.Total)
		}
		lines := op.items
		terminal.KeepLineIDs(cur.Order.Items, lines)
		added, removed = terminal.DiffLines(cur.Order.Items, lines)
		snap := cur.Order
		recust := recustomer(&snap, typ, in.Customer)
		if !recust && len(added) == 0 && len(removed) == 0 && sameMoney(cur.Order.Money, op.money) {
			return nil, nil, nil
		}
		prevOwed := cur.Order.RefundOwedCents()
		owed, _ = terminal.OnlineOwed(op.money.Total, op.base.PaidCents, net)
		snap.Items, snap.Money = lines, op.money
		rec := editRecord{ClientKey: in.ClientKey, Hash: hash, At: now.UTC().Truncate(time.Millisecond), StaffID: st.ID,
			Added: nonNil(terminal.Labels(added)), Removed: nonNil(terminal.Labels(removed))}
		hist := append(edits(cur.Order), rec)
		if len(hist) > terminal.MaxEditKeys {
			hist = hist[len(hist)-terminal.MaxEditKeys:]
		}
		tm := map[string]any{}
		if old, ok := cur.Order.SourceMeta["terminal"].(map[string]any); ok {
			for k, v := range old {
				tm[k] = v
			}
		}
		tm["type"], tm["cart"], tm["edits"], tm["menu_version"] = typ, in.Cart, hist, ms.Version
		snap.SourceMeta = snap.WithPiMeta(map[string]any{"online": op.base, "refund_owed_cents": owed})
		snap.SourceMeta["terminal"] = tm
		d := map[string]any{"added": rec.Added, "removed": rec.Removed, "total_before_cents": cur.Order.Money.Total,
			"total_after_cents": op.money.Total, "number": numberOf(cur), "source": cur.Order.Source, "client_key": in.ClientKey,
			"refund_owed_cents": owed}
		if recust {
			d["customer_changed"] = true
		}
		if approver != nil {
			d["approved_by"] = approver.ID
		}
		entries := []terminal.Entry{{StaffID: st.ID, DeviceID: a.DeviceID, Action: terminal.ActionOrderChanged, OrderID: id, Detail: detail(d)}}
		if owed > prevOwed {
			due = &terminal.RefundDue{Cents: owed - prevOwed, TotalOwed: owed, Reference: onlineReference(cur.Order),
				Source: sourceWord(cur.Order.Source), Number: numberOf(cur), StaffName: st.Name(), DeviceName: a.DeviceName,
				At: now.UTC().Truncate(time.Second).Format(time.RFC3339)}
			for _, r := range terminal.Lines(removed) {
				due.Lines = append(due.Lines, terminal.RefundDueLine{Name: lineName(r), Qty: max(r.Quantity, 1), Cents: r.LineTotal})
			}
			if due.Lines == nil {
				due.Lines = []terminal.RefundDueLine{}
			}
			entries = append(entries, terminal.Entry{StaffID: st.ID, DeviceID: a.DeviceID, Action: terminal.ActionRefundOwed, OrderID: id,
				Detail: detail(map[string]any{"cents": due.Cents, "total_owed_cents": owed, "lines": due.Lines, "reference": due.Reference,
					"source": cur.Order.Source, "number": due.Number})})
		}
		entries = append(entries, overrides...)
		if op.added != nil {
			entries = append(entries, priceEntries(a, st, op.added.changes, approver, id)...)
			entries = append(entries, customEntries(a, st, *op.added, id, keptCustom(cur.Order))...)
		}
		return &snap, entries, nil
	}, func() map[string]any {
		m := map[string]any{"refund_owed_cents": owed}
		if due != nil {
			m["refund_due"] = due
		}
		return m
	})
	if err != nil {
		return nil, err
	}
	if dup != nil {
		return &ChangeResult{Order: so, Added: nonNil(dup.Added), Removed: nonNil(dup.Removed), Changed: true, Duplicate: true,
			BalanceCents: so.Status.CashDueCents}, nil
	}
	res := &ChangeResult{Order: so, Added: nonNil(terminal.Labels(added)), Removed: nonNil(terminal.Labels(removed)), Changed: changed,
		BalanceCents: so.Status.CashDueCents, RefundDue: due}
	if !changed {
		return res, nil
	}
	s.orders.PrintChange(ctx, so, terminal.Lines(added), terminal.Lines(removed))
	s.orders.CounterOrderChanged(ctx, so, nil, false)
	s.publish(EventOrderChanged, map[string]any{"order": so, "added": res.Added, "removed": res.Removed, "device_id": a.DeviceID,
		"refund_due": due})
	s.orders.DevEvent(fmt.Sprintf("Online order #%d changed (+%d / -%d lines)", numberOf(so), len(added), len(removed)))
	s.logger.Info().Ctx(ctx).Str("order_id", id).Int64("number", numberOf(so)).Int("added", len(added)).Int("removed", len(removed)).
		Int64("refund_owed_cents", owed).Str("staff_id", st.ID).Str("device_id", a.DeviceID).Msg("Online order changed")
	return res, nil
}

// onlineReference is the online order's own number (the vendor's / the
// website's: source_event_id, the "#…" under the kitchen number), for the
// manager's refund.
func onlineReference(snap order.Snapshot) string { return snap.SourceEventID }

// sourceWord names an order's source on the slip.
func sourceWord(src string) string {
	switch strings.ToLower(src) {
	case "", order.SourceOnline:
		return "Online"
	case "web":
		return "Website"
	}
	return strings.ToUpper(src[:1]) + src[1:]
}

func lineName(it order.LineItem) string {
	if sz := strings.TrimSpace(it.Size); sz != "" && !strings.EqualFold(sz, "serving") {
		return sz + " " + it.Name
	}
	return it.Name
}
