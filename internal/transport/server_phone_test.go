package transport

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	phoneApp "quiccpos/agent/internal/application/phonepay"
	terminalApp "quiccpos/agent/internal/application/terminal"
	"quiccpos/agent/internal/domain/device"
	"quiccpos/agent/internal/domain/terminal"
	"quiccpos/agent/internal/infra/database/repositories"

	"github.com/rs/zerolog"
)

// noTerm: the terminal service as Send to phone's auth tests need it (nobody
// logged in).
type noTerm struct{}

func (noTerm) CurrentStaff(terminalApp.Actor) (terminal.Staff, error) {
	return terminal.Staff{}, terminal.ErrLoginRequired
}
func (noTerm) RequireManager(context.Context, terminalApp.Actor, string, []string, string) (terminal.Staff, error) {
	return terminal.Staff{}, terminal.ErrLoginRequired
}
func (noTerm) Order(context.Context, string) (*terminalApp.OrderView, error) { return nil, nil }
func (noTerm) RecordAs(context.Context, terminalApp.Actor, string, string, terminalApp.PaymentIn) (*terminalApp.PaymentResult, error) {
	return nil, terminal.ErrLoginRequired
}
func (noTerm) RecordPi(context.Context, string, string, map[string]any) {}
func (noTerm) Store() *terminalApp.Store                                { return nil }
func (noTerm) PrecheckOrder(context.Context, terminalApp.Actor, terminalApp.OrderIn) (*terminalApp.Precheck, error) {
	return nil, terminal.ErrLoginRequired
}
func (noTerm) PlaceAs(context.Context, terminalApp.Actor, string, string, terminalApp.OrderIn) (*terminalApp.PlaceResult, error) {
	return nil, terminal.ErrLoginRequired
}
func (noTerm) OrderByKey(context.Context, string) (*terminalApp.OrderView, error) { return nil, nil }
func (noTerm) Journal(context.Context, int) ([]terminal.Entry, error)             { return nil, nil }

func phoneServer(t *testing.T, appEnv string) (*Server, http.Handler, string) {
	t.Helper()
	s, h, tabletKey, db := diagServerDB(t)
	s.SetPhonePay(phoneApp.New(phoneApp.Deps{Repo: repositories.NewPhoneChargeRepository(db), Term: noTerm{}, Devices: s.devices,
		AppEnv: appEnv, Logger: zerolog.Nop()}))
	return s, h, tabletKey
}

