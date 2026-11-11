package terminal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/terminal"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
)

// Repo is the terminal flow's storage (repositories.OrderRepository).
type Repo interface {
	SnapshotRepo
	CounterOrderByKey(ctx context.Context, key string) (*order.StoredOrder, error)
	PlaceCounterOrder(ctx context.Context, in terminal.NewCounterOrder) (order.StoredOrder, bool, error)
	AddCounterPayment(ctx context.Context, orderID, clientKey string, check terminal.MoneyCheck) (order.StoredOrder, terminal.Payment, bool, error)
	VoidCounterOrder(ctx context.Context, id, reason, staffID, deviceID string, check terminal.MoneyCheck) (so, prev order.StoredOrder, refunds []terminal.Payment, returned bool, err error)
	OrderPayments(ctx context.Context, orderID string) ([]terminal.Payment, error)
	CounterOrders(ctx context.Context) ([]order.StoredOrder, error)
	AddJournal(ctx context.Context, entries []terminal.Entry) error
	Journal(ctx context.Context, limit int) ([]terminal.Entry, error)
}

// Orders is what the terminal flow needs from the floor (orderApp.Service).
type Orders interface {
	PrintCounterTickets(ctx context.Context, so order.StoredOrder)
	PrintVoidSlip(ctx context.Context, so order.StoredOrder)
	CounterOrderChanged(ctx context.Context, so order.StoredOrder, prev *order.StoredOrder, driverReturned bool)
	JournalWritten()
	PrepMinutesFor(fulfillment string) int64
	StoreIDs() (tenantID, locationID string)
	StoreLocation() *time.Location
	DevEvent(text string)
}

// Actor is the terminal (device) a request came from; the web-UI session
// counts as one device, "web".
type Actor struct {
	DeviceID   string
	DeviceName string
}

// Service is the terminal API's logic.
type Service struct {
	store  *Store
	repo   Repo
	orders Orders
	pub    Publisher
	logger zerolog.Logger
	staff  *staffBook
	now    func() time.Time
	drv    driverView
}

// NewService wires the terminal flow; the store's config changes reach the
// staff book (sessions of removed staff end).
func NewService(store *Store, repo Repo, orders Orders, pub Publisher, logger zerolog.Logger) *Service {
	s := &Service{store: store, repo: repo, orders: orders, pub: pub, logger: logger.With().Str("module", "terminal").Logger(),
		staff: newStaffBook(), now: time.Now}
	store.OnConfig(s.staff.configChanged)
	return s
}

// Store is the snapshot store.
func (s *Service) Store() *Store { return s.store }

func (s *Service) publish(typ string, v any) {
	if s.pub != nil {
		s.pub.Publish(typ, v)
	}
}

func detail(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("{}")
	}
	return b
}

// --- staff ---------------------------------------------------------------------------

// LoginResult is POST /api/terminal/login's answer.
type LoginResult struct {
	Staff  terminal.Public `json:"staff"`
	Role   string          `json:"role"`
	Limits terminal.Limits `json:"limits"`
	// Settings: the terminal settings (auto-lock minutes etc.) as main sent them.
	Settings json.RawMessage `json:"settings"`
	// Driver: a driver's Pi driver (linked at his first login; §2.10b).
	Driver *DriverPublic `json:"driver,omitempty"`
}

// Login checks a PIN on a terminal; the staff member is then the one on it.
func (s *Service) Login(ctx context.Context, a Actor, pin string) (*LoginResult, error) {
	if !pinRe.MatchString(pin) {
		return nil, terminal.ErrBadPIN
	}
	now := s.now()
	if wait := s.staff.throttled(a.DeviceID, now); wait > 0 {
		return nil, &terminal.Throttled{Wait: wait}
	}
	cfg, _ := s.store.Config()
	if cfg == nil {
		return nil, terminal.ErrNoConfig
	}
	st, ok := s.staff.match(cfg, pin)
	if !ok {
		s.staff.miss(a.DeviceID, now)
		s.journal(ctx, terminal.Entry{DeviceID: a.DeviceID, Action: terminal.ActionLoginFailed, Detail: detail(map[string]any{"device_name": a.DeviceName})})
		return nil, terminal.ErrWrongPIN
	}
	s.staff.hit(a.DeviceID)
	s.staff.login(a.DeviceID, st.ID)
	s.journal(ctx, terminal.Entry{StaffID: st.ID, DeviceID: a.DeviceID, Action: terminal.ActionLogin,
		Detail: detail(map[string]any{"role": st.Role, "device_name": a.DeviceName})})
	res := &LoginResult{Staff: st.Public(), Role: st.Role, Limits: cfg.LimitsOf(st), Settings: cfg.Settings}
	if st.Role == terminal.RoleDriver && s.drv.floor != nil {
		// Link him to the roster now; a failure never blocks the login (the
		// driver routes try again).
		if d, err := s.ensureDriver(ctx, a, st); err != nil {
			s.logger.Warn().Ctx(ctx).Err(err).Str("staff_id", st.ID).Msg("driver link at login failed")
		} else {
			res.Driver = driverPublic(*d)
		}
	}
	return res, nil
}

