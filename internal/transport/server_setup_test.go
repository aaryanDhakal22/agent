package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/device"
	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/infra/database/repositories"

	"github.com/rs/zerolog"
	"golang.org/x/crypto/bcrypt"
)

// setupServer is diagServer + the setup store, a linked Pi and a paired
// terminal; it returns the terminal's key too.
func setupServer(t *testing.T) (s *Server, h http.Handler, tabletKey, termKey string, logs *bytes.Buffer) {
	t.Helper()
	s, h, tabletKey, db := diagServerDB(t)
	ctx := context.Background()
	s.SetSetup(repositories.NewSetupRepository(db))
	logs = &bytes.Buffer{}
	s.logger = zerolog.New(logs)
	if err := s.settings.SetStoreAPIKey(ctx, "qagt-store-key"); err != nil {
		t.Fatal(err)
	}
	termKey, _ = newDeviceKey()
	if err := s.devices.Create(ctx, "term-1", "Counter 1", device.KindTerminal, hashDeviceKey(termKey), termKey[:11]); err != nil {
		t.Fatal(err)
	}
	return s, h, tabletKey, termKey, logs
}

func callFrom(h http.Handler, ip, path, body string) int {
	req, _ := http.NewRequest("POST", path, strings.NewReader(body))
	req.RemoteAddr = ip + ":40000"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestPairAdmin(t *testing.T) {
	s, h, _, _, logs := setupServer(t)
	ctx := context.Background()

	// A tablet or hub can't pair with the admin code.
	if rec := call(h, "POST", "/api/pair/admin", "", `{"code":"1024","name":"Tab","kind":"tablet"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("kind tablet: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "POST", "/api/pair/admin", "", `{"code":"1024"}`); rec.Code != http.StatusBadRequest || code(rec) != "name_required" {
		t.Fatalf("no name: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "POST", "/api/pair/admin", "", `{"code":"9999","name":"Counter 2"}`); rec.Code != http.StatusUnauthorized || code(rec) != "wrong_admin_code" {
		t.Fatalf("wrong code: %d %s", rec.Code, rec.Body)
	}
	rec := call(h, "POST", "/api/pair/admin", "", `{"code":"1024","name":"Counter 2","kind":"terminal"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("the default code pairs: %d %s", rec.Code, rec.Body)
	}
	var paired struct {
		Kind string `json:"kind"`
		Key  string `json:"device_key"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &paired)
	if paired.Kind != device.KindTerminal || !strings.HasPrefix(paired.Key, deviceKeyPrefix) {
		t.Fatalf("paired as: %s", rec.Body)
	}
	if rec := call(h, "GET", "/api/terminal/session", paired.Key, ""); rec.Code == http.StatusForbidden || rec.Code == http.StatusUnauthorized {
		t.Fatalf("the new key is a terminal key: %d %s", rec.Code, rec.Body)
	}

	// A store code replaces 1024; off refuses.
	if err := s.setups.SetAdminPair(ctx, device.AdminPair{Code: "55667788"}); err != nil {
		t.Fatal(err)
	}
	if rec := call(h, "POST", "/api/pair/admin", "", `{"code":"1024","name":"Counter 3"}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("1024 after a change: %d", rec.Code)
	}
	if rec := call(h, "POST", "/api/pair/admin", "", `{"code":"55667788","name":"Counter 3"}`); rec.Code != http.StatusCreated {
		t.Fatalf("the store's code: %d %s", rec.Code, rec.Body)
	}
	if err := s.setups.SetAdminPair(ctx, device.AdminPair{Off: true}); err != nil {
		t.Fatal(err)
	}
	if rec := call(h, "POST", "/api/pair/admin", "", `{"code":"1024","name":"Counter 4"}`); rec.Code != http.StatusForbidden || code(rec) != "admin_code_off" {
		t.Fatalf("off: %d %s", rec.Code, rec.Body)
	}

	// A blank Pi needs the manager login.
	if err := s.settings.SetStoreAPIKey(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if rec := call(h, "POST", "/api/pair/admin", "", `{"code":"1024","name":"Counter 5"}`); rec.Code != http.StatusConflict || code(rec) != "not_linked" {
		t.Fatalf("blank Pi: %d %s", rec.Code, rec.Body)
	}
	for _, secret := range []string{"1024", "9999", "55667788"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("the code %s is in the logs:\n%s", secret, logs)
		}
	}
}

func TestPairAdminThrottle(t *testing.T) {
	_, h, _, _, _ := setupServer(t)
	for i := 0; i < adminCodePerIP; i++ {
		if c := callFrom(h, "10.1.1.1", "/api/pair/admin", `{"code":"0000","name":"T"}`); c != http.StatusUnauthorized {
			t.Fatalf("miss %d: %d", i, c)
		}
	}
	req, _ := http.NewRequest("POST", "/api/pair/admin", strings.NewReader(`{"code":"1024","name":"T"}`))
	req.RemoteAddr = "10.1.1.1:40000"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("paused even with the right code: %d %v", rec.Code, rec.Header())
	}
	if c := callFrom(h, "10.1.1.2", "/api/pair/admin", `{"code":"1024","name":"T"}`); c != http.StatusCreated {
		t.Fatalf("another address: %d", c)
	}
}

func TestAdminCodeThrottleStoreWide(t *testing.T) {
	var th adminCodeThrottle
	now := time.Now()
	for i := 0; i < adminCodeStore; i++ {
		th.fail("10.0.0."+string(rune('a'+i)), now) // ten addresses, one miss each
	}
	if th.wait("10.9.9.9", now) <= 0 {
		t.Fatal("ten misses across the store pause every address")
	}
	if w := th.wait("10.9.9.9", now.Add(adminCodeBasePause+time.Second)); w != 0 {
		t.Fatalf("the pause ends: %v", w)
	}
	// Doubling: the next store-wide pause is longer.
	for i := 0; i < adminCodeStore; i++ {
		th.fail("10.0.1."+string(rune('a'+i)), now.Add(2*adminCodeBasePause))
	}
	if w := th.wait("10.9.9.9", now.Add(2*adminCodeBasePause)); w < 2*adminCodeBasePause-time.Second {
		t.Fatalf("second pause doubles: %v", w)
	}
}

func TestAdminCodeSetting(t *testing.T) {
	s, h, _, _, _ := setupServer(t)
	ctx := context.Background()
	if v := viewAdminPair(device.AdminPair{}); !v.Enabled || !v.IsDefault || v.Length != 4 {
		t.Fatalf("default: %+v", v)
	}
	for _, bad := range []string{"12", "123456789", "12a4"} {
		r := webCall(s, h, "PUT", "/api/settings/admin-code", `{"code":"`+bad+`"}`)
		if r.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d", bad, r.Code)
		}
	}
	if r := webCall(s, h, "PUT", "/api/settings/admin-code", `{"code":"4321","enabled":true}`); r.Code != http.StatusOK || strings.Contains(r.Body.String(), "4321") {
		t.Fatalf("set: %d %s", r.Code, r.Body)
	}
	a, _ := s.setups.AdminPair(ctx)
	if a.Effective() != "4321" {
		t.Fatalf("stored: %+v", a)
	}
	if r := webCall(s, h, "GET", "/api/settings/admin-code", ""); !strings.Contains(r.Body.String(), `"is_default":false`) || strings.Contains(r.Body.String(), "4321") {
		t.Fatalf("get: %s", r.Body)
	}
	if r := webCall(s, h, "PUT", "/api/settings/admin-code", `{"code":""}`); r.Code != http.StatusOK || !strings.Contains(r.Body.String(), `"is_default":true`) {
		t.Fatalf("back to 1024: %s", r.Body)
	}
	if r := webCall(s, h, "PUT", "/api/settings/admin-code", `{"enabled":false}`); !strings.Contains(r.Body.String(), `"enabled":false`) {
		t.Fatalf("off: %s", r.Body)
	}
}

