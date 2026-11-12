package terminal

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	"quiccpos/agent/internal/domain/order"
)

// The Z report (contract/agent.md §2.10l, TERMINAL-PLAN "End of day": one
// close for everything). Built by the Pi from a closed batch - its orders,
// the terminals' payments, the drawer sessions, the journal and the driver
// reports - kept on the Pi, sent to main as z.report, never printed.
//
// Money identity (the reconciliation, checked by BuildZ and the property
// test):
//
//	tenders.collected - refunds + general_refunds + unpaid = sales.total + tips_on_payments + on_cancelled
//
// where the tenders are the terminals' (cash, card, square_app, manual_card,
// comp: amount + tip as taken), "online" (what the order arrived paid with:
// total - its cash due) and "driver_cash" (the cash due a driver collected
// on a delivered order), unpaid is the cash still due on kept orders at the
// close, and on_cancelled is the net the terminals hold on cancelled /
// voided orders (amounts only: their tips are in tips_on_payments; 0 when
// every void refunded). general_refunds = the cash given back with no order
// (contract 2.10x): in refunds (and the cash tender's refunded) like any
// refund, added back on the left because no order's money holds it. And
// for the cash:
//
//	sum over drawers (expected - start - pay-ins + pay-outs) + cash outside drawers = the cash tender's net

// ZVersion is the Z report's shape version.
const ZVersion = 1

// Z tenders besides the terminal ones.
const (
	TenderOnline     = "online"
	TenderDriverCash = "driver_cash"
)

// ZTenderOrder is the order tenders are listed in.
var ZTenderOrder = []string{TenderCash, TenderCard, TenderSquareApp, TenderManualCard, TenderComp, TenderOnline, TenderDriverCash}

// Z is the Z report.
type Z struct {
	ID           string     `json:"id"`
	Version      int        `json:"version"`
	BatchID      int64      `json:"batch_id"`
	BusinessDate string     `json:"business_date"` // the store-local date the batch opened
	Timezone     string     `json:"timezone"`
	OpenedAt     time.Time  `json:"opened_at"`
	ClosedAt     time.Time  `json:"closed_at"`
	CloseKind    string     `json:"close_kind"` // manual | auto
	Forced       bool       `json:"forced"`
	FirstOrderAt *time.Time `json:"first_order_at"`
	LastOrderAt  *time.Time `json:"last_order_at"`
	Counts       ZCounts    `json:"counts"`
	Sales        ZSales     `json:"sales"`
	Tenders      []ZTender  `json:"tenders"`
	Unpaid       ZAmount    `json:"unpaid"`
	// Flagged (M-9): the money identity doesn't balance - stored as it is,
	// the owner paged, "Needs attention" on the Pi page.
	Flagged bool `json:"flagged"`
	// EarlierChecks: money (amounts, net) taken in this batch on checks an
	// earlier close carried (contract 2.10s §4).
	EarlierChecks ZAmount `json:"earlier_checks"`
	// Waste: Void waste (contract 2.10s §5), from the batch's journal.
	Waste         ZWaste     `json:"waste"`
	Tips          ZTips      `json:"tips"`
	Discounts     ZDiscounts `json:"discounts"`
	Refunds       ZAmount    `json:"refunds"`
	Voids         ZVoids     `json:"voids"`
	PriceChanges  ZPrices    `json:"price_changes"`
	Drivers       []ZDriver  `json:"drivers"`
	DriversCash   int64      `json:"drivers_cash_cents"`
	Drawers       []ZDrawer  `json:"drawers"`
	OverShort     int64      `json:"over_short_cents"` // sum over the counted drawers
	CashOutside   int64      `json:"cash_outside_drawers_cents"`
	NoSales       int64      `json:"no_sales"`
	Reconcile     ZReconcile `json:"reconcile"`
	GeneratedAt   time.Time  `json:"generated_at"`
	POSChecksLeft int64      `json:"pos_checks"` // always 0 since H3 (the old POS's checks; kept for the shape)
	// GeneralRefunds: the part of Refunds given back with no order
	// (contract 2.10x, "General refund").
	GeneralRefunds ZAmount `json:"general_refunds"`
}

// ZCounts are order counts by type (counter / pickup / delivery are the
// terminals' orders; online = every other source).
type ZCounts struct {
	Orders         int64 `json:"orders"` // kept (not cancelled / voided)
	Counter        int64 `json:"counter"`
	Pickup         int64 `json:"pickup"`
	Delivery       int64 `json:"delivery"`
	Online         int64 `json:"online"`
	OnlinePickup   int64 `json:"online_pickup"`
	OnlineDelivery int64 `json:"online_delivery"`
	Voided         int64 `json:"voided"`
	Cancelled      int64 `json:"cancelled"`
}

