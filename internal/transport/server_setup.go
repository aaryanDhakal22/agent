package transport

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	printerApp "quiccpos/agent/internal/application/printer"
	terminalApp "quiccpos/agent/internal/application/terminal"
	"quiccpos/agent/internal/domain/device"
	"quiccpos/agent/internal/domain/storeconfig"
	"quiccpos/agent/internal/domain/terminal"
)

// Centurion's first-time setup (contract/agent.md §2.10y): pairing with the
// store's admin pairing code, that code as a Pi setting, each device's last
// setup result, the readiness summary, raw terminal prints and the setup
// test charge's refund.

// SetSetup wires the setup store (the admin code + setup results).
func (s *Server) SetSetup(st device.SetupStore) { s.setups = st }

// --- the admin pairing code throttle ------------------------------------------

// Admin code guesses: per address adminCodePerIP wrong codes in
// adminCodeWindow, store-wide adminCodeStore; past either the address (or
// every address) waits adminCodeBasePause, doubling per further miss up to
// adminCodeMaxPause. A right code clears that address only.
const (
	adminCodePerIP     = 5
	adminCodeStore     = 10
	adminCodeWindow    = 15 * time.Minute
	adminCodeBasePause = time.Minute
	adminCodeMaxPause  = time.Hour
)

type adminCodeThrottle struct {
	mu    sync.Mutex
	ip    map[string][]time.Time
	all   []time.Time
	until map[string]time.Time // "" = the whole store
	over  map[string]int       // misses past the limit, per key
}

func recentTimes(ts []time.Time, now time.Time) []time.Time {
	var keep []time.Time
	for _, t := range ts {
		if now.Sub(t) < adminCodeWindow {
			keep = append(keep, t)
		}
	}
	return keep
}

// wait is how long ip must wait before its next try (0 = go).
func (t *adminCodeThrottle) wait(ip string, now time.Time) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	var w time.Duration
	for _, k := range []string{ip, ""} {
		if u, ok := t.until[k]; ok && now.Before(u) && u.Sub(now) > w {
			w = u.Sub(now)
		}
	}
	return w
}

func (t *adminCodeThrottle) pause(key string, now time.Time) {
	if t.until == nil {
		t.until, t.over = map[string]time.Time{}, map[string]int{}
	}
	d := adminCodeBasePause << min(t.over[key], 6)
	if d > adminCodeMaxPause {
		d = adminCodeMaxPause
	}
	t.over[key]++
	t.until[key] = now.Add(d)
}

func (t *adminCodeThrottle) fail(ip string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ip == nil {
		t.ip = map[string][]time.Time{}
	}
	t.ip[ip] = append(recentTimes(t.ip[ip], now), now)
	t.all = append(recentTimes(t.all, now), now)
	if len(t.ip[ip]) >= adminCodePerIP {
		t.pause(ip, now)
	}
	if len(t.all) >= adminCodeStore {
		t.pause("", now)
	}
}

func (t *adminCodeThrottle) ok(ip string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.ip, ip)
	delete(t.until, ip)
	delete(t.over, ip)
}

// --- POST /api/pair/admin ------------------------------------------------------

