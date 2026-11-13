package pricing

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Fees are a fee set: the service (online) fee and delivery.
//
// The delivery fee is the total the customer pays (DeliveryFeeCents); it is
// split into the store's share and the driver's share (TERMINAL-PLAN
// "Delivery fee"): DeliveryDriverCents is the driver's part of it (nil = the
// default, DefaultDriverShareCents capped at the fee), the store keeps the
// rest. The split never changes what a customer pays.
type Fees struct {
	ServiceFeeCents       int64  `json:"service_fee_cents"`
	ServiceFeeBps         int    `json:"service_fee_bps"`
	DeliveryFeeCents      int64  `json:"delivery_fee_cents"`
	FreeDeliveryOverCents *int64 `json:"free_delivery_over_cents"`
	DeliveryMinCents      int64  `json:"delivery_min_cents"`
	DeliveryDriverCents   *int64 `json:"delivery_driver_cents,omitempty"`
	// DeliveryBands: the customer's delivery fee by distance (the owner,
	// 2026-11-13; Store › Settings › Map and delivery area). Empty = the
	// flat DeliveryFeeCents. When set, DeliveryFeeCents is kept equal to
	// the first band's fee (Validate), so a reader that doesn't know bands
	// (an older Pi or storefront) charges what an unknown distance costs.
	// Never the driver's pay: DeliveryDriverCents stays as it is.
	DeliveryBands []DeliveryBand `json:"delivery_bands,omitempty"`
}

// Delivery fee band limits.
const (
	MaxDeliveryBands = 10
	// MaxBandM: a band reaches at most 50 miles (the delivery radius's own
	// limit, 80467 m).
	MaxBandM = 80467
)

// DefaultDriverShareCents is the driver's share of the delivery fee when the
// store hasn't set one (the owner's default: store $1 + driver $3 of $4).
const DefaultDriverShareCents = 300

// DriverShare is the driver's part of a delivery fee set (the default when
// unset), never more than the fee.
func (f Fees) DriverShare() int64 {
	d := int64(DefaultDriverShareCents)
	if f.DeliveryDriverCents != nil {
		d = *f.DeliveryDriverCents
	}
	return min(max(d, 0), max(f.DeliveryFeeCents, 0))
}

// DeliverySplit splits the delivery fee an order was charged into the
// store's and the driver's shares: the driver gets his share of it (at most
// what was charged: a free delivery splits 0 + 0), the store the rest.
func DeliverySplit(st Settings, chargedCents int64) (storeCents, driverCents int64) {
	if chargedCents <= 0 {
		return 0, 0
	}
	driverCents = min(max(st.DeliveryDriverCents, 0), chargedCents)
	return chargedCents - driverCents, driverCents
}

// StoreSettings are a store's pricing defaults; TaxBps, Stacking and the
// half-and-half pair are store-wide, the rest each channel kind may replace
// (KindOverride).
type StoreSettings struct {
	TaxBps   int     `json:"tax_bps"`
	Stacking string  `json:"stacking"`
	Fees     Fees    `json:"fees"`
	Tips     Tips    `json:"tips"`
	CardFee  CardFee `json:"card_fee"`
	// Half and half: a topping on a half at full (default) or half price;
	// the cheaper of two half-specialties' extra over plain at this % off
	// (default 0).
	HalfToppingPrice    string `json:"half_topping_price"`
	HalfSpecialtyOffPct int    `json:"half_specialty_off_pct"`
}

// KindOverride replaces whole sets for one channel kind (nil = the store's).
type KindOverride struct {
	Fees    *Fees    `json:"fees"`
	Tips    *Tips    `json:"tips"`
	CardFee *CardFee `json:"card_fee"`
}

// DefaultDeliveryFeeCents is a new store's delivery fee (the owner,
// 2026-11-11: $4 = driver $3 + store $1).
const DefaultDeliveryFeeCents = 400

