package transport

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"quiccpos/agent/internal/domain/device"
	"quiccpos/agent/internal/domain/terminal"
)

// The store-side security round (2026-11-12; contract/agent.md §2.10y
// "Pairing window", §2.10s "PIN guard", §2.10 "A staff login"):
//
//   - the admin pairing code works only while a pairing window is open;
//   - a terminal key alone no longer reads the customer book, the orders,
//     the journal, the reports or the store's Square sign-in: a staff
//     member must be logged in on it;
//   - the daily ceiling of wrong PINs and its unlock;
//   - the admin code checked by the Pi for the apps' admin / developer mode.

// SetAppEnv tells the server which environment it runs in (APP_ENV): a prod
// Pi refuses the default admin code outside a manager's pairing window.
func (s *Server) SetAppEnv(env string) { s.appEnv = strings.TrimSpace(env) }

func (s *Server) securityRoutes() []routeDef {
	return []routeDef{
		{"GET", "/api/pairing/window", "device", "The admin code's pairing window {open, reason: manager|first_setup|\"\", until?, minutes, default_code_ok}", s.handleGetPairWindow},
		{"POST", "/api/pairing/window", "device", "Open the pairing window for the admin code (the Pi page: {}; a tablet: {username, password} = the store manager login) -> the window | 400 | 401 bad_login | 403 terminal_only | 409 | 429", s.handleOpenPairWindow},
		{"DELETE", "/api/pairing/window", "device", "Close the pairing window -> the window", s.handleClosePairWindow},
		{"POST", "/api/terminal/pairing/window", "terminal", "Open the pairing window from a paired terminal {manager_pin?, approvals?} (a manager logged in, or a manager's PIN / approval for pairing_window) -> the window | 401 login_required | 403 manager_required / manager_pin_invalid", s.handleTerminalOpenPairWindow},
		{"POST", "/api/admin-code/check", "device", "Is this the store's admin code {code}? (the apps' admin / developer mode) -> {ok: true} | 400 | 401 wrong_admin_code | 403 admin_code_off | 429 too_many_attempts", s.handleAdminCodeCheck},
		{"GET", "/api/pin-guard", "device", "The daily ceiling of wrong PINs {locked, locked_at?, misses_24h, daily_ceiling}", s.handleGetPinGuard},
		{"POST", "/api/pin-guard/unlock", "device", "Unlock the PIN checks after the daily ceiling (the Pi page: {}; a tablet: {username, password} = the store manager login) -> the guard | 401 bad_login | 403 terminal_only | 429", s.handleUnlockPins},
		{"PUT", "/api/settings/pin-guard", "session", "Set the daily ceiling of wrong PINs {daily_ceiling 10-1000 (default 40)} -> the guard | 400", s.audited("pin-guard", s.readPinGuard, s.handleSetPinGuard)},
	}
}

// --- the pairing window ---------------------------------------------------------

// pairWindow is a manager's open pairing window (in memory: a restart
// closes it).
type pairWindow struct {
	mu    sync.Mutex
	until time.Time
	by    string
}

func (p *pairWindow) open(until time.Time, by string) {
	p.mu.Lock()
	p.until, p.by = until, by
	p.mu.Unlock()
}

// close closes the window: was = it was open at now.
func (p *pairWindow) close(now time.Time) (was bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	was = now.Before(p.until)
	p.until, p.by = time.Time{}, ""
	return was
}

func (p *pairWindow) openUntil(now time.Time) (time.Time, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.until, now.Before(p.until)
}

const (
	pairWindowManager    = "manager"
	pairWindowFirstSetup = "first_setup"
)

type pairWindowView struct {
	Open          bool       `json:"open"`
	Reason        string     `json:"reason"`
	Until         *time.Time `json:"until,omitempty"`
	Minutes       int        `json:"minutes"`
	DefaultCodeOK bool       `json:"default_code_ok"`
}

// firstSetup: a linked Pi that never paired a terminal (revoked ones
// count: a Pi that had one is past its first setup).
func (s *Server) firstSetup(ctx context.Context) bool {
	if s.devices == nil {
		return false
	}
	key, err := s.settings.GetStoreAPIKey(ctx)
	if err != nil || key == "" {
		return false
	}
	ds, err := s.devices.List(ctx)
	if err != nil {
		return false
	}
	for _, d := range ds {
		if d.Kind == device.KindTerminal {
			return false
		}
	}
	return true
}