// webCall is a request from the Pi web page (a session cookie).
func webCall(s *Server, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
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

func TestTerminalKeySetupRoutes(t *testing.T) {
	s, h, tabletKey, termKey, _ := setupServer(t)
	// The diagnostics a terminal may read / call now.
	for _, rt := range []struct{ method, path, body string }{
		{"GET", "/api/diag", ""},
		{"GET", "/api/printers/found", ""},
		{"POST", "/api/printers/test", `{"probe_only":true}`},
		{"POST", "/api/diag/report", `{"v":1}`},
	} {
		if rec := call(h, rt.method, rt.path, termKey, rt.body); rec.Code == http.StatusForbidden || rec.Code == http.StatusUnauthorized {
			t.Errorf("%s %s: %d %s", rt.method, rt.path, rec.Code, rec.Body)
		}
	}
	// Still closed to a terminal.
	if rec := call(h, "POST", "/api/printers/add", termKey, `{}`); rec.Code != http.StatusForbidden || code(rec) != "terminal_only" {
		t.Errorf("printers/add: %d %s", rec.Code, rec.Body)
	}
	// The terminal routes stay closed to a tablet.
	for _, p := range []string{"/api/terminal/setup"} {
		if rec := call(h, "GET", p, tabletKey, ""); rec.Code != http.StatusForbidden || code(rec) != "terminal_required" {
			t.Errorf("%s with a tablet key: %d %s", p, rec.Code, rec.Body)
		}
	}
	for _, p := range []string{"/api/terminal/print", "/api/terminal/square/test-refund"} {
		if rec := call(h, "POST", p, tabletKey, "{}"); rec.Code != http.StatusForbidden || code(rec) != "terminal_required" {
			t.Errorf("%s with a tablet key: %d %s", p, rec.Code, rec.Body)
		}
	}

	// A setup report is kept on the Pi before it goes to main (not linked
	// to an RPC here: 503, still kept).
	report := `{"v":1,"id":"6a4d2f60-0d5c-4b55-9b8e-1f7c3a0e2b11","kind":"setup","outcome":"not_fixed","headline":"Counter 1 set up - drawer not confirmed","created_at":"2026-11-12T15:12:00Z","failed":["drawer"]}`
	if rec := call(h, "POST", "/api/diag/report", termKey, report); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("report: %d %s", rec.Code, rec.Body)
	}
	got := s.deviceSetups(context.Background())["term-1"]
	if got.Outcome != "not_fixed" || got.Kind != "setup" || len(got.Failed) != 1 || got.At.Format(time.RFC3339) != "2026-11-12T15:12:00Z" {
		t.Fatalf("kept: %+v", got)
	}
	// A plain guide report is relayed, not kept.
	_ = call(h, "POST", "/api/diag/report", tabletKey, `{"v":1,"id":"6a4d2f60-0d5c-4b55-9b8e-1f7c3a0e2b12","kind":"guide","outcome":"fixed"}`)
	if _, ok := s.deviceSetups(context.Background())["dev-1"]; ok {
		t.Fatal("a guide report isn't a setup result")
	}

	// /api/diag lists the terminal with its setup.
	rec := call(h, "GET", "/api/diag", termKey, "")
	var d struct {
		Terminals []TabletInfo `json:"terminals"`
		Tablets   []TabletInfo `json:"tablets"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &d)
	if len(d.Terminals) != 1 || d.Terminals[0].Name != "Counter 1" || d.Terminals[0].Setup == nil || d.Terminals[0].Setup.Outcome != "not_fixed" {
		t.Fatalf("terminals: %s", rec.Body)
	}
	if len(d.Tablets) != 1 || d.Tablets[0].Name != "Counter" {
		t.Fatalf("tablets: %s", rec.Body)
	}

	// The summary.
	rec = call(h, "GET", "/api/terminal/setup", termKey, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("setup: %d %s", rec.Code, rec.Body)
	}
	var sum struct {
		Store    map[string]any   `json:"store"`
		Printers []map[string]any `json:"printers"`
		Setups   []map[string]any `json:"setups"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &sum)
	if sum.Store == nil || sum.Printers == nil || len(sum.Setups) != 1 || sum.Setups[0]["name"] != "Counter 1" || sum.Setups[0]["kind_of_report"] != "setup" {
		t.Fatalf("summary: %s", rec.Body)
	}

	// The version a terminal sends is noted.
	req, _ := http.NewRequest("GET", "/api/terminal/status", nil)
	req.Header.Set(deviceKeyHeader, termKey)
	req.Header.Set(terminalVersionHeader, "1.4.0+1762960000")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if v := s.Terminals(context.Background())[0].AppVersion; v != "1.4.0+1762960000" {
		t.Fatalf("terminal version: %q", v)
	}
}

