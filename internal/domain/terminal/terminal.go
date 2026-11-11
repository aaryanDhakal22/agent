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
	if c == nil || c.parsed.CounterStart <= 0 {
		return DefaultCounterStart
	}
	return c.parsed.CounterStart
}

// LimitsOf is a staff member's limits: a manager is unlimited; otherwise
// their own, else the role's default, else none (0 / 0).
func (c *Config) LimitsOf(s Staff) Limits {
	if s.Role == RoleManager {
		return Limits{Unlimited: true}
	}
	if e := s.EffectiveLimits; len(e) > 0 {
		if string(e) == "null" {
			return Limits{Unlimited: true}
		}
		var l Limits
		if json.Unmarshal(e, &l) == nil {
			l.Unlimited = false
			return l
		}
	}
	if s.Limits != nil {
		l := *s.Limits
		l.Unlimited = false
		return l
	}
	if c != nil && s.Role == RoleCashier && (c.parsed.CashierRefundCents != nil || c.parsed.CashierDiscountPct != nil) {
		var l Limits
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
	// Routes: bank item id → its kitchen printer (absent = unmatched: the
	// line prints on every station) and the bank's name for the kitchen.
	Routes map[string]Route `json:"routes"`
}

// Route is one bank item's kitchen routing.
type Route struct {
	PrinterID string `json:"printer_id"`
	Name      string `json:"name"`
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
	return map[string]any{"id": p.ID, "method": p.Tender, "amount_cents": amount, "tip_cents": tip, "last4": p.Last4,
		"auth_code": "", "ref": p.Ref, "at": p.At.UTC().Truncate(time.Millisecond), "staff_id": staff}
}

// Signed is the payment's effect on the balance (a refund counts negative).
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
)

var reserved = map[string]bool{ActionLogin: true, ActionLoginFailed: true, ActionLogout: true, ActionOrderPlaced: true,
	ActionDiscount: true, ActionPayment: true, ActionRefund: true, ActionVoid: true, ActionOverride: true}

var actionRe = regexp.MustCompile(`^[a-z][a-z0-9_.]{1,39}$`)

// CheckPostedAction: what a terminal may journal itself (no sale, drawer,
// line edits, price change, ...).
func CheckPostedAction(a string) error {
	if !actionRe.MatchString(a) {
		return errors.New("action is 2-40 lowercase letters, digits, _ or .")
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
	ErrNoMenu           = errors.New("no counter menu from the server yet")
	ErrNoConfig         = errors.New("no terminal config (staff) from the server yet")
	ErrLoginRequired    = errors.New("log in with your PIN")
	ErrBadPIN           = errors.New("a PIN is 2-4 digits")
	ErrWrongPIN         = errors.New("wrong PIN")
	ErrManagerPINWrong  = errors.New("that isn't a manager's PIN")
	ErrNotCounterOrder  = errors.New("not a counter order")
	ErrNotPayable       = errors.New("this order can't take a payment")
	ErrOverBalance      = errors.New("more than the order's balance")
	ErrRefundTooBig     = errors.New("more than was paid")
	ErrNotVoidable      = errors.New("this order can't be voided")
	ErrDuplicateKeyUsed = errors.New("that client_key belongs to another order")
)

// ManagerRequired: the action is over the staff member's limit and needs a
// manager's PIN in the same request (manager_pin).
type ManagerRequired struct {
	Action    string // discount | refund | void
	Limit     int64  // cents (refund, void) or percent (discount)
	Requested int64
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

// NewCounterOrder is a counter order to store: the snapshot the Pi built (no
// number yet: the repository takes it from the counter sequence in the same
// transaction), its payments and journal rows.
type NewCounterOrder struct {
	Snapshot     order.Snapshot
	ClientKey    string
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