// POST /api/pair/admin {code, name, kind?: "terminal"} (open) — a counter
// terminal pairs with the store's admin pairing code (default 1024, a Pi
// setting). Terminals only; a blank Pi needs the manager login (409
// not_linked). Never logs the code.
func (s *Server) handlePairAdmin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var body struct {
		Code string `json:"code"`
		Name string `json:"name"`
		Kind string `json:"kind"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("invalid body", "bad_request"))
		return
	}
	if k := strings.TrimSpace(body.Kind); k != "" && k != device.KindTerminal {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("the admin code pairs counter terminals only", "bad_request"))
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" || utf8.RuneCountInString(name) > 40 {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("name must be 1-40 characters", "name_required"))
		return
	}
	key, err := s.settings.GetStoreAPIKey(ctx)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	if key == "" {
		s.writeJSON(w, http.StatusConflict, errCodeMap("This Pi isn't set up yet — set it up with the store manager login first.", "not_linked"))
		return
	}
	if s.setups == nil {
		s.writeJSON(w, http.StatusForbidden, errCodeMap("This Pi doesn't take the admin code.", "admin_code_off"))
		return
	}
	ap, err := s.setups.AdminPair(ctx)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	want := ap.Effective()
	if want == "" {
		s.writeJSON(w, http.StatusForbidden, errCodeMap("The admin code is turned off on this Pi — use the manager login or a pairing code.", "admin_code_off"))
		return
	}
	// The pairing window (the security round, C1a): the code is looked at
	// only while a manager's window is open (or during the Pi's first
	// setup). Closed = 403 before the code and the throttle, so the code
	// can't be guessed at and pairing can't be paused from the LAN.
	win, open := s.pairWindowGate(w, r, ap)
	if !open {
		return
	}
	ip := clientIP(r)
	now := time.Now()
	if wait := s.adminCodes.wait(ip, now); wait > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		s.writeJSON(w, http.StatusTooManyRequests, errCodeMap("Too many wrong codes — wait a few minutes and try again.", "too_many_attempts"))
		return
	}
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(body.Code)), []byte(want)) != 1 {
		s.adminCodes.fail(ip, now)
		s.logger.Warn().Ctx(ctx).Str("ip", ip).Msg("Pairing refused: wrong admin code")
		s.writeJSON(w, http.StatusUnauthorized, errCodeMap("That isn't the admin code.", "wrong_admin_code"))
		return
	}
	s.adminCodes.ok(ip)
	// One pairing per window: taken before the device is made, so two
	// terminals racing for one window can't both pair.
	if win.Reason == pairWindowManager && !s.pairWin.close(now) {
		s.writeJSON(w, http.StatusForbidden, errCodeMap("Pairing is closed: another terminal just paired. A manager opens it again.", "pairing_closed"))
		return
	}
	if s.pairDevice(w, r, name, device.KindTerminal, "admin_code") && win.Reason == pairWindowManager {
		s.logger.Info().Ctx(ctx).Msg("Pairing window closed: a terminal paired")
		if s.terminalSvc != nil {
			s.terminalSvc.RecordPi(ctx, "", terminal.ActionPairingWindow, map[string]any{"event": "used"})
		}
	}
}

// journalPaired notes how a device was paired (pairDevice calls it).
func (s *Server) journalPaired(ctx context.Context, id, name, how string) {
	if how != "admin_code" || s.terminalSvc == nil {
		return
	}
	s.terminalSvc.RecordPi(ctx, id, terminal.ActionPairingAdminCode, map[string]any{"device_id": id, "device_name": name})
}

// --- GET / PUT /api/settings/admin-code ---------------------------------------

type adminCodeView struct {
	Enabled   bool `json:"enabled"`
	IsDefault bool `json:"is_default"`
	Length    int  `json:"length"`
	// WindowMinutes: how long a manager's pairing window stays open;
	// Window: the window now (GET / PUT answers only).
	WindowMinutes int             `json:"window_minutes"`
	Window        *pairWindowView `json:"window,omitempty"`
}

func viewAdminPair(a device.AdminPair) adminCodeView {
	return adminCodeView{Enabled: !a.Off, IsDefault: a.Code == "", Length: len(a.Effective()), WindowMinutes: int(a.Window() / time.Minute)}
}

// adminCodeAnswer is the setting + the pairing window as it stands.
func (s *Server) adminCodeAnswer(r *http.Request, a device.AdminPair) adminCodeView {
	v := viewAdminPair(a)
	w := s.pairWindowNow(r.Context(), a, time.Now())
	v.Window = &w
	return v
}

// readAdminCode is what the settings history compares: never the code, only
// whether it is on, the default, and a marker that changes with the code.
func (s *Server) readAdminCode(r *http.Request) any {
	if s.setups == nil {
		return nil
	}
	a, err := s.setups.AdminPair(r.Context())
	if err != nil {
		return nil
	}
	v := viewAdminPair(a)
	return map[string]any{"enabled": v.Enabled, "is_default": v.IsDefault, "code_changed_marker": s.codeMarker(r.Context(), a.Code),
		"window_minutes": v.WindowMinutes}
}

// codeMarker is an opaque marker that changes when the code changes: the
// first 6 hex of sha256(store key + code). "" for the default code.
func (s *Server) codeMarker(ctx context.Context, code string) string {
	if code == "" {
		return ""
	}
	key, _ := s.settings.GetStoreAPIKey(ctx)
	return hashDeviceKey(key + "\x00" + code)[:6]
}

func (s *Server) handleGetAdminCode(w http.ResponseWriter, r *http.Request) {
	if s.setups == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, errCodeMap("not available", "not_available"))
		return
	}
	a, err := s.setups.AdminPair(r.Context())
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	s.writeJSON(w, http.StatusOK, s.adminCodeAnswer(r, a))
}

// PUT /api/settings/admin-code {code?: 4-8 digits | "" (back to 1024),
// enabled?, window_minutes?: 1-120}.
func (s *Server) handleSetAdminCode(w http.ResponseWriter, r *http.Request) {
	if s.setups == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, errCodeMap("not available", "not_available"))
		return
	}
	var body struct {
		Code          *string `json:"code"`
		Enabled       *bool   `json:"enabled"`
		WindowMinutes *int    `json:"window_minutes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || (body.Code == nil && body.Enabled == nil && body.WindowMinutes == nil) {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("send code, enabled and/or window_minutes", "bad_request"))
		return
	}
	if m := body.WindowMinutes; m != nil && (*m < device.MinPairWindowMinutes || *m > device.MaxPairWindowMinutes) {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("window_minutes is 1 to 120.", "bad_window"))
		return
	}
	a, err := s.setups.AdminPair(r.Context())
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	if body.Code != nil {
		c := strings.TrimSpace(*body.Code)
		if c != "" && !device.ValidAdminCode(c) {
			s.writeJSON(w, http.StatusBadRequest, errCodeMap("The admin code is 4 to 8 digits.", "bad_code"))
			return
		}
		if c == device.DefaultAdminPairCode {
			c = ""
		}
		a.Code = c
	}
	if body.Enabled != nil {
		a.Off = !*body.Enabled
	}
	if body.WindowMinutes != nil {
		a.WindowMinutes = *body.WindowMinutes
	}
	if err := s.setups.SetAdminPair(r.Context(), a); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, err)
		return
	}
	s.writeJSON(w, http.StatusOK, s.adminCodeAnswer(r, a))
}

