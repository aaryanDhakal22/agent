// Package terminal is the Pi's side of the store's own POS terminals
// (terminal stage 0d, contract/agent.md §2.10): the snapshots main sends for
// them (the counter menu and the terminal config with staff + PIN hashes),
// staff and their limits, counter-order payments and the action journal.
package terminal

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/pricing"
)

// Snapshot kinds (terminal_snapshots.kind).
const (
	KindCounterMenu    = "counter_menu"
	KindTerminalConfig = "terminal_config"
)

// Snapshot is one stored body main sent: version from the body, ETag = a
// hash of the body.
type Snapshot struct {
	Kind      string
	Version   int64
	ETag      string
	Body      []byte
	FetchedAt time.Time
}

// Roles.
const (
	RoleCashier = "cashier"
	RoleManager = "manager"
	RoleDriver  = "driver"
)

// Limits are what a staff member may do without a manager: refund (and void
// a paid order) up to RefundCents, discount up to DiscountPct. Unlimited: a
// manager (never asked for another PIN).
type Limits struct {
	RefundCents int64 `json:"refund_cents"`
	DiscountPct int   `json:"discount_pct"`
	Unlimited   bool  `json:"unlimited,omitempty"`
}

// Staff is one person in main's staff table as the Pi gets it (GET
// /store/v1/agent/terminal-config). PinHash is bcrypt; the PIN itself never
// leaves the terminal.
type Staff struct {
	ID        string  `json:"id"`
	FirstName string  `json:"first_name"`
	LastName  string  `json:"last_name"`
	Role      string  `json:"role"`
	PinHash   string  `json:"pin_hash"`
	Limits    *Limits `json:"limits"` // nil = the role's default
	Active    *bool   `json:"active"` // nil = active
	// EffectiveLimits is main's own answer (null = unlimited); absent in
	// older configs (then Limits / the role's default apply).
	EffectiveLimits json.RawMessage `json:"effective_limits,omitempty"`
}

// IsActive: not switched off in main.
func (s Staff) IsActive() bool { return s.Active == nil || *s.Active }

// Name is how the Pi names them (journal, logs): first name + last initial.
func (s Staff) Name() string {
	n := strings.TrimSpace(s.FirstName)
	if l := strings.TrimSpace(s.LastName); l != "" {
		r, _ := utf8.DecodeRuneInString(l)
		n += " " + string(r) + "."
	}
	return n
}