// Logout ends the staff session on a terminal (the terminal locked).
func (s *Service) Logout(ctx context.Context, a Actor) {
	if id, ok := s.staff.logout(a.DeviceID); ok {
		s.journal(ctx, terminal.Entry{StaffID: id, DeviceID: a.DeviceID, Action: terminal.ActionLogout})
	}
}

// staffOf is the staff member logged in on the terminal.
func (s *Service) staffOf(a Actor) (terminal.Staff, *terminal.Config, error) {
	cfg, _ := s.store.Config()
	id, ok := s.staff.current(a.DeviceID)
	if !ok || cfg == nil {
		return terminal.Staff{}, cfg, terminal.ErrLoginRequired
	}
	st, ok := cfg.Find(id)
	if !ok {
		s.staff.logout(a.DeviceID)
		return terminal.Staff{}, cfg, terminal.ErrLoginRequired
	}
	return st, cfg, nil
}

// manager checks a manager's PIN given to go over a limit (a wrong one
// counts as a wrong PIN on the terminal).
func (s *Service) manager(a Actor, cfg *terminal.Config, pin string, need *terminal.ManagerRequired) (*terminal.Staff, error) {
	if strings.TrimSpace(pin) == "" {
		return nil, need
	}
	now := s.now()
	if wait := s.staff.throttled(a.DeviceID, now); wait > 0 {
		return nil, &terminal.Throttled{Wait: wait}
	}
	if !pinRe.MatchString(pin) {
		return nil, terminal.ErrManagerPINWrong
	}
	st, ok := s.staff.match(cfg, pin)
	if !ok || st.Role != terminal.RoleManager {
		s.staff.miss(a.DeviceID, now)
		return nil, terminal.ErrManagerPINWrong
	}
	s.staff.hit(a.DeviceID)
	return &st, nil
}

func overrideEntry(a Actor, staff terminal.Staff, mgr *terminal.Staff, need *terminal.ManagerRequired, orderID string) terminal.Entry {
	return terminal.Entry{StaffID: staff.ID, DeviceID: a.DeviceID, Action: terminal.ActionOverride, OrderID: orderID,
		Detail: detail(map[string]any{"for": need.Action, "manager_id": mgr.ID, "limit": need.Limit, "requested": need.Requested})}
}

// --- journal ---------------------------------------------------------------------------

// journal writes Pi-side rows outside an order transaction (login, lock).
func (s *Service) journal(ctx context.Context, entries ...terminal.Entry) {
	if err := s.repo.AddJournal(ctx, entries); err != nil {
		s.logger.Warn().Ctx(ctx).Err(err).Msg("journal write failed")
		return
	}
	s.orders.JournalWritten()
}

// JournalIn is POST /api/terminal/journal: a terminal-only action (no sale,
// drawer, line edit, price change, ...).
type JournalIn struct {
	Action  string          `json:"action"`
	OrderID string          `json:"order_id"`
	Detail  json.RawMessage `json:"detail"`
}

// PostJournal stores a terminal's own journal row.
func (s *Service) PostJournal(ctx context.Context, a Actor, in JournalIn) (*terminal.Entry, error) {
	st, _, err := s.staffOf(a)
	if err != nil {
		return nil, err
	}
	if err := terminal.CheckPostedAction(in.Action); err != nil {
		return nil, badRequest(err.Error())
	}
	if in.OrderID != "" {
		if _, err := uuid.Parse(in.OrderID); err != nil {
			return nil, badRequest("order_id must be a uuid")
		}
	}
	d := in.Detail
	if len(d) == 0 || string(d) == "null" {
		d = json.RawMessage("{}")
	}
	if len(d) > 4096 {
		return nil, badRequest("detail is at most 4 KB")
	}
	var obj map[string]any
	if err := json.Unmarshal(d, &obj); err != nil {
		return nil, badRequest("detail must be a JSON object")
	}
	e := terminal.Entry{ID: uuid.NewString(), At: s.now().UTC().Truncate(time.Millisecond), StaffID: st.ID, DeviceID: a.DeviceID,
		Action: in.Action, OrderID: in.OrderID, Detail: d}
	if err := s.repo.AddJournal(ctx, []terminal.Entry{e}); err != nil {
		return nil, err
	}
	s.orders.JournalWritten()
	return &e, nil
}