// --- setup results ------------------------------------------------------------

// noteSetupReport keeps a device's last setup / full-check result from the
// report it sends (before the relay to main: kept when main is down too).
func (s *Server) noteSetupReport(ctx context.Context, d *device.Device, raw []byte) {
	if s.setups == nil || d == nil {
		return
	}
	res, ok := device.SetupResultFromReport(raw, time.Now())
	if !ok {
		return
	}
	if err := s.setups.SetDeviceSetup(ctx, d.ID, res); err != nil {
		s.logger.Warn().Ctx(ctx).Err(err).Str("device_id", d.ID).Msg("Could not keep the device's setup result")
		return
	}
	s.logger.Info().Ctx(ctx).Str("device_id", d.ID).Str("kind", res.Kind).Str("outcome", res.Outcome).Msg("Device setup result kept")
}

// deviceSetups is every kept setup result (nil when none / unavailable).
func (s *Server) deviceSetups(ctx context.Context) map[string]device.SetupResult {
	if s.setups == nil {
		return nil
	}
	m, err := s.setups.DeviceSetups(ctx)
	if err != nil {
		return nil
	}
	return m
}

// deviceView is a paired device as GET /api/devices answers it (+ setup).
type deviceView struct {
	device.Device
	Setup *device.SetupResult `json:"setup,omitempty"`
}

// --- GET /api/terminal/setup ---------------------------------------------------

type setupStore struct {
	Name          string                   `json:"name"`
	Timezone      string                   `json:"timezone,omitempty"`
	TaxBps        *int                     `json:"tax_bps,omitempty"`
	HoursSet      bool                     `json:"hours_set"`
	OpeningHours  storeconfig.OpeningHours `json:"opening_hours,omitempty"`
	DeliveryArea  map[string]any           `json:"delivery_area"`
	CounterStart  *int64                   `json:"counter_start,omitempty"`
	KitchenStart  *int64                   `json:"kitchen_start,omitempty"`
	AutoCloseAt   *string                  `json:"auto_close_batch_at,omitempty"`
	AddressLine1  string                   `json:"address_line1,omitempty"`
	AddressLine2  string                   `json:"address_line2,omitempty"`
	Phone         string                   `json:"phone,omitempty"`
	Website       string                   `json:"website,omitempty"`
	ReceiptFooter string                   `json:"receipt_footer,omitempty"`
}

