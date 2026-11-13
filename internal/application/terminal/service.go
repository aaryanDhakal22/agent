package terminal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/terminal"
	"quiccpos/agent/internal/infra/notify"
	"quiccpos/agent/internal/observability"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
)

// Repo is the terminal flow's storage (repositories.OrderRepository).
type Repo interface {
	SnapshotRepo
	PickupRepo
	DraftRepo
	KeyRepo
	UnsettledRepo
	CounterOrderByKey(ctx context.Context, key string) (*order.StoredOrder, error)
	PlaceCounterOrder(ctx context.Context, in terminal.NewCounterOrder) (order.StoredOrder, bool, error)
	AddCounterPayment(ctx context.Context, orderID, clientKey, requestHash string, check terminal.MoneyCheck) (order.StoredOrder, terminal.Payment, bool, error)
	VoidCounterOrder(ctx context.Context, id, reason, wasteReason, staffID, deviceID string, check terminal.MoneyCheck) (so, prev order.StoredOrder, refunds []terminal.Payment, returned bool, err error)
	ChangeCounterOrder(ctx context.Context, id, staffID, deviceID string, check terminal.ChangeCheck) (so, prev order.StoredOrder, changed bool, err error)
	OrderPayments(ctx context.Context, orderID string) ([]terminal.Payment, error)
	CounterOrders(ctx context.Context) ([]order.StoredOrder, error)
	AddJournal(ctx context.Context, entries []terminal.Entry) error
	Journal(ctx context.Context, limit int) ([]terminal.Entry, error)
}

// Orders is what the terminal flow needs from the floor (orderApp.Service).
type Orders interface {
	PrintCounterTickets(ctx context.Context, so order.StoredOrder)
	PrintCounterTicketsThen(ctx context.Context, so order.StoredOrder, done func(ctx context.Context))
	PrintVoidSlip(ctx context.Context, so order.StoredOrder)
	PrintChange(ctx context.Context, so order.StoredOrder, added, removed []order.LineItem)
	CounterOrderChanged(ctx context.Context, so order.StoredOrder, prev *order.StoredOrder, driverReturned bool)
	JournalWritten()
	PrepMinutesFor(fulfillment string) int64
	StoreIDs() (tenantID, locationID string)
	StoreLocation() *time.Location
	DevEvent(text string)
}

// deliveryUpdated prints a sent delivery's copies again after its payments
// changed (the order service's PrintDeliveryUpdate, when the floor has it).
func (s *Service) deliveryUpdated(ctx context.Context, so order.StoredOrder) {
	if p, ok := s.orders.(interface {
		PrintDeliveryUpdate(ctx context.Context, so order.StoredOrder)
	}); ok {
		p.PrintDeliveryUpdate(ctx, so)
	}
}

// Actor is the terminal (device) a request came from; the web-UI session
// counts as one device, "web".
type Actor struct {
	DeviceID   string
	DeviceName string
	// Callcenter: a call-center seat (the relay, contract §2.10ab):
	// DeviceID is callcenter:<main's device id>, StaffID the agent main
	// signed in (its session for this one call; callcenter.go).
	Callcenter bool
	StaffID    string
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
	cust   *CustomerBook
	// The Z report (contract 2.10l): driver reports + one build at a time.
	zDrivers func(ctx context.Context, batchID int64) ([]order.DriverReport, error)
	zMu      sync.Mutex
	// The Manager tab (contract 2.10m): batches, driver reports, the close.
	reports   ReportFloor
	storeName func() string
	storeInfo func() map[string]any
	// cm: the counter instruments (HARDENING-PLAN R-1a CM-3..CM-10).
	cm *observability.CounterMeters
	// pager: the owner's Pushover (unbalanced Z, PIN lockouts).
	pager Pager
	// pageCool: one page per text per 15 min (SetPager, 2.5.2).
	pageCool *notify.Cooldown
	// pinRepo keeps the PIN throttle across restarts (X-2).
	pinRepo PinRepo
	// approvals: single-use manager approvals (X-5).
	approvals approvalBook
	// dispatch: the Dispatch / Pickup bars' floor (dispatch.go).
	dispatch Dispatch
	// square: refunds of in-app card payments through main (square_refund.go).
	square SquareRefunder
	// cc: the call center (callcenter.go).
	cc callcenterState
}

// NewService wires the terminal flow; the store's config changes reach the
// staff book (sessions of removed staff end).
func NewService(store *Store, repo Repo, orders Orders, pub Publisher, logger zerolog.Logger) *Service {
	s := &Service{store: store, repo: repo, orders: orders, pub: pub, logger: logger.With().Str("module", "terminal").Logger(),
		staff: newStaffBook(), now: time.Now, cm: observability.Counter()}
	store.OnConfig(s.configChanged)
	// Online orders are placed with a terminal SEND's default prep (§2.10aa).
	if ps, ok := orders.(interface {
		SetPrepSource(func(fulfillment string) int64)
	}); ok {
		ps.SetPrepSource(s.DefaultPrep)
	}
	return s
}

// earlierDay: is the order of an earlier batch, not carried (a refund on it
// needs a manager)? A repo that can't tell answers no.
func earlierDay(ctx context.Context, repo Repo, orderID string) bool {
	if r, ok := repo.(interface {
		EarlierBatchOrder(ctx context.Context, id string) (bool, error)
	}); ok {
		v, err := r.EarlierBatchOrder(ctx, orderID)
		return err == nil && v
	}
	return false
}

// DefaultPrep is a SEND's default minutes for a fulfillment type with no
// time quoted (an online order placed on arrival takes it too, §2.10aa).
func (s *Service) DefaultPrep(fulfillment string) int64 {
	cfg, _ := s.store.Config()
	typ := terminal.TypeDelivery
	if fulfillment != "delivery" {
		typ = terminal.TypePickup
	}
	return s.prepMinutes(cfg, typ, fulfillment, nil)
}

// EventSessionEnded tells the terminals a staff session ended on the Pi's
// side (contract §2.10u): {device_id, staff_id, reason}. The terminal it
// names locks at once; any call with it answers 401 login_required.
const EventSessionEnded = "terminal.session.ended"

// configChanged: a new terminal config is in use. The sessions it ends are
// announced and journaled (session.ended) right away.
func (s *Service) configChanged(cfg *terminal.Config) {
	ended := s.staff.configChanged(cfg)
	ctx := context.Background()
	for _, e := range ended {
		s.publish(EventSessionEnded, e)
		s.journal(ctx, terminal.Entry{StaffID: e.StaffID, DeviceID: e.DeviceID, Action: terminal.ActionSessionEnded,
			Detail: detail(map[string]any{"reason": e.Reason})})
		s.logger.Info().Str("staff_id", e.StaffID).Str("device_id", e.DeviceID).Str("reason", e.Reason).
			Msg("Terminal staff session ended by main's config")
	}
}

// SetCounterMeters swaps the counter instruments (tests).
func (s *Service) SetCounterMeters(cm *observability.CounterMeters) { s.cm = cm }

// SetStoreInfo wires the store's receipt header / footer facts the
// terminals print (contract 2.10q): name, address, phone, website, footer.
func (s *Service) SetStoreInfo(fn func() map[string]any) { s.storeInfo = fn }

// withStore adds "store" (the Pi's store_config + receipt footer) to main's
// terminal settings as the terminals get them, and "waste_reasons" (the
// defaults) when main sends none (contract 2.10s §5); main's own keys stay.
func (s *Service) withStore(settings json.RawMessage) json.RawMessage {
	m := map[string]json.RawMessage{}
	if len(settings) > 0 && json.Unmarshal(settings, &m) != nil {
		return settings
	}
	changed := false
	if raw, ok := m["waste_reasons"]; !ok || string(raw) == "null" || string(raw) == "[]" {
		if b, err := json.Marshal(terminal.DefaultWasteReasons); err == nil {
			m["waste_reasons"], changed = b, true
		}
	}
	if _, ok := m["store"]; !ok && s.storeInfo != nil {
		if info := s.storeInfo(); len(info) > 0 {
			if raw, err := json.Marshal(info); err == nil {
				m["store"], changed = raw, true
			}
		}
	}
	if !changed {
		return settings
	}
	out, err := json.Marshal(m)
	if err != nil {
		return settings
	}
	return out
}

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
	release, err := s.staff.begin(ctx, a.DeviceID, s.now) // one check at a time, reserved before bcrypt (H3)
	if err != nil {
		return nil, err
	}
	defer release()
	cfg, _ := s.store.Config()
	if cfg == nil {
		return nil, terminal.ErrNoConfig
	}
	st, ok := s.staff.match(cfg, pin)
	short := ok && shortManagerPIN(st, pin)
	if !ok || short {
		s.pinMiss(ctx, a, "login", "")
		d := map[string]any{"device_name": a.DeviceName}
		if short {
			d["reason"] = "manager_pin_short"
		}
		s.journal(ctx, terminal.Entry{DeviceID: a.DeviceID, Action: terminal.ActionLoginFailed, Detail: detail(d)})
		if short {
			return nil, terminal.ErrManagerPINShort
		}
		return nil, terminal.ErrWrongPIN
	}
	s.staff.hit(a.DeviceID)
	s.staff.login(a.DeviceID, st.ID, st.PinHash, s.now())
	s.journal(ctx, terminal.Entry{StaffID: st.ID, DeviceID: a.DeviceID, Action: terminal.ActionLogin,
		Detail: detail(map[string]any{"role": st.Role, "device_name": a.DeviceName})})
	res := &LoginResult{Staff: st.Public(), Role: st.Role, Limits: cfg.LimitsOf(st), Settings: s.withStore(cfg.Settings)}
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