// DefaultStore are a new store's settings: Maryland's 6% food tax, a $4
// delivery fee ($3 of it the driver's), no service fee, tips off, no card
// fee, the best single discount. (Migration 00037 saved the old $0 defaults
// for stores that existed before; 00039 moved every store still exactly on
// them to the $4 fee with the driver's $3.)
func DefaultStore() StoreSettings {
	driver := int64(DefaultDriverShareCents)
	return StoreSettings{TaxBps: 600, Stacking: StackBest,
		Fees:    Fees{DeliveryFeeCents: DefaultDeliveryFeeCents, DeliveryDriverCents: &driver},
		Tips:    Tips{Presets: []int{15, 18, 20}, DefaultPct: 18},
		CardFee: CardFee{Mode: CardFeeNone}, HalfToppingPrice: HalfFull}
}

// DefaultOverrides: the website takes tips (the owner, 2026-11-02).
func DefaultOverrides() map[string]KindOverride {
	return map[string]KindOverride{"web": {Tips: &Tips{Enabled: true, Presets: []int{15, 18, 20}, DefaultPct: 18}}}
}

// Kinds that may carry overrides (the channel kinds).
var Kinds = []string{"web", "operator", "counter", "vendor"}

// Effective is what a channel of a kind is priced with.
func Effective(s StoreSettings, o KindOverride) Settings {
	f, t, c := s.Fees, s.Tips, s.CardFee
	if o.Fees != nil {
		f = *o.Fees
	}
	if o.Tips != nil {
		t = *o.Tips
	}
	if o.CardFee != nil {
		c = *o.CardFee
	}
	if t.Presets == nil {
		t.Presets = []int{}
	}
	if s.HalfToppingPrice == "" {
		s.HalfToppingPrice = HalfFull
	}
	return Settings{TaxBps: s.TaxBps, Stacking: s.Stacking, ServiceFeeCents: f.ServiceFeeCents, ServiceFeeBps: f.ServiceFeeBps,
		DeliveryFeeCents: f.DeliveryFeeCents, FreeDeliveryOverCents: f.FreeDeliveryOverCents, DeliveryMinCents: f.DeliveryMinCents,
		DeliveryBands: f.DeliveryBands, DeliveryDriverCents: f.DriverShare(), Tips: t, CardFee: c, HalfToppingPrice: s.HalfToppingPrice, HalfSpecialtyOffPct: s.HalfSpecialtyOffPct}
}

var ErrBadSettings = errors.New("bad pricing settings")

func bad(msg string) error { return fmt.Errorf("%w: %s", ErrBadSettings, msg) }

// Validate checks the store's settings (and normalizes empty values).
func (s *StoreSettings) Validate() error {
	if s.TaxBps < 0 || s.TaxBps > 3000 {
		return bad("tax must be 0–30%")
	}
	if s.Stacking == "" {
		s.Stacking = StackBest
	}
	if s.Stacking != StackBest && s.Stacking != StackAll && s.Stacking != StackSingle {
		return bad("stacking must be best, stack or single")
	}
	if s.HalfToppingPrice == "" {
		s.HalfToppingPrice = HalfFull
	}
	if s.HalfToppingPrice != HalfFull && s.HalfToppingPrice != HalfHalf {
		return bad("half_topping_price must be full or half")
	}
	if s.HalfSpecialtyOffPct < 0 || s.HalfSpecialtyOffPct > 100 {
		return bad("half_specialty_off_pct must be 0–100")
	}
	return validSets(&s.Fees, &s.Tips, &s.CardFee)
}

// Validate checks a kind's override.
func (o *KindOverride) Validate() error {
	return validSets(o.Fees, o.Tips, o.CardFee)
}