// Journal is the newest journal rows (the web page, tests).
func (s *Service) Journal(ctx context.Context, limit int) ([]terminal.Entry, error) {
	return s.repo.Journal(ctx, min(max(limit, 1), 500))
}

// --- menu + quote ------------------------------------------------------------------------

// MenuView is GET /api/terminal/menu: the counter menu as main sent it, the
// terminal settings and both versions. etag changes with either.
func (s *Service) MenuView() (body []byte, etag string, err error) {
	m, ms := s.store.Menu()
	if m == nil {
		return nil, "", terminal.ErrNoMenu
	}
	cfg, cs := s.store.Config()
	settings := json.RawMessage("{}")
	if cfg != nil {
		settings = cfg.Settings
	}
	body, err = json.Marshal(map[string]any{"menu_version": ms.Version, "fetched_at": ms.FetchedAt.UTC(),
		"config_version": cs.Version, "settings": settings, "counter_menu": json.RawMessage(ms.Body)})
	return body, fmt.Sprintf(`"%s-%s"`, ms.ETag, cs.ETag), err
}

// QuoteIn is POST /api/terminal/quote.
type QuoteIn struct {
	Type string `json:"type"`
	Cart Cart   `json:"cart"`
}

// QuoteView is the engine's quote (the manual discount applied) plus what
// the terminal shows around it.
type QuoteView struct {
	Quote        any   `json:"quote"`
	MenuVersion  int64 `json:"menu_version"`
	DiscountPct  int   `json:"discount_pct"`
	NeedsManager bool  `json:"needs_manager"`
}

// ErrBadRequest wraps a request the terminal must fix.
var ErrBadRequest = errors.New("bad request")

func badRequest(msg string) error { return fmt.Errorf("%w: %s", ErrBadRequest, msg) }

func checkType(t string) (string, error) {
	switch t {
	case "":
		return terminal.TypeCounter, nil
	case terminal.TypeCounter, terminal.TypePickup, terminal.TypeDelivery:
		return t, nil
	}
	return "", badRequest("type must be counter, pickup or delivery")
}

// Quote prices a cart (no login needed; needs_manager says whether the
// manual discount is over the logged-in staff member's limit).
func (s *Service) Quote(_ context.Context, a Actor, in QuoteIn) (*QuoteView, error) {
	typ, err := checkType(in.Type)
	if err != nil {
		return nil, err
	}
	m, ms := s.store.Menu()
	if m == nil {
		return nil, terminal.ErrNoMenu
	}
	p, err := priceCart(m, s.orders.StoreLocation(), typ, in.Cart, s.now())
	if err != nil {
		return nil, badRequest(err.Error())
	}
	v := &QuoteView{Quote: p.quote, MenuVersion: ms.Version, DiscountPct: p.manualPct}
	if p.manualCents > 0 {
		st, cfg, err := s.staffOf(a)
		v.NeedsManager = err != nil || !withinDiscount(cfg.LimitsOf(st), p.manualPct)
	}
	return v, nil
}

func withinDiscount(l terminal.Limits, pct int) bool { return l.Unlimited || pct <= l.DiscountPct }

// --- orders ------------------------------------------------------------------------------

var keyRe = regexp.MustCompile(`^[A-Za-z0-9_.:-]{8,64}$`)

// Customer is an order's customer (optional; a delivery needs the address).
type Customer struct {
	Name    string   `json:"name"`
	Phone   string   `json:"phone"`
	Address *Address `json:"address"`
}

// Address is a delivery address.
type Address struct {
	Line1 string `json:"line1"`
	Line2 string `json:"line2"`
	City  string `json:"city"`
	State string `json:"state"`
	Zip   string `json:"zip"`
	Notes string `json:"notes"`
}

// PaymentIn is a payment (or refund) a terminal took.
type PaymentIn struct {
	ClientKey     string `json:"client_key"`
	Tender        string `json:"tender"`
	AmountCents   int64  `json:"amount_cents"`
	TipCents      int64  `json:"tip_cents"`
	TenderedCents int64  `json:"tendered_cents"`
	Ref           string `json:"ref"`
	Last4         string `json:"last4"`
	Refund        bool   `json:"refund"`
	Reason        string `json:"reason"`
	ManagerPIN    string `json:"manager_pin"`
}

