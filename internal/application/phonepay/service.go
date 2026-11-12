// Package phonepay runs "Send to phone" (contract/agent.md §2.10z): a
// terminal sends a card charge to a cashier's phone, the phone page charges
// it with Square's Point of Sale app and posts Square's answer back, and the
// Pi records it on the order as a square_app payment (as the person who sent
// it). Money Square took is never dropped: an answer that can't be recorded
// leaves the charge "unrecorded" for Record it / a manager.
package phonepay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	terminalApp "quiccpos/agent/internal/application/terminal"
	"quiccpos/agent/internal/domain/device"
	"quiccpos/agent/internal/domain/order"
	pp "quiccpos/agent/internal/domain/phonepay"
	"quiccpos/agent/internal/domain/terminal"
	"quiccpos/agent/internal/safego"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
)

// RPC messages (agent → main, contract main §2.5zf).
const (
	MsgPosConfig     = "square.pos_config"
	MsgPosLookup     = "square.pos_lookup"
	MsgPosFakeCharge = "square.pos_fake_charge"
)

// Square modes the phone page runs in.
const (
	ModeSquare = "square"
	ModeFake   = "fake"
	ModeOff    = "off"
)

// EventTerminal is the /api/events frame terminals follow.
const EventTerminal = "terminal.phone_charge"

// EventPhones: a phone paired, unpaired, opened or closed its page.
const EventPhones = "terminal.phones.changed"

// ActionPhoneCharge journals every step of a charge (a Pi action).
const ActionPhoneCharge = terminal.ActionPhoneCharge

// Sender is the duplex RPC (rpc.Registry.Send).
type Sender interface {
	Send(ctx context.Context, msgType string, payload any) (int, json.RawMessage, error)
}

// Terminal is what the charge needs of the terminal service.
type Terminal interface {
	CurrentStaff(a terminalApp.Actor) (terminal.Staff, error)
	RequireManager(ctx context.Context, a terminalApp.Actor, pin string, approvals []string, action string) (terminal.Staff, error)
	Order(ctx context.Context, id string) (*terminalApp.OrderView, error)
	RecordAs(ctx context.Context, a terminalApp.Actor, staffID, orderID string, in terminalApp.PaymentIn) (*terminalApp.PaymentResult, error)
	RecordPi(ctx context.Context, deviceID, action string, d map[string]any)
	Store() *terminalApp.Store
}

// Devices lists paired devices.
type Devices interface {
	List(ctx context.Context) ([]device.Device, error)
}

// Deps wires the service.
type Deps struct {
	Repo    pp.Repo
	Term    Terminal
	Devices Devices
	// RPC returns the registry or nil (an unlinked Pi).
	RPC     func() Sender
	Publish func(typ string, v any)
	// AppEnv: the fake Square POS works only outside prod.
	AppEnv string
	Logger zerolog.Logger
	Now    func() time.Time
}

// Service is Send to phone.
type Service struct {
	d     Deps
	hub   *hub
	locks sync.Map // charge id -> *sync.Mutex
	newMu sync.Mutex

	cfgMu  sync.Mutex
	cfg    SquareConfig
	cfgAt  time.Time
	cfgTTL time.Duration
}

func New(d Deps) *Service {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Publish == nil {
		d.Publish = func(string, any) {}
	}
	return &Service{d: d, hub: newHub()}
}