func TestTerminalPrintValidation(t *testing.T) {
	_, h, _, termKey, _ := setupServer(t)
	for _, c := range []struct {
		body string
		want int
	}{
		{`{"printer":"Front","bytes_b64":"G0A=","kind":"other"}`, http.StatusBadRequest},
		{`{"printer":"Front","bytes_b64":"not base64!","kind":"test"}`, http.StatusBadRequest},
		{`{"printer":"Front","bytes_b64":"","kind":"test"}`, http.StatusBadRequest},
		{`{"printer":"Front","bytes_b64":"G0A=","kind":"test"}`, http.StatusServiceUnavailable}, // no dispatcher here
	} {
		if rec := call(h, "POST", "/api/terminal/print", termKey, c.body); rec.Code != c.want {
			t.Errorf("%s: %d %s", c.body, rec.Code, rec.Body)
		}
	}
	big := `{"printer":"Front","kind":"test","bytes_b64":"` + strings.Repeat("A", maxTerminalPrint*2) + `"}`
	if rec := call(h, "POST", "/api/terminal/print", termKey, big); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("too big: %d", rec.Code)
	}
}

func TestTestRefundValidation(t *testing.T) {
	_, h, _, termKey, _ := setupServer(t)
	for _, body := range []string{
		`{"square_payment_id":"p1","amount_cents":101,"idempotency_key":"k"}`,
		`{"square_payment_id":"p1","amount_cents":0,"idempotency_key":"k"}`,
		`{"square_payment_id":"","amount_cents":100,"idempotency_key":"k"}`,
		`{"square_payment_id":"p1","amount_cents":100,"idempotency_key":""}`,
	} {
		if rec := call(h, "POST", "/api/terminal/square/test-refund", termKey, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", body, rec.Code)
		}
	}
	if rec := call(h, "POST", "/api/terminal/square/test-refund", termKey, `{"square_payment_id":"p1","amount_cents":100,"idempotency_key":"k"}`); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("no RPC: %d", rec.Code)
	}
}

