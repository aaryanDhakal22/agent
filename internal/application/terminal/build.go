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
	// Rungs: each line's rung_cents / rung_name / rung_mods (same index;
	// rung.go reads and writes them on the lines).
	Rungs []Rung `json:"-"`
}

// Discount is a discount on the food: a percent OR an amount, with a
// reason. Kind "manual" (or none) is a typed discount: over the staff
// member's discount_pct it needs a manager's PIN. Kind "employee_meal" is the
// store's Employee meal: up to main's employee_meal_pct (default 50) without
// a manager, whoever is logged in (contract §2.10i).
type Discount struct {
	Pct         int    `json:"pct,omitempty"`
	AmountCents int64  `json:"amount_cents,omitempty"`
	Reason      string `json:"reason"`
	Kind        string `json:"kind,omitempty"`
}

// Discount kinds.
const (
	KindManual       = "manual"
	KindEmployeeMeal = "employee_meal"
)

// employeeMeal: the Employee meal preset.
func (d *Discount) employeeMeal() bool { return d != nil && d.Kind == KindEmployeeMeal }

func (d *Discount) check() error {
	if d == nil {
		return nil
	}
	switch d.Kind {
	case "", KindManual, KindEmployeeMeal:
	default:
		return fmt.Errorf("discount kind is manual or employee_meal")
	}
	switch {
	case (d.Pct > 0) == (d.AmountCents > 0):
		return fmt.Errorf("a discount is a pct (1-100) or an amount_cents, not both")
	case d.Pct < 0 || d.Pct > 100 || d.AmountCents < 0:
		return fmt.Errorf("discount pct is 1-100, amount_cents above 0")
	}
	if d.employeeMeal() && strings.TrimSpace(d.Reason) == "" {
		d.Reason = "Employee meal"
	}
	r := strings.TrimSpace(d.Reason)
	if r == "" || utf8.RuneCountInString(r) > 140 {
		return fmt.Errorf("a discount needs a reason (1-140 characters)")
	}
	return nil
}

// DiscountManual is Discount.type of the manual discount (and the Employee
// meal, told apart by its label) on a quote / order.
const DiscountManual = "manual"

// priced is a quote with the manual discount applied.
type priced struct {
	quote       pricing.Quote
	ctx         pricing.Context
	cart        pricing.Cart
	manualCents int64
	manualPct   int // the manual discount as a percent of the food (rounded up)
	// changes are the lines rung at another price than the menu's.
	changes []terminal.PriceChange
	// gone / kept: lines sold as custom lines because their item is gone,
	// and lines kept at their rung price (rung.go).
	gone []terminal.GoneLine
	kept []terminal.KeptPrice
	// modChanges: the cart options the terminal priced itself (batch A,
	// orderlines.go).
	modChanges []ModPriceChange
}