// payment checks a payment in and makes the stored one.
func (p PaymentIn) payment(a Actor, st terminal.Staff, orderID string, at time.Time) (terminal.Payment, error) {
	if !terminal.ValidTender(p.Tender) {
		return terminal.Payment{}, badRequest("tender must be cash, card, square_app, manual_card or comp")
	}
	if p.AmountCents < 1 || p.AmountCents > 10_000_00 || p.TipCents < 0 || p.TipCents > 1_000_00 {
		return terminal.Payment{}, badRequest("amount_cents is 1-1000000, tip_cents 0-100000")
	}
	if p.ClientKey != "" && !keyRe.MatchString(p.ClientKey) {
		return terminal.Payment{}, badRequest("client_key is 8-64 letters, digits, _ . : -")
	}
	if len(p.Ref) > 120 || len(p.Last4) > 4 {
		return terminal.Payment{}, badRequest("ref is at most 120 characters, last4 at most 4")
	}
	reason := strings.TrimSpace(p.Reason)
	if p.Refund && (reason == "" || utf8.RuneCountInString(reason) > 140) {
		return terminal.Payment{}, badRequest("a refund needs a reason (1-140 characters)")
	}
	out := terminal.Payment{ID: uuid.Must(uuid.NewV7()).String(), OrderID: orderID, ClientKey: p.ClientKey, At: at.UTC().Truncate(time.Millisecond),
		Tender: p.Tender, AmountCents: p.AmountCents, TipCents: p.TipCents, Ref: p.Ref, Last4: p.Last4, Refund: p.Refund,
		Reason: reason, StaffID: st.ID, DeviceID: a.DeviceID}
	if p.Tender == terminal.TenderCash && !p.Refund {
		out.TenderedCents = max(p.TenderedCents, p.AmountCents+p.TipCents)
		out.ChangeCents = out.TenderedCents - p.AmountCents - p.TipCents
	}
	return out, nil
}

// OrderIn is POST /api/terminal/orders.
type OrderIn struct {
	ClientKey  string      `json:"client_key"`
	Type       string      `json:"type"`
	Cart       Cart        `json:"cart"`
	Customer   *Customer   `json:"customer"`
	Payments   []PaymentIn `json:"payments"`
	Note       string      `json:"note"`
	ManagerPIN string      `json:"manager_pin"`
}

// PlaceResult is the order a terminal placed (or the one its client_key
// already made: Duplicate).
type PlaceResult struct {
	Order       order.StoredOrder `json:"order"`
	Number      int64             `json:"number"`
	Duplicate   bool              `json:"duplicate"`
	ChangeCents int64             `json:"change_cents"`
}

func numberOf(so order.StoredOrder) int64 {
	if so.Status.KitchenNumber != nil {
		return *so.Status.KitchenNumber
	}
	return 0
}

