package terminal

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/pricing"
	"quiccpos/agent/internal/domain/terminal"

	"github.com/google/uuid"
)

// Cart is what a terminal prices / orders: the pricing engine's cart (its
// type is the order type's, see priceCart) plus a manual discount.
type Cart struct {
	pricing.Cart
	Discount *Discount `json:"discount,omitempty"`
}

// Discount is a manual discount on the food: a percent OR an amount, with a
// reason. Over the staff member's discount_pct it needs a manager's PIN.
type Discount struct {
	Pct         int    `json:"pct,omitempty"`
	AmountCents int64  `json:"amount_cents,omitempty"`
	Reason      string `json:"reason"`
}

func (d *Discount) check() error {
	if d == nil {
		return nil
	}
	switch {
	case (d.Pct > 0) == (d.AmountCents > 0):
		return fmt.Errorf("a discount is a pct (1-100) or an amount_cents, not both")
	case d.Pct < 0 || d.Pct > 100 || d.AmountCents < 0:
		return fmt.Errorf("discount pct is 1-100, amount_cents above 0")
	}
	r := strings.TrimSpace(d.Reason)
	if r == "" || utf8.RuneCountInString(r) > 140 {
		return fmt.Errorf("a discount needs a reason (1-140 characters)")
	}
	return nil
}

// DiscountManual is Discount.type of the manual discount on a quote / order.
const DiscountManual = "manual"

// priced is a quote with the manual discount applied.
type priced struct {
	quote       pricing.Quote
	ctx         pricing.Context
	cart        pricing.Cart
	manualCents int64
	manualPct   int // the manual discount as a percent of the food (rounded up)
}

// priceCart prices a cart with the Pi's engine copy. typ: counter | pickup |
// delivery (a counter order prices as a pickup). The store takes both types
// at the counter whatever its online status, so Types is always open.
func priceCart(menu *terminal.CounterMenu, tz *time.Location, typ string, c Cart, now time.Time) (priced, error) {
	if err := c.Discount.check(); err != nil {
		return priced{}, err
	}
	pc := c.Cart
	pc.Type = terminal.TypePickup
	if typ == terminal.TypeDelivery {
		pc.Type = terminal.TypeDelivery
	}
	if pc.Payment == "" {
		pc.Payment = "cash"
	}
	ctx := menu.Pricing
	ctx.Types = pricing.Types{Pickup: true, Delivery: true}
	if ctx.Timezone == "" && tz != nil {
		ctx.Timezone = tz.String()
	}
	q := pricing.Price(ctx, pc, now)
	out := priced{quote: q, ctx: ctx, cart: pc}
	if d := c.Discount; d != nil && len(q.Errors) == 0 {
		food := q.SubtotalCents
		for _, x := range q.Discounts {
			food -= x.AmountCents
		}
		food = max(food, 0)
		amt := d.AmountCents
		if d.Pct > 0 {
			amt = pricing.Pct(food, d.Pct*100)
		}
		amt = min(amt, food)
		if amt > 0 {
			// The tax follows the food: it drops in proportion (the engine
			// has no manual discount of its own).
			cut := int64(0)
			if food > 0 {
				cut = (q.TaxCents*amt + food/2) / food
			}
			label := fmt.Sprintf("Discount: %s", strings.TrimSpace(d.Reason))
			if d.Pct > 0 {
				label = fmt.Sprintf("Discount %d%%: %s", d.Pct, strings.TrimSpace(d.Reason))
			}
			q.Discounts = append(q.Discounts, pricing.Discount{Type: DiscountManual, Label: label, AmountCents: amt})
			q.TaxCents -= cut
			q.TotalCents -= amt + cut
			q.SavedCents += amt
			out.quote, out.manualCents = q, amt
			out.manualPct = d.Pct
			if d.Pct == 0 && food > 0 {
				out.manualPct = int((amt*100 + food - 1) / food)
			}
		}
	}
	return out, nil
}

