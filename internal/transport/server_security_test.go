package transport

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	terminalApp "quiccpos/agent/internal/application/terminal"
	"quiccpos/agent/internal/domain/device"
	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/terminal"
	"quiccpos/agent/internal/infra/database/repositories"

	"github.com/rs/zerolog"
	"golang.org/x/crypto/bcrypt"
)

// The store-side security round (2026-11-12): the pairing window (C1a), the
// staff login on the routes a key alone used to open (C1b), the print guard,
// the admin code check (M7), the PIN guard's routes (H3), the store-wide
// manager-password count (M5).

const secManager = "00000000-0000-0000-0000-00000000ee01"

// secRig is a real terminal service behind the routes (built like the
// call-center rig): a cashier (PIN 1234) and a manager (PIN 9876), a paired
// terminal, the setup store and the manager login boss / right-pass; it
// returns a tablet's key too.
type secRigT struct {
	s       *Server
	h       http.Handler
	termKey string
}

func secRig(t *testing.T, extra ...map[string]any) (r *secRigT, tabletKey string) {
	t.Helper()
	s, h, tabletKey, db := diagServerDB(t)
	ctx := context.Background()
	if err := s.settings.SetStoreAPIKey(ctx, "qagt-store-key"); err != nil {
		t.Fatal(err)
	}
	r = &secRigT{s: s, h: h}
	r.termKey, _ = newDeviceKey()
	if err := s.devices.Create(ctx, "term-1", "Counter 1", device.KindTerminal, hashDeviceKey(r.termKey), r.termKey[:11]); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../domain/pricing/testdata/cases/33_one_specialty_on_a_half.json")
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Context json.RawMessage `json:"context"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	menu, _ := json.Marshal(map[string]any{"menu_version": 12, "build": map[string]any{"view": map[string]any{"kind": "counter"}}, "pricing": c.Context})
	pin, err := bcrypt.GenerateFromPassword([]byte("9876"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	config := ccConfig(t, 2, append([]map[string]any{{"id": secManager, "first_name": "Khawar", "last_name": "Ali", "role": "manager", "pin_hash": string(pin)}}, extra...)...)
	repo := repositories.NewOrderRepository(db, zerolog.Nop())
	store := terminalApp.NewStore(repo, func(_ context.Context, path string) (int, []byte, error) {
		if path == terminalApp.PathCounterMenu {
			return 200, menu, nil
		}
		return 200, config, nil
	}, ccPub{}, zerolog.Nop())
	svc := terminalApp.NewService(store, repo, &ccFloor{}, ccPub{}, zerolog.Nop())
	for _, k := range []string{terminal.KindCounterMenu, terminal.KindTerminalConfig} {
		if _, err := store.Refresh(ctx, k); err != nil {
			t.Fatal(err)
		}
	}
	s.SetTerminal(svc)
	s.SetSetup(repositories.NewSetupRepository(db))
	hash, _ := bcrypt.GenerateFromPassword([]byte("right-pass"), bcrypt.MinCost)
	if err := s.settings.SetLinkInfo(ctx, order.LinkInfo{ManagerUsername: "boss"}, string(hash), ""); err != nil {
		t.Fatal(err)
	}
	return r, tabletKey
}

func asWeb(s *Server, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	cookie := httptest.NewRecorder()
	s.session.issue(cookie)
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for _, c := range cookie.Result().Cookies() {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func window(t *testing.T, rec *httptest.ResponseRecorder) pairWindowView {
	t.Helper()
	var v pairWindowView
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &v) != nil {
		t.Fatalf("window: %d %s", rec.Code, rec.Body)
	}
	return v
}

// C1a: who opens the window, how long it lasts, one pairing per window.
func TestPairingWindow(t *testing.T) {
	r, tabletKey := secRig(t)
	s, h := r.s, r.h
	if v := window(t, call(h, "GET", "/api/pairing/window", tabletKey, "")); v.Open || v.Minutes != 15 {
		t.Fatalf("closed at first: %+v", v)
	}
	if rec := call(h, "GET", "/api/pairing/window", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no key: %d", rec.Code)
	}
	// A tablet needs the store manager login.
	if rec := call(h, "POST", "/api/pairing/window", tabletKey, `{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("a tablet with no login: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "POST", "/api/pairing/window", tabletKey, `{"username":"boss","password":"nope"}`); rec.Code != http.StatusUnauthorized || code(rec) != "bad_login" {
		t.Fatalf("a wrong login: %d %s", rec.Code, rec.Body)
	}
	if v := window(t, call(h, "GET", "/api/pairing/window", tabletKey, "")); v.Open {
		t.Fatal("a wrong login opened the window")
	}
	v := window(t, call(h, "POST", "/api/pairing/window", tabletKey, `{"username":"boss","password":"right-pass"}`))
	if !v.Open || v.Reason != pairWindowManager || v.Until == nil || time.Until(*v.Until) > 15*time.Minute || time.Until(*v.Until) < 14*time.Minute {
		t.Fatalf("opened by the tablet: %+v", v)
	}
	// A terminal's key reads it but can't open or close it there.
	if v := window(t, call(h, "GET", "/api/pairing/window", r.termKey, "")); !v.Open {
		t.Fatalf("a terminal reads the window: %+v", v)
	}
	for _, m := range []string{"POST", "DELETE"} {
		if rec := call(h, m, "/api/pairing/window", r.termKey, `{"username":"boss","password":"right-pass"}`); rec.Code != http.StatusForbidden || code(rec) != "terminal_only" {
			t.Fatalf("%s with a terminal key: %d %s", m, rec.Code, rec.Body)
		}
	}
	// Closing needs no login.
	if v := window(t, call(h, "DELETE", "/api/pairing/window", tabletKey, "")); v.Open {
		t.Fatalf("closed by the tablet: %+v", v)
	}
	if rec := call(h, "POST", "/api/pair/admin", "", `{"code":"1024","name":"Counter 2"}`); code(rec) != "pairing_closed" {
		t.Fatalf("after closing: %d %s", rec.Code, rec.Body)
	}
	// The Pi page opens it with its session; the setting's minutes count.
	if rec := asWeb(s, h, "PUT", "/api/settings/admin-code", `{"window_minutes":200}`); rec.Code != http.StatusBadRequest || code(rec) != "bad_window" {
		t.Fatalf("200 minutes: %d %s", rec.Code, rec.Body)
	}
	if rec := asWeb(s, h, "PUT", "/api/settings/admin-code", `{"window_minutes":3}`); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"window_minutes":3`) {
		t.Fatalf("3 minutes: %d %s", rec.Code, rec.Body)
	}
	v = window(t, asWeb(s, h, "POST", "/api/pairing/window", ""))
	if !v.Open || v.Minutes != 3 || time.Until(*v.Until) > 3*time.Minute {
		t.Fatalf("opened by the Pi page: %+v", v)
	}
	if rec := call(h, "POST", "/api/pair/admin", "", `{"code":"1024","name":"Counter 2"}`); rec.Code != http.StatusCreated {
		t.Fatalf("pairing in the window: %d %s", rec.Code, rec.Body)
	}
	if v := window(t, call(h, "GET", "/api/pairing/window", tabletKey, "")); v.Open {
		t.Fatalf("one pairing closes the window: %+v", v)
	}
	// It closes by itself when its time is up.
	s.pairWin.open(time.Now().Add(-time.Second), "test")
	if rec := call(h, "POST", "/api/pair/admin", "", `{"code":"1024","name":"Counter 3"}`); code(rec) != "pairing_closed" {
		t.Fatalf("after its time: %d %s", rec.Code, rec.Body)
	}
	// A paired terminal opens it with a manager's PIN (nobody logged in: 401).
	if rec := call(h, "POST", "/api/terminal/pairing/window", r.termKey, `{}`); rec.Code != http.StatusUnauthorized || code(rec) != "login_required" {
		t.Fatalf("a locked terminal: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "POST", "/api/terminal/login", r.termKey, `{"pin":"1234"}`); rec.Code != http.StatusOK {
		t.Fatalf("cashier login: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "POST", "/api/terminal/pairing/window", r.termKey, `{}`); rec.Code != http.StatusForbidden || code(rec) != "manager_required" {
		t.Fatalf("a cashier: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "POST", "/api/terminal/pairing/window", r.termKey, `{"manager_pin":"1234"}`); rec.Code != http.StatusForbidden || code(rec) != "manager_pin_invalid" {
		t.Fatalf("a cashier's PIN: %d %s", rec.Code, rec.Body)
	}
	if v := window(t, call(h, "POST", "/api/terminal/pairing/window", r.termKey, `{"manager_pin":"9876"}`)); !v.Open || v.Reason != pairWindowManager {
		t.Fatalf("a manager's PIN on the terminal: %+v", v)
	}
	// An approval from the device-level manager check works too.
	s.pairWin.close(time.Now())
	rec := call(h, "POST", "/api/terminal/manager-check", r.termKey, `{"pin":"9876","action":"pairing_window"}`)
	var mc struct {
		ApprovalID string `json:"approval_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &mc)
	if rec.Code != http.StatusOK || mc.ApprovalID == "" {
		t.Fatalf("manager-check: %d %s", rec.Code, rec.Body)
	}
	if v := window(t, call(h, "POST", "/api/terminal/pairing/window", r.termKey, `{"approvals":["`+mc.ApprovalID+`"]}`)); !v.Open {
		t.Fatalf("with the approval: %+v", v)
	}
}

// C1a: a linked Pi that never paired a terminal is in its first setup: the
// window is open until the first terminal pairs. A prod Pi refuses the
// default code there.
func TestFirstSetupWindow(t *testing.T) {
	s, h, tabletKey, db := diagServerDB(t)
	ctx := context.Background()
	s.SetSetup(repositories.NewSetupRepository(db))
	// Not linked: the manager login first (as before).
	if rec := call(h, "POST", "/api/pair/admin", "", `{"code":"1024","name":"Counter 1"}`); rec.Code != http.StatusConflict || code(rec) != "not_linked" {
		t.Fatalf("blank Pi: %d %s", rec.Code, rec.Body)
	}
	if err := s.settings.SetStoreAPIKey(ctx, "qagt-store-key"); err != nil {
		t.Fatal(err)
	}
	if v := window(t, call(h, "GET", "/api/pairing/window", tabletKey, "")); !v.Open || v.Reason != pairWindowFirstSetup || v.Until != nil || !v.DefaultCodeOK {
		t.Fatalf("first setup: %+v", v)
	}
	// A prod Pi: the default code only in a manager's window; the store's own code is fine.
	s.SetAppEnv("prod")
	if v := window(t, call(h, "GET", "/api/pairing/window", tabletKey, "")); !v.Open || v.DefaultCodeOK {
		t.Fatalf("first setup on prod: %+v", v)
	}
	if rec := call(h, "POST", "/api/pair/admin", "", `{"code":"1024","name":"Counter 1"}`); rec.Code != http.StatusForbidden || code(rec) != "default_code_refused" {
		t.Fatalf("the default code on a prod Pi: %d %s", rec.Code, rec.Body)
	}
	openWindow(s)
	if v := window(t, call(h, "GET", "/api/pairing/window", tabletKey, "")); v.Reason != pairWindowManager || !v.DefaultCodeOK {
		t.Fatalf("a manager's window on prod: %+v", v)
	}
	s.pairWin.close(time.Now())
	if err := s.setups.SetAdminPair(ctx, device.AdminPair{Code: "731964"}); err != nil {
		t.Fatal(err)
	}
	if rec := call(h, "POST", "/api/pair/admin", "", `{"code":"731964","name":"Counter 1"}`); rec.Code != http.StatusCreated {
		t.Fatalf("the store's own code in the first setup: %d %s", rec.Code, rec.Body)
	}
	// The first terminal paired: the first setup is over, also for a staging Pi.
	s.SetAppEnv("staging")
	if v := window(t, call(h, "GET", "/api/pairing/window", tabletKey, "")); v.Open {
		t.Fatalf("after the first terminal: %+v", v)
	}
	if rec := call(h, "POST", "/api/pair/admin", "", `{"code":"731964","name":"Counter 2"}`); code(rec) != "pairing_closed" {
		t.Fatalf("a second terminal without a window: %d %s", rec.Code, rec.Body)
	}
}

// C1b: a terminal's key alone reads none of the store's data; logged in it
// does; the Pi page's session still does.
func TestTerminalKeyAloneReadsNothing(t *testing.T) {
	r, _ := secRig(t)
	s, h := r.s, r.h
	gated := []struct{ method, path string }{
		{"GET", "/api/terminal/customers?phone=410"},
		{"POST", "/api/terminal/customers"},
		{"GET", "/api/terminal/customers/c1/orders"},
		{"GET", "/api/terminal/orders"},
		{"GET", "/api/terminal/orders/0193a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b"},
		{"GET", "/api/terminal/journal"},
		{"GET", "/api/terminal/drafts"},
		{"GET", "/api/terminal/unsettled"},
		{"GET", "/api/terminal/pickups"},
		{"GET", "/api/terminal/z"},
		{"GET", "/api/terminal/checks"},
		{"GET", "/api/terminal/reports/batches"},
		{"GET", "/api/terminal/reports/till"},
		{"GET", "/api/terminal/reports/drivers"},
		{"GET", "/api/terminal/square/session"},
	}
	for _, g := range gated {
		if rec := call(h, g.method, g.path, r.termKey, ""); rec.Code != http.StatusUnauthorized || code(rec) != "login_required" {
			t.Errorf("%s %s with a key alone: %d %s", g.method, g.path, rec.Code, rec.Body)
		}
	}
	job := base64.StdEncoding.EncodeToString([]byte{esc, '@', 'H', 'i', '\n'})
	if rec := call(h, "POST", "/api/terminal/print", r.termKey, `{"printer":"Front","kind":"receipt","bytes_b64":"`+job+`"}`); rec.Code != http.StatusUnauthorized {
		t.Errorf("a receipt through the Pi with a key alone: %d %s", rec.Code, rec.Body)
	}
	// Open to a key alone: what the locked terminal shows and needs.
	for _, p := range []string{"/api/terminal/session", "/api/terminal/menu", "/api/terminal/status", "/api/terminal/setup"} {
		if rec := call(h, "GET", p, r.termKey, ""); rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
			t.Errorf("GET %s with a key alone: %d %s", p, rec.Code, rec.Body)
		}
	}
	// The Pi page (the store manager login) reads them as before.
	for _, p := range []string{"/api/terminal/journal", "/api/terminal/orders", "/api/terminal/drafts"} {
		if rec := asWeb(s, h, "GET", p, ""); rec.Code != http.StatusOK {
			t.Errorf("the Pi page GET %s: %d %s", p, rec.Code, rec.Body)
		}
	}
	// Logged in: they answer (whatever they answer, it isn't login_required).
	if rec := call(h, "POST", "/api/terminal/login", r.termKey, `{"pin":"1234"}`); rec.Code != http.StatusOK {
		t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	for _, g := range gated {
		if g.method != "GET" {
			continue
		}
		if rec := call(h, g.method, g.path, r.termKey, ""); code(rec) == "login_required" {
			t.Errorf("%s %s logged in: %d %s", g.method, g.path, rec.Code, rec.Body)
		}
	}
	// Bad bytes are refused even logged in; a clean job passes the guard
	// (no dispatcher in this rig: 503).
	kick := base64.StdEncoding.EncodeToString([]byte{esc, '@', esc, 'p', 0, 25, 250})
	if rec := call(h, "POST", "/api/terminal/print", r.termKey, `{"printer":"Front","kind":"receipt","bytes_b64":"`+kick+`"}`); rec.Code != http.StatusBadRequest || code(rec) != "bad_bytes" {
		t.Errorf("a drawer kick through the Pi: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "POST", "/api/terminal/print", r.termKey, `{"printer":"Front","kind":"receipt","bytes_b64":"`+job+`"}`); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("a clean receipt: %d %s", rec.Code, rec.Body)
	}
	// Every gated pattern is a real route (a renamed route must not fall out of the gate).
	have := map[string]bool{}
	for _, d := range s.routes() {
		have[d.Method+" "+d.Path] = true
	}
	for p := range staffRoutes {
		if !have[p] {
			t.Errorf("staffRoutes has %q, which is not a route", p)
		}
	}
}

func TestCheckTerminalJob(t *testing.T) {
	raster := append([]byte{gs, 'v', '0', 0, 2, 0, 3, 0}, make([]byte, 6)...) // 2 bytes x 3 rows, zeros inside
	ok := [][]byte{
		{esc, '@', esc, 't', 0, esc, 'a', 1, esc, 'E', 1, gs, '!', 0x11, 'A', 0xAB, '\n', gs, 'B', 1, esc, 'M', 1, esc, '-', 1, esc, 'd', 3, gs, 'V', 'B', 3},
		append(append([]byte{esc, '@'}, raster...), '\n', gs, 'V', 0),
		[]byte("plain text\r\n\tindented\n"),
	}
	for i, j := range ok {
		if why := checkTerminalJob(j); why != "" {
			t.Errorf("job %d refused: %s", i, why)
		}
	}
	bad := map[string][]byte{
		"drawer kick ESC p":             {esc, 'p', 0, 25, 250},
		"drawer kick DLE DC4":           {0x10, 0x14, 1, 0, 5},
		"status DLE EOT":                {0x10, 0x04, 1},
		"NV / config GS (":              {gs, '(', 'E', 3, 0, 1, 'I', 'N'},
		"FS command":                    {0x1C, 'q', 1},
		"ESC * bit image":               {esc, '*', 0, 1, 0, 0xFF},
		"cut short":                     {esc},
		"raster longer than job":        {gs, 'v', '0', 0, 10, 0, 10, 0, 1, 2, 3},
		"a NUL outside a picture":       {'a', 0, 'b'},
		"a kick hidden after a picture": append(append([]byte{}, raster...), esc, 'p', 0, 25, 250),
	}
	for name, j := range bad {
		if why := checkTerminalJob(j); why == "" {
			t.Errorf("%s passed", name)
		}
	}
	slip := terminalTestSlip(cleanLabel("Backup for Counter 1\x1bp\x00"), "Front", "Counter 1", time.Date(2026, 11, 12, 18, 0, 0, 0, time.UTC))
	if why := checkTerminalJob(slip); why != "" {
		t.Errorf("the Pi's own test slip fails its own guard: %s", why)
	}
	if !strings.Contains(string(slip), "Backup for Counter 1?p?") {
		t.Errorf("label not cleaned: %q", slip)
	}
}

// M7: the apps' admin / developer mode asks the Pi for the admin code.
func TestAdminCodeCheck(t *testing.T) {
	r, tabletKey := secRig(t)
	s, h := r.s, r.h
	if rec := call(h, "POST", "/api/admin-code/check", "", `{"code":"1024"}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no key: %d", rec.Code)
	}
	for _, key := range []string{tabletKey, r.termKey} {
		if rec := call(h, "POST", "/api/admin-code/check", key, `{"code":"1024"}`); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok":true`) {
			t.Fatalf("the default code: %d %s", rec.Code, rec.Body)
		}
	}
	if rec := call(h, "POST", "/api/admin-code/check", tabletKey, `{"code":"12"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("2 digits: %d", rec.Code)
	}
	if err := s.setups.SetAdminPair(context.Background(), device.AdminPair{Code: "731964"}); err != nil {
		t.Fatal(err)
	}
	if rec := call(h, "POST", "/api/admin-code/check", r.termKey, `{"code":"1024"}`); rec.Code != http.StatusUnauthorized || code(rec) != "wrong_admin_code" {
		t.Fatalf("1024 after the store set its own: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "POST", "/api/admin-code/check", r.termKey, `{"code":"731964"}`); rec.Code != http.StatusOK {
		t.Fatalf("the store's code: %d %s", rec.Code, rec.Body)
	}
	for i := 0; i < adminCodePerIP; i++ {
		call(h, "POST", "/api/admin-code/check", r.termKey, `{"code":"0000"}`)
	}
	rec := call(h, "POST", "/api/admin-code/check", r.termKey, `{"code":"731964"}`)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("paused after 5 wrong codes: %d %s", rec.Code, rec.Body)
	}
	if err := s.setups.SetAdminPair(context.Background(), device.AdminPair{Off: true}); err != nil {
		t.Fatal(err)
	}
	if rec := call(h, "POST", "/api/admin-code/check", tabletKey, `{"code":"1024"}`); rec.Code != http.StatusForbidden || code(rec) != "admin_code_off" {
		t.Fatalf("off: %d %s", rec.Code, rec.Body)
	}
}

// H3: the daily ceiling's routes: the state, the unlock (the Pi page, or a
// tablet with the manager login), the setting; the answers a locked store
// gives a terminal.
func TestPinGuardRoutes(t *testing.T) {
	r, tabletKey := secRig(t)
	s, h := r.s, r.h
	if rec := asWeb(s, h, "PUT", "/api/settings/pin-guard", `{"daily_ceiling":5}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("a ceiling of 5: %d %s", rec.Code, rec.Body)
	}
	if rec := asWeb(s, h, "PUT", "/api/settings/pin-guard", `{"daily_ceiling":10}`); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"daily_ceiling":10`) {
		t.Fatalf("a ceiling of 10: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "PUT", "/api/settings/pin-guard", tabletKey, `{"daily_ceiling":500}`); rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
		t.Fatalf("a tablet sets the ceiling: %d", rec.Code)
	}
	// Ten wrong PINs from ten terminals (under each one's own limit).
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		k, _ := newDeviceKey()
		id := "term-x" + string(rune('a'+i))
		if err := s.devices.Create(ctx, id, id, device.KindTerminal, hashDeviceKey(k), k[:11]); err != nil {
			t.Fatal(err)
		}
		rec := call(h, "POST", "/api/terminal/login", k, `{"pin":"0000"}`)
		if i < 9 && (rec.Code != http.StatusUnauthorized || code(rec) != "wrong_pin") {
			t.Fatalf("miss %d: %d %s", i, rec.Code, rec.Body)
		}
	}
	rec := call(h, "POST", "/api/terminal/login", r.termKey, `{"pin":"1234"}`)
	if rec.Code != http.StatusForbidden || code(rec) != "pin_locked" || !strings.Contains(rec.Body.String(), "locked_at") {
		t.Fatalf("a right PIN while locked: %d %s", rec.Code, rec.Body)
	}
	for _, key := range []string{tabletKey, r.termKey} {
		if rec := call(h, "GET", "/api/pin-guard", key, ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"locked":true`) {
			t.Fatalf("the guard: %d %s", rec.Code, rec.Body)
		}
	}
	if rec := call(h, "POST", "/api/pin-guard/unlock", r.termKey, `{"username":"boss","password":"right-pass"}`); rec.Code != http.StatusForbidden || code(rec) != "terminal_only" {
		t.Fatalf("a terminal unlocks: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "POST", "/api/pin-guard/unlock", tabletKey, `{"username":"boss","password":"nope"}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a wrong login unlocks: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "POST", "/api/pin-guard/unlock", tabletKey, `{"username":"boss","password":"right-pass"}`); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"locked":false`) {
		t.Fatalf("the tablet unlocks with the manager login: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "POST", "/api/terminal/login", r.termKey, `{"pin":"1234"}`); rec.Code != http.StatusOK {
		t.Fatalf("login after the unlock: %d %s", rec.Code, rec.Body)
	}
	if rec := asWeb(s, h, "POST", "/api/pin-guard/unlock", ""); rec.Code != http.StatusOK {
		t.Fatalf("the Pi page unlocks: %d %s", rec.Code, rec.Body)
	}
}

// M5: wrong manager passwords from many addresses pause every address.
func TestLoginThrottleStoreWide(t *testing.T) {
	var th loginThrottle
	now := time.Now()
	for i := 0; i < loginStoreMisses-1; i++ {
		th.fail("10.0.0."+string(rune('a'+i)), now) // one miss per address: none is paused by itself
	}
	if w := th.blockedFor("10.9.9.9", now); w != 0 {
		t.Fatalf("paused before the store's limit: %v", w)
	}
	th.fail("10.0.0.z", now)
	if w := th.blockedFor("10.9.9.9", now); w <= 0 || w > loginStoreBasePause {
		t.Fatalf("ten misses across the store pause a new address: %v", w)
	}
	if _, wait := th.begin("10.9.9.9", now); wait <= 0 {
		t.Fatal("begin passed during the store's pause")
	}
	th.ok("10.0.0.a") // a right password clears its own address, not the store's count
	if w := th.blockedFor("10.0.0.a", now); w <= 0 {
		t.Fatal("a right password lifted the store's pause")
	}
	later := now.Add(loginStoreBasePause + time.Second)
	if w := th.blockedFor("10.9.9.9", later); w != 0 {
		t.Fatalf("still paused after the pause: %v", w)
	}
	th.fail("10.0.0.y", later) // still over the limit in the window: the pause doubles
	if w := th.blockedFor("10.9.9.9", later); w <= loginStoreBasePause {
		t.Fatalf("the next miss doubles the pause: %v", w)
	}
	if w := th.blockedFor("10.9.9.9", later.Add(loginStoreWindow+2*time.Hour)); w != 0 {
		t.Fatalf("the count never ages out: %v", w)
	}
}

// A driver's login (owner 2026-11-13, contract §2.10b "A driver's login"):
// whatever app asks, the Pi answers him only his own routes and what keeps a
// terminal running; every other terminal route is 403 driver_not_allowed. A
// cashier after him on the same terminal is unchanged.
func TestDriverLoginReachesOnlyHisRoutes(t *testing.T) {
	pin, err := bcrypt.GenerateFromPassword([]byte("5511"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := secRig(t, map[string]any{"id": "00000000-0000-0000-0000-00000000dd01", "first_name": "Ali", "last_name": "Khan", "role": "driver", "pin_hash": string(pin)})
	s, h := r.s, r.h
	rec := call(h, "POST", "/api/terminal/login", r.termKey, `{"pin":"5511"}`)
	var login struct {
		Role  string `json:"role"`
		Staff struct {
			Role string `json:"role"`
		} `json:"staff"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &login) != nil || login.Role != "driver" || login.Staff.Role != "driver" {
		t.Fatalf("driver login: %d %s", rec.Code, rec.Body)
	}
	have := map[string]bool{}
	refused := 0
	for _, d := range s.routes() {
		p := d.Method + " " + d.Path
		have[p] = true
		if d.Auth != "terminal" || !strings.HasPrefix(d.Path, "/api/terminal/") || p == "POST /api/terminal/logout" || p == "POST /api/terminal/login" {
			continue
		}
		path := strings.NewReplacer("{id}", "x1", "{payment_id}", "p1").Replace(d.Path)
		body := ""
		if d.Method != "GET" {
			body = "{}"
		}
		rec := call(h, d.Method, path, r.termKey, body)
		if driverRoutes[p] {
			if code(rec) == "driver_not_allowed" {
				t.Errorf("%s is his, refused: %s", p, rec.Body)
			}
			continue
		}
		refused++
		if rec.Code != http.StatusForbidden || code(rec) != "driver_not_allowed" {
			t.Errorf("%s with a driver logged in: %d %s", p, rec.Code, rec.Body)
		}
	}
	if refused < 40 {
		t.Errorf("only %d terminal routes refused to a driver", refused)
	}
	for p := range driverRoutes {
		if !have[p] {
			t.Errorf("driverRoutes has %q, which is not a route", p)
		}
	}
	// Still logged in (the refusals don't end his login), then a cashier after him.
	if rec := call(h, "GET", "/api/terminal/session", r.termKey, ""); !strings.Contains(rec.Body.String(), `"driver"`) {
		t.Errorf("his session after the refusals: %d %s", rec.Code, rec.Body)
	}
	call(h, "POST", "/api/terminal/logout", r.termKey, "")
	if rec := call(h, "POST", "/api/terminal/login", r.termKey, `{"pin":"1234"}`); rec.Code != http.StatusOK {
		t.Fatalf("cashier login: %d %s", rec.Code, rec.Body)
	}
	for _, g := range []struct{ method, path string }{{"GET", "/api/terminal/pickups"}, {"GET", "/api/terminal/drafts"}, {"POST", "/api/terminal/quote"}} {
		body := ""
		if g.method != "GET" {
			body = `{"type":"pickup","cart":{"lines":[]}}`
		}
		if rec := call(h, g.method, g.path, r.termKey, body); code(rec) == "driver_not_allowed" {
			t.Errorf("a cashier %s %s: %d %s", g.method, g.path, rec.Code, rec.Body)
		}
	}
}