// PlaceOrder stores a counter order accepted at once (no ring) with the next
// counter number, prints its station tickets and syncs it to main
// (order.placed). Idempotent by client_key.
func (s *Service) PlaceOrder(ctx context.Context, a Actor, in OrderIn) (*PlaceResult, error) {
	st, cfg, err := s.staffOf(a)
	if err != nil {
		return nil, err
	}
	if !keyRe.MatchString(in.ClientKey) {
		return nil, badRequest("client_key is 8-64 letters, digits, _ . : -")
	}
	if prev, err := s.repo.CounterOrderByKey(ctx, in.ClientKey); err != nil {
		return nil, err
	} else if prev != nil {
		return &PlaceResult{Order: *prev, Number: numberOf(*prev), Duplicate: true}, nil
	}
	typ, err := checkType(in.Type)
	if err != nil {
		return nil, err
	}
	note := strings.TrimSpace(in.Note)
	if utf8.RuneCountInString(note) > 500 {
		return nil, badRequest("note is at most 500 characters")
	}
	cust := Customer{}
	if in.Customer != nil {
		cust = *in.Customer
	}
	if utf8.RuneCountInString(cust.Name) > 80 || utf8.RuneCountInString(cust.Phone) > 32 {
		return nil, badRequest("customer name is at most 80 characters, phone 32")
	}
	if typ == terminal.TypeDelivery && (cust.Address == nil || strings.TrimSpace(cust.Address.Line1) == "" || strings.TrimSpace(cust.Address.City) == "") {
		return nil, badRequest("a delivery needs the customer's address (line1 and city)")
	}
	m, ms := s.store.Menu()
	if m == nil {
		return nil, terminal.ErrNoMenu
	}
	now := s.now()
	p, err := priceCart(m, s.orders.StoreLocation(), typ, in.Cart, now)
	if err != nil {
		return nil, badRequest(err.Error())
	}
	if len(p.quote.Errors) > 0 {
		return nil, &terminal.CartInvalid{Issues: p.quote.Errors}
	}
	journal := []terminal.Entry{}
	var approver *terminal.Staff
	if p.manualCents > 0 {
		lim := cfg.LimitsOf(st)
		if !withinDiscount(lim, p.manualPct) {
			need := &terminal.ManagerRequired{Action: terminal.ActionDiscount, Limit: int64(lim.DiscountPct), Requested: int64(p.manualPct)}
			if approver, err = s.manager(a, cfg, in.ManagerPIN, need); err != nil {
				return nil, err
			}
			journal = append(journal, overrideEntry(a, st, approver, need, ""))
		}
	}
	items, off, fees, err := buildItems(m, p)
	if err != nil {
		return nil, err
	}
	money := moneyOf(items, p.quote, off, fees)

	id := uuid.Must(uuid.NewV7()).String()
	var pays []terminal.Payment
	var paidCents, change int64
	for _, pi := range in.Payments {
		if pi.Refund {
			return nil, badRequest("a new order takes no refund")
		}
		pay, err := pi.payment(a, st, id, now)
		if err != nil {
			return nil, err
		}
		pay.ClientKey = ""           // the order's own key covers them
		paidCents += pay.AmountCents // a tip taken with it stays on the payment (main's rule)
		change += pay.ChangeCents
		pays = append(pays, pay)
	}
	if paidCents > money.Total {
		return nil, fmt.Errorf("%w: %d paid, the order is %d", terminal.ErrOverBalance, paidCents, money.Total)
	}

	tenant, location := s.orders.StoreIDs()
	ful := order.Fulfillment{Type: terminal.FulfillmentOf(typ)}
	if typ == terminal.TypeDelivery {
		ad := cust.Address
		ful.Address = &order.Address{Line1: strings.TrimSpace(ad.Line1), Line2: strings.TrimSpace(ad.Line2), City: strings.TrimSpace(ad.City),
			State: strings.TrimSpace(ad.State), PostalCode: strings.TrimSpace(ad.Zip), Country: "US", Notes: strings.TrimSpace(ad.Notes)}
	}
	snap := order.Snapshot{ID: id, TenantID: tenant, LocationID: location, Source: order.SourceCounter,
		SourceEventID: in.ClientKey, Status: "accepted", Currency: "USD", Money: money,
		Customer:    order.Customer{DisplayName: strings.TrimSpace(cust.Name), Phone: strings.TrimSpace(cust.Phone)},
		Fulfillment: ful, Items: items, Notes: note, PlacedAt: now.UTC(), ReceivedAt: now.UTC(),
		SourceMeta: map[string]any{"terminal": map[string]any{"type": typ, "device_id": a.DeviceID, "device_name": a.DeviceName,
			"staff_id": st.ID, "staff_name": st.Name(), "client_key": in.ClientKey, "menu_version": ms.Version}}}
	for _, pay := range pays {
		snap.Payments = append(snap.Payments, order.PaymentSummary{Method: terminal.MethodOf(pay.Tender), AmountCents: pay.AmountCents, Last4: pay.Last4})
	}
	journal = append(journal, terminal.Entry{StaffID: st.ID, DeviceID: a.DeviceID, Action: terminal.ActionOrderPlaced, OrderID: id,
		Detail: detail(map[string]any{"type": typ, "total_cents": money.Total, "paid_cents": paidCents, "lines": len(items), "client_key": in.ClientKey})})
	if p.manualCents > 0 {
		d := map[string]any{"cents": p.manualCents, "pct": p.manualPct, "reason": strings.TrimSpace(in.Cart.Discount.Reason)}
		if approver != nil {
			d["approved_by"] = approver.ID
		}
		journal = append(journal, terminal.Entry{StaffID: st.ID, DeviceID: a.DeviceID, Action: terminal.ActionDiscount, OrderID: id, Detail: detail(d)})
	}
	for _, pay := range pays {
		journal = append(journal, terminal.Entry{StaffID: st.ID, DeviceID: a.DeviceID, Action: terminal.ActionPayment, OrderID: id,
			Detail: detail(map[string]any{"payment_id": pay.ID, "tender": pay.Tender, "amount_cents": pay.AmountCents, "tip_cents": pay.TipCents, "change_cents": pay.ChangeCents})})
	}
	so, created, err := s.repo.PlaceCounterOrder(ctx, terminal.NewCounterOrder{Snapshot: snap, ClientKey: in.ClientKey,
		CounterStart: cfg.CounterStart(), PrepMinutes: s.orders.PrepMinutesFor(ful.Type), Payments: pays, Journal: journal,
		StaffID: st.ID, DeviceID: a.DeviceID, Now: now})
	if err != nil {
		return nil, err
	}
	if !created {
		return &PlaceResult{Order: so, Number: numberOf(so), Duplicate: true}, nil
	}
	s.orders.PrintCounterTickets(ctx, so)
	s.orders.CounterOrderChanged(ctx, so, nil, false)
	s.publish(EventOrderPlaced, map[string]any{"order": so, "device_id": a.DeviceID})
	s.orders.DevEvent(fmt.Sprintf("Counter order #%d placed (%s)", numberOf(so), typ))
	s.logger.Info().Ctx(ctx).Str("order_id", so.Order.ID).Int64("number", numberOf(so)).Str("type", typ).
		Str("staff_id", st.ID).Str("device_id", a.DeviceID).Msg("Counter order placed")
	return &PlaceResult{Order: so, Number: numberOf(so), ChangeCents: change}, nil
}