// Public is a staff member as a terminal sees them (no PIN hash).
type Public struct {
	ID        string `json:"id"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Name      string `json:"name"`
	Role      string `json:"role"`
}

func (s Staff) Public() Public {
	return Public{ID: s.ID, FirstName: s.FirstName, LastName: s.LastName, Name: s.Name(), Role: s.Role}
}

// Settings are the terminal settings the Pi itself reads; the whole settings
// object goes to the terminals as main sent it.
type Settings struct {
	// CounterStart is where the counter sequence starts (and starts again
	// after Close batch). 0 / missing = DefaultCounterStart.
	CounterStart int64 `json:"counter_start"`
	// The cashier role's defaults (main's terminal settings).
	CashierRefundCents *int64 `json:"cashier_refund_cents"`
	CashierDiscountPct *int   `json:"cashier_discount_pct"`
	// RoleLimits are each role's default limits (a staff member's own
	// limits override them; the cashier_* fields win for cashiers).
	RoleLimits map[string]Limits `json:"role_limits"`
	// AutoCloseBatchAt is when the Pi closes a batch nobody closed ("HH:MM"
	// store time; "" = off; missing = DefaultAutoCloseAt).
	AutoCloseBatchAt *string `json:"auto_close_batch_at"`
	// CashierPriceDropPct: a cashier lowers a rung item's price by up to
	// this % alone; more needs a manager's PIN (missing = DefaultPriceDropPct).
	CashierPriceDropPct *int `json:"cashier_price_drop_pct"`
	// EmployeeMealPct: the Employee meal discount anyone logged in may give
	// without a manager (missing = DefaultEmployeeMealPct).
	EmployeeMealPct *int `json:"employee_meal_pct"`
	// CashierPayoutCents: a cashier's drawer pay-out without a manager's PIN
	// (missing = DefaultCashierPayoutCents; contract 2.10k).
	CashierPayoutCents *int64 `json:"cashier_payout_cents"`
	// DefaultPickupMinutes: a pickup's quoted minutes when the terminal
	// sends none (main's default_pickup_minutes, default 15).
	DefaultPickupMinutes *int `json:"default_pickup_minutes"`
	// DrawerStartCents: the cash left in each terminal's drawer at opening
	// (the owner, 2026-11-12; missing = DefaultDrawerStartCents).
	DrawerStartCents *int64 `json:"drawer_start_cents"`
	// WasteReasons: the store's Void waste reasons (contract 2.10s §5;
	// missing / empty = DefaultWasteReasons).
	WasteReasons []string `json:"waste_reasons"`
}

// DefaultDrawerStartCents is the starting cash in each drawer when main's
// settings don't say (main's own default, the owner's X: $100).
const DefaultDrawerStartCents = 10000

// DrawerStartCents is the starting cash a drawer session opened by itself
// gets (bounded 0-1,000,000 cents).
func (c *Config) DrawerStartCents() int64 {
	if c == nil || c.parsed.DrawerStartCents == nil {
		return DefaultDrawerStartCents
	}
	return min(max(*c.parsed.DrawerStartCents, 0), 1_000_000)
}

// DefaultPickupMinutes is main's default_pickup_minutes (false when main's
// settings don't carry it), bounded 0-240.
func (c *Config) DefaultPickupMinutes() (int64, bool) {
	if c == nil || c.parsed.DefaultPickupMinutes == nil {
		return 0, false
	}
	return int64(min(max(*c.parsed.DefaultPickupMinutes, 0), 240)), true
}

// DefaultEmployeeMealPct is the Employee meal's percent when main's settings
// don't say (main's own default since 00039, the owner's 2026-11-12: 20 %).
const DefaultEmployeeMealPct = 20

// The cashier's limits when main's settings say nothing (main's own
// defaults, the owner's X, 2026-11-12): cash refunds up to $20 in all per
// order, typed discounts up to 20 %.
const (
	DefaultCashierRefundCents = 2000
	DefaultCashierDiscountPct = 20
)

// EmployeeMealPct is how far (percent) an Employee meal goes without a
// manager.
func (c *Config) EmployeeMealPct() int {
	if c != nil && c.parsed.EmployeeMealPct != nil {
		return min(max(*c.parsed.EmployeeMealPct, 0), 100)
	}
	return DefaultEmployeeMealPct
}

// MayComp: a manager (or anyone main made unlimited) comps alone; a cashier
// or a driver needs a manager's PIN / approval (the owner, 2026-11-12
// evening: a comp is a manager's again; that morning a cashier comped
// freely). Reversing a comp stays free.
func (c *Config) MayComp(s Staff) bool {
	return s.Role == RoleManager || c.LimitsOf(s).Unlimited
}

// MayVoid: who voids (or wastes) an order that was sent to the kitchen, or
// takes a sent line off it, without a manager's PIN / approval: a manager,
// or anyone main made unlimited (the owner, 2026-11-12 evening). An order
// not sent yet never reaches the Pi (the terminal's Cancel / Wipe is free).
func (c *Config) MayVoid(s Staff) bool {
	return s.Role == RoleManager || c.LimitsOf(s).Unlimited
}

// VoidNeed is what a void of a sent order (the whole order, a waste, or a
// sent line taken off so that the check gets smaller) needs from a manager:
// nil for a manager / anyone main made unlimited, and for a call-center
// agent (no manager there: it cancels its own unpaid orders with a picked
// reason, contract 2.10ab). cents = the order's total, or how much the check
// goes down by.
func (c *Config) VoidNeed(s Staff, cents int64) *ManagerRequired {
	if s.Role == RoleCallcenter || c.MayVoid(s) {
		return nil
	}
	return &ManagerRequired{Action: ActionVoid, Requested: cents}
}

// DefaultPriceDropPct is the cashier's price-drop limit when main's settings
// don't say (main's own default, TERMINAL-PLAN "Item price change").
const DefaultPriceDropPct = 20

// PriceDropOf is how far (percent) a staff member may lower a rung item's
// price without a manager: unlimited for a manager (or anyone main made
// unlimited), the cashier setting for a cashier, 0 for anyone else. Raising
// a price is always free.
func (c *Config) PriceDropOf(s Staff) (pct int, unlimited bool) {
	if c.LimitsOf(s).Unlimited {
		return 0, true
	}
	if s.Role != RoleCashier {
		return 0, false
	}
	if c != nil && c.parsed.CashierPriceDropPct != nil {
		return min(max(*c.parsed.CashierPriceDropPct, 0), 100), false
	}
	return DefaultPriceDropPct, false
}

// PriceChange is one cart line whose price_cents replaced the item's own
// price (TERMINAL-PLAN "Item price change": not a discount; the order total
// is never set directly).
type PriceChange struct {
	Line         int    `json:"line"` // the cart line's index
	ItemID       string `json:"item_id"`
	SizeID       string `json:"size_id,omitempty"`
	ChoiceID     string `json:"choice_id,omitempty"`
	Name         string `json:"name"`
	FromCents    int64  `json:"from_cents"` // the item's own price (the engine's list_cents)
	ToCents      int64  `json:"to_cents"`   // the line's price_cents
	DropPct      int    `json:"drop_pct"`   // how far it is lowered, rounded up (0 = raised / same)
	NeedsManager bool   `json:"needs_manager"`
	// Approved: the same change was on the order already (a kitchen change
	// keeps it without asking again).
	Approved bool `json:"-"`
	// Comp: rung at $0 on an item that costs something - a comp line, which
	// always needs a manager (TERMINAL-PLAN "Discounts": comp items).
	Comp bool `json:"comp,omitempty"`
}

// Key identifies a change across a placed cart and its later edits.
func (c PriceChange) Key() string {
	return fmt.Sprintf("%s|%s|%s|%d", c.ItemID, c.SizeID, c.ChoiceID, c.ToCents)
}

// DropPctOf is how far `to` lowers `from`, in percent rounded up (0 when it
// is not lower).
func DropPctOf(from, to int64) int {
	if to >= from || from <= 0 {
		return 0
	}
	return int(((from-to)*100 + from - 1) / from)
}

// DropAllowed: lowering from → to stays within pct % (raising always does).
func DropAllowed(from, to int64, pct int) bool {
	return to >= from || (from-to)*100 <= from*int64(pct)
}

// DefaultAutoCloseAt is the batch auto-close time when main's settings
// don't say (main's own default, the owner's 4:00 am).
const DefaultAutoCloseAt = "04:00"

// AutoCloseAt is the batch auto-close time in use: "" = off (also when the
// Pi has no terminal config from main yet: nil).
func (c *Config) AutoCloseAt() string {
	if c == nil {
		return ""
	}
	if c.parsed.AutoCloseBatchAt == nil {
		return DefaultAutoCloseAt
	}
	return strings.TrimSpace(*c.parsed.AutoCloseBatchAt)
}

// DefaultCounterStart is the counter sequence's start when main's config
// doesn't say (the store setting X, contract §2.10).
const DefaultCounterStart = 1

// Config is GET /store/v1/agent/terminal-config.
type Config struct {
	Version  int64           `json:"version"`
	Staff    []Staff         `json:"staff"`
	Settings json.RawMessage `json:"settings"`
	parsed   Settings
}

// ParseConfig decodes and checks a terminal config body.
func ParseConfig(body []byte) (*Config, error) {
	var c Config
	if err := json.Unmarshal(body, &c); err != nil {
		return nil, fmt.Errorf("terminal config: %w", err)
	}
	if len(c.Settings) > 0 && string(c.Settings) != "null" {
		if err := json.Unmarshal(c.Settings, &c.parsed); err != nil {
			return nil, fmt.Errorf("terminal config settings: %w", err)
		}
	} else {
		c.Settings = json.RawMessage("{}")
	}
	for i, s := range c.Staff {
		if strings.TrimSpace(s.ID) == "" {
			return nil, fmt.Errorf("terminal config: staff #%d has no id", i)
		}
	}
	return &c, nil
}

// CounterStart is the counter sequence's start.
func (c *Config) CounterStart() int64 {
	if c == nil || c.parsed.CounterStart <= 0 || c.parsed.CounterStart >= MaxCounterStart {
		// S-6: counter numbers stay below the online kitchen numbers (1100+).
		return DefaultCounterStart
	}
	return c.parsed.CounterStart
}

// MaxCounterStart: a counter_start at or above it (the online kitchen
// numbers start at 1100) falls back to the default (S-6).
const MaxCounterStart = 1100

// LimitsOf is a staff member's limits: a manager is unlimited; otherwise
// their own, else the role's default, else none (0 / 0).
func (c *Config) LimitsOf(s Staff) Limits {
	if s.Role == RoleManager {
		return Limits{Unlimited: true}
	}
	if s.Role == RoleCallcenter { // refund 0, discount 0, never unlimited (§2.10ab)
		return Limits{}
	}
	if e := s.EffectiveLimits; len(e) > 0 {
		if string(e) == "null" {
			return Limits{Unlimited: true}
		}
		// main's wire: {refund_cents, discount_pct}, null = unlimited; both
		// null = a person main made unlimited (contract/money-rules.json "uma").
		var w struct {
			RefundCents *int64 `json:"refund_cents"`
			DiscountPct *int   `json:"discount_pct"`
		}
		if json.Unmarshal(e, &w) == nil {
			if w.RefundCents == nil && w.DiscountPct == nil {
				return Limits{Unlimited: true}
			}
			var l Limits
			if w.RefundCents != nil {
				l.RefundCents = *w.RefundCents
			}
			if w.DiscountPct != nil {
				l.DiscountPct = *w.DiscountPct
			}
			return l
		}
	}
	if s.Limits != nil {
		l := *s.Limits
		l.Unlimited = false
		return l
	}
	if c != nil && s.Role == RoleCashier && (c.parsed.CashierRefundCents != nil || c.parsed.CashierDiscountPct != nil) {
		l := Limits{RefundCents: DefaultCashierRefundCents, DiscountPct: DefaultCashierDiscountPct}
		if v := c.parsed.CashierRefundCents; v != nil {
			l.RefundCents = *v
		}
		if v := c.parsed.CashierDiscountPct; v != nil {
			l.DiscountPct = *v
		}
		return l
	}
	if c != nil {
		if l, ok := c.parsed.RoleLimits[s.Role]; ok {
			l.Unlimited = false
			return l
		}
	}
	if s.Role == RoleCashier {
		return Limits{RefundCents: DefaultCashierRefundCents, DiscountPct: DefaultCashierDiscountPct}
	}
	return Limits{}
}

// Find is an active staff member by id.
func (c *Config) Find(id string) (Staff, bool) {
	if c == nil {
		return Staff{}, false
	}
	for _, s := range c.Staff {
		if s.ID == id && s.IsActive() {
			return s, true
		}
	}
	return Staff{}, false
}

// CounterMenu is GET /store/v1/agent/counter-menu: the counter view's build
// (passed to the terminals as is), the pricing context the Pi prices with,
// and each bank item's kitchen printer.
type CounterMenu struct {
	MenuVersion int64           `json:"menu_version"`
	Build       json.RawMessage `json:"build"`
	Pricing     pricing.Context `json:"pricing"`
	// Routes: bank item id → its kitchen printer (main: the item's own, else
	// its category's default; absent = no printer: the line prints on every
	// station).
	Routes map[string]Route `json:"routes"`
}

// Route is one bank item's kitchen routing. Name is the printer's name
// (informational; the line keeps the menu's item name).
type Route struct {
	PrinterID string `json:"printer_id"`
	Name      string `json:"name,omitempty"`
	// NoKitchen: the item (or its category) is marked "no kitchen ticket" in
	// main: its lines print on no station and never in "Also on this order"
	// (order.RoutingNone); the receipt still lists them.
	NoKitchen bool `json:"no_kitchen,omitempty"`
}

// ParseCounterMenu decodes a counter menu body. The pricing context's menu
// is filled from the build when main sends it once.
func ParseCounterMenu(body []byte) (*CounterMenu, error) {
	var m CounterMenu
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("counter menu: %w", err)
	}
	if len(m.Pricing.Menu.Categories) == 0 && len(m.Build) > 0 {
		var pm pricing.Menu
		if err := json.Unmarshal(m.Build, &pm); err != nil {
			return nil, fmt.Errorf("counter menu build: %w", err)
		}
		m.Pricing.Menu = pm
	}
	if len(m.Pricing.Menu.Categories) == 0 {
		return nil, errors.New("counter menu: no categories")
	}
	return &m, nil
}

// Order types a terminal places (the snapshot's fulfillment: counter →
// dine_in, as the POS counter checks).
const (
	TypeCounter  = "counter"
	TypePickup   = "pickup"
	TypeDelivery = "delivery"
)

// FulfillmentOf is the Pi's snapshot fulfillment type of a terminal order
// type (main's order.placed snapshot keeps "counter").
func FulfillmentOf(t string) string {
	if t == TypeCounter {
		return "dine_in"
	}
	return t
}

// Tenders.
const (
	TenderCash       = "cash"
	TenderCard       = "card"        // the Square Reader in the terminal app
	TenderSquareApp  = "square_app"  // charged on Square's own phone app, recorded here
	TenderManualCard = "manual_card" // manual card entry
	TenderComp       = "comp"
)

// ValidTender reports a known tender.
func ValidTender(t string) bool {
	switch t {
	case TenderCash, TenderCard, TenderSquareApp, TenderManualCard, TenderComp:
		return true
	}
	return false
}

// MethodOf is the snapshot payment method (main's PaymentSummary.method).
func MethodOf(tender string) string {
	switch tender {
	case TenderCash:
		return "cash"
	case TenderComp:
		return "other"
	}
	return "card"
}

// TenderOf is the tender a snapshot payment's method stands for (MethodOf
// back: cash, other = comp, anything else a card; F-9 restore).
func TenderOf(method string) string {
	switch method {
	case "cash":
		return TenderCash
	case "other":
		return TenderComp
	}
	return TenderCard
}

// Payment is money taken on a counter order (or given back: Refund, the
// amounts stay positive here and count negative in the order's balance).
type Payment struct {
	ID            string    `json:"id"`
	OrderID       string    `json:"order_id"`
	ClientKey     string    `json:"client_key,omitempty"`
	At            time.Time `json:"at"`
	Tender        string    `json:"tender"`
	AmountCents   int64     `json:"amount_cents"`
	TipCents      int64     `json:"tip_cents"`
	TenderedCents int64     `json:"tendered_cents,omitempty"`
	ChangeCents   int64     `json:"change_cents,omitempty"`
	Refund        bool      `json:"refund,omitempty"`
	Reason        string    `json:"reason,omitempty"`
	Ref           string    `json:"ref,omitempty"`
	Last4         string    `json:"last4,omitempty"`
	StaffID       string    `json:"staff_id"`
	DeviceID      string    `json:"device_id"`
	ApprovedBy    string    `json:"approved_by,omitempty"` // the manager whose PIN allowed it
	// DrawerID: the terminal drawer session a cash payment went in (set by
	// the Pi; "" = none open, or not cash).
	DrawerID string `json:"drawer_id,omitempty"`
	// Brand: a card payment's brand (BrandAmex ... BrandOther; "" = none
	// given: the till report counts it as Credit Other).
	Brand string `json:"brand,omitempty"`
	// RefundKind: a refund's kind (contract 2.10n): "" plain | RefundGoodwill
	// (money back, no item removed: an adjustment on the order) |
	// RefundOutside (refunded in Square's dashboard, recorded here).
	RefundKind string `json:"refund_kind,omitempty"`
	// DrawerStart (not stored): the starting cash a drawer session the Pi
	// opens by itself with this payment gets (main's drawer_start_cents).
	DrawerStart int64 `json:"-"`
	// RequestHash (00033): the keyed request's fingerprint (contract 2.10s:
	// the same client_key with another body is 409 key_reused).
	RequestHash string `json:"-"`
	// BatchID (00033): the batch the payment was taken in (a carried check's
	// later payment counts on that batch's Z).
	BatchID int64 `json:"batch_id,omitempty"`
	// Square's answer for an in-app card payment (contract 2.10r, table
	// payment_cards): kept, returned and printed; never a card number.
	CardInfo
}

// CardInfo is what Square answered for a card the terminal's reader or
// manual entry took (contract 2.10r §3), or for a refund given through
// Square (§4).
type CardInfo struct {
	AuthCode            string            `json:"auth_code,omitempty"`
	Entry               string            `json:"entry,omitempty"` // contactless | chip | swiped | keyed
	EMV                 map[string]string `json:"emv,omitempty"`   // application_label, aid, cvm
	SquarePaymentID     string            `json:"square_payment_id,omitempty"`
	SquareReceiptNumber string            `json:"square_receipt_number,omitempty"`
	// A refund through Square: Square's refund id + the Pi payment it gave
	// money back on.
	SquareRefundID string `json:"square_refund_id,omitempty"`
	RefundOf       string `json:"refund_of,omitempty"`
}

// Empty reports whether nothing about the card is known.
func (c CardInfo) Empty() bool {
	return c.AuthCode == "" && c.Entry == "" && len(c.EMV) == 0 && c.SquarePaymentID == "" && c.SquareReceiptNumber == "" &&
		c.SquareRefundID == "" && c.RefundOf == ""
}

// Card entries (CardInfo.Entry).
var CardEntries = map[string]bool{"contactless": true, "chip": true, "swiped": true, "keyed": true}

// EMVKeys are the EMV labels a payment may carry (Square's SDK gives no
// TVR / IAD / ARC).
var EMVKeys = map[string]bool{"application_label": true, "aid": true, "cvm": true}

// Refund kinds (Payment.RefundKind).
const (
	RefundGoodwill = "goodwill"
	RefundOutside  = "outside"
)

// DiscountAdjustment is the discount type of a goodwill refund on the order's
// money (contract 2.10n; the Z / till report count it with "other").
const DiscountAdjustment = "adjustment"

// GoodwillLabel is a goodwill refund's adjustment label.
func GoodwillLabel(reason string) string {
	if r := strings.TrimSpace(reason); r != "" {
		return "Goodwill: " + r
	}
	return "Goodwill"
}

// Card brands (Payment.Brand, the till report's cards).
const (
	BrandAmex       = "amex"
	BrandDiscover   = "discover"
	BrandMastercard = "mastercard"
	BrandVisa       = "visa"
	BrandOther      = "other"
)

// Brands are the till report's card brands, in order.
var Brands = []string{BrandAmex, BrandDiscover, BrandMastercard, BrandVisa, BrandOther}

// IsCard: a card tender (the reader, the Square app, manual entry).
func IsCard(tender string) bool {
	return tender == TenderCard || tender == TenderSquareApp || tender == TenderManualCard
}

// NormBrand maps a brand as a terminal (or Square) names it to ours: "" stays
// "" (none given), anything unknown is BrandOther.
func NormBrand(b string) string {
	k := strings.ToLower(strings.TrimSpace(b))
	k = strings.NewReplacer(" ", "", "_", "", "-", "").Replace(k)
	switch k {
	case "":
		return ""
	case "amex", "americanexpress":
		return BrandAmex
	case "discover", "discoverdiners", "diners", "dinersclub":
		return BrandDiscover
	case "mastercard", "mc":
		return BrandMastercard
	case "visa":
		return BrandVisa
	}
	return BrandOther
}

// Wire is the payment as main's order.payment / order.placed carry it
// (contract/main.md §2.5zb): method = the tender, a refund negative (its tip
// folded into the amount).
func (p Payment) Wire() map[string]any {
	amount, tip := p.AmountCents, p.TipCents
	if p.Refund {
		amount, tip = -(p.AmountCents + p.TipCents), 0
	}
	var staff any
	if p.StaffID != "" {
		staff = p.StaffID
	}
	ref := p.Ref
	if p.SquareRefundID != "" { // a refund through Square: its refund id
		ref = p.SquareRefundID
	}
	m := map[string]any{"id": p.ID, "method": p.Tender, "amount_cents": amount, "tip_cents": tip, "last4": p.Last4,
		"auth_code": p.AuthCode, "ref": ref, "at": p.At.UTC().Truncate(time.Millisecond), "staff_id": staff}
	// Hardening (main §2.5zd): the batch it was taken in (a carried check's
	// later payment), the tap's key, the approving manager, the payment a
	// tip / refund is about.
	if p.BatchID > 0 {
		m["batch_id"] = p.BatchID
	}
	if p.ClientKey != "" {
		m["client_key"] = p.ClientKey
	}
	if p.ApprovedBy != "" {
		m["approved_by"] = p.ApprovedBy
	}
	if p.Ref != "" && p.AmountCents == 0 { // a tip added afterwards points at its card payment
		m["refund_of"] = p.Ref
	}
	if p.RefundOf != "" { // a refund through Square: the payment it gave money back on
		m["refund_of"] = p.RefundOf
	}
	return m
}

// Signed is the payment's effect on the balance (a refund counts negative).
// Summary is the payment as the order's snapshot lists it (what the Pi's
// printed copies read): signed amount and tip, the card's last four, auth
// code, tender, entry and processor reference (Square's payment id first).
func (p Payment) Summary() order.PaymentSummary {
	amt, tip := p.Signed()
	ref := strings.TrimSpace(p.SquarePaymentID)
	if ref == "" {
		ref = strings.TrimSpace(p.Ref)
	}
	s := order.PaymentSummary{Method: MethodOf(p.Tender), AmountCents: amt, Last4: p.Last4, AuthCode: p.AuthCode,
		TipCents: tip, Tender: p.Tender, Entry: p.Entry, Ref: ref, ID: p.ID, Brand: p.Brand}
	if !p.At.IsZero() {
		at := p.At.UTC()
		s.At = &at
	}
	return s
}

func (p Payment) Signed() (amount, tip int64) {
	if p.Refund {
		return -p.AmountCents, -p.TipCents
	}
	return p.AmountCents, p.TipCents
}

// Journal entry actions the Pi writes itself; a terminal can't post them.
const (
	ActionLogin       = "login"
	ActionLoginFailed = "login_failed"
	ActionLogout      = "logout"
	ActionOrderPlaced = "order.placed"
	ActionDiscount    = "discount"
	ActionPayment     = "payment"
	ActionRefund      = "refund"
	ActionVoid        = "void"
	ActionOverride    = "override"
	// ActionPriceChange: a line rung at another price than the menu's
	// {line, item_id, item, size_id?, from_cents, to_cents, drop_pct, approved_by?}.
	ActionPriceChange = "price_change"
	// ActionManagerCheck: POST /api/terminal/manager-check {for, ok, manager_id?}.
	ActionManagerCheck = "manager_check"
	// ActionEmployeeMeal: the Employee meal discount {cents, pct, reason, approved_by?}.
	ActionEmployeeMeal = "employee_meal"
	// ActionComp: a line rung at $0 {line, item_id, item, size_id?, from_cents, approved_by?}.
	ActionComp = "comp"
	// ActionPickedUp: a pickup handed over at the counter {number, source, state}.
	ActionPickedUp = "order.picked_up"
	// ActionDraftTakeOver: TAKE OVER of a draft open on another terminal
	// {draft_id, from_device_id, from_staff_id}.
	ActionDraftTakeOver = "draft.take_over"
	// The driver view (contract §2.10b).
	ActionDriverLinked = "driver.linked"
	ActionDriverClaim  = "driver.claim"
	ActionDriverTip    = "driver.tip"
	ActionDriverClose  = "driver.close"
	ActionDriverReport = "driver.report"
	// Terminal stage 2 (contract 2.4e, 2.10k, 2.10l).
	ActionSyncParked     = "sync.parked"
	ActionSyncRetry      = "sync.retry"
	ActionDrawerOpen     = "drawer.open"
	ActionDrawerPayIn    = "drawer.pay_in"
	ActionDrawerPayOut   = "drawer.pay_out"
	ActionDrawerClose    = "drawer.close"
	ActionDrawerHandover = "drawer.handover"
	ActionZReport        = "z.report"
	// The Manager tab (contract 2.10m): a driver-report print {kind, batch_id,
	// driver_id?, printed, failed} and the day's close from a terminal
	// {batch_id, forced, approved_by?}.
	ActionReportPrint = "report.print"
	ActionBatchClose  = "batch.close"
	// ActionDrawerKick: cash went back (a refund, a void): the terminal opens
	// its drawer {order_id, reason: refund | void, cents} (contract 2.10n).
	ActionDrawerKick = "drawer.kick"
	// ActionPaymentTip: a tip added to a card payment afterwards
	// {payment_id, from_cents, to_cents} (contract 2.10p).
	ActionPaymentTip = "payment.tip"
	// ActionNoSale is the terminal's own no-sale row (it posts it; the Pi
	// adds the open drawer's id and counts them per drawer).
	ActionNoSale = "no_sale"
)

var reserved = map[string]bool{ActionLogin: true, ActionLoginFailed: true, ActionLogout: true, ActionOrderPlaced: true,
	ActionDiscount: true, ActionPayment: true, ActionRefund: true, ActionVoid: true, ActionOverride: true,
	ActionPriceChange: true, ActionManagerCheck: true, ActionEmployeeMeal: true, ActionComp: true, ActionPickedUp: true,
	ActionDraftTakeOver: true,
	ActionDriverLinked:  true, ActionDriverClaim: true, ActionDriverTip: true, ActionDriverClose: true, ActionDriverReport: true,
	ActionSyncParked: true, ActionSyncRetry: true, ActionDrawerOpen: true, ActionDrawerPayIn: true, ActionDrawerPayOut: true,
	ActionDrawerClose: true, ActionDrawerHandover: true, ActionZReport: true, ActionCustomLine: true,
	ActionReportPrint: true, ActionBatchClose: true, ActionDrawerKick: true, ActionPaymentTip: true,
	ActionVoidWaste: true, ActionUnsettled: true, ActionPairingCode: true, ActionPinLockout: true,
	ActionMenuGoneLine: true, ActionPriceKept: true, ActionSessionEnded: true, ActionCouponApplied: true, ActionCouponRemoved: true,
	ActionSendOut: true, ActionDriverBack: true, ActionOrderTime: true,
	ActionPairingAdminCode: true, ActionTerminalPrint: true, ActionSetupCardRefund: true, ActionSquareKeyed: true,
	ActionSquareSession: true}

var actionRe = regexp.MustCompile(`^[a-z][a-z0-9_.]{1,39}$`)

// CheckPostedAction: what a terminal may journal itself (no sale, drawer,
// line edits, price change, ...).
func CheckPostedAction(a string) error {
	if !actionRe.MatchString(a) {
		return errors.New("action must be 2-40 lowercase letters, digits, underscores or dots")
	}
	if reserved[a] {
		return fmt.Errorf("%q is journaled by the Pi itself", a)
	}
	return nil
}

// Entry is one journal row: who did what, on which terminal (device), when.
type Entry struct {
	ID       string          `json:"id"`
	At       time.Time       `json:"at"`
	StaffID  string          `json:"staff_id"`
	DeviceID string          `json:"device_id"`
	Action   string          `json:"action"`
	OrderID  string          `json:"order_id,omitempty"`
	Detail   json.RawMessage `json:"detail"`
}

// Floor events (contract §2.4c) the terminal flow writes.
const (
	EventOrderPlaced    = "order.placed"
	EventOrderPayment   = "order.payment"
	EventOrderVoid      = "order.void"
	EventJournalEntries = "journal.entries"
)

// Errors.
var (
	ErrNoMenu          = errors.New("no counter menu from the server yet")
	ErrNoConfig        = errors.New("no terminal config (staff) from the server yet")
	ErrLoginRequired   = errors.New("log in with your PIN")
	ErrBadPIN          = errors.New("a PIN is 2-4 digits")
	ErrWrongPIN        = errors.New("wrong PIN")
	ErrManagerPINWrong = errors.New("that isn't a manager's PIN")
	ErrNotCounterOrder = errors.New("not a counter order")
	// ErrPaidOnline (agent 2.5.3): an online vendor order with money paid
	// online (all or part) isn't voided at a terminal: the online ordering
	// company refunds it (409 paid_online).
	ErrPaidOnline = errors.New("paid online: the online ordering company refunds it")
	ErrNotPayable      = errors.New("this order can't take a payment")
	// ErrTipCardOnly (the owner, 2026-11-12): a tip goes on a card payment
	// only - a cash or comp payment with a tip is 400 tip_card_only.
	ErrTipCardOnly = errors.New("a tip goes on a card payment only")
	// ErrNotTippable: a tip set afterwards goes on a card payment only (409).
	ErrNotTippable  = errors.New("a tip is added to a card payment only")
	ErrOverBalance  = errors.New("more than the order's balance")
	ErrRefundTooBig = errors.New("more than was paid")
	// ErrGoodwillCounterOnly: a goodwill refund (contract 2.10n) lowers the
	// order's money, which only a terminal's own order may (400).
	ErrGoodwillCounterOnly = errors.New("a goodwill refund is for the terminal's own orders (an online order's money is main's)")
	ErrNotVoidable         = errors.New("this order can't be voided")
	ErrDuplicateKeyUsed    = errors.New("that client_key belongs to another order")
	ErrDriverOnly          = errors.New("only a driver can do this")
	ErrNotYourOrder        = errors.New("that isn't one of your orders")
	ErrDriverOut           = errors.New("you are out on a run: close your orders first")
)

// ManagerRequired: the action is over the staff member's limit and needs a
// manager's PIN in the same request (manager_pin).
type ManagerRequired struct {
	Action    string // discount | employee_meal | refund | card_refund | cash_for_card | void | price_change | comp | pay_out | drawer_close | close_batch
	Limit     int64  // cents (refund, void, cash_for_card) or percent (discount, price_change)
	Requested int64
}

// Manager-only refund actions (the owner, 2026-11-12).
const (
	// NeedCardRefund: money back to a card (card, Square app, manual entry),
	// any amount.
	NeedCardRefund = "card_refund"
	// NeedCashForCard: cash given back for more than the order took in cash
	// (a card sale refunded from the drawer).
	NeedCashForCard = "cash_for_card"
	// NeedCloseBatch: the day's close (Close batch = the Z) from a terminal.
	NeedCloseBatch = "close_batch"
	// NeedRefundEarlierDay: a refund on an earlier day's order (not carried):
	// a manager's (the owner, 2026-11-12; money-rules R24/R25/R33-R36). It
	// lands in today's batch, drawer and Z.
	NeedRefundEarlierDay = "refund_earlier_day"
)

// EarlierDayRefundNeed is what a refund on an earlier day's order needs: a
// manager (or anyone main made unlimited) gives it alone, anyone else gets
// manager_required refund_earlier_day for the whole refund (amount + tip).
// The other refund rules (RefundRefused) still apply first.
func EarlierDayRefundNeed(c *Config, st Staff, refunds []Payment) *ManagerRequired {
	if c.LimitsOf(st).Unlimited {
		return nil
	}
	var n int64
	for _, r := range refunds {
		n += r.AmountCents + r.TipCents
	}
	return &ManagerRequired{Action: NeedRefundEarlierDay, Requested: n}
}

// TipAllowed: a payment may carry a tip only on a card (card, Square app,
// manually entered); a refund gives back what it gives back.
func TipAllowed(p Payment) bool { return p.Refund || p.TipCents == 0 || IsCard(p.Tender) }

// CashierRefunded is what the order's refunds gave back without a manager
// (amount + tip): refunds no manager approved, made by someone with a limit
// (a manager's own refunds never count against a cashier's limit), comps
// reversed aside (they move no money: contract/money-rules.json R29).
func CashierRefunded(c *Config, paid []Payment) int64 {
	var n int64
	for _, p := range paid {
		if !p.Refund || p.ApprovedBy != "" || p.Tender == TenderComp {
			continue
		}
		if st, ok := c.Find(p.StaffID); ok && c.LimitsOf(st).Unlimited {
			continue
		}
		n += p.AmountCents + p.TipCents
	}
	return n
}

// RefundRefused is why a refund can't be made at all, for anyone (nil =
// it can): more back than the order holds (amount or tip: 409
// refund_too_big), or more comp back than was comped (a comp reversed is
// free; past the comp it would turn paid money into a comp:
// contract/money-rules.json R14).
func RefundRefused(paid []Payment, r Payment) error {
	var amount, tip, comp int64
	for _, p := range paid {
		a, t := p.Signed()
		amount += a
		tip += t
		if p.Tender == TenderComp {
			comp += a + t
		}
	}
	if r.AmountCents > amount || r.TipCents > tip {
		return fmt.Errorf("%w: %d paid", ErrRefundTooBig, amount)
	}
	if r.Tender == TenderComp && r.AmountCents+r.TipCents > comp {
		return fmt.Errorf("%w: %d comped", ErrRefundTooBig, max(comp, 0))
	}
	return nil
}

// PayableBatch: an order takes money only in the open batch, or while it is
// carried past its close (contract 2.10s §4); an earlier batch's is refused
// (409 not_payable) - except a refund with a manager (EarlierDayRefundNeed,
// 2026-11-12).
func PayableBatch(inOpenBatch, carried bool) bool { return inOpenBatch || carried }

// RefundNeed is what a manager must approve for refunds about to be made
// on an order (nil = nothing), the owner's rules of 2026-11-12: a cashier
// refunds cash only, on the open batch's orders (older ones are never
// payable), up to the cashier limit for the whole order (the refunds before
// count: several small refunds can't go past it); a card refund (any
// amount) and cash given back for a card sale need a manager. A manager (or
// anyone main made unlimited) needs no one. A comp's refund moves no money
// and is free. overLimit names the action for the limit (refund | void).
func RefundNeed(c *Config, st Staff, paid, refunds []Payment, overLimit string) *ManagerRequired {
	lim := c.LimitsOf(st)
	if lim.Unlimited {
		return nil
	}
	var card, cash, cashNet int64
	for _, r := range refunds {
		switch {
		case IsCard(r.Tender):
			card += r.AmountCents + r.TipCents
		case r.Tender == TenderCash:
			cash += r.AmountCents + r.TipCents
		}
	}
	if card > 0 {
		return &ManagerRequired{Action: NeedCardRefund, Requested: card}
	}
	if cash <= 0 {
		return nil
	}
	for _, p := range paid {
		if p.Tender == TenderCash {
			a, t := p.Signed()
			cashNet += a + t
		}
	}
	if cash > cashNet {
		return &ManagerRequired{Action: NeedCashForCard, Limit: max(cashNet, 0), Requested: cash}
	}
	if total := CashierRefunded(c, paid) + cash; total > lim.RefundCents {
		return &ManagerRequired{Action: overLimit, Limit: lim.RefundCents, Requested: total}
	}
	return nil
}

func (e *ManagerRequired) Error() string {
	return fmt.Sprintf("%s over the limit (%d > %d): a manager's PIN is needed", e.Action, e.Requested, e.Limit)
}

// Throttled: too many wrong PINs on this terminal.
type Throttled struct{ Wait time.Duration }

func (e *Throttled) Error() string { return "too many wrong PINs - wait and try again" }

// CartInvalid: the pricing engine refused the cart.
type CartInvalid struct{ Issues []pricing.Issue }

func (e *CartInvalid) Error() string {
	if len(e.Issues) > 0 {
		return e.Issues[0].Message
	}
	return "the cart can't be ordered as it is"
}

// ErrAlreadyVoided: the order was voided before (a repeated void answers
// with it as it is).
var ErrAlreadyVoided = errors.New("already voided")

// VoidReason is cancel_reason on a voided counter order.
const VoidReason = "void"

// WasteReason is cancel_reason on a counter order thrown away unpaid (Void
// waste, contract 2.10s §5; its reason text is in the journal).
const WasteReason = "waste"

// NewCounterOrder is a counter order to store: the snapshot the Pi built (no
// number yet: the repository takes it from the counter sequence in the same
// transaction), its payments and journal rows.
type NewCounterOrder struct {
	Snapshot  order.Snapshot
	ClientKey string
	// RequestHash: the SEND's fingerprint (contract 2.10s key_reused).
	RequestHash  string
	CounterStart int64
	PrepMinutes  int64
	Payments     []Payment
	Journal      []Entry
	StaffID      string
	DeviceID     string
	Now          time.Time
}

// MoneyCheck decides, inside the payment / void transaction, what is written
// given the order and its payments so far (balance, refund limits): the
// payments (refunds) and the journal rows.
type MoneyCheck func(so order.StoredOrder, paid []Payment) ([]Payment, []Entry, error)