// shortManagerPIN: the PIN is a manager's but not ManagerPINDigits long
// (main's Staff API refuses such a PIN now; one set before is refused here).
func shortManagerPIN(st terminal.Staff, pin string) bool {
	return st.Role == terminal.RoleManager && len(pin) != terminal.ManagerPINDigits
}

// Logout ends the staff session on a terminal (the terminal locked).
func (s *Service) Logout(ctx context.Context, a Actor) {
	if id, ok := s.staff.logout(a.DeviceID); ok {
		s.journal(ctx, terminal.Entry{StaffID: id, DeviceID: a.DeviceID, Action: terminal.ActionLogout})
	}
}

// staffOf is the staff member logged in on the terminal.
func (s *Service) staffOf(a Actor) (terminal.Staff, *terminal.Config, error) {
	if a.Callcenter {
		return s.callcenterStaff(a)
	}
	cfg, _ := s.store.Config()
	cur, ok, expired := s.staff.live(a.DeviceID, cfg, s.now())
	if expired { // idle past the auto-lock + a margin (M4): over on the Pi too
		s.sessionIdled(a, cur)
	}
	if !ok || cfg == nil {
		return terminal.Staff{}, cfg, terminal.ErrLoginRequired
	}
	st, ok := cfg.Find(cur.staffID)
	if !ok || st.PinHash != cur.pinHash { // gone, switched off or a new PIN (§2.10u)
		s.staff.logout(a.DeviceID)
		return terminal.Staff{}, cfg, terminal.ErrLoginRequired
	}
	return st, cfg, nil
}

// sessionIdled journals a session the Pi ended for idling (once: live
// dropped it).
func (s *Service) sessionIdled(a Actor, cur session) {
	ctx := context.Background()
	s.journal(ctx, terminal.Entry{StaffID: cur.staffID, DeviceID: a.DeviceID, Action: terminal.ActionSessionEnded,
		Detail: detail(map[string]any{"reason": terminal.SessionIdle, "device_name": a.DeviceName})})
	s.logger.Info().Str("staff_id", cur.staffID).Str("device_id", a.DeviceID).Str("reason", terminal.SessionIdle).
		Msg("Terminal staff session ended: no call from the terminal past the auto-lock time")
}

// Touch notes a call from a terminal (any /api/terminal/* request): its
// staff session stays alive while the terminal talks to the Pi (M4).
func (s *Service) Touch(a Actor) {
	if a.Callcenter {
		return
	}
	cfg, _ := s.store.Config()
	s.staff.touch(a.DeviceID, cfg, s.now())
}

// manager checks a manager's PIN given to go over a limit (a wrong one
// counts as a wrong PIN on the terminal).
func (s *Service) manager(ctx context.Context, a Actor, cfg *terminal.Config, pin string, need *terminal.ManagerRequired) (*terminal.Staff, error) {
	if mgr, ok := preApproved(ctx, cfg); ok { // a phone charge's cart, approved at its creation
		return mgr, nil
	}
	if mgr, ok := s.approvals.take(approvalsOf(ctx), a.DeviceID, need, s.now()); ok {
		if cur, found := cfg.Find(mgr.ID); found && cur.Role == terminal.RoleManager {
			s.cm.Override(ctx, need.Action, "approval", 0)
			s.logger.Info().Ctx(ctx).Str("action", need.Action).Str("approver_id", mgr.ID).Str("device_id", a.DeviceID).
				Int64("requested", need.Requested).Msg("Manager override")
			return &cur, nil
		}
	}
	if strings.TrimSpace(pin) == "" {
		return nil, need
	}
	mgr, err := s.managerPIN(ctx, a, cfg, pin, need)
	if err != nil {
		return nil, err
	}
	s.overrideUsed(ctx, a, need, mgr, "manager_pin")
	return mgr, nil
}

// managerPIN checks a manager's PIN sent with a request (manager_pin).
func (s *Service) managerPIN(ctx context.Context, a Actor, cfg *terminal.Config, pin string, need *terminal.ManagerRequired) (*terminal.Staff, error) {
	release, err := s.staff.begin(ctx, a.DeviceID, s.now) // reserved before bcrypt (H3)
	if err != nil {
		return nil, err
	}
	defer release()
	if !pinRe.MatchString(pin) {
		return nil, terminal.ErrManagerPINWrong
	}
	st, ok := s.staff.match(cfg, pin)
	short := ok && shortManagerPIN(st, pin)
	if !ok || st.Role != terminal.RoleManager || short {
		// L-1 / X-2: an inline manager miss is journaled and counted too.
		cur, _ := s.staff.current(a.DeviceID)
		who := cur.staffID
		s.pinMiss(ctx, a, "inline_manager", who)
		s.journal(ctx, terminal.Entry{StaffID: who, DeviceID: a.DeviceID, Action: terminal.ActionManagerCheck,
			Detail: detail(map[string]any{"for": need.Action, "ok": false, "inline": true})})
		if short {
			return nil, terminal.ErrManagerPINShortInline
		}
		return nil, terminal.ErrManagerPINWrong
	}
	s.staff.hit(a.DeviceID)
	return &st, nil
}

// pinMiss counts a wrong PIN (X-2): the throttle (kept in settings), the
// CM-8 counters, a Warn line (never the PIN), a lockout journaled and the
// owner paged after pinPageMisses in pinPageWindow (once an hour).
func (s *Service) pinMiss(ctx context.Context, a Actor, kind, staffID string) {
	r := s.staff.miss(a.DeviceID, s.now())
	s.savePins(ctx)
	s.cm.PinFailure(ctx, kind, r.Lockout)
	s.logger.Warn().Ctx(ctx).Str("device_id", a.DeviceID).Str("device_name", a.DeviceName).Str("kind", kind).Str("staff_id", staffID).
		Int("device_misses", r.DeviceMisses).Int("store_misses", r.StoreMisses).Int("day_misses", r.DayMisses).Msg("PIN failed")
	if r.Lockout {
		s.logger.Warn().Ctx(ctx).Str("device_id", a.DeviceID).Str("kind", kind).Int("device_misses", r.DeviceMisses).
			Int("store_misses", r.StoreMisses).Msg("PIN lockout")
		s.journal(ctx, terminal.Entry{StaffID: staffID, DeviceID: a.DeviceID, Action: terminal.ActionPinLockout,
			Detail: detail(map[string]any{"kind": kind, "device_misses": r.DeviceMisses, "store_misses": r.StoreMisses})})
	}
	if r.DayLocked {
		// The daily ceiling (H3): every PIN check is refused until a manager
		// unlocks them; the owner is paged.
		s.logger.Error().Ctx(ctx).Str("device_id", a.DeviceID).Str("device_name", a.DeviceName).Int("day_misses", r.DayMisses).
			Int("daily_ceiling", r.Ceiling).Msg("PIN logins locked: the daily ceiling of wrong PINs was reached")
		s.journal(ctx, terminal.Entry{StaffID: staffID, DeviceID: a.DeviceID, Action: terminal.ActionPinLocked,
			Detail: detail(map[string]any{"misses_24h": r.DayMisses, "daily_ceiling": r.Ceiling, "device_name": a.DeviceName})})
		s.publish(EventPins, map[string]any{"locked": true})
		s.page(ctx, fmt.Sprintf("%d wrong PINs at the store's terminals in 24 hours (latest on %s). PIN logins are locked: a manager unlocks them on the Pi page (Settings > Tablets) or in Quibase (Help).",
			r.DayMisses, a.DeviceName))
		return
	}
	if r.Page {
		s.page(ctx, fmt.Sprintf("%d wrong PINs at the store's terminals in 10 minutes (latest on %s). The PIN pads pause; check who is trying.",
			r.StoreMisses, a.DeviceName))
	}
}

// EventPins tells the terminals and tablets the PIN checks locked /
// unlocked ({locked}).
const EventPins = "terminal.pins"

// PinGuard is the daily ceiling's state (GET /api/pin-guard).
func (s *Service) PinGuard() PinGuard { return s.staff.guard(s.now()) }

// UnlockPins: a manager unlocked the PIN checks (the store manager login on
// the Pi page or Quibase); every counted miss is cleared.
func (s *Service) UnlockPins(ctx context.Context, by string) PinGuard {
	was := s.staff.unlock()
	s.savePins(ctx)
	if was {
		s.journal(ctx, terminal.Entry{Action: terminal.ActionPinUnlocked, Detail: detail(map[string]any{"by": by})})
		s.logger.Warn().Ctx(ctx).Str("by", by).Msg("PIN logins unlocked by a manager")
		s.publish(EventPins, map[string]any{"locked": false})
	}
	return s.PinGuard()
}

// SetPinCeiling stores the daily ceiling of wrong PINs (MinPinDailyCeiling
// to MaxPinDailyCeiling).
func (s *Service) SetPinCeiling(ctx context.Context, n int) (PinGuard, error) {
	if n < MinPinDailyCeiling || n > MaxPinDailyCeiling {
		return PinGuard{}, badRequest(fmt.Sprintf("daily_ceiling is %d-%d", MinPinDailyCeiling, MaxPinDailyCeiling))
	}
	s.staff.setCeiling(n)
	s.savePins(ctx)
	return s.PinGuard(), nil
}