// OrderView is one counter order with its payments.
type OrderView struct {
	Order    order.StoredOrder  `json:"order"`
	Payments []terminal.Payment `json:"payments"`
}

// Order is one counter order with its payments (404 for another source).
func (s *Service) Order(ctx context.Context, id string) (*OrderView, error) {
	all, err := s.repo.CounterOrders(ctx)
	if err != nil {
		return nil, err
	}
	for _, so := range all {
		if so.Order.ID == id {
			pays, err := s.repo.OrderPayments(ctx, id)
			if err != nil {
				return nil, err
			}
			if pays == nil {
				pays = []terminal.Payment{}
			}
			return &OrderView{Order: so, Payments: pays}, nil
		}
	}
	return nil, order.ErrNotFound
}

// IsOpen: a counter order for the Pick Up list - not voided, and still owing
// money or not picked up yet (a pickup / counter order not finished, a
// delivery not out).
func IsOpen(so order.StoredOrder) bool {
	if so.Status.State == order.StateCancelled {
		return false
	}
	if so.Status.CashDueCents > 0 {
		return true
	}
	switch so.Status.State {
	case order.StateOnDelivery, order.StateDelivered:
		return false
	}
	return so.Status.FinishedAt == nil
}

// Orders are the open batch's counter orders (open: only the Pick Up list's).
func (s *Service) Orders(ctx context.Context, open bool) ([]order.StoredOrder, error) {
	all, err := s.repo.CounterOrders(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]order.StoredOrder, 0, len(all))
	for _, so := range all {
		if !open || IsOpen(so) {
			out = append(out, so)
		}
	}
	return out, nil
}

// PaymentResult is POST /api/terminal/orders/{id}/payments' answer.
type PaymentResult struct {
	Order        order.StoredOrder `json:"order"`
	Payment      terminal.Payment  `json:"payment"`
	Duplicate    bool              `json:"duplicate"`
	BalanceCents int64             `json:"balance_cents"`
}

func netPaid(pays []terminal.Payment) (amount, tip int64) {
	for _, p := range pays {
		a, t := p.Signed()
		amount += a
		tip += t
	}
	return amount, tip
}