func validSets(f *Fees, t *Tips, c *CardFee) error {
	if f != nil {
		if f.ServiceFeeCents < 0 || f.ServiceFeeCents > 10000 || f.ServiceFeeBps < 0 || f.ServiceFeeBps > 3000 ||
			f.DeliveryFeeCents < 0 || f.DeliveryFeeCents > 10000 || f.DeliveryMinCents < 0 || f.DeliveryMinCents > 100000 ||
			(f.FreeDeliveryOverCents != nil && (*f.FreeDeliveryOverCents < 0 || *f.FreeDeliveryOverCents > 100000)) {
			return bad("fees out of range (service ≤ $100 or 30%, delivery ≤ $100, minimum ≤ $1000)")
		}
		if len(f.DeliveryBands) > MaxDeliveryBands {
			return bad("delivery fee by distance: at most 10 rows")
		}
		if len(f.DeliveryBands) == 0 {
			f.DeliveryBands = nil
		}
		lowest := f.DeliveryFeeCents
		for i, b := range f.DeliveryBands {
			if b.UpToM < 1 || b.UpToM > MaxBandM || b.FeeCents < 0 || b.FeeCents > 10000 {
				return bad("delivery fee by distance: each row up to 1 m–50 mi, a fee up to $100")
			}
			if i > 0 && b.UpToM <= f.DeliveryBands[i-1].UpToM {
				return bad("delivery fee by distance: each row reaches farther than the one before")
			}
			if i == 0 || b.FeeCents < lowest {
				lowest = b.FeeCents
			}
		}
		if len(f.DeliveryBands) > 0 {
			f.DeliveryFeeCents = f.DeliveryBands[0].FeeCents // older readers: the unknown-distance fee
		}
		if d := f.DeliveryDriverCents; d != nil && (*d < 0 || *d > lowest) {
			return bad("the driver's share of the delivery fee must be 0 up to the fee (the lowest fee by distance)")
		}
	}
	if t != nil {
		if t.DefaultPct < 0 || t.DefaultPct > 100 || len(t.Presets) > 6 {
			return bad("tips: default 0–100%, at most 6 presets")
		}
		for _, p := range t.Presets {
			if p < 0 || p > 100 {
				return bad("tip presets are 0–100%")
			}
		}
		if t.Presets == nil {
			t.Presets = []int{}
		}
	}
	if c != nil {
		if c.Mode == "" {
			c.Mode = CardFeeNone
		}
		if c.Mode != CardFeeNone && c.Mode != CardFeeSurcharge && c.Mode != CardFeeCashDiscount || c.Bps < 0 || c.Bps > 1000 {
			return bad("card fee: none, surcharge or cash_discount, at most 10%")
		}
	}
	return nil
}

var (
	ErrBadDeal = errors.New("bad deal")
	codeRe     = regexp.MustCompile(`^[A-Z0-9-]{2,20}$`)
	dayKeys    = map[string]bool{"mon": true, "tue": true, "wed": true, "thu": true, "fri": true, "sat": true, "sun": true}
)

func badDeal(msg string) error { return fmt.Errorf("%w: %s", ErrBadDeal, msg) }

func emptyEligible(e *Eligible) bool {
	return e == nil || len(e.Items)+len(e.Categories) == 0
}

// Validate checks a deal's shape for its kind (the app checks that its ids
// are the store's) and normalizes it.
func (d *Deal) Validate() error {
	d.Name = strings.TrimSpace(d.Name)
	if d.Name == "" || len(d.Name) > 80 {
		return badDeal("a name (≤ 80 characters) is required")
	}
	switch d.Fulfil {
	case "", "both", "pickup", "delivery":
	default:
		return badDeal("fulfil must be pickup, delivery or both")
	}
	if s := d.Schedule; s != nil {
		for _, day := range s.Days {
			if !dayKeys[day] {
				return badDeal("schedule days are mon..sun")
			}
		}
		for _, t := range []string{s.Start, s.End} {
			if _, ok := hhmm(t); t != "" && !ok {
				return badDeal("schedule times are HH:MM")
			}
		}
	}
	switch d.Kind {
	case DealBundle:
		if d.PriceCents < 0 || len(d.Slots) == 0 || len(d.Slots) > 10 {
			return badDeal("a bundle needs a price and 1–10 steps")
		}
		for i := range d.Slots {
			if strings.TrimSpace(d.Slots[i].Label) == "" || emptyEligible(&d.Slots[i].Eligible) || d.Slots[i].IncludedOptions < 0 {
				return badDeal("every bundle step needs a label and items or categories")
			}
		}
	case DealMix:
		if d.Qty < 2 || d.EachCents < 0 || emptyEligible(d.Eligible) {
			return badDeal("mix & match needs 2 or more, a price each and items or categories")
		}
	case DealNth:
		if d.N < 2 || d.Pct < 1 || d.Pct > 100 || emptyEligible(d.Eligible) {
			return badDeal("every-nth needs n ≥ 2, 1–100% off and items or categories")
		}
	case DealCode:
		d.Code = strings.ToUpper(strings.TrimSpace(d.Code))
		if !codeRe.MatchString(d.Code) || (d.Pct > 0) == (d.AmountCents > 0) || d.Pct > 100 || d.AmountCents < 0 || d.MinSubtotalCents < 0 {
			return badDeal("a code is 2–20 letters/digits with either a % or an amount off")
		}
	case DealFreeItem:
		if emptyEligible(d.Eligible) || d.MinSubtotalCents < 0 {
			return badDeal("a free item needs items or categories and a minimum")
		}
	case DealCoupon:
		return d.validCoupon()
	default:
		return badDeal("kind must be bundle, mix, nth, code, free_item or coupon")
	}
	return nil
}