type setupPrinter struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Role string `json:"role"` // receipt | station
	Up   bool   `json:"up"`
	IP   string `json:"ip"`
}

type setupDevice struct {
	DeviceID     string    `json:"device_id"`
	Name         string    `json:"name"`
	Kind         string    `json:"kind"`
	Outcome      string    `json:"outcome"`
	KindOfReport string    `json:"kind_of_report"`
	Headline     string    `json:"headline,omitempty"`
	At           time.Time `json:"at"`
	Failed       []string  `json:"failed,omitempty"`
}

// GET /api/terminal/setup — one readiness summary for the setup wizard.
func (s *Server) handleTerminalSetup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sc := s.storeContext()
	loc := sc.Location
	out := map[string]any{"at": time.Now().UTC()}
	if s.mainClient != nil { // the wizard's "change it in main" links (settings map, contract 2.10af)
		out["main_url"] = strings.TrimRight(s.mainClient.BaseURL(), "/")
	}

	var roster []terminalApp.RosterPrinter
	printers := []setupPrinter{}
	if s.diag.Dispatcher != nil {
		for _, e := range s.diag.Dispatcher.Roster() {
			roster = append(roster, terminalApp.RosterPrinter{ID: e.ID, Name: e.Name})
			p := setupPrinter{ID: e.ID, Name: e.Name, Role: "station"}
			if e.IsReceipt {
				p.Role = "receipt"
			}
			if st, ok := s.printers.Get(e.Name); ok {
				p.IP, p.Up = st.IP, st.Up
			}
			printers = append(printers, p)
		}
	}
	out["printers"] = printers

	st := setupStore{Name: sc.StoreName(), Timezone: loc.Timezone, AddressLine1: strings.TrimSpace(loc.AddressLine1),
		AddressLine2: strings.TrimSpace(loc.AddressLine2), Phone: strings.TrimSpace(loc.Phone), DeliveryArea: map[string]any{}}
	if loc.Config != nil {
		if h, ok, err := storeconfig.HoursFromConfig(loc.Config); err == nil && ok {
			st.HoursSet, st.OpeningHours = true, h
		}
	}
	if loc.Lat != nil && loc.Lng != nil {
		st.DeliveryArea["lat"], st.DeliveryArea["lng"] = *loc.Lat, *loc.Lng
	}
	if loc.DeliveryRadiusM > 0 {
		st.DeliveryArea["radius_m"] = loc.DeliveryRadiusM
	}
	if s.terminalSvc != nil {
		v := s.terminalSvc.SetupView(roster)
		if v.Menu != nil {
			out["menu"] = v.Menu
		}
		if v.Routes != nil {
			out["routes"] = v.Routes
		}
		if v.Staff != nil {
			out["staff"] = v.Staff
		}
		st.TaxBps, st.CounterStart, st.KitchenStart, st.AutoCloseAt = v.TaxBps, v.CounterStart, v.KitchenStart, v.AutoCloseAt
		if st.Timezone == "" {
			st.Timezone = v.Timezone
		}
		str := func(k string) string { s, _ := v.Store[k].(string); return strings.TrimSpace(s) }
		st.Website, st.ReceiptFooter = str("website"), str("footer")
	}
	out["store"] = st

	if s.termApps != nil {
		if rel := s.termApps.Current(); rel != nil {
			out["app"] = map[string]any{"version_code": rel.VersionCode, "version_name": rel.VersionName}
		}
	}

	setups := []setupDevice{}
	if all := s.deviceSetups(ctx); len(all) > 0 && s.devices != nil {
		if ds, err := s.devices.List(ctx); err == nil {
			for _, d := range ds {
				res, ok := all[d.ID]
				if !ok || d.RevokedAt != nil {
					continue
				}
				setups = append(setups, setupDevice{DeviceID: d.ID, Name: d.Name, Kind: d.Kind, Outcome: res.Outcome,
					KindOfReport: res.Kind, Headline: res.Headline, At: res.At, Failed: res.Failed})
			}
		}
	}
	sort.SliceStable(setups, func(i, j int) bool { return setups[i].At.After(setups[j].At) })
	out["setups"] = setups
	s.writeJSON(w, http.StatusOK, out)
}