// AddPayment takes a payment (or, with refund, gives money back) on a
// counter order of the open batch. A refund over the staff member's limit
// needs manager_pin. Idempotent by the payment's client_key.
func (s *Service) AddPayment(ctx context.Context, a Actor, orderID string, in PaymentIn) (*PaymentResult, error) {
	st, cfg, err := s.staffOf(a)
	if err != nil {
		return nil, err
	}
	if _, err := uuid.Parse(orderID); err != nil {
		return nil, order.ErrNotFound
	}
	pay, err := in.payment(a, st, orderID, s.now())
	if err != nil {
		return nil, err
	}
	var approver *terminal.Staff
	var need *terminal.ManagerRequired
	lim := cfg.LimitsOf(st)
	if pay.Refund && !lim.Unlimited && pay.AmountCents+pay.TipCents > lim.RefundCents {
		need = &terminal.ManagerRequired{Action: terminal.ActionRefund, Limit: lim.RefundCents, Requested: pay.AmountCents + pay.TipCents}
		if approver, err = s.manager(a, cfg, in.ManagerPIN, need); err != nil {
			return nil, err
		}
		pay.ApprovedBy = approver.ID
	}
	so, got, dup, err := s.repo.AddCounterPayment(ctx, orderID, in.ClientKey, func(cur order.StoredOrder, paid []terminal.Payment) ([]terminal.Payment, []terminal.Entry, error) {
		amount, tip := netPaid(paid)
		if pay.Refund {
			if pay.AmountCents > amount || pay.TipCents > tip {
				return nil, nil, fmt.Errorf("%w: %d paid", terminal.ErrRefundTooBig, amount)
			}
		} else if pay.AmountCents > cur.Order.CashDue() {
			return nil, nil, fmt.Errorf("%w: the balance is %d", terminal.ErrOverBalance, cur.Order.CashDue())
		}
		action := terminal.ActionPayment
		if pay.Refund {
			action = terminal.ActionRefund
		}
		d := map[string]any{"payment_id": pay.ID, "tender": pay.Tender, "amount_cents": pay.AmountCents, "tip_cents": pay.TipCents,
			"change_cents": pay.ChangeCents}
		if pay.Refund {
			d["reason"] = pay.Reason
		}
		if approver != nil {
			d["approved_by"] = approver.ID
		}
		entries := []terminal.Entry{{StaffID: st.ID, DeviceID: a.DeviceID, Action: action, OrderID: orderID, Detail: detail(d)}}
		if approver != nil {
			entries = append(entries, overrideEntry(a, st, approver, need, orderID))
		}
		return []terminal.Payment{pay}, entries, nil
	})
	if err != nil {
		return nil, err
	}
	if !dup {
		s.orders.CounterOrderChanged(ctx, so, nil, false)
		s.publish(EventOrderPaid, map[string]any{"order": so, "payment": got})
	}
	return &PaymentResult{Order: so, Payment: got, Duplicate: dup, BalanceCents: so.Status.CashDueCents}, nil
}

// VoidIn is POST /api/terminal/orders/{id}/void.
type VoidIn struct {
	Reason     string `json:"reason"`
	ManagerPIN string `json:"manager_pin"`
}

// VoidResult is the voided order and the money given back.
type VoidResult struct {
	Order         order.StoredOrder  `json:"order"`
	Refunds       []terminal.Payment `json:"refunds"`
	RefundedCents int64              `json:"refunded_cents"`
	Already       bool               `json:"already"`
}