func (s *Server) isProd() bool { return s.appEnv == "prod" || s.appEnv == "production" }

// pairWindowNow is the window as it stands: a manager's (until), else the
// first-setup one.
func (s *Server) pairWindowNow(ctx context.Context, ap device.AdminPair, now time.Time) pairWindowView {
	v := pairWindowView{Minutes: int(ap.Window() / time.Minute)}
	if until, ok := s.pairWin.openUntil(now); ok {
		u := until.UTC()
		v.Open, v.Reason, v.Until = true, pairWindowManager, &u
	} else if s.firstSetup(ctx) {
		v.Open, v.Reason = true, pairWindowFirstSetup
	}
	// A prod Pi takes the default code only in a manager's window.
	v.DefaultCodeOK = !s.isProd() || v.Reason == pairWindowManager
	return v
}

func (s *Server) adminPair(ctx context.Context) device.AdminPair {
	if s.setups == nil {
		return device.AdminPair{}
	}
	ap, err := s.setups.AdminPair(ctx)
	if err != nil {
		return device.AdminPair{}
	}
	return ap
}

func (s *Server) handleGetPairWindow(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, s.pairWindowNow(r.Context(), s.adminPair(r.Context()), time.Now()))
}

// managerBody is the store manager login a tablet sends with a
// manager-only action.
type managerBody struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// managerAction passes the Pi page's session at once; a tablet's key needs
// the store manager login in the body (checked like /api/pairing/code's,
// under the login throttle); a terminal's key is refused (terminals have
// their own PIN-gated routes). by = who did it, for the journal.
func (s *Server) managerAction(w http.ResponseWriter, r *http.Request) (by string, ok bool) {
	d, isDevice := r.Context().Value(deviceCtxKey{}).(*device.Device)
	if !isDevice {
		return "Pi web page", true
	}
	if d.Kind != device.KindTablet {
		s.writeJSON(w, http.StatusForbidden, errCodeMap("a tablet (with the store manager login) or the Pi web page does this", "terminal_only"))
		return "", false
	}
	var body managerBody
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
			s.writeJSON(w, http.StatusBadRequest, errCodeMap("invalid body", "bad_request"))
			return "", false
		}
	}
	if strings.TrimSpace(body.Username) == "" || body.Password == "" {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("the store manager login (username, password) is needed", "bad_request"))
		return "", false
	}
	if !s.managerLoginOK(w, r, body.Username, body.Password) {
		return "", false
	}
	return d.Name, true
}

// openPairWindow opens a manager's window for the setting's minutes.
func (s *Server) openPairWindow(ctx context.Context, deviceID, by string) pairWindowView {
	ap := s.adminPair(ctx)
	now := time.Now()
	s.pairWin.open(now.Add(ap.Window()), by)
	s.logger.Info().Ctx(ctx).Str("by", by).Int("minutes", int(ap.Window()/time.Minute)).Msg("Pairing window opened")
	if s.terminalSvc != nil {
		s.terminalSvc.RecordPi(ctx, deviceID, terminal.ActionPairingWindow, map[string]any{"event": "opened", "by": by, "minutes": int(ap.Window() / time.Minute)})
	}
	return s.pairWindowNow(ctx, ap, now)
}

// POST /api/pairing/window (the Pi page's session, or a tablet + the store
// manager login).
func (s *Server) handleOpenPairWindow(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if key, err := s.settings.GetStoreAPIKey(ctx); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	} else if key == "" {
		s.writeJSON(w, http.StatusConflict, errCodeMap("This Pi isn't set up yet — set it up with the store manager login first.", "not_linked"))
		return
	}
	by, ok := s.managerAction(w, r)
	if !ok {
		return
	}
	s.writeJSON(w, http.StatusOK, s.openPairWindow(ctx, deviceIDOf(r), by))
}

// deviceIDOf is the calling device's id ("web" = the Pi page's session).
func deviceIDOf(r *http.Request) string {
	if d, ok := r.Context().Value(deviceCtxKey{}).(*device.Device); ok {
		return d.ID
	}
	return "web"
}