// buildItems turns the priced cart into order lines, as main builds a
// channel order's (orderport build): one line per item with its options as
// children (portion / mode kept), a bundle deal's picks as lines of their own
// ("Combo: <deal>") with the difference to the deal price as a discount (or
// fee), and each line's kitchen printer from the counter menu's routes
// (the line keeps the menu's item name: a route's name is its printer's).
func buildItems(menu *terminal.CounterMenu, p priced) (items []order.LineItem, off []order.Discount, fees []order.Fee, err error) {
	type built struct {
		item  order.LineItem
		route string
	}
	var lines []built
	lineOf := func(r pricing.Resolved, qty int, note string, pos int) built {
		route := r.BankItem
		if route == "" {
			route = r.RateItem
		}
		li := order.LineItem{ID: uuid.Must(uuid.NewV7()).String(), Position: pos, Name: r.Name, Size: r.SizeName, Quantity: qty,
			UnitPrice: r.BaseCents, Notes: note}
		total := r.BaseCents * int64(qty)
		for k, o := range r.Options {
			child := order.LineItem{ID: uuid.Must(uuid.NewV7()).String(), Position: k, Quantity: qty, Name: o.Label,
				Portion: o.Portion, Mode: o.Mode, UnitPrice: o.PriceCents}
			if o.RemovedDefault {
				child.Quantity, child.UnitPrice = 1, 0
			}
			if o.Included {
				child.UnitPrice = 0
			}
			child.LineTotal = child.UnitPrice * int64(child.Quantity)
			total += child.LineTotal
			li.Children = append(li.Children, child)
		}
		li.LineTotal = total
		return built{item: li, route: route}
	}
	for i, l := range p.cart.Lines {
		qty := min(max(l.Qty, 1), 50)
		if l.DealID == "" {
			lines = append(lines, lineOf(pricing.Resolve(p.ctx, l.Selection, 0), qty, strings.TrimSpace(l.Note), len(lines)))
			continue
		}
		var deal *pricing.Deal
		for k := range p.ctx.Deals {
			if p.ctx.Deals[k].ID == l.DealID {
				deal = &p.ctx.Deals[k]
			}
		}
		if deal == nil {
			return nil, nil, nil, &terminal.CartInvalid{Issues: []pricing.Issue{{Code: pricing.IssueDealEnded, Message: "A deal in the order has ended."}}}
		}
		var picks int64
		for k, slot := range deal.Slots {
			if k >= len(l.Picks) {
				break
			}
			b := lineOf(pricing.Resolve(p.ctx, l.Picks[k], slot.IncludedOptions), qty, "Combo: "+deal.Name, len(lines))
			picks += b.item.LineTotal
			lines = append(lines, b)
		}
		if i < len(p.quote.Lines) {
			switch diff := picks - p.quote.Lines[i].TotalCents; {
			case diff > 0:
				off = append(off, order.Discount{Type: "deal", Label: "Combo: " + deal.Name, Amount: diff})
			case diff < 0:
				fees = append(fees, order.Fee{Type: "other", Label: "Combo: " + deal.Name, Amount: -diff})
			}
		}
	}
	items = make([]order.LineItem, len(lines))
	for i, b := range lines {
		it := b.item
		// main's routes leave out an item with no kitchen printer: a bank
		// item without one is unassigned (every station, like an online
		// line without a printer); only an added item is unmatched.
		switch rt, ok := menu.Routes[b.route]; {
		case b.route == "":
			it.Routing = order.RoutingUnmatched
		case !ok || rt.PrinterID == "":
			it.MenuItemID, it.Routing = b.route, order.RoutingUnassigned
		default:
			it.MenuItemID, it.PrinterID, it.Routing = b.route, rt.PrinterID, order.RoutingMatched
		}
		items[i] = it
	}
	return items, off, fees, nil
}

// moneyOf is the order's money from its lines and the quote (as main's).
// The delivery fee carries the driver's share (main's split of the store's
// delivery fee, pricing.DeliverySplit) for the driver reports.
func moneyOf(items []order.LineItem, p priced, off []order.Discount, fees []order.Fee) order.Money {
	q := p.quote
	var sub int64
	for _, it := range items {
		sub += it.LineTotal
	}
	m := order.Money{Subtotal: sub, Tax: q.TaxCents, Tip: q.TipCents, Total: q.TotalCents, Fees: fees, Discounts: off}
	for _, d := range q.Discounts {
		t, code := d.Type, ""
		if d.Type == "code" {
			t, code = "promo", strings.TrimPrefix(d.Label, "Code ")
		}
		m.Discounts = append(m.Discounts, order.Discount{Type: t, Code: code, Label: d.Label, Amount: d.AmountCents})
	}
	if q.DeliveryFeeCents > 0 {
		_, driver := pricing.DeliverySplit(p.ctx.Settings, q.DeliveryFeeCents)
		m.Fees = append(m.Fees, order.Fee{Type: "delivery", Label: "Delivery fee", Amount: q.DeliveryFeeCents, DriverCents: driver})
	}
	if q.ServiceFeeCents > 0 {
		m.Fees = append(m.Fees, order.Fee{Type: "service", Label: "Online fee", Amount: q.ServiceFeeCents})
	}
	if q.CardFeeCents > 0 {
		m.Fees = append(m.Fees, order.Fee{Type: "card", Label: "Card fee", Amount: q.CardFeeCents})
	}
	return m
}