// PinRepo keeps the PIN throttle (repositories.SettingsRepository).
type PinRepo interface {
	GetPinThrottle(ctx context.Context) ([]byte, error)
	SetPinThrottle(ctx context.Context, raw []byte) error
}

// SetPinRepo wires the throttle's storage and loads it (cmd/main.go).
func (s *Service) SetPinRepo(ctx context.Context, r PinRepo) {
	s.pinRepo = r
	if raw, err := r.GetPinThrottle(ctx); err == nil {
		s.staff.restorePins(raw)
	}
}

func (s *Service) savePins(ctx context.Context) {
	if s.pinRepo == nil {
		return
	}
	if err := s.pinRepo.SetPinThrottle(ctx, s.staff.pinSnapshot()); err != nil {
		s.logger.Warn().Ctx(ctx).Err(err).Msg("pin throttle: save failed")
	}
}

// ManagerCheckIn is POST /api/terminal/manager-check: a manager's PIN for a
// terminal dialog (a price drop) and what it is for.
type ManagerCheckIn struct {
	PIN    string `json:"pin"`
	Action string `json:"action"`
	// AmountCents (X-5): what the approval may cover (cents, or a percent
	// for discount / price_change; 0 = any).
	AmountCents int64 `json:"amount_cents"`
}

// ManagerCheckResult answers a manager's PIN.
type ManagerCheckResult struct {
	OK    bool            `json:"ok"`
	Staff terminal.Public `json:"staff"`
	// ApprovalID (X-5): single use, this terminal, this action, up to
	// amount_cents, for ApprovalTTL - sent as approvals: [id].
	ApprovalID string    `json:"approval_id"`
	ExpiresAt  time.Time `json:"expires_at"`
}

var forRe = regexp.MustCompile(`^[a-z][a-z0-9_.]{0,39}$`)

// ManagerCheck tells a terminal whether a PIN is an active manager's (for its
// dialogs; the order still carries the PIN as manager_pin, which the Pi
// checks again). Needs a staff member logged in on the terminal; throttled
// with the terminal's logins (a wrong or non-manager PIN is a miss, 401
// wrong_pin either way, so it never tells whose PIN it was); journaled.
func (s *Service) ManagerCheck(ctx context.Context, a Actor, in ManagerCheckIn) (*ManagerCheckResult, error) {
	what := strings.TrimSpace(in.Action)
	if what == "" {
		what = terminal.ActionPriceChange
	}
	st, cfg, err := s.staffOf(a)
	if err != nil {
		// A device-level action (admin mode's drawer test, Unpair, opening
		// the pairing window) is asked with nobody logged in (M7).
		if !errors.Is(err, terminal.ErrLoginRequired) || !deviceLevelChecks[what] || a.Callcenter {
			return nil, err
		}
		if cfg == nil {
			return nil, terminal.ErrNoConfig
		}
		st = terminal.Staff{}
	}
	pin := strings.TrimSpace(in.PIN)
	if !pinRe.MatchString(pin) {
		return nil, terminal.ErrBadPIN
	}
	if !forRe.MatchString(what) {
		return nil, badRequest("action is 1-40 lowercase letters, digits, underscores or dots")
	}
	release, err := s.staff.begin(ctx, a.DeviceID, s.now) // reserved before bcrypt (H3)
	if err != nil {
		return nil, err
	}
	defer release()
	mgr, ok := s.staff.match(cfg, pin)
	short := ok && shortManagerPIN(mgr, pin)
	if !ok || mgr.Role != terminal.RoleManager || short {
		s.pinMiss(ctx, a, "manager_check", st.ID)
		s.journal(ctx, terminal.Entry{StaffID: st.ID, DeviceID: a.DeviceID, Action: terminal.ActionManagerCheck,
			Detail: detail(map[string]any{"for": what, "ok": false})})
		if short {
			return nil, terminal.ErrManagerPINShort
		}
		return nil, terminal.ErrWrongPIN
	}
	s.staff.hit(a.DeviceID)
	s.journal(ctx, terminal.Entry{StaffID: st.ID, DeviceID: a.DeviceID, Action: terminal.ActionManagerCheck,
		Detail: detail(map[string]any{"for": what, "ok": true, "manager_id": mgr.ID})})
	return &ManagerCheckResult{OK: true, Staff: mgr.Public(), ApprovalID: s.newApproval(a, mgr, what, in.AmountCents),
		ExpiresAt: s.now().Add(ApprovalTTL).UTC()}, nil
}

// deviceLevelChecks: the manager-check actions a terminal may ask with
// nobody logged in (contract 2.10y "Admin mode"): its admin-mode drawer
// test, Unpair, and opening the pairing window.
var deviceLevelChecks = map[string]bool{"admin_drawer_test": true, "unpair": true, terminal.NeedPairingWindow: true}

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
	e, _, err := s.PostJournalDedup(ctx, a, in)
	return e, err
}

// ActionAdminMode is the terminal's admin mode (the developer PIN): a
// terminal journals it with nobody logged in (contract 2.10s §9).
const ActionAdminMode = "admin_mode"

// deviceLevelActions are what a terminal journals with nobody logged in
// (contract 2.10s §9, 2.10y): admin mode and its drawer tests, the setup
// wizard's steps (setup is done logged out). The row has no staff.
var deviceLevelActions = map[string]bool{
	ActionAdminMode: true, "admin_drawer_test": true, "drawer_kick": true,
	"setup.started": true, "setup.finished": true, "setup.skipped_by_admin": true, "setup.card_test": true,
}

// JournalKeyRepo finds a terminal's journal row by its client_key.
type JournalKeyRepo interface {
	JournalByClientKey(ctx context.Context, deviceID, action, key string) (*terminal.Entry, error)
}

// PostJournalDedup is PostJournal answering whether the row came before: a
// detail.client_key the terminal already sent for that action gives the
// stored row back (dup true, nothing written; contract 2.10s §9). A
// device-level action (admin_mode, setup.*, ...) needs no staff login.
func (s *Service) PostJournalDedup(ctx context.Context, a Actor, in JournalIn) (*terminal.Entry, bool, error) {
	st, _, err := s.staffOf(a)
	if err != nil && !deviceLevelActions[in.Action] {
		return nil, false, err
	}
	if err := terminal.CheckPostedAction(in.Action); err != nil {
		return nil, false, badRequest(err.Error())
	}
	if in.OrderID != "" {
		if _, err := uuid.Parse(in.OrderID); err != nil {
			return nil, false, badRequest("order_id must be a uuid")
		}
	}
	d := in.Detail
	if len(d) == 0 || string(d) == "null" {
		d = json.RawMessage("{}")
	}
	if len(d) > 4096 {
		return nil, false, badRequest("detail is at most 4 KB")
	}
	var obj map[string]any
	if err := json.Unmarshal(d, &obj); err != nil {
		return nil, false, badRequest("detail must be a JSON object")
	}
	if key, _ := obj["client_key"].(string); key != "" {
		if kr, ok := s.repo.(JournalKeyRepo); ok {
			if prev, err := kr.JournalByClientKey(ctx, a.DeviceID, in.Action, key); err != nil {
				return nil, false, err
			} else if prev != nil {
				return prev, true, nil
			}
		}
	}
	if in.Action == terminal.ActionNoSale {
		d = s.noSaleDetail(ctx, a, d) // + the open drawer (contract 2.10k)
	}
	e := terminal.Entry{ID: uuid.NewString(), At: s.now().UTC().Truncate(time.Millisecond), StaffID: st.ID, DeviceID: a.DeviceID,
		Action: in.Action, OrderID: in.OrderID, Detail: d}
	if err := s.repo.AddJournal(ctx, []terminal.Entry{e}); err != nil {
		return nil, false, err
	}
	if in.Action == terminal.ActionNoSale {
		s.logger.Info().Ctx(ctx).Str("staff_id", st.ID).Str("staff_name", st.Name()).Str("device_id", a.DeviceID).
			Str("reason", "no_sale").Msg("Drawer kicked")
	}
	s.orders.JournalWritten()
	return &e, false, nil
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
	settings = s.withStore(settings)
	body, err = json.Marshal(map[string]any{"menu_version": ms.Version, "fetched_at": ms.FetchedAt.UTC(),
		"config_version": cs.Version, "settings": settings, "counter_menu": json.RawMessage(ms.Body)})
	return body, fmt.Sprintf(`"%s-%s"`, ms.ETag, cs.ETag), err
}

// QuoteIn is POST /api/terminal/quote.
type QuoteIn struct {
	Type string `json:"type"`
	Cart Cart   `json:"cart"`
	// OrderID (contract 2.10s §9): a reopened order's cart, priced as PUT
	// .../cart would (its lines keep their rung prices, M-15).
	OrderID string `json:"order_id,omitempty"`
	// Customer: a delivery's customer (the call center sends it): the Pi
	// prices the delivery fee by distance from its address when it can
	// place it (2.10ae), else from the cart's distance_m.
	Customer *Customer `json:"customer,omitempty"`
}