// ZSales are the kept orders' money.
type ZSales struct {
	ItemsCents       int64 `json:"items_cents"` // subtotals
	DiscountsCents   int64 `json:"discounts_cents"`
	FeesCents        int64 `json:"fees_cents"`
	TaxCents         int64 `json:"tax_cents"`
	TipsInTotalCents int64 `json:"tips_in_total_cents"` // checkout tips inside order totals (online)
	TotalCents       int64 `json:"total_cents"`
}

// ZTender is one tender's money.
type ZTender struct {
	Tender         string `json:"tender"`
	Count          int64  `json:"count"`
	CollectedCents int64  `json:"collected_cents"` // amount + tip taken
	RefundedCents  int64  `json:"refunded_cents"`  // amount + tip given back
	TipsCents      int64  `json:"tips_cents"`      // net tips on it
	NetCents       int64  `json:"net_cents"`       // collected - refunded
}

// ZAmount is a count and cents.
type ZAmount struct {
	Count int64 `json:"count"`
	Cents int64 `json:"cents"`
}

// ZWaste is the Waste section (contract 2.10s §5): orders thrown away
// unpaid (Void waste), by reason; cents = their totals.
type ZWaste struct {
	Count   int64          `json:"count"`
	Cents   int64          `json:"cents"`
	Reasons []ZWasteReason `json:"reasons"`
}

// ZWasteReason is one waste reason's count and total.
type ZWasteReason struct {
	Reason string `json:"reason"`
	Count  int64  `json:"count"`
	Cents  int64  `json:"cents"`
}

// WasteOf sums the journal's void.waste rows (in their order of reasons'
// first appearance).
func WasteOf(journal []Entry) ZWaste {
	w := ZWaste{Reasons: []ZWasteReason{}}
	idx := map[string]int{}
	for _, e := range journal {
		if e.Action != ActionVoidWaste {
			continue
		}
		var d struct {
			Reason string `json:"reason"`
			Total  int64  `json:"total_cents"`
		}
		_ = json.Unmarshal(e.Detail, &d)
		i, ok := idx[d.Reason]
		if !ok {
			i = len(w.Reasons)
			idx[d.Reason] = i
			w.Reasons = append(w.Reasons, ZWasteReason{Reason: d.Reason})
		}
		w.Reasons[i].Count++
		w.Reasons[i].Cents += d.Total
		w.Count++
		w.Cents += d.Total
	}
	return w
}

// ZTips are the tips by where they came from.
type ZTips struct {
	CardCents        int64 `json:"card_cents"`         // on terminal card tenders (card, square_app, manual_card)
	CashCents        int64 `json:"cash_cents"`         // taken in cash at a terminal
	OnlineCents      int64 `json:"online_cents"`       // checkout tips on online orders (in their totals)
	AddedCents       int64 `json:"added_cents"`        // ADD TIP on website card orders after the order (charged by main)
	DriversOwedCents int64 `json:"drivers_owed_cents"` // card tips the store owes its drivers (driver reports)
}

// ZDiscounts are the kept orders' discounts by kind.
type ZDiscounts struct {
	Typed        ZAmount `json:"typed"`         // a cashier's typed discount
	EmployeeMeal ZAmount `json:"employee_meal"` // the Employee meal preset
	Deals        ZAmount `json:"deals"`         // automatic deals + codes
	Other        ZAmount `json:"other"`
}

// ZVoids are the voided terminal orders.
type ZVoids struct {
	Count         int64 `json:"count"`
	Cents         int64 `json:"cents"`          // their totals
	RefundedCents int64 `json:"refunded_cents"` // given back by the voids
}

// ZPrices are the item price changes and comp lines rung (journal).
type ZPrices struct {
	Count        int64 `json:"count"`
	LoweredCents int64 `json:"lowered_cents"`
	RaisedCents  int64 `json:"raised_cents"`
	Comps        int64 `json:"comps"`
	CompCents    int64 `json:"comp_cents"`
}

// ZDriver is one driver's cash (from his driver report).
type ZDriver struct {
	DriverID         string `json:"driver_id"`
	Name             string `json:"name"`
	Deliveries       int    `json:"deliveries"`
	CashCents        int64  `json:"cash_cents"`
	TipCents         int64  `json:"tip_cents"`
	DriverShareCents int64  `json:"driver_share_cents"`
	OwedCents        int64  `json:"owed_cents"`
}