func (s *Service) lock(id string) func() {
	m, _ := s.locks.LoadOrStore(id, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// ---- Square's set-up (main's square.pos_config) ---------------------------

// SquareConfig is what the phone page needs to open Square.
type SquareConfig struct {
	Mode        string `json:"mode"`
	AppID       string `json:"app_id,omitempty"`
	CallbackURL string `json:"callback_url,omitempty"`
	LocationID  string `json:"location_id,omitempty"`
	Environment string `json:"environment,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

// FakeAllowed: the fake Square POS never runs on a prod Pi.
func (s *Service) FakeAllowed() bool {
	switch strings.ToLower(strings.TrimSpace(s.d.AppEnv)) {
	case "prod", "production", "prd":
		return false
	}
	return true
}

// Square is the current set-up (cached 5 min, 30 s after a failure).
func (s *Service) Square(ctx context.Context) SquareConfig {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	now := s.d.Now()
	if !s.cfgAt.IsZero() && now.Sub(s.cfgAt) < s.cfgTTL {
		return s.cfg
	}
	cfg, ttl := s.fetchSquare(ctx)
	s.cfg, s.cfgAt, s.cfgTTL = cfg, now, ttl
	return cfg
}

func (s *Service) fetchSquare(ctx context.Context) (SquareConfig, time.Duration) {
	fallback := func(reason string) (SquareConfig, time.Duration) {
		if s.FakeAllowed() {
			return SquareConfig{Mode: ModeFake, Reason: reason}, 30 * time.Second
		}
		return SquareConfig{Mode: ModeOff, Reason: reason}, 30 * time.Second
	}
	rpc := s.rpc()
	if rpc == nil {
		return fallback("not_linked")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	status, raw, err := rpc.Send(ctx, MsgPosConfig, map[string]any{})
	if err != nil {
		return fallback("main_unreachable")
	}
	if status != http.StatusOK {
		return fallback("main_unsupported")
	}
	var r struct {
		AppID       string `json:"app_id"`
		Environment string `json:"environment"`
		CallbackURL string `json:"callback_url"`
		Linked      bool   `json:"linked"`
		LocationID  string `json:"location_id"`
	}
	_ = json.Unmarshal(raw, &r)
	cfg := SquareConfig{AppID: r.AppID, CallbackURL: r.CallbackURL, LocationID: r.LocationID, Environment: r.Environment}
	switch {
	case r.Environment == "production" && r.AppID != "" && r.CallbackURL != "" && r.Linked:
		cfg.Mode = ModeSquare
		return cfg, 5 * time.Minute
	case r.Environment == "production" && !r.Linked:
		cfg.Reason = "square_not_linked"
	case r.Environment == "production":
		cfg.Reason = "no_callback_url"
	default:
		cfg.Reason = "no_sandbox" // Square's POS API has no sandbox: the fake stands in
	}
	if s.FakeAllowed() {
		cfg.Mode = ModeFake
	} else {
		cfg.Mode = ModeOff
	}
	return cfg, 5 * time.Minute
}

func (s *Service) rpc() Sender {
	if s.d.RPC == nil {
		return nil
	}
	return s.d.RPC()
}

// TimeoutSeconds is main's phone_charge_timeout_seconds (the owner's X,
// default 180, 30-1800).
func (s *Service) TimeoutSeconds() int {
	n := pp.DefaultTimeoutSeconds
	st := s.d.Term.Store()
	if st == nil {
		return n
	}
	if cfg, _ := st.Config(); cfg != nil && len(cfg.Settings) > 0 {
		var v struct {
			T *int `json:"phone_charge_timeout_seconds"`
		}
		if json.Unmarshal(cfg.Settings, &v) == nil && v.T != nil && *v.T > 0 {
			n = min(max(*v.T, pp.MinTimeoutSeconds), pp.MaxTimeoutSeconds)
		}
	}
	return n
}

// ---- errors with a shape ----------------------------------------------------

// Refusal is a 4xx with a code and extra fields.
type Refusal struct {
	Status  int
	Code    string
	Message string
	Extra   map[string]any
}

func (r *Refusal) Error() string { return r.Message }

func refuse(status int, code, msg string, extra map[string]any) *Refusal {
	return &Refusal{Status: status, Code: code, Message: msg, Extra: extra}
}

var keyRe = regexp.MustCompile(`^[A-Za-z0-9_.:-]{8,64}$`)

// ---- phones ----------------------------------------------------------------

// Phone is one paired cashier phone as the terminal sees it.
type Phone struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	Online       bool       `json:"online"`
	LastSeenAt   *time.Time `json:"last_seen_at,omitempty"`
	BusyChargeID string     `json:"busy_charge_id,omitempty"`
}

// Phones lists the paired phones.
func (s *Service) Phones(ctx context.Context) ([]Phone, error) {
	all, err := s.d.Devices.List(ctx)
	if err != nil {
		return nil, err
	}
	open, err := s.d.Repo.OpenFor(ctx, "")
	if err != nil {
		return nil, err
	}
	busy := map[string]string{}
	for _, c := range open {
		if pp.Open(c.Status) {
			busy[c.PhoneID] = c.ID
		}
	}
	out := []Phone{}
	for _, d := range all {
		if d.Kind != device.KindPhone || d.RevokedAt != nil {
			continue
		}
		out = append(out, Phone{ID: d.ID, Name: d.Name, Online: s.hub.online(d.ID), LastSeenAt: d.LastSeenAt, BusyChargeID: busy[d.ID]})
	}
	return out, nil
}

func (s *Service) phone(ctx context.Context, id string) (*device.Device, error) {
	all, err := s.d.Devices.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, d := range all {
		if d.ID == id && d.Kind == device.KindPhone && d.RevokedAt == nil {
			return &d, nil
		}
	}
	return nil, refuse(http.StatusNotFound, "unknown_phone", "That phone isn't paired (any more).", nil)
}

// PhoneCode mints nothing itself: it checks the manager need for the
// terminal's Pair a phone (the transport mints the code).
func (s *Service) PhoneCodeAllowed(ctx context.Context, a terminalApp.Actor, pin string, approvals []string) (terminal.Staff, error) {
	return s.d.Term.RequireManager(ctx, a, pin, approvals, terminalApp.ActionPairPhone)
}

// ---- create ----------------------------------------------------------------

// CreateIn is POST /api/terminal/orders/{id}/phone-charges.
type CreateIn struct {
	ClientKey   string `json:"client_key"`
	PhoneID     string `json:"phone_id"`
	AmountCents int64  `json:"amount_cents"`
	TipCents    int64  `json:"tip_cents"`
}

// Create sends a charge to a phone. dup = the same client_key again.
func (s *Service) Create(ctx context.Context, a terminalApp.Actor, orderID string, in CreateIn) (pp.Charge, bool, error) {
	st, err := s.d.Term.CurrentStaff(a)
	if err != nil {
		return pp.Charge{}, false, err
	}
	in.ClientKey, in.PhoneID = strings.TrimSpace(in.ClientKey), strings.TrimSpace(in.PhoneID)
	if in.ClientKey == "" {
		return pp.Charge{}, false, refuse(http.StatusBadRequest, "client_key_required", "client_key is required", nil)
	}
	if !keyRe.MatchString(in.ClientKey) {
		return pp.Charge{}, false, refuse(http.StatusBadRequest, "bad_request", "client_key is 8-64 letters, digits, _ . : -", nil)
	}
	if in.AmountCents < 1 || in.AmountCents > 1_000_000 || in.TipCents < 0 || in.TipCents > 100_000 {
		return pp.Charge{}, false, refuse(http.StatusBadRequest, "bad_request", "amount_cents is 1-1000000, tip_cents 0-100000", nil)
	}
	if _, err := uuid.Parse(orderID); err != nil {
		return pp.Charge{}, false, refuse(http.StatusNotFound, "not_found", "no such order", nil)
	}
	hash := pp.Fingerprint(orderID, in.PhoneID, in.AmountCents, in.TipCents)
	s.newMu.Lock()
	defer s.newMu.Unlock()
	if prev, err := s.d.Repo.ByKey(ctx, in.ClientKey); err == nil {
		if prev.RequestHash != hash {
			return pp.Charge{}, false, refuse(http.StatusConflict, "key_reused", "this client_key was used for another charge",
				map[string]any{"client_key": in.ClientKey, "order_id": prev.OrderID})
		}
		return prev, true, nil
	} else if !errors.Is(err, pp.ErrNotFound) {
		return pp.Charge{}, false, err
	}
	sq := s.Square(ctx)
	if sq.Mode == ModeOff {
		return pp.Charge{}, false, refuse(http.StatusConflict, "square_off", "Send to phone isn't set up for this store.", map[string]any{"reason": sq.Reason})
	}
	ov, err := s.d.Term.Order(ctx, orderID)
	if errors.Is(err, order.ErrNotFound) {
		return pp.Charge{}, false, refuse(http.StatusNotFound, "not_found", "no such order", nil)
	}
	if err != nil {
		return pp.Charge{}, false, err
	}
	so := ov.Order
	if so.Status.State == order.StateCancelled {
		return pp.Charge{}, false, refuse(http.StatusConflict, "not_payable", "This order was cancelled.", nil)
	}
	if due := terminal.DueOf(so); in.AmountCents > due {
		return pp.Charge{}, false, refuse(http.StatusConflict, "over_balance", fmt.Sprintf("The order owes %s.", dollars(due)),
			map[string]any{"balance_cents": due})
	}
	ph, err := s.phone(ctx, in.PhoneID)
	if err != nil {
		return pp.Charge{}, false, err
	}
	open, err := s.d.Repo.OpenFor(ctx, ph.ID)
	if err != nil {
		return pp.Charge{}, false, err
	}
	for _, c := range open {
		if pp.Open(c.Status) {
			return pp.Charge{}, false, refuse(http.StatusConflict, "phone_busy", ph.Name+" already has a charge waiting.",
				map[string]any{"charge_id": c.ID, "order_id": c.OrderID})
		}
	}
	now := s.d.Now().UTC().Truncate(time.Millisecond)
	id := uuid.Must(uuid.NewV7()).String()
	var number int64
	if so.Status.KitchenNumber != nil {
		number = *so.Status.KitchenNumber
	}
	code := pp.CodeOf(id)
	c := pp.Charge{ID: id, ClientKey: in.ClientKey, RequestHash: hash, OrderID: orderID, Number: number,
		PhoneID: ph.ID, PhoneName: ph.Name, DeviceID: a.DeviceID, DeviceName: a.DeviceName, StaffID: st.ID, StaffName: st.Name(),
		AmountCents: in.AmountCents, TipCents: in.TipCents, Code: code, Note: pp.NoteOf(number, code),
		Status: pp.StatusPending, CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Duration(s.TimeoutSeconds()) * time.Second)}
	got, dup, err := s.d.Repo.Create(ctx, c)
	if err != nil {
		return pp.Charge{}, false, err
	}
	if dup { // raced another create with this key
		return got, got.RequestHash == hash, nil
	}
	s.journal(ctx, c, "sent", nil)
	s.d.Logger.Info().Ctx(ctx).Str("charge_id", c.ID).Str("order_id", orderID).Int64("number", number).Str("phone_id", ph.ID).
		Str("device_id", a.DeviceID).Str("staff_id", st.ID).Int64("amount_cents", c.AmountCents).Int64("tip_cents", c.TipCents).
		Msg("Phone charge sent")
	s.changed(c)
	return c, false, nil
}

func dollars(c int64) string { return fmt.Sprintf("$%d.%02d", c/100, c%100) }

// ---- reads -----------------------------------------------------------------

func (s *Service) Get(ctx context.Context, id string) (pp.Charge, error) {
	c, err := s.d.Repo.Get(ctx, id)
	if errors.Is(err, pp.ErrNotFound) {
		return c, refuse(http.StatusNotFound, "not_found", "no such charge", nil)
	}
	return c, err
}

// Recent is the newest charges (the Pi page).
func (s *Service) Recent(ctx context.Context, limit int) ([]pp.Charge, error) {
	return s.d.Repo.Recent(ctx, limit)
}

func (s *Service) ForOrder(ctx context.Context, orderID string) ([]pp.Charge, error) {
	return s.d.Repo.ForOrder(ctx, orderID)
}

// PhoneCharges are what a phone shows: its open charges and an unrecorded
// one of the last 12 h.
func (s *Service) PhoneCharges(ctx context.Context, phoneID string) ([]pp.Charge, error) {
	all, err := s.d.Repo.OpenFor(ctx, phoneID)
	if err != nil {
		return nil, err
	}
	out := []pp.Charge{}
	cut := s.d.Now().Add(-12 * time.Hour)
	for _, c := range all {
		if c.Status == pp.StatusUnrecorded && c.UpdatedAt.Before(cut) {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

func (s *Service) mine(ctx context.Context, phoneID, id string) (pp.Charge, error) {
	c, err := s.Get(ctx, id)
	if err != nil {
		return c, err
	}
	if c.PhoneID != phoneID {
		return c, refuse(http.StatusNotFound, "not_found", "no such charge", nil)
	}
	return c, nil
}

func finalErr(c pp.Charge) *Refusal {
	return refuse(http.StatusConflict, "charge_final", "This charge is already "+strings.ReplaceAll(c.Status, "_", " ")+".",
		map[string]any{"charge": c})
}

// ---- the phone's steps -----------------------------------------------------

// Open: the cashier pressed CHARGE (Square is being opened).
func (s *Service) Open(ctx context.Context, phoneID, id string) (pp.Charge, error) {
	defer s.lock(id)()
	c, err := s.mine(ctx, phoneID, id)
	if err != nil {
		return c, err
	}
	switch c.Status {
	case pp.StatusInApp:
		return c, nil
	case pp.StatusPending:
	default:
		return c, finalErr(c)
	}
	now := s.d.Now().UTC().Truncate(time.Millisecond)
	c.Status, c.OpenedAt, c.UpdatedAt = pp.StatusInApp, &now, now
	if err := s.save(ctx, c); err != nil {
		return c, err
	}
	s.journal(ctx, c, "opened", nil)
	return c, nil
}

// Cancel: either side calls it off. by = phone | terminal.
func (s *Service) Cancel(ctx context.Context, phoneID, id, by string) (pp.Charge, error) {
	defer s.lock(id)()
	var c pp.Charge
	var err error
	if phoneID != "" {
		c, err = s.mine(ctx, phoneID, id)
	} else {
		c, err = s.Get(ctx, id)
	}
	if err != nil {
		return c, err
	}
	switch {
	case c.Status == pp.StatusCancelled:
		return c, nil
	case pp.Open(c.Status), c.Status == pp.StatusUnrecorded && c.TransactionID == "":
	default:
		return c, finalErr(c)
	}
	s.finish(&c, pp.StatusCancelled, "")
	if err := s.save(ctx, c); err != nil {
		return c, err
	}
	s.journal(ctx, c, "cancelled", map[string]any{"by": by})
	return c, nil
}

// Result is Square's answer, from the phone.
func (s *Service) Result(ctx context.Context, phoneID, id string, r pp.Result) (pp.Charge, bool, error) {
	r, err := r.Normalize()
	if err != nil {
		return pp.Charge{}, false, refuse(http.StatusBadRequest, "bad_request", err.Error(), nil)
	}
	defer s.lock(id)()
	c, err := s.mine(ctx, phoneID, id)
	if err != nil {
		return c, false, err
	}
	switch r.Status {
	case pp.ResultOK, pp.ResultWentThrough:
		if c.Status == pp.StatusPaid {
			return c, true, nil
		}
		late := pp.Final(c.Status)
		c, err = s.record(ctx, c, r.TransactionID, r.ClientTransactionID, r.Status == pp.ResultWentThrough, late)
		return c, false, err
	case pp.ResultError:
		if c.Status == pp.StatusDeclined && c.ErrorCode == r.ErrorCode {
			return c, true, nil
		}
		if !pp.Open(c.Status) {
			return c, false, finalErr(c)
		}
		c.ErrorCode = r.ErrorCode
		s.finish(&c, pp.StatusDeclined, pp.WordsFor(r.ErrorCode))
	case pp.ResultCancelled:
		if c.Status == pp.StatusCancelled {
			return c, true, nil
		}
		if !pp.Open(c.Status) {
			return c, false, finalErr(c)
		}
		c.ErrorCode = r.ErrorCode
		s.finish(&c, pp.StatusCancelled, "Cancelled in Square: nothing was charged.")
	}
	if err := s.save(ctx, c); err != nil {
		return c, false, err
	}
	s.journal(ctx, c, c.Status, map[string]any{"error_code": c.ErrorCode})
	return c, false, nil
}

// Record is the terminal's Record it: find the charge in Square and record.
func (s *Service) Record(ctx context.Context, id string) (pp.Charge, error) {
	defer s.lock(id)()
	c, err := s.Get(ctx, id)
	if err != nil {
		return c, err
	}
	if c.Status == pp.StatusPaid {
		return c, nil
	}
	lk, avail := s.lookup(ctx, c, c.TransactionID)
	switch {
	case !avail.ok:
		return c, refuse(http.StatusConflict, "square_unavailable", "Can't check Square from here: use the Square app tender (the last 4) instead.",
			map[string]any{"reason": avail.reason})
	case lk == nil:
		return c, refuse(http.StatusConflict, "not_found_in_square", "Square doesn't show this charge: charge again or use another tender.", nil)
	}
	return s.recordFound(ctx, c, c.TransactionID, c.ClientTransactionID, lk, pp.Final(c.Status))
}

// ---- recording -------------------------------------------------------------

// Found is a payment main found in Square.
type Found struct {
	ID            string `json:"id"`
	TransactionID string `json:"transaction_id"`
	Status        string `json:"status"`
	Brand         string `json:"brand"`
	Last4         string `json:"last4"`
	Entry         string `json:"entry"`
	AmountCents   int64  `json:"amount_cents"`
	TipCents      int64  `json:"tip_cents"`
	TotalCents    int64  `json:"total_cents"`
	ReceiptNumber string `json:"receipt_number"`
}

type availability struct {
	ok     bool
	reason string
}

// lookup asks main (square.pos_lookup, <= 5 s). nil + ok = not found.
func (s *Service) lookup(ctx context.Context, c pp.Charge, txID string) (*Found, availability) {
	rpc := s.rpc()
	if rpc == nil {
		return nil, availability{reason: "not_linked"}
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	payload := map[string]any{"total_cents": c.TotalCents(), "since": c.CreatedAt.Add(-2 * time.Minute).UTC()}
	if txID != "" {
		payload["transaction_id"] = txID
	} else {
		payload["code"] = c.Code
	}
	status, raw, err := rpc.Send(ctx, MsgPosLookup, payload)
	if err != nil {
		return nil, availability{reason: "main_unreachable"}
	}
	if status != http.StatusOK {
		return nil, availability{reason: "main_unsupported"}
	}
	var r struct {
		Found   bool   `json:"found"`
		Code    string `json:"code"`
		Payment Found  `json:"payment"`
	}
	if json.Unmarshal(raw, &r) != nil {
		return nil, availability{reason: "main_unsupported"}
	}
	if r.Found {
		return &r.Payment, availability{ok: true}
	}
	if r.Code != "" && r.Code != "not_found" {
		return nil, availability{reason: r.Code}
	}
	return nil, availability{ok: true}
}

// record handles an ok / went_through answer.
func (s *Service) record(ctx context.Context, c pp.Charge, txID, clientTx string, wentThrough, late bool) (pp.Charge, error) {
	if txID != "" {
		c.TransactionID = txID
	}
	if clientTx != "" {
		c.ClientTransactionID = clientTx
	}
	lk, avail := s.lookup(ctx, c, c.TransactionID)
	if lk == nil && c.TransactionID == "" && c.ClientTransactionID == "" || lk == nil && wentThrough && c.TransactionID == "" {
		// The cashier says it went through; Square doesn't show it (yet).
		msg := "Square doesn't show this charge yet. If the Square app shows it approved, press Record it on the terminal."
		if !avail.ok {
			msg = "Can't check Square from here. If the Square app shows it approved, record it with the Square app tender (the last 4)."
		}
		c.Status, c.Message, c.UpdatedAt = pp.StatusUnrecorded, msg, s.d.Now().UTC().Truncate(time.Millisecond)
		if err := s.save(ctx, c); err != nil {
			return c, err
		}
		s.journal(ctx, c, "unrecorded", map[string]any{"why": "not_found", "lookup": avail.reason})
		return c, nil
	}
	return s.recordFound(ctx, c, c.TransactionID, c.ClientTransactionID, lk, late)
}

// recordFound records the payment (lk nil = Square's word without card
// details) and finishes the charge: paid, or unrecorded with the reason.
func (s *Service) recordFound(ctx context.Context, c pp.Charge, txID, clientTx string, lk *Found, late bool) (pp.Charge, error) {
	ref := txID
	if ref == "" {
		ref = clientTx
	}
	if lk != nil {
		if lk.TransactionID != "" && c.TransactionID == "" {
			c.TransactionID = lk.TransactionID
		}
		c.SquarePaymentID, c.Brand, c.Last4, c.Entry = lk.ID, normBrand(lk.Brand), lastFour(lk.Last4), normEntry(lk.Entry)
		c.ExtraTipCents = max(lk.TipCents, 0)
		if lk.ID != "" {
			ref = lk.ID
		}
	}
	if len(ref) > 120 {
		ref = ref[:120]
	}
	in := terminalApp.PaymentIn{ClientKey: c.ClientKey, Tender: terminal.TenderSquareApp, AmountCents: c.AmountCents,
		TipCents: min(c.TipCents+c.ExtraTipCents, 100_000), Ref: ref, Last4: c.Last4, Brand: c.Brand}
	a := terminalApp.Actor{DeviceID: c.DeviceID, DeviceName: c.DeviceName}
	res, err := s.d.Term.RecordAs(ctx, a, c.StaffID, c.OrderID, in)
	if err != nil {
		if p := s.paidWithKey(ctx, c); p != nil { // recorded already (a race, or a retry with other details)
			res, err = &terminalApp.PaymentResult{Payment: *p}, nil
		}
	}
	if err != nil {
		var kr *terminal.KeyReused
		if !errors.As(err, &kr) && !isRefusal(err) {
			return c, err // a DB problem: the phone tries again, nothing changed
		}
		c.Status, c.Message, c.UpdatedAt = pp.StatusUnrecorded, unrecordedWords(err, c), s.d.Now().UTC().Truncate(time.Millisecond)
		if serr := s.save(ctx, c); serr != nil {
			return c, serr
		}
		s.journal(ctx, c, "unrecorded", map[string]any{"why": err.Error(), "late": late})
		s.d.Logger.Error().Ctx(ctx).Str("charge_id", c.ID).Str("order_id", c.OrderID).Int64("number", c.Number).
			Int64("amount_cents", c.TotalCents()).Str("error", err.Error()).Msg("Phone charge paid in Square but not recorded")
		return c, nil
	}
	c.PaymentID = res.Payment.ID
	s.finish(&c, pp.StatusPaid, "")
	if err := s.save(ctx, c); err != nil {
		return c, err
	}
	s.journal(ctx, c, "recorded", map[string]any{"payment_id": c.PaymentID, "late": late, "transaction_id": c.TransactionID,
		"square_payment_id": c.SquarePaymentID, "extra_tip_cents": c.ExtraTipCents})
	s.d.Logger.Info().Ctx(ctx).Str("charge_id", c.ID).Str("order_id", c.OrderID).Int64("number", c.Number).
		Int64("amount_cents", c.AmountCents).Int64("tip_cents", c.TipCents+c.ExtraTipCents).Bool("late", late).
		Bool("looked_up", lk != nil).Msg("Phone charge recorded")
	return c, nil
}

func (s *Service) paidWithKey(ctx context.Context, c pp.Charge) *terminal.Payment {
	ov, err := s.d.Term.Order(ctx, c.OrderID)
	if err != nil || ov == nil {
		return nil
	}
	for _, p := range ov.Payments {
		if p.ClientKey == c.ClientKey && !p.Refund {
			return &p
		}
	}
	return nil
}

// isRefusal: a definite "no" from the terminal service (not a DB error).
func isRefusal(err error) bool {
	for _, e := range []error{terminal.ErrOverBalance, terminal.ErrNotPayable, order.ErrNotFound, terminal.ErrLoginRequired} {
		if errors.Is(err, e) {
			return true
		}
	}
	return errors.Is(err, terminalApp.ErrBadRequest)
}

func unrecordedWords(err error, c pp.Charge) string {
	amt := dollars(c.TotalCents() + c.ExtraTipCents)
	switch {
	case errors.Is(err, terminal.ErrOverBalance):
		return "Square took " + amt + " but the order no longer owes it (paid another way). A manager refunds it in Square's dashboard."
	case errors.Is(err, terminal.ErrNotPayable), errors.Is(err, order.ErrNotFound):
		return "Square took " + amt + " but the order can't take a payment (cancelled or an earlier day). A manager refunds it in Square's dashboard."
	}
	return "Square took " + amt + " but the Pi couldn't record it (" + err.Error() + "). Press Record it, or a manager sorts it out."
}

func normBrand(b string) string {
	switch strings.ToUpper(strings.TrimSpace(b)) {
	case "VISA":
		return "visa"
	case "MASTERCARD":
		return "mastercard"
	case "AMERICAN_EXPRESS", "AMEX":
		return "amex"
	case "DISCOVER", "DISCOVER_DINERS":
		return "discover"
	case "":
		return ""
	}
	return "other"
}

func normEntry(e string) string {
	switch strings.ToUpper(strings.TrimSpace(e)) {
	case "CONTACTLESS":
		return "contactless"
	case "EMV":
		return "chip"
	case "SWIPED":
		return "swiped"
	case "KEYED":
		return "keyed"
	}
	return ""
}

func lastFour(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 4 {
		s = s[len(s)-4:]
	}
	return s
}

// Problems: a charge Square took that isn't on an order (the Pi page's
// Needs attention).
func (s *Service) Problems(ctx context.Context) []string {
	open, err := s.d.Repo.OpenFor(ctx, "")
	if err != nil {
		return nil
	}
	for _, c := range open {
		if c.Status == pp.StatusUnrecorded && c.TransactionID != "" {
			return []string{"phone_charge_unrecorded"}
		}
	}
	return nil
}

// ---- expiry ----------------------------------------------------------------

// Sweep expires open charges past their time (the loop calls it every 5 s).
func (s *Service) Sweep(ctx context.Context) {
	open, err := s.d.Repo.OpenFor(ctx, "")
	if err != nil {
		s.d.Logger.Warn().Ctx(ctx).Err(err).Msg("phone charges: sweep failed")
		return
	}
	now := s.d.Now()
	for _, c := range open {
		if !pp.Open(c.Status) || now.Before(c.ExpiresAt) {
			continue
		}
		func() {
			defer s.lock(c.ID)()
			cur, err := s.d.Repo.Get(ctx, c.ID)
			if err != nil || !pp.Open(cur.Status) {
				return
			}
			s.finish(&cur, pp.StatusExpired, "Nobody finished it in time: don't charge it in Square now.")
			if s.save(ctx, cur) == nil {
				s.journal(ctx, cur, "expired", nil)
			}
		}()
	}
}

// Run sweeps until ctx ends.
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = safego.Do(ctx, "phonepay.sweep", func() { s.Sweep(ctx) })
		}
	}
}

// ---- fake Square ------------------------------------------------------------

// FakeCharge makes a transaction in main's fake Square (dev); ok false =
// main isn't fake (a made-up id is used).
func (s *Service) FakeCharge(ctx context.Context, totalCents int64, note string) (txID, clientTx string, ok bool) {
	if rpc := s.rpc(); rpc != nil {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if status, raw, err := rpc.Send(ctx, MsgPosFakeCharge, map[string]any{"total_cents": totalCents, "note": note}); err == nil && status == http.StatusOK {
			var r struct {
				OK            bool   `json:"ok"`
				TransactionID string `json:"transaction_id"`
				ClientTx      string `json:"client_transaction_id"`
			}
			if json.Unmarshal(raw, &r) == nil && r.OK && r.TransactionID != "" {
				return r.TransactionID, r.ClientTx, true
			}
		}
	}
	return "fake-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:20], "fakeclient-" + uuid.NewString()[:8], false
}

// ---- plumbing --------------------------------------------------------------

func (s *Service) finish(c *pp.Charge, status, msg string) {
	now := s.d.Now().UTC().Truncate(time.Millisecond)
	c.Status, c.Message, c.UpdatedAt, c.FinishedAt = status, msg, now, &now
}

func (s *Service) save(ctx context.Context, c pp.Charge) error {
	if err := s.d.Repo.Update(ctx, c); err != nil {
		return err
	}
	s.changed(c)
	return nil
}

func (s *Service) changed(c pp.Charge) {
	s.d.Publish(EventTerminal, map[string]any{"charge": c})
	s.hub.send(c.PhoneID, c)
}

func (s *Service) journal(ctx context.Context, c pp.Charge, status string, extra map[string]any) {
	d := map[string]any{"charge_id": c.ID, "order_id": c.OrderID, "number": c.Number, "phone_id": c.PhoneID, "phone": c.PhoneName,
		"amount_cents": c.AmountCents, "tip_cents": c.TipCents, "status": status, "staff_id": c.StaffID}
	for k, v := range extra {
		d[k] = v
	}
	s.d.Term.RecordPi(ctx, c.DeviceID, ActionPhoneCharge, d)
}

// Subscribe follows one phone's charges (the phone page's SSE); the
// terminals hear that the phone is online / offline.
func (s *Service) Subscribe(phoneID string) (<-chan pp.Charge, func()) {
	ch, stop := s.hub.sub(phoneID)
	s.PhonesChanged()
	return ch, func() { stop(); s.PhonesChanged() }
}

// PhonesChanged tells the terminals to read GET /api/terminal/phones again.
func (s *Service) PhonesChanged() { s.d.Publish(EventPhones, map[string]any{}) }

// ---- the phone hub -----------------------------------------------------------

type hub struct {
	mu   sync.Mutex
	subs map[string]map[chan pp.Charge]struct{}
}

func newHub() *hub { return &hub{subs: map[string]map[chan pp.Charge]struct{}{}} }

func (h *hub) sub(phoneID string) (<-chan pp.Charge, func()) {
	ch := make(chan pp.Charge, 16)
	h.mu.Lock()
	if h.subs[phoneID] == nil {
		h.subs[phoneID] = map[chan pp.Charge]struct{}{}
	}
	h.subs[phoneID][ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs[phoneID], ch)
		h.mu.Unlock()
	}
}

func (h *hub) send(phoneID string, c pp.Charge) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[phoneID] {
		select {
		case ch <- c:
		default: // a stuck page reloads from /api/phone/me
		}
	}
}

func (h *hub) online(phoneID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs[phoneID]) > 0
}