// DELETE /api/pairing/window (the Pi page or a tablet: closing needs no login).
func (s *Server) handleClosePairWindow(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if d, ok := r.Context().Value(deviceCtxKey{}).(*device.Device); ok && d.Kind != device.KindTablet {
		s.writeJSON(w, http.StatusForbidden, errCodeMap("a tablet or the Pi web page does this", "terminal_only"))
		return
	}
	s.closePairWindow(ctx, deviceIDOf(r), "closed")
	s.writeJSON(w, http.StatusOK, s.pairWindowNow(ctx, s.adminPair(ctx), time.Now()))
}

func (s *Server) closePairWindow(ctx context.Context, deviceID, event string) {
	if !s.pairWin.close(time.Now()) {
		return
	}
	s.logger.Info().Ctx(ctx).Str("event", event).Msg("Pairing window closed")
	if s.terminalSvc != nil {
		s.terminalSvc.RecordPi(ctx, deviceID, terminal.ActionPairingWindow, map[string]any{"event": event})
	}
}

// POST /api/terminal/pairing/window {manager_pin?, approvals?} — a paired
// terminal opens the window: a manager logged in on it, or a manager's PIN /
// approval for pairing_window.
func (s *Server) handleTerminalOpenPairWindow(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	var body struct {
		ManagerPIN string   `json:"manager_pin"`
		Approvals  []string `json:"approvals"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if !s.decodeTerminal(w, r, &body) {
			return
		}
	}
	a := actor(r)
	if a.Callcenter {
		s.terminalError(w, r, &terminal.NotAllowedForCallcenter{What: terminal.NeedPairingWindow})
		return
	}
	mgr, err := s.terminalSvc.RequireManager(r.Context(), a, body.ManagerPIN, body.Approvals, terminal.NeedPairingWindow)
	if err != nil {
		s.terminalError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, s.openPairWindow(r.Context(), a.DeviceID, mgr.Name()+" on "+a.DeviceName))
}

// pairWindowGate is POST /api/pair/admin's window check, before the code is
// looked at: false = answered (403 pairing_closed / default_code_refused).
// Nothing is counted against the caller: a closed window can't be guessed
// at, and can't be used to pause pairing for the store.
func (s *Server) pairWindowGate(w http.ResponseWriter, r *http.Request, ap device.AdminPair) (pairWindowView, bool) {
	v := s.pairWindowNow(r.Context(), ap, time.Now())
	if !v.Open {
		s.logger.Debug().Ctx(r.Context()).Str("ip", clientIP(r)).Msg("Pairing refused: the pairing window is closed")
		s.writeJSON(w, http.StatusForbidden, errCodeMap("Pairing is closed. A manager opens it on the Pi page (Settings > Tablets > Open pairing) or in Quibase (Help > Pair a counter terminal); it stays open for "+
			strconv.Itoa(v.Minutes)+" minutes.", "pairing_closed"))
		return v, false
	}
	if ap.IsDefault() && !v.DefaultCodeOK {
		s.logger.Warn().Ctx(r.Context()).Str("ip", clientIP(r)).Msg("Pairing refused: the default admin code outside a manager's pairing window")
		s.writeJSON(w, http.StatusForbidden, errCodeMap("This store still uses the standard admin code: a manager opens pairing first (the Pi page, Settings > Tablets, or Quibase, Help > Pair a counter terminal), or sets the store's own code there.", "default_code_refused"))
		return v, false
	}
	return v, true
}

// --- POST /api/admin-code/check -----------------------------------------------

// POST /api/admin-code/check {code} (a paired tablet or terminal, the Pi
// page) — the apps' admin / developer mode asks the Pi whether a code is the
// store's admin code (it was a literal on the device). Wrong codes count in
// the admin code's own throttle; never logs the code.
func (s *Server) handleAdminCodeCheck(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&body); err != nil {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("invalid body", "bad_request"))
		return
	}
	code := strings.TrimSpace(body.Code)
	if !device.ValidAdminCode(code) {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("The admin code is 4 to 8 digits.", "bad_request"))
		return
	}
	if s.setups == nil {
		s.writeJSON(w, http.StatusForbidden, errCodeMap("This Pi has no admin code.", "admin_code_off"))
		return
	}
	ap, err := s.setups.AdminPair(ctx)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	want := ap.Effective()
	if want == "" {
		s.writeJSON(w, http.StatusForbidden, errCodeMap("The admin code is turned off on this Pi.", "admin_code_off"))
		return
	}
	ip := clientIP(r)
	now := time.Now()
	if wait := s.adminCodes.wait(ip, now); wait > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		s.writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "Too many wrong codes — wait a few minutes and try again.", "code": "too_many_attempts",
			"retry_after": int(wait.Seconds()) + 1})
		return
	}
	ok := subtle.ConstantTimeCompare([]byte(code), []byte(want)) == 1
	if s.terminalSvc != nil {
		s.terminalSvc.RecordPi(ctx, deviceIDOf(r), terminal.ActionAdminCodeCheck, map[string]any{"ok": ok})
	}
	if !ok {
		s.adminCodes.fail(ip, now)
		s.logger.Warn().Ctx(ctx).Str("ip", ip).Str("device_id", deviceIDOf(r)).Msg("Admin code check: wrong code")
		s.writeJSON(w, http.StatusUnauthorized, errCodeMap("That isn't the admin code.", "wrong_admin_code"))
		return
	}
	s.adminCodes.ok(ip)
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// --- the PIN guard (the daily ceiling) ----------------------------------------

func (s *Server) handleGetPinGuard(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	s.writeJSON(w, http.StatusOK, s.terminalSvc.PinGuard())
}

// POST /api/pin-guard/unlock (the Pi page's session, or a tablet + the store
// manager login).
func (s *Server) handleUnlockPins(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	by, ok := s.managerAction(w, r)
	if !ok {
		return
	}
	s.writeJSON(w, http.StatusOK, s.terminalSvc.UnlockPins(r.Context(), by))
}

func (s *Server) readPinGuard(*http.Request) any {
	if s.terminalSvc == nil {
		return nil
	}
	return map[string]any{"daily_ceiling": s.terminalSvc.PinGuard().DailyCeiling}
}

// PUT /api/settings/pin-guard {daily_ceiling}.
func (s *Server) handleSetPinGuard(w http.ResponseWriter, r *http.Request) {
	if !s.terminalReady(w) {
		return
	}
	var body struct {
		DailyCeiling *int `json:"daily_ceiling"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.DailyCeiling == nil {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("send daily_ceiling", "bad_request"))
		return
	}
	g, err := s.terminalSvc.SetPinCeiling(r.Context(), *body.DailyCeiling)
	if err != nil {
		s.terminalError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, g)
}