// ZDrawer is one drawer session.
type ZDrawer struct {
	ID             string `json:"id"`
	DeviceName     string `json:"device_name"`
	StaffID        string `json:"staff_id"`
	StaffName      string `json:"staff_name"`
	StartCents     int64  `json:"start_cents"`
	CashSalesCents int64  `json:"cash_sales_cents"`
	CashTipsCents  int64  `json:"cash_tips_cents"`
	CashRefunds    int64  `json:"cash_refunds_cents"`
	PayInsCents    int64  `json:"pay_ins_cents"`
	PayOutsCents   int64  `json:"pay_outs_cents"`
	ExpectedCents  int64  `json:"expected_cents"`
	CountedCents   *int64 `json:"counted_cents"`
	OverShortCents *int64 `json:"over_short_cents"`
	Flagged        bool   `json:"flagged"`
	CloseReason    string `json:"close_reason"`
	NoSales        int64  `json:"no_sales"`
}

// ZReconcile is the money identity's two sides (difference 0 = balanced).
type ZReconcile struct {
	TendersCents       int64 `json:"tenders_cents"` // every tender's collected
	RefundsCents       int64 `json:"refunds_cents"`
	UnpaidCents        int64 `json:"unpaid_cents"`
	SalesTotalCents    int64 `json:"sales_total_cents"`
	TipsCents          int64 `json:"tips_cents"`           // net tips taken with terminal payments
	OnCancelledCents   int64 `json:"on_cancelled_cents"`   // terminal amounts (not tips) left on cancelled / voided orders
	EarlierChecksCents int64 `json:"earlier_checks_cents"` // amounts taken here on earlier batches' carried checks
	DifferenceCents    int64 `json:"difference_cents"`     // left side - right side
	DrawersCashCents   int64 `json:"drawers_cash_cents"`   // the drawers' cash flows + cash outside them
	DrawersDifferences int64 `json:"drawers_difference_cents"`
	// GeneralRefundsCents: cash given back with no order (in RefundsCents
	// too; added back on the left side).
	GeneralRefundsCents int64 `json:"general_refunds_cents"`
}

// ZPending is a Z report Close batch queued (built right after).
type ZPending struct {
	BatchID   int64
	ID        string
	CloseKind string
	Forced    bool
}

// ZInput is what the Z report is built from.
type ZInput struct {
	ID        string
	BatchID   int64
	OpenedAt  time.Time
	ClosedAt  time.Time
	Loc       *time.Location
	CloseKind string
	Forced    bool
	Orders    []order.StoredOrder
	Payments  []Payment // the terminal payments taken in the batch
	// Earlier: the earlier batches' (carried) checks some of Payments are
	// on (contract 2.10s §4): their money counts here as earlier_checks,
	// never as a sale.
	Earlier []order.StoredOrder
	Drawers []Drawer // the batch's sessions (closed)
	Journal []Entry  // the batch's journal rows
	Drivers []order.DriverReport
	Now     time.Time
	// Open: the batch is still open (ClosedAt is then the read time).
	Open bool
}

func isVoided(so order.StoredOrder) bool {
	return so.Order.Source == order.SourceCounter && so.Status.State == order.StateCancelled && so.Status.CancelReason == VoidReason
}

