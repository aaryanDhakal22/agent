package terminal

import (
	"sort"
	"strings"
	"time"

	"quiccpos/agent/internal/domain/menuview"
	"quiccpos/agent/internal/domain/order"
)

// The till report (contract/agent.md §2.10m, the owner's 2026-11-12: "like
// SoftTouch's TILL REPORT"): for one drawer session or a whole batch, the
// header facts, a SUMMARY and a DETAIL. Built by the Pi on request (never
// stored), laid out and printed by the terminal's Manager tab.
//
// Money identity (checked by the property test):
//
//	total_collected + unpaid = gross_receipts
//	gross_receipts = net_sales + service_charges + delivery_fees + tax + tips_collected + gratuity + net_payments_applied
//	net_sales = gross_sales + adjustments + refunds   (adjustments, refunds negative)
//
// Per sale counted: its cash due less what was taken here (net) is short;
// that shortfall is the sale's refunds as far as they reach, the rest is
// unpaid (or the driver's cash on a delivered delivery). Money taken here
// for a sale not counted here is net payments applied.

// TillVersion is the till report's shape version.
const TillVersion = 1

// Till scopes.
const (
	TillScopeDrawer = "drawer"
	TillScopeBatch  = "batch"
)

// Labels on the till report.
const (
	CategoryOpenItems = "Open items"
	CategoryOther     = "Other"
)

var brandLabels = map[string]string{BrandAmex: "Amex", BrandDiscover: "Discover", BrandMastercard: "Mastercard",
	BrandVisa: "Visa", BrandOther: "Credit Other"}

var otherLabels = map[string]string{TenderComp: "Comp", TenderOnline: "Paid online", TenderDriverCash: "Driver cash"}

// otherOrder is the order of the till's other tenders.
var otherOrder = []string{TenderComp, TenderOnline, TenderDriverCash}

// Till is the till report.
type Till struct {
	Version      int          `json:"version"`
	Scope        string       `json:"scope"`
	State        string       `json:"state"` // open | closed
	Store        string       `json:"store"`
	GeneratedAt  time.Time    `json:"generated_at"`
	Timezone     string       `json:"timezone"`
	BusinessDate string       `json:"business_date"`
	From         time.Time    `json:"from"`
	To           time.Time    `json:"to"`
	Batch        TillBatchRef `json:"batch"`
	Drawer       *TillDrawer  `json:"drawer"`
	Summary      TillSummary  `json:"summary"`
	Detail       TillDetail   `json:"detail"`
}

// TillBatchRef is the batch the report belongs to.
type TillBatchRef struct {
	ID       int64      `json:"id"`
	OpenedAt time.Time  `json:"opened_at"`
	ClosedAt *time.Time `json:"closed_at"`
	State    string     `json:"state"`
}

// TillDrawer is the drawer session of a drawer-scope report.
type TillDrawer struct {
	ID             string     `json:"id"`
	DeviceID       string     `json:"device_id"`
	DeviceName     string     `json:"device_name"`
	StaffID        string     `json:"staff_id"`
	StaffName      string     `json:"staff_name"`
	OpenedAt       time.Time  `json:"opened_at"`
	ClosedAt       *time.Time `json:"closed_at"`
	State          string     `json:"state"`
	CloseReason    string     `json:"close_reason"`
	StartCents     int64      `json:"start_cents"`
	ExpectedCents  int64      `json:"expected_cents"`
	CountedCents   *int64     `json:"counted_cents"`
	OverShortCents *int64     `json:"over_short_cents"`
	Flagged        bool       `json:"flagged"`
	NoSales        int64      `json:"no_sales"`
}