// Void cancels a counter order of the open batch (accepted, ready or out).
// Unpaid: free. Paid: everything taken is refunded by tender, which over the
// staff member's refund limit needs manager_pin. A VOID slip prints on the
// stations its tickets went to; main hears order.void.
func (s *Service) Void(ctx context.Context, a Actor, id string, in VoidIn) (*VoidResult, error) {
	st, cfg, err := s.staffOf(a)
	if err != nil {
		return nil, err
	}
	if _, err := uuid.Parse(id); err != nil {
		return nil, order.ErrNotFound
	}
	reason := strings.TrimSpace(in.Reason)
	if reason == "" || utf8.RuneCountInString(reason) > 140 {
		return nil, badRequest("a void needs a reason (1-140 characters)")
	}
	// Who may give back how much is decided before the transaction (a
	// manager's PIN is a bcrypt check); the transaction checks it again.
	paid, err := s.repo.OrderPayments(ctx, id)
	if err != nil {
		return nil, err
	}
	lim := cfg.LimitsOf(st)
	allowed := int64(-1) // -1 = no limit
	if !lim.Unlimited {
		allowed = lim.RefundCents
	}
	var approver *terminal.Staff
	var need *terminal.ManagerRequired
	if amt, tip := netPaid(paid); allowed >= 0 && amt+tip > allowed {
		need = &terminal.ManagerRequired{Action: terminal.ActionVoid, Limit: lim.RefundCents, Requested: amt + tip}
		if approver, err = s.manager(a, cfg, in.ManagerPIN, need); err != nil {
			return nil, err
		}
		allowed = -1
	}
	now := s.now().UTC().Truncate(time.Millisecond)
	so, prev, refunds, returned, err := s.repo.VoidCounterOrder(ctx, id, reason, st.ID, a.DeviceID, func(cur order.StoredOrder, paid []terminal.Payment) ([]terminal.Payment, []terminal.Entry, error) {
		amount, tip := netPaid(paid)
		if allowed >= 0 && amount+tip > allowed {
			return nil, nil, &terminal.ManagerRequired{Action: terminal.ActionVoid, Limit: lim.RefundCents, Requested: amount + tip}
		}
		// One refund per tender, of what that tender still holds.
		type net struct{ amount, tip int64 }
		by := map[string]*net{}
		var tenders []string
		for _, p := range paid {
			n := by[p.Tender]
			if n == nil {
				n = &net{}
				by[p.Tender] = n
				tenders = append(tenders, p.Tender)
			}
			pa, pt := p.Signed()
			n.amount += pa
			n.tip += pt
		}
		var refunds []terminal.Payment
		entries := []terminal.Entry{}
		for _, t := range tenders {
			n := by[t]
			if n.amount <= 0 && n.tip <= 0 {
				continue
			}
			r := terminal.Payment{ID: uuid.Must(uuid.NewV7()).String(), OrderID: id, At: now, Tender: t, AmountCents: max(n.amount, 0),
				TipCents: max(n.tip, 0), Refund: true, Reason: reason, StaffID: st.ID, DeviceID: a.DeviceID}
			if approver != nil {
				r.ApprovedBy = approver.ID
			}
			refunds = append(refunds, r)
			entries = append(entries, terminal.Entry{StaffID: st.ID, DeviceID: a.DeviceID, Action: terminal.ActionRefund, OrderID: id,
				Detail: detail(map[string]any{"payment_id": r.ID, "tender": t, "amount_cents": r.AmountCents, "tip_cents": r.TipCents, "reason": reason, "void": true})})
		}
		d := map[string]any{"reason": reason, "refunded_cents": amount + tip, "number": numberOf(cur), "state": cur.Status.State}
		if approver != nil {
			d["approved_by"] = approver.ID
			entries = append(entries, overrideEntry(a, st, approver, need, id))
		}
		entries = append([]terminal.Entry{{StaffID: st.ID, DeviceID: a.DeviceID, Action: terminal.ActionVoid, OrderID: id, Detail: detail(d)}}, entries...)
		return refunds, entries, nil
	})
	if errors.Is(err, terminal.ErrAlreadyVoided) {
		pays, _ := s.repo.OrderPayments(ctx, id)
		var back []terminal.Payment
		var cents int64
		for _, p := range pays {
			if p.Refund {
				back = append(back, p)
				cents += p.AmountCents + p.TipCents
			}
		}
		if back == nil {
			back = []terminal.Payment{}
		}
		return &VoidResult{Order: so, Refunds: back, RefundedCents: cents, Already: true}, nil
	}
	if err != nil {
		return nil, err
	}
	s.orders.PrintVoidSlip(ctx, prev)
	s.orders.CounterOrderChanged(ctx, so, &prev, returned)
	s.publish(EventOrderVoided, map[string]any{"order": so})
	var cents int64
	for _, r := range refunds {
		cents += r.AmountCents + r.TipCents
	}
	if refunds == nil {
		refunds = []terminal.Payment{}
	}
	s.orders.DevEvent(fmt.Sprintf("Counter order #%d voided", numberOf(so)))
	return &VoidResult{Order: so, Refunds: refunds, RefundedCents: cents}, nil
}

// SessionView is GET /api/terminal/session: who is on this terminal and the
// snapshots in use.
type SessionView struct {
	Staff         *terminal.Public `json:"staff"`
	Role          string           `json:"role,omitempty"`
	Limits        *terminal.Limits `json:"limits,omitempty"`
	MenuVersion   int64            `json:"menu_version"`
	MenuFetchedAt *time.Time       `json:"menu_fetched_at"`
	ConfigVersion int64            `json:"config_version"`
}

// Session is the terminal's staff session and the snapshot versions.
func (s *Service) Session(a Actor) SessionView {
	_, ms := s.store.Menu()
	_, cs := s.store.Config()
	v := SessionView{MenuVersion: ms.Version, ConfigVersion: cs.Version}
	if !ms.FetchedAt.IsZero() {
		t := ms.FetchedAt.UTC()
		v.MenuFetchedAt = &t
	}
	if st, cfg, err := s.staffOf(a); err == nil {
		p, l := st.Public(), cfg.LimitsOf(st)
		v.Staff, v.Role, v.Limits = &p, st.Role, &l
	}
	return v
}