// Coupon limits.
const (
	MaxCouponParts     = 8
	MaxCouponPartQty   = 10
	MaxCouponFreeMods  = 20
	MaxCouponCardLines = 6
	MaxCouponCardLine  = 60
)

// validCoupon checks a coupon (the app checks its families and sizes are the
// store's) and trims its card and labels.
func (d *Deal) validCoupon() error {
	if d.PriceCents < 0 || d.PriceCents > MaxOpenPriceCents {
		return badDeal("a coupon needs a price ($0 to $10,000)")
	}
	if len(d.Parts) == 0 || len(d.Parts) > MaxCouponParts {
		return badDeal("a coupon has 1–8 parts")
	}
	card := []string{}
	for _, l := range d.Card {
		if l = strings.TrimSpace(l); l != "" {
			card = append(card, l)
		}
	}
	if len(card) > MaxCouponCardLines {
		return badDeal("a coupon's card has up to 6 lines")
	}
	for _, l := range card {
		if utf8.RuneCountInString(l) > MaxCouponCardLine {
			return badDeal("a card line is up to 60 characters")
		}
	}
	d.Card = card
	for i := range d.Parts {
		p := &d.Parts[i]
		p.Label = strings.TrimSpace(p.Label)
		if utf8.RuneCountInString(p.Label) > 60 {
			return badDeal("a part's label is up to 60 characters")
		}
		if len(p.Families) == 0 || p.Qty < 1 || p.Qty > MaxCouponPartQty || p.RegularModsIncluded < 0 || p.RegularModsIncluded > MaxCouponFreeMods {
			return badDeal("every coupon part needs families, 1–10 items and 0–20 free regular mods")
		}
		base := []string{}
		for _, f := range p.UpchargeOver {
			if !contains(p.Families, f) {
				return badDeal("a part's upcharge_over families are some of its own families")
			}
			if !contains(base, f) {
				base = append(base, f)
			}
		}
		p.UpchargeOver = base
		all := true
		for _, f := range p.Families {
			all = all && contains(base, f)
		}
		if len(base) == 0 || all {
			p.UpchargeOver = nil // every unit is in the base: no upcharge
		}
	}
	// A coupon's own fields only (none of the other kinds').
	d.Slots, d.Eligible, d.Qty, d.EachCents, d.N, d.Pct, d.Code, d.AmountCents, d.FirstOrderOnly, d.MinSubtotalCents = nil, nil, 0, 0, 0, 0, "", 0, false, 0
	return nil
}

// StoreDeal is a deal as the store keeps it: the deal plus the channel kinds
// it runs on.
type StoreDeal struct {
	Deal
	ChannelKinds []string  `json:"channel_kinds"`
	Position     int       `json:"position"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// ValidKinds checks a deal's channel kinds.
func (d *StoreDeal) ValidKinds() error {
	if d.ChannelKinds == nil {
		d.ChannelKinds = []string{"web", "operator", "counter"}
		if d.Kind == DealCoupon { // the counter's (the cashier applies it)
			d.ChannelKinds = []string{"counter"}
		}
	}
	for _, k := range d.ChannelKinds {
		ok := false
		for _, x := range Kinds {
			ok = ok || x == k
		}
		if !ok {
			return badDeal("channels are web, operator, counter and vendor")
		}
	}
	return nil
}