// TillSummary is the SUMMARY section.
type TillSummary struct {
	GrossSales         int64 `json:"gross_sales_cents"`
	Adjustments        int64 `json:"adjustments_cents"`
	Refunds            int64 `json:"refunds_cents"`
	NetSales           int64 `json:"net_sales_cents"`
	ServiceCharges     int64 `json:"service_charges_cents"`
	DeliveryFees       int64 `json:"delivery_fees_cents"`
	Tax                int64 `json:"tax_cents"`
	TipsCollected      int64 `json:"tips_collected_cents"`
	TipsPaid           int64 `json:"tips_paid_cents"`
	Gratuity           int64 `json:"gratuity_cents"`
	NetPaymentsApplied int64 `json:"net_payments_applied_cents"`
	GrossReceipts      int64 `json:"gross_receipts_cents"`
	TotalCash          int64 `json:"total_cash_cents"`
	TotalCreditCard    int64 `json:"total_credit_card_cents"`
	TotalOther         int64 `json:"total_other_cents"`
	TotalCollected     int64 `json:"total_collected_cents"`
	Unpaid             int64 `json:"unpaid_cents"`
	PaidInOut          int64 `json:"paid_in_out_cents"`
	TransfersDrops     int64 `json:"transfers_drops_cents"`
}

// TillCount is a count and an amount.
type TillCount struct {
	Count  int64 `json:"count"`
	Amount int64 `json:"amount_cents"`
}

// TillCategory is one category's gross sales.
type TillCategory struct {
	Name   string `json:"name"`
	Count  int64  `json:"count"`
	Amount int64  `json:"amount_cents"`
}

// TillAdjustment is one discount label's total (negative).
type TillAdjustment struct {
	Kind   string `json:"kind"` // typed | employee_meal | deals | other
	Label  string `json:"label"`
	Count  int64  `json:"count"`
	Amount int64  `json:"amount_cents"`
}

// TillCard is one card brand's money.
type TillCard struct {
	Brand  string `json:"brand"`
	Label  string `json:"label"`
	Count  int64  `json:"count"`
	Amount int64  `json:"amount_cents"`
}

// TillOther is one other tender's money (comp, online, driver cash).
type TillOther struct {
	Tender string `json:"tender"`
	Label  string `json:"label"`
	Count  int64  `json:"count"`
	Amount int64  `json:"amount_cents"`
}

// TillEmployee is one staff member who took money.
type TillEmployee struct {
	StaffID string `json:"staff_id"`
	Name    string `json:"name"`
	Count   int64  `json:"count"`
	Amount  int64  `json:"amount_cents"`
}

// TillDetail is the DETAIL section.
type TillDetail struct {
	Categories      []TillCategory   `json:"categories"`
	CategoriesTotal TillCount        `json:"categories_total"`
	Adjustments     []TillAdjustment `json:"adjustments"`
	Cash            TillCount        `json:"cash"`
	Cards           []TillCard       `json:"cards"`
	CardsTotal      TillCount        `json:"cards_total"`
	Other           []TillOther      `json:"other"`
	TotalCollected  int64            `json:"total_collected_cents"`
	PayIns          int64            `json:"pay_ins_cents"`
	PayOuts         int64            `json:"pay_outs_cents"`
	Employees       []TillEmployee   `json:"employees"`
}

// CategoryIndex maps a line's menu item id to its category on the counter
// menu (the counter view's categories, in their order).
type CategoryIndex struct {
	names []string
	of    map[string]int
}

// NewCategoryIndex indexes the counter view's categories: a view item's id
// and its anchor's bank items.
func NewCategoryIndex(cats []menuview.BuildCategory) *CategoryIndex {
	ix := &CategoryIndex{of: map[string]int{}}
	sorted := append([]menuview.BuildCategory(nil), cats...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Position < sorted[j].Position })
	for _, c := range sorted {
		n := len(ix.names)
		ix.names = append(ix.names, c.Name)
		for _, it := range c.Items {
			if _, ok := ix.of[it.ID]; !ok && it.ID != "" {
				ix.of[it.ID] = n
			}
			for _, id := range it.Anchor.ItemIDs {
				if _, ok := ix.of[id.String()]; !ok {
					ix.of[id.String()] = n
				}
			}
		}
	}
	return ix
}

// TillInput is what a till report is built from: the batch's orders,
// terminal payments, drawers and journal (ZInput), and the scope.
type TillInput struct {
	Z          ZInput
	Store      string
	Drawer     *Drawer // nil = the whole batch
	Categories *CategoryIndex
	StaffNames map[string]string // staff id -> name ("Cara S.")
}