// --- POST /api/terminal/print -------------------------------------------------

// maxTerminalPrint caps one raw job (after base64).
const maxTerminalPrint = 256 << 10

// maxTestLabel caps the test slip's label (characters).
const maxTestLabel = 60

// POST /api/terminal/print {printer?: name, printer_id?, kind: receipt |
// reprint | report | test, bytes_b64?, label?, order_id?} — a terminal's
// print on one of the Pi's printers (its reroute / backup receipt printer,
// the setup test). No failover: a job for X never comes out of Y.
//
// The security round (C1b): a terminal key alone gets the Pi's own test
// slip only (kind test: the Pi builds it from the label; bytes ignored). A
// receipt / reprint / report needs a staff member logged in on the
// terminal, and its bytes must be the terminal renderer's command set
// (checkTerminalJob): no drawer kick, no printer set-up or memory commands.
func (s *Server) handleTerminalPrint(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Printer   string `json:"printer"`
		PrinterID string `json:"printer_id"`
		BytesB64  string `json:"bytes_b64"`
		Kind      string `json:"kind"`
		OrderID   string `json:"order_id"`
		Label     string `json:"label"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxTerminalPrint*4/3+4096)).Decode(&body); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			s.writeJSON(w, http.StatusRequestEntityTooLarge, errCodeMap("The print job is too large.", "too_large"))
			return
		}
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("invalid body", "bad_request"))
		return
	}
	var data []byte
	switch body.Kind {
	case "test":
	case "receipt", "reprint", "report":
		if !s.loggedIn(r) {
			s.terminalError(w, r, terminal.ErrLoginRequired)
			return
		}
		var err error
		data, err = base64.StdEncoding.DecodeString(body.BytesB64)
		if err != nil || len(data) == 0 {
			s.writeJSON(w, http.StatusBadRequest, errCodeMap("bytes_b64 must be the job's bytes in base64", "bad_request"))
			return
		}
		if len(data) > maxTerminalPrint {
			s.writeJSON(w, http.StatusRequestEntityTooLarge, errCodeMap("The print job is too large.", "too_large"))
			return
		}
		if why := checkTerminalJob(data); why != "" {
			s.logger.Warn().Ctx(r.Context()).Str("device_id", actor(r).DeviceID).Str("kind", body.Kind).Str("why", why).
				Msg("Terminal print refused: not a receipt's bytes")
			s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "That isn't a receipt: " + why + ".", "code": "bad_bytes", "reason": why})
			return
		}
	default:
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("kind must be receipt, reprint, report or test", "bad_request"))
		return
	}
	if s.diag.Dispatcher == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, errCodeMap("printing isn't available", "not_available"))
		return
	}
	var target *printerApp.RosterEntry
	for _, e := range s.diag.Dispatcher.Roster() {
		if (body.PrinterID != "" && e.ID == body.PrinterID) ||
			(body.PrinterID == "" && body.Printer != "" && strings.EqualFold(strings.TrimSpace(e.Name), strings.TrimSpace(body.Printer))) {
			e := e
			target = &e
			break
		}
	}
	if target == nil {
		s.writeJSON(w, http.StatusNotFound, errCodeMap("This Pi has no such printer.", "unknown_printer"))
		return
	}
	if body.Kind == "test" {
		data = terminalTestSlip(cleanLabel(body.Label), target.Name, actor(r).DeviceName, time.Now())
	}
	res := s.diag.Dispatcher.PrintTest(r.Context(), target.ID, func(printerApp.RosterEntry) []byte { return data })
	ok := len(res) == 1 && res[0].OK
	errText := ""
	if !ok && len(res) == 1 {
		errText = res[0].Error
	}
	if s.terminalSvc != nil {
		d := map[string]any{"kind": body.Kind, "printer": target.Name, "printer_id": target.ID, "bytes": len(data), "ok": ok}
		if body.OrderID != "" {
			d["order_id"] = body.OrderID
		}
		if errText != "" {
			d["error"] = errText
		}
		s.terminalSvc.RecordPi(r.Context(), actor(r).DeviceID, terminal.ActionTerminalPrint, d)
	}
	if !ok {
		if errText == "" {
			errText = "the printer didn't take the job"
		}
		s.writeJSON(w, http.StatusBadGateway, map[string]any{"error": errText, "code": "print_failed", "printer": target.Name})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "printer": target.Name})
}

// --- POST /api/terminal/square/test-refund -------------------------------------

// MsgSquareTestRefund is the RPC main refunds the setup's test charge with.
const MsgSquareTestRefund = "square.test_refund"

// maxTestRefundCents: the setup wizard's test charge is $1.00.
const maxTestRefundCents = 100

// POST /api/terminal/square/test-refund {square_payment_id, amount_cents
// (1..100), idempotency_key} → RPC square.test_refund → main refunds the
// setup's $1 test charge (no order). 200 {ok, refund_id, status}.
func (s *Server) handleTerminalTestRefund(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PaymentID      string `json:"square_payment_id"`
		AmountCents    int64  `json:"amount_cents"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("invalid body", "bad_request"))
		return
	}
	body.PaymentID, body.IdempotencyKey = strings.TrimSpace(body.PaymentID), strings.TrimSpace(body.IdempotencyKey)
	if body.PaymentID == "" || len(body.PaymentID) > 64 || body.IdempotencyKey == "" || len(body.IdempotencyKey) > 45 ||
		body.AmountCents < 1 || body.AmountCents > maxTestRefundCents {
		s.writeJSON(w, http.StatusBadRequest, errCodeMap("square_payment_id, idempotency_key (<= 45) and amount_cents 1-100 are required", "bad_request"))
		return
	}
	if s.diag.RPC == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, errCodeMap("This Pi isn't linked to a store.", "not_linked"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	status, reply, err := s.diag.RPC.Send(ctx, MsgSquareTestRefund, map[string]any{
		"square_payment_id": body.PaymentID, "amount_cents": body.AmountCents, "idempotency_key": body.IdempotencyKey,
	})
	jd := map[string]any{"square_payment_id": body.PaymentID, "amount_cents": body.AmountCents}
	record := func(ok bool, extra map[string]any) {
		if s.terminalSvc == nil {
			return
		}
		jd["ok"] = ok
		for k, v := range extra {
			jd[k] = v
		}
		s.terminalSvc.RecordPi(r.Context(), actor(r).DeviceID, terminal.ActionSetupCardRefund, jd)
	}
	if err != nil {
		record(false, map[string]any{"code": "main_unreachable"})
		s.writeJSON(w, http.StatusBadGateway, errCodeMap("Can't reach the server.", "main_unreachable"))
		return
	}
	switch status {
	case http.StatusOK:
		var res map[string]any
		_ = json.Unmarshal(reply, &res)
		if res == nil {
			res = map[string]any{}
		}
		if ok, has := res["ok"].(bool); has && !ok {
			// main's refusal is an answer {ok: false, code} (main §2.5e).
			code, _ := res["code"].(string)
			record(false, map[string]any{"code": code})
			status := http.StatusPaymentRequired // square_refused
			switch code {
			case "square_not_linked":
				status = http.StatusConflict
			case "not_a_test_charge":
				status = http.StatusBadRequest
			}
			if _, has := res["error"]; !has {
				res["error"] = "Square refused the refund."
			}
			s.writeJSON(w, status, res)
			return
		}
		res["ok"] = true
		record(true, map[string]any{"refund_id": res["refund_id"], "status": res["status"]})
		s.writeJSON(w, http.StatusOK, res)
	case http.StatusAccepted:
		record(false, map[string]any{"code": "main_unsupported"})
		s.writeJSON(w, http.StatusBadGateway, errCodeMap("The server can't refund a test charge yet: refund it in Square.", "main_unsupported"))
	default:
		var e struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(reply, &e)
		record(false, map[string]any{"code": e.Code, "status_code": status})
		s.mainFailure(w, status, reply)
	}
}