func send(h http.Handler, method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestPhonePairsWithACodeAndACookie(t *testing.T) {
	s, h, tabletKey := phoneServer(t, "dev")
	// A tablet's code doesn't pair a phone (and is used up).
	pc, _, _ := s.pairing.issueFor("dev-1", device.KindTablet, time.Now())
	if rec := send(h, "POST", "/api/phone/pair", `{"code":"`+pc+`","name":"Ali's phone"}`, nil); rec.Code != http.StatusForbidden || code_(rec) != "kind_not_allowed" {
		t.Fatalf("tablet code: %d %s", rec.Code, rec.Body)
	}
	if rec := send(h, "POST", "/api/phone/pair", `{"code":"123456","name":""}`, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("no name: %d", rec.Code)
	}
	pc, _, _ = s.pairing.issueFor("term-1", device.KindPhone, time.Now())
	rec := send(h, "POST", "/api/phone/pair", `{"code":"`+pc+`","name":"Ali's phone"}`, nil)
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"kind":"phone"`) || strings.Contains(rec.Body.String(), "qagt") {
		t.Fatalf("pair: %d %s", rec.Code, rec.Body)
	}
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == phoneCookie {
			cookie = c
		}
	}
	if cookie == nil || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.MaxAge < 300*24*3600 || cookie.Path != "/" {
		t.Fatalf("cookie: %+v", cookie)
	}
	// The code is used up.
	if rec := send(h, "POST", "/api/phone/pair", `{"code":"`+pc+`","name":"Again"}`, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("reused code: %d", rec.Code)
	}
	me := send(h, "GET", "/api/phone/me", "", cookie)
	if me.Code != http.StatusOK || !strings.Contains(me.Body.String(), `"name":"Ali's phone"`) || !strings.Contains(me.Body.String(), `"mode":"fake"`) ||
		!strings.Contains(me.Body.String(), `"timeout_seconds":180`) {
		t.Fatalf("me: %d %s", me.Code, me.Body)
	}
	// The key works as a header too; it opens nothing else.
	if rec := call(h, "GET", "/api/floor", cookie.Value, ""); rec.Code != http.StatusForbidden || code(rec) != "phone_only" {
		t.Fatalf("floor: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "GET", "/api/terminal/phones", cookie.Value, ""); rec.Code != http.StatusForbidden || code(rec) != "terminal_required" {
		t.Fatalf("terminal: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "GET", "/api/phone/me", tabletKey, ""); rec.Code != http.StatusForbidden || code(rec) != "phone_required" {
		t.Fatalf("a tablet on /api/phone: %d", rec.Code)
	}
	if rec := call(h, "GET", "/api/phone/me", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no key: %d", rec.Code)
	}
	// The Pi page's devices list shows it as a phone.
	ds, _ := s.devices.List(context.Background())
	if len(ds) != 2 || ds[1].Kind != device.KindPhone && ds[0].Kind != device.KindPhone {
		t.Fatalf("devices: %+v", ds)
	}
	// Unpair: revoked, cookie cleared.
	rec = send(h, "POST", "/api/phone/unpair", "", cookie)
	if rec.Code != http.StatusNoContent || !strings.Contains(rec.Header().Get("Set-Cookie"), "Max-Age=0") {
		t.Fatalf("unpair: %d %v", rec.Code, rec.Header())
	}
	if rec := send(h, "GET", "/api/phone/me", "", cookie); rec.Code != http.StatusUnauthorized {
		t.Fatalf("after unpair: %d", rec.Code)
	}
}

func code_(rec *httptest.ResponseRecorder) string { return code(rec) }

func TestPhonePagesAndTheFakeSquare(t *testing.T) {
	_, h, _ := phoneServer(t, "dev")
	for _, p := range []string{"/pay", "/pay/back"} {
		rec := send(h, "GET", p, "", nil)
		if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") || !strings.Contains(rec.Body.String(), "Quicc Pay") {
			t.Fatalf("%s: %d", p, rec.Code)
		}
	}
	if rec := send(h, "GET", "/pay/fake-square?amount=1677&note=x&meta=m&cb=%2Fpay%2Fback", "", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "FAKE SQUARE") {
		t.Fatalf("fake page: %d", rec.Code)
	}
	meta := url.QueryEscape(`{"c":"abc","r":"http://quiccpos-pi"}`)
	for _, tc := range []struct{ do, want string }{
		{"approve", "com.squareup.pos.SERVER_TRANSACTION_ID=fake-"},
		{"decline", "com.squareup.pos.ERROR_CODE=com.squareup.pos.ERROR_NO_NETWORK"},
		{"cancel", "com.squareup.pos.ERROR_CODE=com.squareup.pos.ERROR_TRANSACTION_CANCELED"},
	} {
		// cb is forced to our own page: never an open redirect.
		rec := send(h, "GET", "/pay/fake-square?do="+tc.do+"&amount=1677&note=x&meta="+meta+"&cb=https%3A%2F%2Fevil.example%2F", "", nil)
		loc := rec.Header().Get("Location")
		if rec.Code != http.StatusSeeOther || !strings.HasPrefix(loc, "/pay/back?") || !strings.Contains(loc, tc.want) ||
			!strings.Contains(loc, "REQUEST_METADATA="+meta) {
			t.Errorf("%s: %d %s", tc.do, rec.Code, loc)
		}
	}
	_, prod, _ := phoneServer(t, "prod")
	if rec := send(prod, "GET", "/pay/fake-square?do=approve&amount=1", "", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("prod fake: %d", rec.Code)
	}
}

func TestTerminalPhoneRoutesNeedALogin(t *testing.T) {
	s, h, _ := phoneServer(t, "dev")
	key, _ := newDeviceKey()
	if err := s.devices.Create(context.Background(), "term-1", "Counter 1", device.KindTerminal, hashDeviceKey(key), key[:11]); err != nil {
		t.Fatal(err)
	}
	rec := call(h, "GET", "/api/terminal/phones", key, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"phones":[]`) || !strings.Contains(rec.Body.String(), `"mode":"fake"`) {
		t.Fatalf("phones: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "POST", "/api/terminal/phones/code", key, `{}`); rec.Code != http.StatusUnauthorized || code(rec) != "login_required" {
		t.Fatalf("code without a login: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "POST", "/api/terminal/orders/0193a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b/phone-charges", key,
		`{"client_key":"ph-key-0001","phone_id":"x","amount_cents":100}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("charge without a login: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "GET", "/api/terminal/phone-charges/nope", key, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown charge: %d", rec.Code)
	}
}