// QuoteView is the engine's quote (the manual discount applied) plus what
// the terminal shows around it.
type QuoteView struct {
	Quote        any   `json:"quote"`
	MenuVersion  int64 `json:"menu_version"`
	DiscountPct  int   `json:"discount_pct"`
	NeedsManager bool  `json:"needs_manager"`
	// PriceChanges are the lines rung at another price than the menu's,
	// each with needs_manager for the logged-in staff member.
	PriceChanges []terminal.PriceChange `json:"price_changes,omitempty"`
	// MenuGone: lines whose item left the menu, priced as custom lines at
	// their rung_cents; KeptPrices: lines kept at their rung price (§2.10u).
	MenuGone   []terminal.GoneLine  `json:"menu_gone,omitempty"`
	KeptPrices []terminal.KeptPrice `json:"kept_prices,omitempty"`
}

// asBadRequest wraps a pricing input error as a 400 (one already wrapped
// stays as it is).
func asBadRequest(err error) error {
	if errors.Is(err, ErrBadRequest) {
		return err
	}
	return badRequest(err.Error())
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
func (s *Service) Quote(ctx context.Context, a Actor, in QuoteIn) (*QuoteView, error) {
	typ, err := checkType(in.Type)
	if err != nil {
		return nil, err
	}
	if err := callcenterQuoteGuard(a, in.Cart); err != nil {
		return nil, err
	}
	m, ms := s.store.Menu()
	if m == nil {
		return nil, terminal.ErrNoMenu
	}
	var placed *order.Snapshot
	if id := strings.TrimSpace(in.OrderID); id != "" {
		cur, err := s.Order(ctx, id)
		if err != nil {
			return nil, err
		}
		if cur.Order.Order.Source != order.SourceCounter {
			return s.onlineQuote(m, cur.Order.Order, in.Cart) // §2.10aa
		}
		placed = &cur.Order.Order
	}
	// The delivery fee by distance (2.10ae): the cart's distance_m (the
	// terminal's lookup), or the Pi's own for a sent delivery's address.
	switch {
	case typ != terminal.TypeDelivery:
		in.Cart.DistanceM = nil
	case orderAddress(in.Customer) != nil:
		in.Cart = s.withDistance(ctx, typ, in.Customer, in.Cart)
	case placed != nil && placed.Fulfillment.Address != nil:
		in.Cart = s.withDistance(ctx, typ, customerAt(placed.Fulfillment.Address), in.Cart)
	}
	// Rung prices kept, gone items sold as custom lines (rung.go, §2.10u).
	p, _, err := priceRung(m, s.orders.StoreLocation(), typ, placed, in.Cart, s.now(), s.toppingMax())
	if err != nil {
		return nil, asBadRequest(err)
	}
	v := &QuoteView{Quote: p.quote, MenuVersion: ms.Version, DiscountPct: p.manualPct, PriceChanges: p.changes,
		MenuGone: p.gone, KeptPrices: p.kept}
	if p.manualCents > 0 || len(p.changes) > 0 {
		st, cfg, err := s.staffOf(a)
		if p.manualCents > 0 {
			v.NeedsManager = err != nil || discountNeed(cfg, st, in.Cart.Discount, p) != nil
		}
		if err != nil { // nobody logged in: any drop would need a manager
			for i := range v.PriceChanges {
				v.PriceChanges[i].NeedsManager = v.PriceChanges[i].DropPct > 0
				v.NeedsManager = v.NeedsManager || v.PriceChanges[i].NeedsManager
			}
		} else if price, comp := checkPrices(cfg, st, v.PriceChanges, nil); price != nil || comp != nil {
			v.NeedsManager = true
		}
	}
	return v, nil
}

func withinDiscount(l terminal.Limits, pct int) bool { return l.Unlimited || pct <= l.DiscountPct }

// discountNeed is what a manager's PIN must cover for the cart's discount
// (nil = nothing): a typed discount over the staff member's discount_pct, or
// an Employee meal over main's employee_meal_pct (anyone logged in gives one
// up to it; a manager or anyone unlimited has no limit).
func discountNeed(cfg *terminal.Config, st terminal.Staff, d *Discount, p priced) *terminal.ManagerRequired {
	if p.manualCents <= 0 {
		return nil
	}
	lim := cfg.LimitsOf(st)
	if d.employeeMeal() {
		if meal := cfg.EmployeeMealPct(); !lim.Unlimited && p.manualPct > meal {
			return &terminal.ManagerRequired{Action: terminal.ActionEmployeeMeal, Limit: int64(meal), Requested: int64(p.manualPct)}
		}
		return nil
	}
	if !withinDiscount(lim, p.manualPct) {
		return &terminal.ManagerRequired{Action: terminal.ActionDiscount, Limit: int64(lim.DiscountPct), Requested: int64(p.manualPct)}
	}
	return nil
}

// discountEntry journals the cart's discount: employee_meal or discount.
func discountEntry(a Actor, st terminal.Staff, d *Discount, p priced, need *terminal.ManagerRequired, approver *terminal.Staff, orderID string) terminal.Entry {
	det := map[string]any{"cents": p.manualCents, "pct": p.manualPct, "reason": strings.TrimSpace(d.Reason)}
	if approver != nil && need != nil {
		det["approved_by"] = approver.ID
	}
	action := terminal.ActionDiscount
	if d.employeeMeal() {
		action = terminal.ActionEmployeeMeal
	}
	return terminal.Entry{StaffID: st.ID, DeviceID: a.DeviceID, Action: action, OrderID: orderID, Detail: detail(det)}
}

// checkPrices marks each price change that lowers a price past the staff
// member's limit (TERMINAL-PLAN "Item price change": raising is free, a
// cashier lowers by up to cashier_price_drop_pct, a manager is unlimited)
// and answers what a manager's PIN must cover (nil = nothing): the price
// drops, and the comp lines (rung at $0: a manager always, contract
// §2.10i). A change in `kept` (Key → count: the order's earlier cart) was
// approved already.
func checkPrices(cfg *terminal.Config, st terminal.Staff, changes []terminal.PriceChange, kept map[string]int) (need, comp *terminal.ManagerRequired) {
	pct, unlimited := cfg.PriceDropOf(st)
	for i := range changes {
		c := &changes[i]
		if k := c.Key(); kept[k] > 0 {
			kept[k]--
			c.Approved = true
			continue
		}
		if c.Comp {
			if unlimited || cfg.MayComp(st) {
				continue
			}
			c.NeedsManager = true
			if comp == nil {
				comp = &terminal.ManagerRequired{Action: terminal.ActionComp}
			}
			comp.Requested += c.FromCents
			continue
		}
		if unlimited || terminal.DropAllowed(c.FromCents, c.ToCents, pct) {
			continue
		}
		c.NeedsManager = true
		if need == nil {
			need = &terminal.ManagerRequired{Action: terminal.ActionPriceChange, Limit: int64(pct)}
		}
		need.Requested = max(need.Requested, int64(c.DropPct))
	}
	return need, comp
}

// priceEntries journals every new price change (who, from → to, approver).
func priceEntries(a Actor, st terminal.Staff, changes []terminal.PriceChange, approver *terminal.Staff, orderID string) []terminal.Entry {
	var out []terminal.Entry
	for _, c := range changes {
		if c.Approved {
			continue
		}
		d := map[string]any{"line": c.Line, "item_id": c.ItemID, "item": c.Name, "from_cents": c.FromCents, "to_cents": c.ToCents,
			"drop_pct": c.DropPct}
		if c.SizeID != "" {
			d["size_id"] = c.SizeID
		}
		if c.NeedsManager && approver != nil {
			d["approved_by"] = approver.ID
		}
		action := terminal.ActionPriceChange
		if c.Comp {
			action = terminal.ActionComp
		}
		out = append(out, terminal.Entry{StaffID: st.ID, DeviceID: a.DeviceID, Action: action, OrderID: orderID, Detail: detail(d)})
	}
	return out
}

// approve covers everything in this request that is over the staff member's
// limits (nil needs are skipped) and returns the manager (nil when nothing
// needed one) with an override row per need. Each need takes its own
// single-use approval (M2: one approval used to cover every other need of
// the request); the needs without one are covered by the request's
// manager_pin (one PIN, checked once, is that manager approving the whole
// request). A need left uncovered refuses the request with that need, and
// nothing is used up.
func (s *Service) approve(ctx context.Context, a Actor, st terminal.Staff, cfg *terminal.Config, pin, orderID string, needs ...*terminal.ManagerRequired) (*terminal.Staff, []terminal.Entry, error) {
	var live []*terminal.ManagerRequired
	for _, n := range needs {
		if n != nil {
			live = append(live, n)
		}
	}
	if len(live) == 0 {
		return nil, nil, nil
	}
	by := make([]*terminal.Staff, len(live))
	if mgr, ok := preApproved(ctx, cfg); ok { // a phone charge's cart, approved at its creation
		for i := range by {
			by[i] = mgr
		}
	} else {
		isManager := func(m terminal.Staff) bool {
			cur, found := cfg.Find(m.ID)
			return found && cur.Role == terminal.RoleManager
		}
		ids := s.approvals.match(approvalsOf(ctx), a.DeviceID, live, s.now(), isManager)
		var uncovered *terminal.ManagerRequired
		for i, id := range ids {
			if id == "" && uncovered == nil {
				uncovered = live[i]
			}
		}
		var pinMgr *terminal.Staff
		if uncovered != nil {
			if strings.TrimSpace(pin) == "" {
				return nil, nil, uncovered
			}
			var err error
			if pinMgr, err = s.managerPIN(ctx, a, cfg, pin, uncovered); err != nil {
				return nil, nil, err
			}
		}
		for i, id := range ids {
			if id == "" {
				by[i] = pinMgr
				s.overrideUsed(ctx, a, live[i], pinMgr, "manager_pin")
				continue
			}
			mgr, ok := s.approvals.use(id)
			if !ok { // used by a parallel request meanwhile
				return nil, nil, live[i]
			}
			cur, _ := cfg.Find(mgr.ID)
			by[i] = &cur
			s.overrideUsed(ctx, a, live[i], &cur, "approval")
		}
	}
	rows := make([]terminal.Entry, 0, len(live))
	for i, n := range live {
		rows = append(rows, overrideEntry(a, st, by[i], n, orderID))
	}
	return by[0], rows, nil
}

// overrideUsed counts and logs one manager override.
func (s *Service) overrideUsed(ctx context.Context, a Actor, need *terminal.ManagerRequired, mgr *terminal.Staff, how string) {
	var cents int64
	if how == "manager_pin" {
		switch need.Action {
		case terminal.ActionRefund, terminal.ActionVoid, terminal.NeedCardRefund, terminal.NeedCashForCard, terminal.MovePayOut, terminal.ActionComp:
			cents = need.Requested
		}
	}
	s.cm.Override(ctx, need.Action, how, cents)
	s.logger.Info().Ctx(ctx).Str("action", need.Action).Str("approver_id", mgr.ID).Str("device_id", a.DeviceID).
		Int64("limit", need.Limit).Int64("requested", need.Requested).Bool("approval", how == "approval").Msg("Manager override")
}

// --- orders ------------------------------------------------------------------------------

var keyRe = regexp.MustCompile(`^[A-Za-z0-9_.:-]{8,64}$`)

// Customer is an order's customer (optional; a delivery needs the address).
type Customer struct {
	// ID: the store customer the order is for (GET /api/terminal/customers; optional).
	ID      string   `json:"id"`
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
	// Brand: a card payment's brand (contract 2.10m; amex | discover |
	// mastercard | visa | other, Square's names too). Ignored on cash / comp.
	Brand string `json:"brand"`
	// Goodwill: a refund with no item removed, kept as an adjustment on the
	// order (contract 2.10n). Outside: a Square app / manual card refund a
	// manager already made in Square's dashboard, recorded here.
	Goodwill bool `json:"goodwill"`
	Outside  bool `json:"outside"`
	// Approvals (X-5): single-use manager approvals instead of manager_pin.
	Approvals []string `json:"approvals,omitempty"`
	// Square's answer for a card the terminal's reader or manual entry took
	// (contract 2.10r §3; card / manual_card, ignored on other tenders). On a
	// refund, square_payment_id picks which Square payment gives the money
	// back (else the order's newest one of that tender that can).
	AuthCode            string            `json:"auth_code,omitempty"`
	Entry               string            `json:"entry,omitempty"`
	EMV                 map[string]string `json:"emv,omitempty"`
	SquarePaymentID     string            `json:"square_payment_id,omitempty"`
	SquareReceiptNumber string            `json:"square_receipt_number,omitempty"`
}

// cardInfo checks the Square fields of a card payment.
func (p PaymentIn) cardInfo() (terminal.CardInfo, error) {
	c := terminal.CardInfo{AuthCode: strings.TrimSpace(p.AuthCode), Entry: strings.TrimSpace(p.Entry),
		SquarePaymentID: strings.TrimSpace(p.SquarePaymentID), SquareReceiptNumber: strings.TrimSpace(p.SquareReceiptNumber)}
	if len(c.AuthCode) > 32 || len(c.SquarePaymentID) > 192 || len(c.SquareReceiptNumber) > 32 {
		return c, badRequest("auth_code is at most 32 characters, square_payment_id 192, square_receipt_number 32")
	}
	if c.Entry != "" && !terminal.CardEntries[c.Entry] {
		return c, badRequest("entry is contactless, chip, swiped or keyed")
	}
	if len(p.EMV) > 0 {
		c.EMV = map[string]string{}
		for k, v := range p.EMV {
			if !terminal.EMVKeys[k] || len(v) > 64 {
				return c, badRequest("emv takes application_label, aid and cvm (at most 64 characters each)")
			}
			if v = strings.TrimSpace(v); v != "" {
				c.EMV[k] = v
			}
		}
		if len(c.EMV) == 0 {
			c.EMV = nil
		}
	}
	return c, nil
}

// payment checks a payment in and makes the stored one.
func (p PaymentIn) payment(a Actor, st terminal.Staff, orderID string, at time.Time) (terminal.Payment, error) {
	if !terminal.ValidTender(p.Tender) {
		return terminal.Payment{}, badRequest("tender must be cash, card, square_app, manual_card or comp")
	}
	// A refund may give back a tip alone (amount 0): a void needs the
	// order's net tip back too (contract 2.10s §5).
	tipOnlyRefund := p.Refund && p.AmountCents == 0 && p.TipCents > 0
	if (p.AmountCents < 1 && !tipOnlyRefund) || p.AmountCents > 10_000_00 || p.TipCents < 0 || p.TipCents > 1_000_00 {
		return terminal.Payment{}, badRequest("amount_cents is 1-1000000 (0 on a tip-only refund), tip_cents 0-100000")
	}
	if p.ClientKey != "" && !keyRe.MatchString(p.ClientKey) {
		return terminal.Payment{}, badRequest("client_key is 8-64 letters, digits, _ . : -")
	}
	if len(p.Ref) > 120 || len(p.Last4) > 4 {
		return terminal.Payment{}, badRequest("ref is at most 120 characters, last4 at most 4")
	}
	if !p.Refund && p.TipCents > 0 && !terminal.IsCard(p.Tender) {
		return terminal.Payment{}, terminal.ErrTipCardOnly // tips are card only (the owner, 2026-11-12)
	}
	reason := strings.TrimSpace(p.Reason)
	if p.Refund && (reason == "" || utf8.RuneCountInString(reason) > 140) {
		return terminal.Payment{}, badRequest("a refund needs a reason (1-140 characters)")
	}
	kind := ""
	switch {
	case (p.Goodwill || p.Outside) && !p.Refund:
		return terminal.Payment{}, badRequest("goodwill / outside are kinds of refund")
	case p.Goodwill && p.Outside:
		return terminal.Payment{}, badRequest("a refund is goodwill or outside, not both")
	case p.Goodwill && p.TipCents != 0:
		return terminal.Payment{}, badRequest("a goodwill refund gives back an amount, not a tip")
	case p.Goodwill:
		kind = terminal.RefundGoodwill
	case p.Outside && p.Tender != terminal.TenderSquareApp && p.Tender != terminal.TenderManualCard:
		return terminal.Payment{}, badRequest("refunded outside is for square_app or manual_card payments")
	case p.Outside:
		kind = terminal.RefundOutside
	}
	out := terminal.Payment{ID: uuid.Must(uuid.NewV7()).String(), OrderID: orderID, ClientKey: p.ClientKey, At: at.UTC().Truncate(time.Millisecond),
		Tender: p.Tender, AmountCents: p.AmountCents, TipCents: p.TipCents, Ref: p.Ref, Last4: p.Last4, Refund: p.Refund,
		Reason: reason, StaffID: st.ID, DeviceID: a.DeviceID, RefundKind: kind}
	if terminal.IsCard(p.Tender) {
		out.Brand = terminal.NormBrand(p.Brand)
		card, err := p.cardInfo()
		if err != nil {
			return terminal.Payment{}, err
		}
		if !p.Refund { // a refund's square_payment_id names its source (squareRefund)
			out.CardInfo = card
		}
	}
	if p.Tender == terminal.TenderCash && !p.Refund {
		out.TenderedCents = max(p.TenderedCents, p.AmountCents+p.TipCents)
		out.ChangeCents = out.TenderedCents - p.AmountCents - p.TipCents
	}
	return out, nil
}

// MaxPrepMinutes bounds a placed order's quoted minutes.
const MaxPrepMinutes = 240

// prepMinutes is a placed order's minutes until ready: what the terminal
// quoted, else main's default_pickup_minutes for a pickup, else the Pi's
// default for the fulfillment.
func (s *Service) prepMinutes(cfg *terminal.Config, typ, fulfillment string, quoted *int64) int64 {
	if quoted != nil {
		return *quoted
	}
	if typ == terminal.TypePickup {
		if m, ok := cfg.DefaultPickupMinutes(); ok {
			return m
		}
	}
	return s.orders.PrepMinutesFor(fulfillment)
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
	// PrepMinutes: the minutes the cashier quoted (ASAP + quoted, +-5 on the
	// terminal): the order's ready time = now + this (0-240). Absent: main's
	// default_pickup_minutes for a pickup, else the Pi's default for the type.
	PrepMinutes *int64 `json:"prep_minutes"`
	// Approvals (X-5): single-use manager approvals instead of manager_pin.
	Approvals []string `json:"approvals,omitempty"`
	// DraftID: the shared draft this SEND came from, deleted in the order's
	// transaction (contract 2.10s §9).
	DraftID string `json:"draft_id,omitempty"`
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
	ctx = terminal.WithDraftID(WithApprovals(ctx, in.Approvals), strings.TrimSpace(in.DraftID))
	res, err := s.placeOrder(ctx, a, in)
	if err == nil && !res.Duplicate && in.DraftID != "" {
		s.publish(terminal.EventDraftRemoved, map[string]any{"id": in.DraftID})
	}
	typ := in.Type
	if typ == "" {
		typ = terminal.TypeCounter
	}
	result := resultOf(err)
	if err == nil && res.Duplicate {
		result = "duplicate"
	}
	s.cm.Placed(ctx, typ, result)
	return res, err
}

func (s *Service) placeOrder(ctx context.Context, a Actor, in OrderIn) (*PlaceResult, error) {
	st, cfg, err := s.staffFor(ctx, a) // staffOf, or a phone charge's sender (phonepay_hooks.go)
	if err != nil {
		return nil, err
	}
	if err := requireKey(in.ClientKey); err != nil {
		return nil, err
	}
	hash := orderFingerprint(in)
	if dup, err := s.duplicateOrder(ctx, in.ClientKey, hash); err != nil || dup != nil {
		return dup, err
	}
	typ, err := checkType(in.Type)
	if err != nil {
		return nil, err
	}
	if err := callcenterPlaceGuard(a, in); err != nil { // §2.10ab: before any money or print
		return nil, err
	}
	note := strings.TrimSpace(in.Note)
	if utf8.RuneCountInString(note) > 500 {
		return nil, badRequest("note is at most 500 characters")
	}
	if in.PrepMinutes != nil && (*in.PrepMinutes < 0 || *in.PrepMinutes > MaxPrepMinutes) {
		return nil, badRequest("prep_minutes is 0-240")
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
	cust.ID = strings.TrimSpace(cust.ID)
	if cust.ID != "" {
		if s.cust == nil {
			return nil, badRequest("no such customer")
		}
		if _, err := s.cust.repo.Customer(ctx, cust.ID); errors.Is(err, terminal.ErrCustomerNotFound) {
			return nil, badRequest("no such customer")
		} else if err != nil {
			return nil, err
		}
	}
	m, ms := s.store.Menu()
	if m == nil {
		return nil, terminal.ErrNoMenu
	}
	now := s.now()
	if err := callcenterCartGuard(a, counterCtx(m, s.orders.StoreLocation(), s.toppingMax()), in.Cart, nil); err != nil {
		return nil, err
	}
	in.Cart = s.withDistance(ctx, typ, &cust, in.Cart) // the delivery fee by distance (2.10ae)
	p, _, err := priceRung(m, s.orders.StoreLocation(), typ, nil, in.Cart, now, s.toppingMax())
	if err != nil {
		return nil, asBadRequest(err)
	}
	if len(p.quote.Errors) > 0 {
		return nil, &terminal.CartInvalid{Issues: p.quote.Errors}
	}
	dNeed := discountNeed(cfg, st, in.Cart.Discount, p)
	priceNeed, compNeed := checkPrices(cfg, st, p.changes, nil)
	// A comp tender needs a manager too (contract §2.10i).
	for _, pi := range in.Payments {
		if pi.Tender == terminal.TenderComp && !pi.Refund && !cfg.MayComp(st) {
			if compNeed == nil {
				compNeed = &terminal.ManagerRequired{Action: terminal.ActionComp}
			}
			compNeed.Requested += pi.AmountCents
		}
	}
	approver, journal, err := s.approve(ctx, a, st, cfg, in.ManagerPIN, "", dNeed, priceNeed, compNeed)
	if err != nil {
		return nil, err
	}
	if journal == nil {
		journal = []terminal.Entry{}
	}
	items, off, fees, err := buildItems(m, p)
	if err != nil {
		return nil, err
	}
	money := moneyOf(items, p, off, fees)
	if err := s.checkMoney(ctx, money, items); err != nil {
		return nil, err
	}

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
		pay.ClientKey = "" // the order's own key covers them
		if pay.Tender == terminal.TenderComp && compNeed != nil && approver != nil {
			pay.ApprovedBy = approver.ID
		}
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
			"staff_id": st.ID, "staff_name": st.Name(), "client_key": in.ClientKey, "menu_version": ms.Version,
			"cart": in.Cart, "pick_cents": pickCents(p)}}}
	if cust.ID != "" {
		snap.SourceMeta["terminal"].(map[string]any)["customer_id"] = cust.ID
	}
	takenBy(a, st, &snap) // a call-center order: "Call center - Name" (§2.10ab)
	for _, pay := range pays {
		snap.Payments = append(snap.Payments, pay.Summary())
	}
	journal = append(journal, terminal.Entry{StaffID: st.ID, DeviceID: a.DeviceID, Action: terminal.ActionOrderPlaced, OrderID: id,
		Detail: detail(map[string]any{"type": typ, "total_cents": money.Total, "paid_cents": paidCents, "lines": len(items), "client_key": in.ClientKey})})
	journal = append(journal, priceEntries(a, st, p.changes, approver, id)...)
	journal = append(journal, customEntries(a, st, p, id, nil)...)
	journal = append(journal, rungEntries(a, st, p, id, nil)...)
	journal = append(journal, couponEntries(a, st, p, id, nil)...)
	if p.manualCents > 0 {
		journal = append(journal, discountEntry(a, st, in.Cart.Discount, p, dNeed, approver, id))
	}
	for _, pay := range pays {
		d := map[string]any{"payment_id": pay.ID, "tender": pay.Tender, "amount_cents": pay.AmountCents, "tip_cents": pay.TipCents, "change_cents": pay.ChangeCents}
		if pay.Tender == terminal.TenderComp && compNeed != nil && approver != nil {
			d["approved_by"] = approver.ID
		}
		journal = append(journal, terminal.Entry{StaffID: st.ID, DeviceID: a.DeviceID, Action: terminal.ActionPayment, OrderID: id, Detail: detail(d)})
	}
	prep := s.prepMinutes(cfg, typ, ful.Type, in.PrepMinutes)
	stampStart(cfg, pays)
	so, created, err := s.repo.PlaceCounterOrder(ctx, terminal.NewCounterOrder{Snapshot: snap, ClientKey: in.ClientKey, RequestHash: hash,
		CounterStart: cfg.CounterStart(), PrepMinutes: prep, Payments: pays, Journal: journal,
		StaffID: st.ID, DeviceID: a.DeviceID, Now: now})
	if err != nil {
		return nil, err
	}
	if !created {
		return s.duplicateOrder(ctx, in.ClientKey, hash)
	}
	s.printKitchen(ctx, so)
	if a.Callcenter {
		// No terminal printer for it: the Pi prints the customer receipt /
		// delivery ticket on the store's receipt printer (§2.10ab).
		s.printCallcenterReceipt(ctx, so)
	}
	s.orders.CounterOrderChanged(ctx, so, nil, false)
	if s.cust != nil {
		s.cust.NoteOrder(ctx, cust.ID, cust.Phone, now)
	}
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