// securityError answers the security round's terminal errors (terminalError
// asks it first): true = answered.
func (s *Server) securityError(w http.ResponseWriter, err error) bool {
	var locked *terminal.PinLocked
	switch {
	case errors.As(err, &locked):
		// The daily ceiling of wrong PINs: every PIN check is refused until
		// a manager unlocks them (the Pi page / Quibase).
		s.writeJSON(w, http.StatusForbidden, map[string]any{"error": err.Error(), "code": "pin_locked", "locked_at": locked.At.UTC()})
	case errors.Is(err, terminal.ErrManagerPINShort):
		s.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "A manager's PIN is 4 digits: set a new one on the Staff page.",
			"code": "wrong_pin", "reason": "manager_pin_short"})
	case errors.Is(err, terminal.ErrManagerPINShortInline):
		s.writeJSON(w, http.StatusForbidden, map[string]any{"error": "A manager's PIN is 4 digits: set a new one on the Staff page.",
			"code": "manager_pin_invalid", "reason": "manager_pin_short"})
	default:
		return false
	}
	return true
}

// --- a staff login on the terminal routes that read the store's data ----------

// staffRoutes are the /api/terminal/* routes (mux patterns) a terminal's key
// alone used to open: they need a staff member logged in on that terminal
// (401 login_required). The routes that change something check the login
// themselves; the Pi page's session (the store manager login) reads them as
// before. Left open to a key alone: login, logout, session, menu, quote,
// status, setup, the app release, the device-level journal rows.
var staffRoutes = map[string]bool{
	"GET /api/terminal/customers":                 true,
	"POST /api/terminal/customers":                true,
	"PATCH /api/terminal/customers/{id}":          true,
	"POST /api/terminal/customers/{id}/addresses": true,
	"GET /api/terminal/customers/{id}/orders":     true,
	"GET /api/terminal/addresses":                 true,
	"GET /api/terminal/orders":                    true,
	"GET /api/terminal/orders/{id}":               true,
	"GET /api/terminal/journal":                   true,
	"GET /api/terminal/drafts":                    true,
	"PUT /api/terminal/drafts/{id}":               true,
	"DELETE /api/terminal/drafts/{id}":            true,
	"GET /api/terminal/unsettled":                 true,
	"GET /api/terminal/pickups":                   true,
	"GET /api/terminal/z":                         true,
	"GET /api/terminal/checks":                    true,
	"GET /api/terminal/checks/{id}":               true,
	"GET /api/terminal/reports/batches":           true,
	"GET /api/terminal/reports/till":              true,
	"GET /api/terminal/reports/drivers":           true,
	"GET /api/terminal/square/session":            true,
	"GET /api/terminal/orders/{id}/phone-charges": true,
	"GET /api/terminal/phone-charges/{id}":        true,
	"GET /api/terminal/refunds/card-payments":     true, // batch J: the general card refund's fallback
}