// priceCart prices a cart with the Pi's engine copy. typ: counter | pickup |
// delivery (a counter order prices as a pickup). The store takes both types
// at the counter whatever its online status, so Types is always open. A
// line's price_cents on a set-price item is an item price change
// (PriceOverrides: the engine prices it; who may make it is checkPrices').
// Counter: a missed required group is a quote warning, not an error, and a
// cart may carry custom lines; maxRepeat caps a double / triple topping
// (main's topping_max_count, Service.toppingMax).
func priceCart(menu *terminal.CounterMenu, tz *time.Location, typ string, c Cart, now time.Time, maxRepeat int) (priced, error) {
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
	ctx := counterCtx(menu, tz, maxRepeat)
	q := pricing.Price(ctx, pc, now)
	out := priced{quote: q, ctx: ctx, cart: pc, changes: priceChanges(pc, q)}
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
			switch {
			case d.employeeMeal() && d.Pct > 0:
				label = fmt.Sprintf("Employee meal %d%%", d.Pct)
			case d.employeeMeal():
				label = "Employee meal"
			case d.Pct > 0:
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

// toppingMax is main's topping_max_count (default 3): the engine's MaxRepeat.
func (s *Service) toppingMax() int {
	cfg, _ := s.store.Config()
	return cfg.ToppingMaxCount()
}

// customLineKey tells custom lines apart for the journal (name + price).
func customLineKey(name string, cents int64) string { return fmt.Sprintf("%s|%d", name, cents) }

// customEntries journals the cart's custom lines (Quicc Add) with who added
// them; one in `kept` (customLineKey → count: the order's earlier cart) was
// journaled already.
func customEntries(a Actor, st terminal.Staff, p priced, orderID string, kept map[string]int) []terminal.Entry {
	var out []terminal.Entry
	for i, l := range p.cart.Lines {
		if !l.Custom || i >= len(p.quote.Lines) || p.isGone(i) { // a gone line is menu_gone_line
			continue
		}
		ql := p.quote.Lines[i]
		if k := customLineKey(ql.Name, ql.UnitCents); kept[k] > 0 {
			kept[k]--
			continue
		}
		out = append(out, terminal.Entry{StaffID: st.ID, DeviceID: a.DeviceID, Action: terminal.ActionCustomLine, OrderID: orderID,
			Detail: detail(map[string]any{"line": i, "name": ql.Name, "price_cents": ql.UnitCents, "qty": ql.Qty})})
	}
	return out
}

// keptCustom are the custom lines on the order's stored cart (customLineKey → count).
func keptCustom(snap order.Snapshot) map[string]int {
	out := map[string]int{}
	for _, it := range snap.Items {
		if it.SKU == order.SKUCustom {
			out[customLineKey(it.Name, it.UnitPrice)]++
		}
	}
	return out
}

// priceChanges are the cart's lines whose price_cents replaced a set price
// (the engine's list_cents on the line).
func priceChanges(c pricing.Cart, q pricing.Quote) []terminal.PriceChange {
	var out []terminal.PriceChange
	for i, l := range c.Lines {
		if l.Custom || l.DealID != "" || l.PriceCents == nil || i >= len(q.Lines) || q.Lines[i].ListCents == nil {
			continue
		}
		from, to := *q.Lines[i].ListCents, *l.PriceCents
		out = append(out, terminal.PriceChange{Line: i, ItemID: l.ItemID, SizeID: l.SizeID, ChoiceID: l.ChoiceID, Name: q.Lines[i].Name,
			FromCents: from, ToCents: to, DropPct: terminal.DropPctOf(from, to), Comp: to == 0 && from > 0})
	}
	return out
}

// buildItems turns the priced cart into order lines, as main builds a
// channel order's (orderport build): one line per item with its options as
// children (portion / mode kept; a cart line with units in coupons: one line
// per price it sold at, pricing.SoldGroups), a bundle deal's picks as lines of their own
// ("Combo: <deal>") with the difference to the deal price as a discount (or
// fee), and each line's kitchen printer from the counter menu's routes
// (the line keeps the menu's item name: a route's name is its printer's).
func buildItems(menu *terminal.CounterMenu, p priced) (items []order.LineItem, off []order.Discount, fees []order.Fee, err error) {
	type built struct {
		item  order.LineItem
		route string
		// printer: a Quicc Add's printer_id (2026-11-14, #17): Set = the
		// cashier's pick decides (ID "" = no kitchen paper), not the route.
		printer pricing.LinePrinter
	}
	var lines []built
	drivers := driverIndexFor(menu) // show_on_driver_ticket stamps (driverticket.go)
	lineOf := func(r pricing.Resolved, qty int, note string, pos int) built {
		route := r.BankItem
		if route == "" {
			route = r.RateItem
		}
		li := order.LineItem{ID: uuid.Must(uuid.NewV7()).String(), Position: pos, Name: r.Name, Size: r.SizeName, Quantity: qty,
			UnitPrice: r.BaseCents, Notes: note}
		total := r.BaseCents * int64(qty)
		// One child per mod line (the owner, 2026-11-14: mods never stack; a
		// qualifier joins its pick: "Lite Fried Onions").
		for k, o := range pricing.ModLines(r.Options) {
			child := order.LineItem{ID: uuid.Must(uuid.NewV7()).String(), Position: k, Quantity: qty, Name: o.Label,
				Portion: o.Portion, Mode: o.Mode, UnitPrice: o.PriceCents * int64(o.Count)}
			if o.RemovedDefault {
				child.Quantity, child.UnitPrice = 1, 0
			}
			if o.Included {
				child.UnitPrice = 0
			}
			child.ShowOnDriverTicket = drivers.optionShow(route, o)
			child.LineTotal = child.UnitPrice * int64(child.Quantity)
			total += child.LineTotal
			li.Children = append(li.Children, child)
		}
		li.LineTotal = total
		return built{item: li, route: route}
	}
	firstOf := map[int]int{} // cart line → its first built line (a priced note's item)
	for i, l := range p.cart.Lines {
		qty := min(max(l.Qty, 1), 50)
		firstOf[i] = len(lines)
		if l.Custom && l.NoteOf != nil && i < len(p.quote.Lines) {
			// A priced note (§2.10an): its own line under its item, with the
			// item's routing (it prints with its item, on its station).
			if at, ok := firstOf[*l.NoteOf]; ok && at < len(lines) && *l.NoteOf < i {
				ql := p.quote.Lines[i]
				par := lines[at]
				lines = append(lines, built{item: order.LineItem{ID: uuid.Must(uuid.NewV7()).String(), Position: len(lines), SKU: order.SKUNote,
					Name: ql.Name, Quantity: qty, UnitPrice: ql.UnitCents, LineTotal: ql.TotalCents, ParentLineID: par.item.ID}, route: par.route})
				continue
			}
		}
		if l.Custom {
			// A custom line (Quicc Add): the quote's name and price, routed by
			// the store's Quick add item (main's Store › Menu: its printer, its
			// category's, or receipt only). Without one (an older main), and for
			// a line sold as custom because its item left the menu, no route:
			// it prints on every station (unmatched).
			if i >= len(p.quote.Lines) {
				continue
			}
			ql := p.quote.Lines[i]
			route := ""
			if menu != nil && !p.isGone(i) {
				route = menu.QuickAddItemID
			}
			lines = append(lines, built{item: order.LineItem{ID: uuid.Must(uuid.NewV7()).String(), Position: len(lines), SKU: order.SKUCustom,
				Name: ql.Name, Quantity: qty, UnitPrice: ql.UnitCents, LineTotal: ql.TotalCents, Notes: strings.TrimSpace(l.Note)}, route: route})
			if !p.isGone(i) {
				lines[len(lines)-1].printer = l.PrinterID // contract §2.1i / §2.10an: null = nowhere
			}
			continue
		}
		if l.DealID == "" {
			// A coupon re-prices its units (never a discount): one order
			// line per price the cart line sold at, the coupon's lines
			// carrying its name (receipts print it over them).
			r := pricing.Resolve(p.ctx, l.Selection, 0)
			for _, g := range pricing.SoldGroups(p.quote, i, qty) {
				if g.Coupon < 0 {
					lines = append(lines, lineOf(r, g.Qty, strings.TrimSpace(l.Note), len(lines)))
					continue
				}
				rc := r
				rc.Options = pricing.FreeOptions(r.Options, g.FreeMods)
				b := lineOf(rc, g.Qty, strings.TrimSpace(l.Note), len(lines))
				var mods int64
				for _, c := range b.item.Children {
					mods += c.UnitPrice
				}
				b.item.UnitPrice = g.UnitCents - mods
				b.item.LineTotal = g.UnitCents * int64(g.Qty)
				b.item.Coupon, b.item.CouponNo = g.Name, g.Coupon+1
				if i < len(p.quote.Lines) {
					b.item.RungCents = p.quote.Lines[i].UnitCents
				}
				lines = append(lines, b)
			}
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
		case b.printer.Set && b.printer.ID == "": // a Quicc Add sent nowhere: no kitchen paper (#17)
			it.MenuItemID, it.Routing = b.route, order.RoutingNone
		case b.printer.Set: // a Quicc Add on the cashier's printer, that one alone (#17)
			it.MenuItemID, it.PrinterID, it.Routing = b.route, b.printer.ID, order.RoutingMatched
		case b.route == "":
			it.Routing = order.RoutingUnmatched
		case ok && rt.NoKitchen:
			it.MenuItemID, it.Routing = b.route, order.RoutingNone // no kitchen ticket
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