// Order is one counter order with its payments, or (contract §2.10i) an
// online / website pickup of the open batch with the payments the terminals
// took on it (404 for anything else).
func (s *Service) Order(ctx context.Context, id string) (*OrderView, error) {
	all, err := s.repo.CounterOrders(ctx)
	if err != nil {
		return nil, err
	}
	var found *order.StoredOrder
	for _, so := range all {
		if so.Order.ID == id {
			found = &so
			break
		}
	}
	if found == nil {
		if found, err = s.pickupOrder(ctx, id); err != nil {
			// An online delivery (§2.10aa: edited like ours).
			orp, ok := s.repo.(onlineRepo)
			if !ok {
				return nil, err
			}
			so, oerr := orp.OnlineOrder(ctx, id)
			if oerr != nil {
				return nil, err
			}
			found = &so
		}
	}
	if found.Order.Source != order.SourceCounter {
		v := withOnlineCart(*found)
		found = &v
	}
	pays, err := s.repo.OrderPayments(ctx, id)
	if err != nil {
		return nil, err
	}
	if pays == nil {
		pays = []terminal.Payment{}
	}
	return &OrderView{Order: *found, Payments: pays}, nil
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
		if !open && so.Status.Carried {
			continue // an earlier batch's check (contract 2.10s §4): on the open list only
		}
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
	// KickDrawer: cash went back - the terminal opens its drawer (2.10n).
	KickDrawer bool `json:"kick_drawer"`
}