// discountKind is the Z report's bucket of a discount.
func discountKind(d order.Discount) string {
	switch {
	case d.Type == "manual" && strings.HasPrefix(d.Label, "Employee meal"):
		return "employee_meal"
	case d.Type == "manual":
		return "typed"
	case d.Type == "deal" || d.Type == "code":
		return "deals"
	}
	return "other"
}

// BuildTill builds the till report (pure).
func BuildTill(in TillInput) Till {
	z := in.Z
	loc := z.Loc
	if loc == nil {
		loc = time.UTC
	}
	t := Till{Version: TillVersion, Scope: TillScopeBatch, State: "closed", Store: in.Store, GeneratedAt: z.Now.UTC(),
		Timezone: loc.String(), BusinessDate: z.OpenedAt.In(loc).Format("2006-01-02"), From: z.OpenedAt.UTC(), To: z.ClosedAt.UTC(),
		Batch:  TillBatchRef{ID: z.BatchID, OpenedAt: z.OpenedAt.UTC(), State: "closed"},
		Detail: TillDetail{Categories: []TillCategory{}, Adjustments: []TillAdjustment{}, Other: []TillOther{}, Employees: []TillEmployee{}}}
	if z.Open {
		t.State, t.Batch.State = "open", "open"
		t.To = z.Now.UTC()
	} else {
		c := z.ClosedAt.UTC()
		t.Batch.ClosedAt = &c
	}

	// The scope: which payments, which sales.
	inPay := func(p Payment) bool { return true }
	sales := map[string]bool{}
	if d := in.Drawer; d != nil {
		t.Scope = TillScopeDrawer
		from, to := d.OpenedAt, z.Now
		if d.ClosedAt != nil {
			to = *d.ClosedAt
		}
		t.From, t.To = from.UTC(), to.UTC()
		t.State = d.State
		td := &TillDrawer{ID: d.ID, DeviceID: d.DeviceID, DeviceName: d.DeviceName, StaffID: d.StaffID, StaffName: d.StaffName,
			OpenedAt: d.OpenedAt.UTC(), State: d.State, CloseReason: d.CloseReason, StartCents: d.StartCents, ExpectedCents: d.ExpectedCents,
			CountedCents: d.CountedCents, OverShortCents: d.OverShortCents, Flagged: d.Flagged, NoSales: d.NoSales}
		if d.ClosedAt != nil {
			c := d.ClosedAt.UTC()
			td.ClosedAt = &c
		}
		t.Drawer = td
		within := func(at time.Time) bool { return !at.Before(from) && !at.After(to) }
		inPay = func(p Payment) bool {
			if p.DrawerID != "" {
				return p.DrawerID == d.ID
			}
			return p.Tender != TenderCash && p.DeviceID == d.DeviceID && within(p.At)
		}
		for _, e := range z.Journal {
			if e.Action == ActionOrderPlaced && e.DeviceID == d.DeviceID && e.OrderID != "" && within(e.At) {
				sales[e.OrderID] = true
			}
		}
		t.Detail.PayIns, t.Detail.PayOuts = d.PayInsCents, d.PayOutsCents
	} else {
		for _, d := range z.Drawers {
			t.Detail.PayIns += d.PayInsCents
			t.Detail.PayOuts += d.PayOutsCents
		}
		for _, r := range z.Drivers {
			t.Summary.TipsPaid += r.TipCents
		}
	}

	held := map[string]order.StoredOrder{}
	for _, so := range z.Orders {
		held[so.Order.ID] = so
	}
	isSale := func(so order.StoredOrder) bool {
		if so.Status.State == order.StateCancelled {
			return false
		}
		if in.Drawer != nil {
			return sales[so.Order.ID]
		}
		return true
	}

	// Money taken here: by tender / brand / employee, and per order.
	var cash TillCount
	cards := map[string]*TillCount{}
	other := map[string]*TillCount{}
	emp := map[string]*TillEmployee{}
	type taken struct{ paid, refunded int64 } // amounts only (tips apart)
	byOrder := map[string]*taken{}
	for _, p := range z.Payments {
		so, ok := held[p.OrderID]
		if !ok || !inPay(p) {
			continue
		}
		a, tip := p.Signed()
		money := a + tip
		t.Summary.TipsCollected += tip
		var bucket *TillCount
		switch {
		case p.Tender == TenderCash:
			bucket = &cash
		case IsCard(p.Tender):
			b := p.Brand
			if b == "" || brandLabels[b] == "" {
				b = BrandOther
			}
			if cards[b] == nil {
				cards[b] = &TillCount{}
			}
			bucket = cards[b]
		default:
			if other[p.Tender] == nil {
				other[p.Tender] = &TillCount{}
			}
			bucket = other[p.Tender]
		}
		bucket.Amount += money
		e := emp[p.StaffID]
		if e == nil {
			name := in.StaffNames[p.StaffID]
			if name == "" {
				name = "Unknown"
			}
			e = &TillEmployee{StaffID: p.StaffID, Name: name}
			emp[p.StaffID] = e
		}
		e.Amount += money
		if !p.Refund {
			bucket.Count++
			e.Count++
		}
		if !isSale(so) {
			t.Summary.NetPaymentsApplied += a
			continue
		}
		tk := byOrder[p.OrderID]
		if tk == nil {
			tk = &taken{}
			byOrder[p.OrderID] = tk
		}
		if p.Refund {
			tk.refunded += p.AmountCents
		} else {
			tk.paid += p.AmountCents
		}
	}

	// The sales.
	catAmt := map[int]*TillCount{}
	var openItems, rest TillCount
	adj := map[string]*TillAdjustment{}
	var adjOrder []string
	for _, so := range z.Orders {
		if !isSale(so) {
			continue
		}
		snap := so.Order
		m := snap.Money
		counter := snap.Source == order.SourceCounter
		t.Summary.GrossSales += m.Subtotal
		t.Summary.Tax += m.Tax
		var disc, fees int64
		for _, f := range m.Fees {
			fees += f.Amount
			if f.Type == "delivery" {
				t.Summary.DeliveryFees += f.Amount
			} else {
				t.Summary.ServiceCharges += f.Amount
			}
		}
		for _, d := range m.Discounts {
			disc += d.Amount
			label := strings.TrimSpace(d.Label)
			if label == "" {
				label = d.Type
			}
			k := discountKind(d) + "|" + label
			a := adj[k]
			if a == nil {
				a = &TillAdjustment{Kind: discountKind(d), Label: label}
				adj[k] = a
				adjOrder = append(adjOrder, k)
			}
			a.Count++
			a.Amount -= d.Amount
		}
		t.Summary.Adjustments -= disc
		var tipIn int64
		if !counter {
			tipIn = m.Tip
			t.Summary.TipsCollected += tipIn
		}
		// Whatever the total holds besides items, discounts, fees, tax and
		// its tip (a vendor's rounding) counts with the service charges.
		t.Summary.ServiceCharges += m.Total - (m.Subtotal - disc + fees + m.Tax + tipIn)

		// Categories: the lines, and the subtotal's rest as Other.
		var lines int64
		for _, it := range snap.Items {
			qty := int64(max(it.Quantity, 1))
			lines += it.LineTotal
			switch {
			case it.SKU == order.SKUCustom:
				openItems.Count += qty
				openItems.Amount += it.LineTotal
			default:
				n, ok := -1, false
				if in.Categories != nil && it.MenuItemID != "" {
					n, ok = in.Categories.of[it.MenuItemID]
				}
				if !ok {
					rest.Count += qty
					rest.Amount += it.LineTotal
					continue
				}
				if catAmt[n] == nil {
					catAmt[n] = &TillCount{}
				}
				catAmt[n].Count += qty
				catAmt[n].Amount += it.LineTotal
			}
		}
		rest.Amount += m.Subtotal - lines

		// What was owed here and what came in for it.
		baseDue := m.Total
		if !counter {
			baseDue = snap.CashDue()
			if pre := m.Total - baseDue; pre != 0 {
				if other[TenderOnline] == nil {
					other[TenderOnline] = &TillCount{}
				}
				other[TenderOnline].Count++
				other[TenderOnline].Amount += pre
			}
		}
		var tk taken
		if p := byOrder[snap.ID]; p != nil {
			tk = *p
		}
		short := baseDue - (tk.paid - tk.refunded)
		refundPart := min(tk.refunded, max(short, 0))
		t.Summary.Refunds -= refundPart
		left := short - refundPart
		if left == 0 {
			continue
		}
		if so.IsDelivery() && so.Status.State == order.StateDelivered && left > 0 {
			if other[TenderDriverCash] == nil {
				other[TenderDriverCash] = &TillCount{}
			}
			other[TenderDriverCash].Count++
			other[TenderDriverCash].Amount += left
			continue
		}
		t.Summary.Unpaid += left
	}

	// DETAIL lists.
	if in.Categories != nil {
		for n, name := range in.Categories.names {
			if c := catAmt[n]; c != nil {
				t.Detail.Categories = append(t.Detail.Categories, TillCategory{Name: name, Count: c.Count, Amount: c.Amount})
			}
		}
	}
	if openItems.Count != 0 || openItems.Amount != 0 {
		t.Detail.Categories = append(t.Detail.Categories, TillCategory{Name: CategoryOpenItems, Count: openItems.Count, Amount: openItems.Amount})
	}
	if rest.Count != 0 || rest.Amount != 0 {
		t.Detail.Categories = append(t.Detail.Categories, TillCategory{Name: CategoryOther, Count: rest.Count, Amount: rest.Amount})
	}
	for _, c := range t.Detail.Categories {
		t.Detail.CategoriesTotal.Count += c.Count
		t.Detail.CategoriesTotal.Amount += c.Amount
	}
	for _, k := range adjOrder {
		t.Detail.Adjustments = append(t.Detail.Adjustments, *adj[k])
	}
	t.Detail.Cash = cash
	for _, b := range Brands {
		c := TillCard{Brand: b, Label: brandLabels[b]}
		if v := cards[b]; v != nil {
			c.Count, c.Amount = v.Count, v.Amount
		}
		t.Detail.Cards = append(t.Detail.Cards, c)
		t.Detail.CardsTotal.Count += c.Count
		t.Detail.CardsTotal.Amount += c.Amount
	}
	var otherNames []string
	for name := range other {
		known := false
		for _, o := range otherOrder {
			known = known || o == name
		}
		if !known {
			otherNames = append(otherNames, name)
		}
	}
	sort.Strings(otherNames)
	for _, name := range append(append([]string(nil), otherOrder...), otherNames...) {
		v := other[name]
		if v == nil || (v.Count == 0 && v.Amount == 0) {
			continue
		}
		label := otherLabels[name]
		if label == "" {
			label = name
		}
		t.Detail.Other = append(t.Detail.Other, TillOther{Tender: name, Label: label, Count: v.Count, Amount: v.Amount})
		t.Summary.TotalOther += v.Amount
	}
	for _, e := range emp {
		t.Detail.Employees = append(t.Detail.Employees, *e)
	}
	sort.Slice(t.Detail.Employees, func(i, j int) bool {
		a, b := t.Detail.Employees[i], t.Detail.Employees[j]
		if a.Amount != b.Amount {
			return a.Amount > b.Amount
		}
		return a.Name < b.Name
	})

	// SUMMARY totals.
	s := &t.Summary
	s.NetSales = s.GrossSales + s.Adjustments + s.Refunds
	s.GrossReceipts = s.NetSales + s.ServiceCharges + s.DeliveryFees + s.Tax + s.TipsCollected + s.Gratuity + s.NetPaymentsApplied
	s.TotalCash = cash.Amount
	s.TotalCreditCard = t.Detail.CardsTotal.Amount
	s.TotalCollected = s.TotalCash + s.TotalCreditCard + s.TotalOther
	s.PaidInOut = t.Detail.PayIns - t.Detail.PayOuts
	t.Detail.TotalCollected = s.TotalCollected
	return t
}