// driverRoutes are the /api/terminal/* routes (mux patterns) a terminal with a
// driver logged in may call (contract §2.10b "A driver's login"): the login
// itself, what keeps any terminal running (session, menu, status, receipt
// header, setup summary, the device journal, its self-update) and his own
// /driver/* routes. Every other terminal route answers him 403
// driver_not_allowed: no order taking, payments, voids, refunds, customers,
// drafts, lists, checks, reports, Square or printing.
var driverRoutes = map[string]bool{
	"POST /api/terminal/login":                     true,
	"POST /api/terminal/logout":                    true,
	"GET /api/terminal/session":                    true,
	"GET /api/terminal/menu":                       true,
	"GET /api/terminal/status":                     true,
	"GET /api/terminal/receipt-header":             true,
	"GET /api/terminal/setup":                      true,
	"POST /api/terminal/journal":                   true,
	"POST /api/terminal/app/state":                 true,
	"GET /api/terminal/app/latest":                 true,
	"GET /api/terminal/app/apk":                    true,
	"GET /api/terminal/driver/available":           true,
	"POST /api/terminal/driver/claim":              true,
	"GET /api/terminal/driver/mine":                true,
	"POST /api/terminal/driver/orders/{id}/tip":    true,
	"POST /api/terminal/driver/orders/{id}/no-tip": true,
	"GET /api/terminal/driver/tips":                true,
	"POST /api/terminal/driver/orders/{id}/close":  true,
	"GET /api/terminal/driver/report":              true,
	"POST /api/terminal/driver/report":             true,
}

// staffGate is the terminal wrapper's check for a request with a terminal's
// key: it notes the call (the staff session stays alive while its terminal
// talks to the Pi, M4) and refuses a staffRoutes route with nobody logged
// in. false = answered.
func (s *Server) staffGate(w http.ResponseWriter, r *http.Request) bool {
	if s.terminalSvc == nil {
		return true // the handler answers terminal_unavailable
	}
	a := actor(r)
	s.terminalSvc.Touch(a)
	// A driver logged in on the terminal (owner 2026-11-13): his own routes
	// and what any terminal needs, nothing of the store's (no order, money,
	// customer, list or report), whatever app version asks.
	if !driverRoutes[r.Pattern] {
		if st, err := s.terminalSvc.CurrentStaff(a); err == nil && st.Role == terminal.RoleDriver {
			s.writeJSON(w, http.StatusForbidden, errCodeMap("a driver's login has only his deliveries and his report", "driver_not_allowed"))
			return false
		}
	}
	if !staffRoutes[r.Pattern] {
		return true
	}
	if _, err := s.terminalSvc.CurrentStaff(a); err != nil {
		s.terminalError(w, r, err)
		return false
	}
	return true
}

// loggedIn: a staff member is logged in on the calling terminal (the Pi
// page's session counts: the store manager login).
func (s *Server) loggedIn(r *http.Request) bool {
	if _, ok := r.Context().Value(deviceCtxKey{}).(*device.Device); !ok {
		if _, seat := callcenterActor(r); !seat {
			return true // the Pi page
		}
	}
	if s.terminalSvc == nil {
		return false
	}
	_, err := s.terminalSvc.CurrentStaff(actor(r))
	return err == nil
}