// kicks: cash given back by these refunds (the drawer opens for it).
func kicks(refunds []terminal.Payment) int64 {
	var n int64
	for _, r := range refunds {
		if r.Refund && r.Tender == terminal.TenderCash && r.RefundKind != terminal.RefundOutside {
			n += r.AmountCents + r.TipCents
		}
	}
	return n
}

func kickEntry(a Actor, st terminal.Staff, orderID, reason string, cents int64) terminal.Entry {
	return terminal.Entry{StaffID: st.ID, DeviceID: a.DeviceID, Action: terminal.ActionDrawerKick, OrderID: orderID,
		Detail: detail(map[string]any{"order_id": orderID, "reason": reason, "cents": cents})}
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
// counter order of the open batch. A refund follows terminal.RefundNeed (a
// card refund, cash back for a card sale, or the order's cashier refunds
// past the cashier limit need manager_pin). Idempotent by the payment's
// client_key.
func (s *Service) AddPayment(ctx context.Context, a Actor, orderID string, in PaymentIn) (*PaymentResult, error) {
	ctx = WithApprovals(ctx, in.Approvals)
	res, err := s.addPayment(ctx, a, orderID, in)
	kind := "sale"
	if in.Refund {
		kind = "refund"
	}
	result := resultOf(err)
	if err == nil && res.Duplicate {
		result = "duplicate"
	}
	tender := in.Tender
	if !terminal.ValidTender(tender) {
		tender = "other"
	}
	s.cm.Payment(ctx, tender, kind, result)
	if err == nil && res.KickDrawer {
		s.logger.Info().Ctx(ctx).Str("order_id", orderID).Str("device_id", a.DeviceID).Str("reason", "refund").
			Int64("amount_cents", res.Payment.AmountCents+res.Payment.TipCents).Msg("Drawer kicked")
	}
	if err == nil && !res.Duplicate {
		// L-1: one line per money action (stable messages, Grafana's Counter
		// dashboard matches them).
		st, _, _ := s.staffOf(a)
		msg := "Counter payment taken"
		if in.Refund {
			msg = "Counter refund given"
		}
		ev := s.logger.Info()
		if in.Refund {
			ev = s.logger.Warn()
		}
		ev.Ctx(ctx).Str("order_id", orderID).Int64("number", numberOf(res.Order)).Str("staff_id", st.ID).Str("staff_name", st.Name()).
			Str("role", st.Role).Str("device_id", a.DeviceID).Str("tender", res.Payment.Tender).Int64("amount_cents", res.Payment.AmountCents).
			Int64("tip_cents", res.Payment.TipCents).Str("drawer_id", res.Payment.DrawerID).Str("approver_id", res.Payment.ApprovedBy).
			Str("reason", res.Payment.Reason).Int64("balance_cents", res.BalanceCents).Msg(msg)
	}
	return res, err
}

// resultOf is a terminal error's bounded metric label.
func resultOf(err error) string {
	var mr *terminal.ManagerRequired
	var ci *terminal.CartInvalid
	var kr *terminal.KeyReused
	switch {
	case err == nil:
		return "ok"
	case errors.As(err, &kr):
		return "key_reused"
	case errors.As(err, &mr), errors.Is(err, terminal.ErrManagerPINWrong):
		return "manager_required"
	case errors.As(err, &ci):
		return "cart_invalid"
	case errors.Is(err, terminal.ErrOverBalance):
		return "over_balance"
	case errors.Is(err, terminal.ErrRefundTooBig):
		return "refund_too_big"
	case errors.Is(err, terminal.ErrNotPayable):
		return "not_payable"
	case errors.Is(err, terminal.ErrNoMenu):
		return "menu_unavailable"
	}
	return "error"
}

func (s *Service) addPayment(ctx context.Context, a Actor, orderID string, in PaymentIn) (*PaymentResult, error) {
	st, cfg, err := s.staffFor(ctx, a) // staffOf, or the phone charge's sender (phonepay_hooks.go)
	if err != nil {
		return nil, err
	}
	if a.Callcenter { // §2.10ab: a typed card only; no manager, no refund
		if strings.TrimSpace(in.ManagerPIN) != "" || len(in.Approvals) > 0 {
			return nil, notAllowed(terminal.NotAllowedManager)
		}
		if err := callcenterPaymentsGuard(a, in); err != nil {
			return nil, err
		}
	}
	if _, err := uuid.Parse(orderID); err != nil {
		return nil, order.ErrNotFound
	}
	if err := requireKey(in.ClientKey); err != nil {
		return nil, err
	}
	hash := paymentFingerprint(in)
	// The same key again: the stored answer (or 409 key_reused) before any
	// limit is checked - a retried refund is not a second refund.
	if prev, err := s.repo.PaymentByKey(ctx, orderID, in.ClientKey); err != nil {
		return nil, err
	} else if prev != nil {
		if !terminal.SameRequest(prev.RequestHash, hash) {
			return nil, &terminal.KeyReused{ClientKey: in.ClientKey, OrderID: orderID}
		}
		return s.duplicatePayment(ctx, orderID, *prev)
	}
	pay, err := in.payment(a, st, orderID, s.now())
	if err != nil {
		return nil, err
	}
	pay.RequestHash = hash
	var approver *terminal.Staff
	var need *terminal.ManagerRequired
	var before []terminal.Payment
	switch {
	case pay.Refund:
		// Decided before the transaction (a manager's PIN is a bcrypt
		// check); the transaction checks it again.
		if before, err = s.repo.OrderPayments(ctx, orderID); err != nil {
			return nil, err
		}
		need = terminal.RefundNeed(cfg, st, before, []terminal.Payment{pay}, terminal.ActionRefund)
		if err := terminal.RefundRefused(before, pay); err != nil {
			return nil, err // refused for anyone: no manager is asked first
		}
		// An earlier day's order (not carried): a manager's refund (2026-11-12).
		if earlierDay(ctx, s.repo, orderID) {
			need = terminal.EarlierDayRefundNeed(cfg, st, []terminal.Payment{pay})
		}
	case pay.Tender == terminal.TenderComp && !cfg.MayComp(st):
		// A driver's comp needs a manager (contract §2.10i).
		need = &terminal.ManagerRequired{Action: terminal.ActionComp, Requested: pay.AmountCents}
	}
	if need != nil {
		if approver, err = s.manager(ctx, a, cfg, in.ManagerPIN, need); err != nil {
			return nil, err
		}
		pay.ApprovedBy = approver.ID
	}
	// A refund of an in-app card payment goes back through Square first
	// (contract 2.10r §4): recorded only once Square took it.
	if pay.Refund && pay.RefundKind == "" {
		if err := s.squareRefund(ctx, orderID, in, &pay, before); err != nil {
			return nil, err
		}
	}
	so, got, dup, err := s.repo.AddCounterPayment(ctx, orderID, in.ClientKey, hash, func(cur order.StoredOrder, paid []terminal.Payment) ([]terminal.Payment, []terminal.Entry, error) {
		if pay.Refund {
			if err := terminal.RefundRefused(paid, pay); err != nil {
				return nil, nil, err // more back than was paid, or than was comped
			}
			if approver == nil {
				if n := terminal.RefundNeed(cfg, st, paid, []terminal.Payment{pay}, terminal.ActionRefund); n != nil {
					return nil, nil, n // another refund raced this one
				}
				if cur.Status.EarlierBatch {
					if n := terminal.EarlierDayRefundNeed(cfg, st, []terminal.Payment{pay}); n != nil {
						return nil, nil, n
					}
				}
			}
		} else if cur.Status.EarlierBatch {
			return nil, nil, terminal.ErrNotPayable // an earlier day's order takes refunds only
		} else if due := terminal.DueOf(cur); pay.AmountCents > due {
			return nil, nil, fmt.Errorf("%w: the balance is %d", terminal.ErrOverBalance, due)
		}
		action := terminal.ActionPayment
		if pay.Refund {
			action = terminal.ActionRefund
		}
		d := map[string]any{"payment_id": pay.ID, "tender": pay.Tender, "amount_cents": pay.AmountCents, "tip_cents": pay.TipCents,
			"change_cents": pay.ChangeCents}
		if pay.Refund {
			d["reason"] = pay.Reason
			switch pay.RefundKind {
			case terminal.RefundGoodwill:
				d["goodwill"] = true
			case terminal.RefundOutside:
				d["outside"] = true
			}
			if pay.SquareRefundID != "" {
				d["square_refund_id"], d["square_payment_id"] = pay.SquareRefundID, pay.SquarePaymentID
			}
		}
		if approver != nil {
			d["approved_by"] = approver.ID
		}
		entries := []terminal.Entry{{StaffID: st.ID, DeviceID: a.DeviceID, Action: action, OrderID: orderID, Detail: detail(d)}}
		if approver != nil {
			entries = append(entries, overrideEntry(a, st, approver, need, orderID))
		}
		if cents := kicks([]terminal.Payment{pay}); cents > 0 {
			entries = append(entries, kickEntry(a, st, orderID, "refund", cents))
		}
		pay.DrawerStart = cfg.DrawerStartCents()
		return []terminal.Payment{pay}, entries, nil
	})
	if err != nil {
		return nil, err
	}
	if !dup {
		s.orders.CounterOrderChanged(ctx, so, nil, false)
		s.publish(EventOrderPaid, map[string]any{"order": so, "payment": got})
		s.deliveryUpdated(ctx, so)
	}
	return &PaymentResult{Order: so, Payment: got, Duplicate: dup, BalanceCents: so.Status.CashDueCents,
		KickDrawer: !dup && kicks([]terminal.Payment{got}) > 0}, nil
}

// voidReduceOrder: which tenders' refunds shrink first when another tender
// already gave back more than it took (a void never returns more than the
// order holds). Cash last: the drawer keeps what it was paid.
var voidReduceOrder = []string{terminal.TenderComp, terminal.TenderManualCard, terminal.TenderSquareApp, terminal.TenderCard, terminal.TenderCash}

// voidRefunds are the refunds a void makes: one per tender, of what that
// tender still holds. A tender refunded past zero (a tender change: card
// paid, cash given back) already returned money for the others: what it
// gave back over its own comes off the other tenders' refunds (comp first,
// cash last), so the void returns exactly the order's net amount and net
// tip. approver (nil = none) is set on each.
func voidRefunds(paid []terminal.Payment, id, reason string, st terminal.Staff, a Actor, now time.Time, approver *terminal.Staff) []terminal.Payment {
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
	amtBack, tipBack := map[string]int64{}, map[string]int64{}
	var amtDeficit, tipDeficit int64
	for _, t := range tenders {
		n := by[t]
		amtBack[t], tipBack[t] = max(n.amount, 0), max(n.tip, 0)
		amtDeficit += max(-n.amount, 0)
		tipDeficit += max(-n.tip, 0)
	}
	for _, t := range voidReduceOrder {
		cut := min(amtBack[t], amtDeficit)
		amtBack[t] -= cut
		amtDeficit -= cut
		cut = min(tipBack[t], tipDeficit)
		tipBack[t] -= cut
		tipDeficit -= cut
	}
	var refunds []terminal.Payment
	for _, t := range tenders {
		if amtBack[t] <= 0 && tipBack[t] <= 0 {
			continue
		}
		r := terminal.Payment{ID: uuid.Must(uuid.NewV7()).String(), OrderID: id, At: now, Tender: t, AmountCents: amtBack[t],
			TipCents: tipBack[t], Refund: true, Reason: reason, StaffID: st.ID, DeviceID: a.DeviceID}
		if approver != nil {
			r.ApprovedBy = approver.ID
		}
		refunds = append(refunds, r)
	}
	return refunds
}

// VoidIn is POST /api/terminal/orders/{id}/void (and /waste).
type VoidIn struct {
	Reason string `json:"reason"`
	// ManagerPIN / Approvals: a void of a sent order is a manager's (the
	// owner, 2026-11-12 evening): a cashier or a driver sends a manager's
	// PIN or an approval for "void"; a manager voids alone.
	ManagerPIN string   `json:"manager_pin"`
	Approvals  []string `json:"approvals,omitempty"`
}

// VoidResult is the voided (or wasted) order.
type VoidResult struct {
	Order         order.StoredOrder  `json:"order"`
	Refunds       []terminal.Payment `json:"refunds"`
	RefundedCents int64              `json:"refunded_cents"`
	Already       bool               `json:"already"`
	// KickDrawer: always false now (a void gives no money back); kept for
	// older terminals.
	KickDrawer bool `json:"kick_drawer"`
}

// heldBack is the money a void would have to give back: every tender's net
// but a comp's (a comp moves no money and is reversed for free).
func heldBack(refunds []terminal.Payment) int64 {
	var n int64
	for _, r := range refunds {
		if r.Tender != terminal.TenderComp {
			n += r.AmountCents + r.TipCents
		}
	}
	return n
}

// Void cancels an unpaid counter order of the open batch, or a carried one
// (accepted, ready or out): a manager alone, anyone else with a manager's
// PIN / approval for "void" (the owner, 2026-11-12 evening: every order here
// was sent to the kitchen; that morning it was any staff member). The call
// center cancels its own unpaid orders without one (2.10ab). An order that
// holds money answers 409 refund_first: refund it
// first under the refund rules. A comp on it is reversed for free. Every
// station its tickets went to gets the VOID ticket (its lines under REMOVE /
// QUITAR); main hears order.void {kind: void}.
func (s *Service) Void(ctx context.Context, a Actor, id string, in VoidIn) (*VoidResult, error) {
	return s.void(ctx, a, id, in, "")
}

// WasteIn is POST /api/terminal/orders/{id}/waste.
type WasteIn struct {
	Reason string `json:"reason"`
	// ManagerPIN / Approvals: as VoidIn's (a waste is a void of a sent order).
	ManagerPIN string   `json:"manager_pin"`
	Approvals  []string `json:"approvals,omitempty"`
}

// BadWasteReason: the reason isn't on the store's list (400 bad_reason
// {reasons}).
type BadWasteReason struct{ Reasons []string }

func (e *BadWasteReason) Error() string { return "pick a waste reason from the store's list" }

// Waste throws away an unpaid order that was made (Void waste, contract
// 2.10s §5): like a void but nothing prints; the reason (one of main's
// waste_reasons) goes on the till / Z report's Waste section (journal
// void.waste).
func (s *Service) Waste(ctx context.Context, a Actor, id string, in WasteIn) (*VoidResult, error) {
	_, cfg, err := s.staffOf(a)
	if err != nil {
		return nil, err
	}
	reason := cfg.WasteReasonOf(in.Reason)
	if reason == "" {
		return nil, &BadWasteReason{Reasons: cfg.WasteReasons()}
	}
	return s.void(ctx, a, id, VoidIn{Reason: reason, ManagerPIN: in.ManagerPIN, Approvals: in.Approvals}, reason)
}

// voidNeed: voiding (or wasting) a sent order is a manager's (the owner,
// 2026-11-12 evening): nil for a manager / anyone main made unlimited, for
// the call center (it cancels its own unpaid orders with a picked reason and
// has no manager, contract 2.10ab), and for an order that is voided already
// or isn't there (the void's own answers come first).
func (s *Service) voidNeed(ctx context.Context, a Actor, st terminal.Staff, cfg *terminal.Config, id string) *terminal.ManagerRequired {
	if a.Callcenter || cfg.VoidNeed(st, 1) == nil {
		return nil
	}
	cur, err := s.Order(ctx, id)
	if err != nil || cur.Order.Status.State == order.StateCancelled {
		return nil
	}
	return cfg.VoidNeed(st, cur.Order.Order.Money.Total)
}

func (s *Service) void(ctx context.Context, a Actor, id string, in VoidIn, waste string) (*VoidResult, error) {
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
	if err := s.storeHolds(ctx, a, id); err != nil { // the store wins (§2.10ab)
		return nil, err
	}
	paid, err := s.repo.OrderPayments(ctx, id)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC().Truncate(time.Millisecond)
	if held := heldBack(voidRefunds(paid, id, reason, st, a, now, nil)); held > 0 {
		s.cm.Void(ctx, st.Role, voidKind(waste), false)
		return nil, &terminal.PaidFirst{PaidCents: held}
	}
	// A manager for a sent order (after refund_first: the money comes back
	// under the refund rules first).
	var approver *terminal.Staff
	need := s.voidNeed(ctx, a, st, cfg, id)
	if need != nil {
		if approver, err = s.manager(WithApprovals(ctx, in.Approvals), a, cfg, in.ManagerPIN, need); err != nil {
			return nil, err
		}
		ctx = terminal.WithVoidApprover(ctx, approver.ID)
	}
	so, prev, refunds, returned, err := s.repo.VoidCounterOrder(ctx, id, reason, waste, st.ID, a.DeviceID, func(cur order.StoredOrder, paid []terminal.Payment) ([]terminal.Payment, []terminal.Entry, error) {
		refunds := voidRefunds(paid, id, reason, st, a, now, nil)
		if held := heldBack(refunds); held > 0 {
			return nil, nil, &terminal.PaidFirst{PaidCents: held} // a payment raced the void
		}
		entries := []terminal.Entry{}
		for _, r := range refunds { // comps reversed: no money moves
			entries = append(entries, terminal.Entry{StaffID: st.ID, DeviceID: a.DeviceID, Action: terminal.ActionRefund, OrderID: id,
				Detail: detail(map[string]any{"payment_id": r.ID, "tender": r.Tender, "amount_cents": r.AmountCents, "tip_cents": r.TipCents, "reason": reason, "void": true})})
		}
		d := map[string]any{"reason": reason, "refunded_cents": 0, "number": numberOf(cur), "state": cur.Status.State,
			"total_cents": cur.Order.Money.Total}
		action := terminal.ActionVoid
		if waste != "" {
			action = terminal.ActionVoidWaste
			items := make([]map[string]any, 0, len(cur.Order.Items))
			for _, it := range cur.Order.Items {
				items = append(items, map[string]any{"qty": max(it.Quantity, 1), "name": it.Name})
			}
			d["items"] = items
		}
		if approver != nil {
			d["approved_by"] = approver.ID
			entries = append(entries, overrideEntry(a, st, approver, need, id))
		}
		entries = append([]terminal.Entry{{StaffID: st.ID, DeviceID: a.DeviceID, Action: action, OrderID: id, Detail: detail(d)}}, entries...)
		return refunds, entries, nil
	})
	if errors.Is(err, terminal.ErrAlreadyVoided) {
		return &VoidResult{Order: so, Refunds: []terminal.Payment{}, Already: true}, nil
	}
	if err != nil {
		return nil, err
	}
	if waste == "" {
		s.orders.PrintVoidSlip(ctx, prev)
	}
	s.orders.CounterOrderChanged(ctx, so, &prev, returned)
	s.publish(EventOrderVoided, map[string]any{"order": so})
	if refunds == nil {
		refunds = []terminal.Payment{}
	}
	s.cm.Void(ctx, st.Role, voidKind(waste), false)
	msg, what := "Counter order voided", "voided"
	if waste != "" {
		msg, what = "Counter order wasted", "wasted ("+waste+")"
	}
	s.orders.DevEvent(fmt.Sprintf("Counter order #%d %s", numberOf(so), what))
	s.logger.Info().Ctx(ctx).Str("order_id", id).Int64("number", numberOf(so)).Str("staff_id", st.ID).Str("staff_name", st.Name()).
		Str("role", st.Role).Str("device_id", a.DeviceID).Int64("amount_cents", prev.Order.Money.Total).Str("reason", reason).Msg(msg)
	return &VoidResult{Order: so, Refunds: refunds}, nil
}

func voidKind(waste string) string {
	if waste != "" {
		return "waste"
	}
	return "void"
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

// stampStart gives each payment the starting cash of a drawer session it
// may open by itself (main's drawer_start_cents, default $100).
func stampStart(cfg *terminal.Config, pays []terminal.Payment) {
	for i := range pays {
		pays[i].DrawerStart = cfg.DrawerStartCents()
	}
}