func TestTabletTerminalCodeNeedsManagerLogin(t *testing.T) {
	s, h, tabletKey, _, logs := setupServer(t)
	ctx := context.Background()
	if rec := call(h, "POST", "/api/pairing/code", tabletKey, `{"kind":"terminal"}`); rec.Code != http.StatusForbidden || code(rec) != "kind_not_allowed" {
		t.Fatalf("no login: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "POST", "/api/pairing/code", tabletKey, `{"kind":"terminal","username":"boss","password":"pw"}`); rec.Code != http.StatusConflict || code(rec) != "manager_login_unavailable" {
		t.Fatalf("a Pi without the manager login: %d %s", rec.Code, rec.Body)
	}
	hash, _ := bcrypt.GenerateFromPassword([]byte("right-pass"), bcrypt.MinCost)
	if err := s.settings.SetLinkInfo(ctx, order.LinkInfo{ManagerUsername: "boss"}, string(hash), ""); err != nil {
		t.Fatal(err)
	}
	if rec := call(h, "POST", "/api/pairing/code", tabletKey, `{"kind":"terminal","username":"boss","password":"wrong"}`); rec.Code != http.StatusUnauthorized || code(rec) != "bad_login" {
		t.Fatalf("wrong password: %d %s", rec.Code, rec.Body)
	}
	rec := call(h, "POST", "/api/pairing/code", tabletKey, `{"kind":"terminal","username":"boss","password":"right-pass"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"kind":"terminal"`) {
		t.Fatalf("with the login: %d %s", rec.Code, rec.Body)
	}
	var b struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &b)
	if ok, wrong := s.pairing.consumeKind(b.Code, device.KindTerminal, time.Now()); !ok || wrong {
		t.Fatal("the code pairs a terminal")
	}
	if strings.Contains(logs.String(), "right-pass") {
		t.Fatal("the password is in the logs")
	}
	// A plain tablet code is still a tablet code.
	rec = call(h, "POST", "/api/pairing/code", tabletKey, `{}`)
	_ = json.Unmarshal(rec.Body.Bytes(), &b)
	if ok, wrong := s.pairing.consumeKind(b.Code, device.KindTerminal, time.Now()); ok || !wrong {
		t.Fatal("a tablet code doesn't pair a terminal")
	}
}

func TestSetupResultFromReport(t *testing.T) {
	now := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	if _, ok := device.SetupResultFromReport([]byte(`{"kind":"guide","outcome":"fixed"}`), now); ok {
		t.Fatal("a guide isn't a setup")
	}
	if _, ok := device.SetupResultFromReport([]byte(`{"kind":"setup"}`), now); ok {
		t.Fatal("no outcome")
	}
	r, ok := device.SetupResultFromReport([]byte(`{"id":"x","kind":"full_check","outcome":"ok","created_at":"bad","failed":["a",1,""]}`), now)
	if !ok || !r.At.Equal(now) || len(r.Failed) != 1 || r.Kind != "full_check" {
		t.Fatalf("%+v", r)
	}
}