// BuildZ builds the Z report (pure).
func BuildZ(in ZInput) Z {
	loc := in.Loc
	if loc == nil {
		loc = time.UTC
	}
	z := Z{ID: in.ID, Version: ZVersion, BatchID: in.BatchID, BusinessDate: in.OpenedAt.In(loc).Format("2006-01-02"),
		Timezone: loc.String(), OpenedAt: in.OpenedAt.UTC(), ClosedAt: in.ClosedAt.UTC(), CloseKind: in.CloseKind, Forced: in.Forced,
		GeneratedAt: in.Now.UTC(), Tenders: []ZTender{}, Drivers: []ZDriver{}, Drawers: []ZDrawer{}}
	if z.CloseKind == "" {
		z.CloseKind = order.CloseManual
	}
	byOrder := map[string][]Payment{}
	tenders := map[string]*ZTender{}
	tender := func(t string) *ZTender {
		if tenders[t] == nil {
			tenders[t] = &ZTender{Tender: t}
		}
		return tenders[t]
	}
	held := map[string]bool{}
	for _, so := range in.Orders {
		held[so.Order.ID] = true
	}
	earlier := map[string]bool{}
	for _, so := range in.Earlier {
		earlier[so.Order.ID] = true
	}
	for _, p := range in.Payments {
		if IsGeneralRefund(p) {
			// No order (contract 2.10x): a refund on its tender, cash out of
			// its drawer, nothing else (never a tip).
			cents := p.AmountCents + p.TipCents
			t := tender(p.Tender)
			t.RefundedCents += cents
			z.Refunds.Count++
			z.Refunds.Cents += cents
			z.GeneralRefunds.Count++
			z.GeneralRefunds.Cents += cents
			z.Reconcile.GeneralRefundsCents += cents
			if p.Tender == TenderCash {
				if p.DrawerID == "" {
					z.CashOutside -= cents
				}
			}
			continue
		}
		if !held[p.OrderID] && !earlier[p.OrderID] {
			continue // an order the Pi no longer holds (never on a fresh close)
		}
		if earlier[p.OrderID] {
			a, _ := p.Signed()
			z.EarlierChecks.Cents += a
			if !p.Refund && p.AmountCents > 0 {
				z.EarlierChecks.Count++
			}
			z.Reconcile.EarlierChecksCents += a
		} else {
			byOrder[p.OrderID] = append(byOrder[p.OrderID], p)
		}
		t := tender(p.Tender)
		a, tip := p.Signed()
		if p.Refund {
			t.RefundedCents += p.AmountCents + p.TipCents
			z.Refunds.Count++
			z.Refunds.Cents += p.AmountCents + p.TipCents
		} else {
			if p.AmountCents > 0 { // a tip added afterwards (amount 0) is no new payment
				t.Count++
			}
			t.CollectedCents += p.AmountCents + p.TipCents
		}
		t.TipsCents += tip
		switch p.Tender {
		case TenderCash:
			z.Tips.CashCents += tip
			if p.DrawerID == "" {
				z.CashOutside += a + tip
			}
		case TenderCard, TenderSquareApp, TenderManualCard:
			z.Tips.CardCents += tip
		}
		z.Reconcile.TipsCents += tip
	}
	for _, so := range in.Orders {
		snap := so.Order
		counter := snap.Source == order.SourceCounter
		var netAmt, netTip int64
		for _, p := range byOrder[snap.ID] {
			a, t := p.Signed()
			netAmt, netTip = netAmt+a, netTip+t
		}
		if so.Status.State == order.StateCancelled {
			if isVoided(so) {
				z.Counts.Voided++
				z.Voids.Count++
				z.Voids.Cents += snap.Money.Total
				for _, p := range byOrder[snap.ID] {
					if p.Refund {
						z.Voids.RefundedCents += p.AmountCents + p.TipCents
					}
				}
			} else {
				z.Counts.Cancelled++
			}
			z.Reconcile.OnCancelledCents += netAmt // its tips are in TipsCents already
			_ = netTip
			continue
		}
		at := snap.ReceivedAt
		if at.IsZero() {
			at = snap.PlacedAt
		}
		if !at.IsZero() {
			a := at.UTC()
			if z.FirstOrderAt == nil || a.Before(*z.FirstOrderAt) {
				z.FirstOrderAt = &a
			}
			if z.LastOrderAt == nil || a.After(*z.LastOrderAt) {
				a2 := a
				z.LastOrderAt = &a2
			}
		}
		z.Counts.Orders++
		delivery := so.IsDelivery()
		switch {
		case counter && delivery:
			z.Counts.Delivery++
		case counter && snap.Fulfillment.Type == "pickup":
			z.Counts.Pickup++
		case counter:
			z.Counts.Counter++
		default:
			z.Counts.Online++
			if delivery {
				z.Counts.OnlineDelivery++
			} else {
				z.Counts.OnlinePickup++
			}
		}
		m := snap.Money
		z.Sales.ItemsCents += m.Subtotal
		z.Sales.TaxCents += m.Tax
		z.Sales.TotalCents += m.Total
		for _, f := range m.Fees {
			z.Sales.FeesCents += f.Amount
		}
		for _, d := range m.Discounts {
			z.Sales.DiscountsCents += d.Amount
			bucket := &z.Discounts.Other
			switch {
			case d.Type == "manual" && strings.HasPrefix(d.Label, "Employee meal"):
				bucket = &z.Discounts.EmployeeMeal
			case d.Type == "manual":
				bucket = &z.Discounts.Typed
			case d.Type == "deal" || d.Type == "code":
				bucket = &z.Discounts.Deals
			}
			bucket.Count++
			bucket.Cents += d.Amount
		}
		// What the order owed in cash (counter: its total) and what it
		// arrived paid with (online: the rest).
		baseDue := m.Total
		if !counter {
			z.Sales.TipsInTotalCents += m.Tip
			z.Tips.OnlineCents += m.Tip
			z.Tips.AddedCents += so.Status.AddedTipCents
			baseDue = snap.CashDue()
			if pre := m.Total - baseDue; pre != 0 {
				t := tender(TenderOnline)
				t.Count++
				t.CollectedCents += pre
			}
		}
		left := baseDue - netAmt
		if left != 0 {
			if delivery && so.Status.State == order.StateDelivered && left > 0 {
				t := tender(TenderDriverCash)
				t.Count++
				t.CollectedCents += left
			} else {
				z.Unpaid.Count++
				z.Unpaid.Cents += left
			}
		}
	}
	for _, name := range ZTenderOrder {
		if t := tenders[name]; t != nil {
			t.NetCents = t.CollectedCents - t.RefundedCents
			z.Tenders = append(z.Tenders, *t)
			delete(tenders, name)
		}
	}
	var rest []string
	for name := range tenders {
		rest = append(rest, name)
	}
	sort.Strings(rest)
	for _, name := range rest {
		t := tenders[name]
		t.NetCents = t.CollectedCents - t.RefundedCents
		z.Tenders = append(z.Tenders, *t)
	}
	for _, t := range z.Tenders {
		z.Reconcile.TendersCents += t.CollectedCents
		z.Reconcile.RefundsCents += t.RefundedCents
	}
	z.Reconcile.UnpaidCents = z.Unpaid.Cents
	z.Reconcile.SalesTotalCents = z.Sales.TotalCents
	z.Reconcile.DifferenceCents = z.Reconcile.TendersCents - z.Reconcile.RefundsCents + z.Reconcile.GeneralRefundsCents + z.Reconcile.UnpaidCents -
		(z.Reconcile.SalesTotalCents + z.Reconcile.TipsCents + z.Reconcile.OnCancelledCents + z.Reconcile.EarlierChecksCents)

	z.Waste = WasteOf(in.Journal)

	// Price changes and comp lines (journal: price_change / comp rows).
	for _, e := range in.Journal {
		switch e.Action {
		case ActionPriceChange, ActionComp:
			var d struct {
				From int64 `json:"from_cents"`
				To   int64 `json:"to_cents"`
			}
			_ = json.Unmarshal(e.Detail, &d)
			if e.Action == ActionComp {
				z.PriceChanges.Comps++
				z.PriceChanges.CompCents += d.From
				continue
			}
			z.PriceChanges.Count++
			if d.To < d.From {
				z.PriceChanges.LoweredCents += d.From - d.To
			} else {
				z.PriceChanges.RaisedCents += d.To - d.From
			}
		case ActionNoSale:
			z.NoSales++
		}
	}

	// Drivers' cash and tips (the driver reports).
	for _, r := range in.Drivers {
		z.Drivers = append(z.Drivers, ZDriver{DriverID: r.DriverID, Name: r.DriverName, Deliveries: r.Deliveries, CashCents: r.CashCents,
			TipCents: r.TipCents, DriverShareCents: r.DriverShareCents, OwedCents: r.OwedCents})
		z.DriversCash += r.CashCents
		z.Tips.DriversOwedCents += r.TipCents
	}

	// Drawers.
	var drawerCash int64
	for _, d := range in.Drawers {
		zd := ZDrawer{ID: d.ID, DeviceName: d.DeviceName, StaffID: d.StaffID, StaffName: d.StaffName, StartCents: d.StartCents,
			CashSalesCents: d.CashSalesCents, CashTipsCents: d.CashTipsCents, CashRefunds: d.CashRefundsCents, PayInsCents: d.PayInsCents,
			PayOutsCents: d.PayOutsCents, ExpectedCents: d.ExpectedCents, CountedCents: d.CountedCents, OverShortCents: d.OverShortCents,
			Flagged: d.Flagged, CloseReason: d.CloseReason, NoSales: d.NoSales}
		if d.OverShortCents != nil {
			z.OverShort += *d.OverShortCents
		}
		drawerCash += d.ExpectedCents - d.StartCents - d.PayInsCents + d.PayOutsCents
		z.Drawers = append(z.Drawers, zd)
	}
	var cashNet int64
	for _, t := range z.Tenders {
		if t.Tender == TenderCash {
			cashNet = t.NetCents
		}
	}
	z.Reconcile.DrawersCashCents = drawerCash + z.CashOutside
	z.Reconcile.DrawersDifferences = z.Reconcile.DrawersCashCents - cashNet
	return z
}
