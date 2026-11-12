package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	orderApp "quiccpos/agent/internal/application/order"
	"quiccpos/agent/internal/application/squareterm"
	terminalApp "quiccpos/agent/internal/application/terminal"
	"quiccpos/agent/internal/domain/device"
	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/terminal"
	"quiccpos/agent/internal/infra/database/repositories"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"golang.org/x/crypto/bcrypt"
)

// The call center's relay (contract §2.10ab): RPC terminal.relay runs a
// call through the same /api/terminal/* handlers as a call-center seat.

const (
	ccAgent   = "cc-1"
	ccCashier = "cash-1"
	ccCheese  = "00000000-0000-0000-0000-0000000000a1"
	ccXL      = "00000000-0000-0000-0000-0000000000b2"
)

// ccFloor is the terminal service's floor in these tests: nothing prints.
type ccFloor struct {
	mu       sync.Mutex
	receipts []string
	reprints []string
	reprint  error
}

func (f *ccFloor) PrintCounterTickets(context.Context, order.StoredOrder) {}
func (f *ccFloor) PrintCounterTicketsThen(ctx context.Context, _ order.StoredOrder, done func(context.Context)) {
	if done != nil {
		done(ctx)
	}
}
func (f *ccFloor) PrintVoidSlip(context.Context, order.StoredOrder) {}
func (f *ccFloor) PrintChange(context.Context, order.StoredOrder, []order.LineItem, []order.LineItem) {
}
func (f *ccFloor) CounterOrderChanged(context.Context, order.StoredOrder, *order.StoredOrder, bool) {
}
func (f *ccFloor) JournalWritten()               {}
func (f *ccFloor) PrepMinutesFor(string) int64   { return 15 }
func (f *ccFloor) StoreIDs() (string, string)    { return "tenant", "location" }
func (f *ccFloor) StoreLocation() *time.Location { return time.UTC }
func (f *ccFloor) DevEvent(string)               {}
func (f *ccFloor) PrintOrderReceipt(_ context.Context, so order.StoredOrder) {
	f.mu.Lock()
	f.receipts = append(f.receipts, so.Order.ID)
	f.mu.Unlock()
}
func (f *ccFloor) ReprintOn(_ context.Context, id string) (*order.StoredOrder, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reprint != nil {
		return nil, "", f.reprint
	}
	f.reprints = append(f.reprints, id)
	n := int64(301)
	return &order.StoredOrder{Order: order.Snapshot{ID: id}, Status: order.Status{KitchenNumber: &n}}, "Front", nil
}

type ccPub struct{}

func (ccPub) Publish(string, any) {}

type ccRig struct {
	s       *Server
	h       http.Handler
	termKey string
	repo    *repositories.OrderRepository
	floor   *ccFloor
	fetches atomic.Int64
	config  atomic.Value // []byte: main's terminal-config
}

func ccConfig(t *testing.T, version int, staff ...map[string]any) []byte {
	t.Helper()
	pin, err := bcrypt.GenerateFromPassword([]byte("1234"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	all := append([]map[string]any{{"id": ccCashier, "first_name": "Cara", "last_name": "Smith", "role": "cashier", "pin_hash": string(pin)}}, staff...)
	b, _ := json.Marshal(map[string]any{"version": version, "staff": all, "settings": map[string]any{"counter_start": 300}})
	return b
}

// ccAgentStaff is a call-center agent as main sends them: no PIN hash (main
// checks that PIN, the Pi never does).
func ccAgentStaff() map[string]any {
	return map[string]any{"id": ccAgent, "first_name": "Elena", "last_name": "Vega", "role": "callcenter", "pin_hash": ""}
}

func newCCRig(t *testing.T) *ccRig {
	t.Helper()
	s, h, _, db := diagServerDB(t)
	ctx := context.Background()
	if err := s.settings.SetStoreAPIKey(ctx, "qagt-store-key"); err != nil {
		t.Fatal(err)
	}
	r := &ccRig{s: s, h: h, floor: &ccFloor{}}
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
	r.config.Store(ccConfig(t, 1, ccAgentStaff()))
	repo := repositories.NewOrderRepository(db, zerolog.Nop())
	r.repo = repo
	store := terminalApp.NewStore(repo, func(_ context.Context, path string) (int, []byte, error) {
		if path == terminalApp.PathCounterMenu {
			return 200, menu, nil
		}
		r.fetches.Add(1)
		return 200, r.config.Load().([]byte), nil
	}, ccPub{}, zerolog.Nop())
	svc := terminalApp.NewService(store, repo, r.floor, ccPub{}, zerolog.Nop())
	svc.SetReports(s.service) // the checks list
	// The store's customers (no feed from main here) and its delivery area: a pin + a 3-mile radius.
	lat, lng := 39.4, -76.6
	svc.SetCustomers(terminalApp.NewCustomerBook(repo, func(context.Context, string, string) (int, []byte, error) { return 404, nil, nil }, ccPub{},
		func() string { return "location" }, func() (*float64, *float64, int64) { return &lat, &lng, 4828 }, zerolog.Nop()))
	for _, k := range []string{terminal.KindCounterMenu, terminal.KindTerminalConfig} {
		if _, err := store.Refresh(ctx, k); err != nil {
			t.Fatal(err)
		}
	}
	s.SetTerminal(svc)
	return r
}

type relayed struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	Body    json.RawMessage   `json:"body"`
}

func (r relayed) code() string {
	var b struct {
		Code string `json:"code"`
		What string `json:"what"`
	}
	_ = json.Unmarshal(r.Body, &b)
	if b.What != "" {
		return b.Code + ":" + b.What
	}
	return b.Code
}

// relay sends one terminal.relay command the way main does (the reply goes
// through JSON, as it does on the wire).
func (r *ccRig) relay(t *testing.T, staff, method, path string, body any, extra ...func(map[string]any)) relayed {
	t.Helper()
	p := map[string]any{"device": "desk-2", "device_name": "Elevate desk 2", "staff_id": staff, "method": method, "path": path,
		"query": "", "headers": map[string]string{}, "body": body}
	for _, fn := range extra {
		fn(p)
	}
	raw, _ := json.Marshal(p)
	out, err := r.s.HandleTerminalRelay(context.Background(), raw)
	if err != nil {
		t.Fatalf("relay %s %s: %v", method, path, err)
	}
	wire, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var got relayed
	if err := json.Unmarshal(wire, &got); err != nil {
		t.Fatalf("reply %s: %v", wire, err)
	}
	if got.Headers == nil {
		t.Fatalf("the reply always carries headers (an object): %s", wire)
	}
	return got
}

func ccOrder(key string) map[string]any {
	return map[string]any{"client_key": key, "type": "pickup", "customer": map[string]any{"name": "Ada Test", "phone": "4105550101"},
		"cart": map[string]any{"lines": []map[string]any{{"item_id": ccCheese, "size_id": ccXL, "qty": 1}}}}
}

func TestRelayRunsTheTerminalHandlers(t *testing.T) {
	r := newCCRig(t)
	// The menu with its ETag; If-None-Match -> 304, no body.
	menu := r.relay(t, ccAgent, "GET", "menu", nil)
	etag := menu.Headers["ETag"]
	if menu.Status != 200 || etag == "" || !strings.HasPrefix(string(menu.Body), `{`) || !strings.Contains(string(menu.Body), `"counter_menu"`) {
		t.Fatalf("menu: %d %v %.80s", menu.Status, menu.Headers, menu.Body)
	}
	again := r.relay(t, ccAgent, "GET", "menu", nil, func(p map[string]any) { p["headers"] = map[string]string{"if-none-match": etag} })
	if again.Status != http.StatusNotModified || string(again.Body) != "null" || again.Headers["ETag"] != etag {
		t.Fatalf("If-None-Match: %d %s %v", again.Status, again.Body, again.Headers)
	}
	// The agent's session on the Pi: role callcenter, limits 0 / 0.
	sess := r.relay(t, ccAgent, "get", "/session", nil) // method case and a leading slash don't matter
	var sv struct {
		Staff  struct{ ID, Name string }
		Role   string
		Limits struct {
			RefundCents int64 `json:"refund_cents"`
			DiscountPct int   `json:"discount_pct"`
			Unlimited   bool  `json:"unlimited"`
		}
	}
	if err := json.Unmarshal(sess.Body, &sv); err != nil || sess.Status != 200 || sv.Staff.ID != ccAgent || sv.Staff.Name != "Elena V." ||
		sv.Role != "callcenter" || sv.Limits.RefundCents != 0 || sv.Limits.DiscountPct != 0 || sv.Limits.Unlimited {
		t.Fatalf("session: %d %s", sess.Status, sess.Body)
	}
	// Place: 201 with the Pi's own body; the same key again: 200 duplicate.
	placed := r.relay(t, ccAgent, "POST", "orders", ccOrder("cc-key-000001"))
	var pv struct {
		Order struct {
			Order struct {
				ID         string `json:"id"`
				TakenBy    string `json:"taken_by"`
				DeviceKind string `json:"device_kind"`
			} `json:"order"`
			Status struct {
				State string `json:"state"`
			} `json:"status"`
		} `json:"order"`
		Number    int64 `json:"number"`
		Duplicate bool  `json:"duplicate"`
	}
	if err := json.Unmarshal(placed.Body, &pv); err != nil || placed.Status != http.StatusCreated || pv.Order.Order.TakenBy != "Call center - Elena V." ||
		pv.Order.Order.DeviceKind != "callcenter" || pv.Order.Status.State != "accepted" || pv.Number != 300 {
		t.Fatalf("place: %d %s", placed.Status, placed.Body)
	}
	id := pv.Order.Order.ID
	if len(r.floor.receipts) != 1 || r.floor.receipts[0] != id {
		t.Fatalf("the Pi prints the receipt of a call-center order: %v", r.floor.receipts)
	}
	dup := r.relay(t, ccAgent, "POST", "orders", ccOrder("cc-key-000001"))
	if dup.Status != http.StatusOK || !strings.Contains(string(dup.Body), `"duplicate":true`) {
		t.Fatalf("the same key: %d %.120s", dup.Status, dup.Body)
	}
	// The query string rides along; a path value too.
	open := r.relay(t, ccAgent, "GET", "orders", nil, func(p map[string]any) { p["query"] = "open=1" })
	if open.Status != 200 || !strings.Contains(string(open.Body), id) {
		t.Fatalf("orders?open=1: %d %.120s", open.Status, open.Body)
	}
	one := r.relay(t, ccAgent, "GET", "orders/"+id, nil)
	if one.Status != 200 || !strings.Contains(string(one.Body), `"payments":[]`) {
		t.Fatalf("orders/{id}: %d %.120s", one.Status, one.Body)
	}
	if nf := r.relay(t, ccAgent, "GET", "orders/0199c0de-0000-7000-8000-00000000dead", nil); nf.Status != http.StatusNotFound || nf.code() != "not_found" {
		t.Fatalf("an unknown order: %d %s", nf.Status, nf.Body)
	}
	// The handlers' own errors come back as they are.
	if bad := r.relay(t, ccAgent, "POST", "orders/"+id+"/void", map[string]any{}); bad.Status != http.StatusBadRequest || bad.code() != "bad_request" {
		t.Fatalf("a void without a reason: %d %s", bad.Status, bad.Body)
	}
	if bad := r.relay(t, ccAgent, "POST", "orders", map[string]any{"type": "pickup"}); bad.Status != http.StatusBadRequest || bad.code() != "client_key_required" {
		t.Fatalf("no client_key: %d %s", bad.Status, bad.Body)
	}
	// The call center's own guards (403 + what), the paid lock (409 + lines).
	disc := ccOrder("cc-key-000002")
	disc["cart"].(map[string]any)["discount"] = map[string]any{"pct": 5, "reason": "regular"}
	if no := r.relay(t, ccAgent, "POST", "orders", disc); no.Status != http.StatusForbidden || no.code() != "not_allowed_for_callcenter:discount" {
		t.Fatalf("a discount: %d %s", no.Status, no.Body)
	}
	if pay := r.relay(t, ccAgent, "POST", "orders/"+id+"/payments", map[string]any{"client_key": "cc-pay-000001", "tender": "manual_card", "amount_cents": 500, "last4": "1881"}); pay.Status != 200 {
		t.Fatalf("a typed card: %d %s", pay.Status, pay.Body)
	}
	if cash := r.relay(t, ccAgent, "POST", "orders/"+id+"/payments", map[string]any{"client_key": "cc-pay-000002", "tender": "cash", "amount_cents": 100}); cash.Status != http.StatusForbidden || cash.code() != "not_allowed_for_callcenter:tender" {
		t.Fatalf("cash: %d %s", cash.Status, cash.Body)
	}
	locked := r.relay(t, ccAgent, "PUT", "orders/"+id+"/cart", map[string]any{"client_key": "cc-chg-000001",
		"cart": map[string]any{"lines": []map[string]any{{"item_id": ccCheese, "qty": 1}}}}) // another size: the paid line changed
	var lv struct {
		Code  string   `json:"code"`
		Lines []string `json:"lines"`
	}
	if err := json.Unmarshal(locked.Body, &lv); err != nil || locked.Status != http.StatusConflict || lv.Code != "paid_locked" || len(lv.Lines) != 1 {
		t.Fatalf("a paid line changed: %d %s", locked.Status, locked.Body)
	}
	// Reprint, the floor, the lists.
	if rp := r.relay(t, ccAgent, "POST", "orders/"+id+"/reprint", nil); rp.Status != 200 || string(rp.Body) != `{"ok":true,"printed_on":"Front"}` {
		t.Fatalf("reprint: %d %s", rp.Status, rp.Body)
	}
	for _, path := range []string{"floor", "pickups", "drafts", "status"} {
		if got := r.relay(t, ccAgent, "GET", path, nil); got.Status != 200 || !json.Valid(got.Body) || string(got.Body) == "null" {
			t.Errorf("GET %s: %d %.120s", path, got.Status, got.Body)
		}
	}
	// The store's device list shows the one call-center device (made by itself), last.
	var devs []struct{ ID, Name, Kind string }
	if rec := webCall(r.s, r.h, "GET", "/api/devices", ""); json.Unmarshal(rec.Body.Bytes(), &devs) != nil || len(devs) != 3 ||
		devs[2].ID != "callcenter" || devs[2].Kind != "callcenter" || devs[2].Name != "Call center" {
		t.Fatalf("devices: %s", rec.Body)
	}
	if rec := webCall(r.s, r.h, "POST", "/api/devices/callcenter/revoke", ""); rec.Code != http.StatusConflict || code(rec) != "callcenter_device" {
		t.Fatalf("revoking the call-center device: %d %s", rec.Code, rec.Body)
	}
}

func TestRelayAllowlistSessionAndManager(t *testing.T) {
	r := newCCRig(t)
	for _, rt := range [][2]string{{"POST", "login"}, {"POST", "manager-check"}, {"POST", "refunds"}, {"GET", "drawer"}, {"POST", "drawer/pay-out"},
		{"POST", "close"}, {"GET", "z"}, {"GET", "reports/till"}, {"GET", "journal"}, {"POST", "print"}, {"GET", "square/session"},
		{"POST", "orders/x/waste"}, {"POST", "orders/x/picked-up"}, {"POST", "deliveries/send-out"}, {"DELETE", "orders/x"},
		{"GET", "../devices"}, {"GET", "orders/../../devices"}, {"GET", ""}, {"POST", "floor"}} {
		got := r.relay(t, ccAgent, rt[0], rt[1], map[string]any{"pin": "1234"})
		if got.Status != http.StatusForbidden || got.code() != "not_allowed_for_callcenter:route" {
			t.Errorf("%s %q: %d %s", rt[0], rt[1], got.Status, got.Body)
		}
	}
	// A manager's PIN or an approval in any body: refused before the handler.
	for name, body := range map[string]map[string]any{
		"manager_pin": {"type": "pickup", "manager_pin": "9876", "cart": map[string]any{"lines": []any{}}},
		"approvals":   {"type": "pickup", "approvals": []string{"ap-1"}, "cart": map[string]any{"lines": []any{}}},
	} {
		if got := r.relay(t, ccAgent, "POST", "quote", body); got.Status != http.StatusForbidden || got.code() != "not_allowed_for_callcenter:manager" {
			t.Errorf("%s: %d %s", name, got.Status, got.Body)
		}
	}
	// Fail closed: a body whose guarded fields have the wrong JSON type (or that isn't an object) is a 400
	// before any handler or guard reads it half-decoded.
	orders := r.relay(t, ccAgent, "GET", "orders", nil)
	for _, c := range []struct{ method, path, body string }{
		{"POST", "orders/x/payments", `{"tender":"comp","client_key":7}`},
		{"POST", "orders/x/payments", `{"tender":["comp"],"amount_cents":100}`},
		{"POST", "orders/x/payments", `{"tender":"manual_card","amount_cents":100,"refund":"yes"}`},
		{"POST", "orders/x/payments", `{"tender":"manual_card","amount_cents":"100"}`},
		{"POST", "orders", `{"client_key":"cc-key-shape1","type":"pickup","payments":"x","cart":{"lines":[]}}`},
		{"POST", "orders", `{"client_key":"cc-key-shape2","type":"pickup","payments":[{"tender":"manual_card","refund":1}],"cart":{"lines":[]}}`},
		{"POST", "orders", `{"client_key":"cc-key-shape3","type":"pickup","cart":{"lines":"x"}}`},
		{"POST", "orders", `{"client_key":"cc-key-shape4","type":"pickup","cart":"x"}`},
		{"POST", "orders", `{"client_key":"cc-key-shape5","type":"pickup","cart":{"lines":[{"item_id":"a","custom":"true"}]}}`},
		{"POST", "orders", `{"client_key":"cc-key-shape6","type":"pickup","cart":{"lines":[{"item_id":"a","price_cents":"500"}]}}`},
		{"POST", "orders", `{"client_key":"cc-key-shape7","type":"pickup","cart":{"lines":[{"item_id":"a","options":[{"price_cents":"1"}]}]}}`},
		{"POST", "orders", `{"client_key":"cc-key-shape8","type":"pickup","cart":{"lines":[],"discount":"5%"}}`},
		{"POST", "orders", `{"client_key":"cc-key-shape9","type":["counter"],"cart":{"lines":[]}}`},
		{"POST", "orders", `{"client_key":"cc-key-shap10","type":"pickup","manager_pin":["1"],"cart":{"lines":[]}}`},
		{"POST", "orders", `{"client_key":"cc-key-shap11","type":"pickup","manager_pin":9876,"cart":{"lines":[]}}`},
		{"POST", "orders", `{"client_key":"cc-key-shap12","type":"pickup","approvals":"ap-1","cart":{"lines":[]}}`},
		{"POST", "orders", `{"client_key":"cc-key-shap13","type":"pickup","approvals":[1],"cart":{"lines":[]}}`},
		{"PUT", "orders/x/cart", `{"client_key":"cc-key-shap14","cart":{"lines":[7]}}`},
		{"POST", "quote", `["type","pickup"]`},
		{"POST", "quote", `"pickup"`},
		{"POST", "quote", `7`},
	} {
		raw, _ := json.Marshal(map[string]any{"device": "desk-2", "staff_id": ccAgent, "method": c.method, "path": c.path, "headers": map[string]string{},
			"body": json.RawMessage(c.body)})
		out, err := r.s.HandleTerminalRelay(context.Background(), raw)
		rr, _ := out.(RelayReply)
		if err != nil || rr.Status != http.StatusBadRequest || !strings.Contains(string(rr.Body), `"code":"bad_request"`) {
			t.Errorf("%s %s %s: %d %s, want 400 bad_request", c.method, c.path, c.body, rr.Status, rr.Body)
		}
	}
	if after := r.relay(t, ccAgent, "GET", "orders", nil); string(after.Body) != string(orders.Body) || len(r.floor.receipts) != 0 {
		t.Fatalf("a refused body places nothing: %s", after.Body)
	}
	empty := ccOrder("")
	delete(empty, "client_key")
	empty["manager_pin"], empty["approvals"] = "", []string{}
	if got := r.relay(t, ccAgent, "POST", "quote", empty); got.Status != 200 {
		t.Fatalf("an empty manager_pin / approvals is no manager: %d %s", got.Status, got.Body)
	}
	// Who the relay says is on the seat: an active call-center agent of the snapshot, else login_required.
	for _, staff := range []string{"", ccCashier, "nobody"} {
		if got := r.relay(t, staff, "GET", "menu", nil); got.Status != http.StatusUnauthorized || got.code() != "login_required" {
			t.Errorf("staff %q: %d %s", staff, got.Status, got.Body)
		}
	}
	// A bad seat id never reaches a handler.
	if got := r.relay(t, ccAgent, "GET", "menu", nil, func(p map[string]any) { p["device"] = "a b/../c" }); got.Status != http.StatusBadRequest {
		t.Fatalf("a bad device: %d %s", got.Status, got.Body)
	}
	if got := r.relay(t, ccAgent, "POST", "quote", map[string]any{"pad": strings.Repeat("x", relayMaxBody)}); got.Status != http.StatusRequestEntityTooLarge {
		t.Fatalf("a body over 256 KB: %d", got.Status)
	}
	out, err := r.s.HandleTerminalRelay(context.Background(), json.RawMessage(`not json`))
	if rr, _ := out.(RelayReply); err != nil || rr.Status != http.StatusBadRequest {
		t.Fatalf("a bad payload: %+v %v", out, err)
	}
}

// A person made in main a minute ago: the Pi pulls the config once for an
// id it doesn't know, then the call runs.
func TestRelayPullsTheSnapshotForAnUnknownAgent(t *testing.T) {
	r := newCCRig(t)
	before := r.fetches.Load()
	if got := r.relay(t, "cc-new", "GET", "session", nil); got.Status != http.StatusUnauthorized || r.fetches.Load() != before+1 {
		t.Fatalf("unknown to main too: %d, config pulls %d", got.Status, r.fetches.Load()-before)
	}
	r.config.Store(ccConfig(t, 2, ccAgentStaff(), map[string]any{"id": "cc-new", "first_name": "Noor", "role": "callcenter", "pin_hash": ""}))
	// Within the pause nothing is pulled again (a wrong id can't hammer main).
	if got := r.relay(t, "cc-new", "GET", "session", nil); got.Status != http.StatusUnauthorized || r.fetches.Load() != before+1 {
		t.Fatalf("no second pull at once: %d, pulls %d", got.Status, r.fetches.Load()-before)
	}
}

// The seat is a context value: nothing a caller sends over plain HTTP makes
// a request the call center's.
func TestCallcenterIsNeverReachableOverHTTP(t *testing.T) {
	r := newCCRig(t)
	spoof := func(key string) *http.Request {
		req := httptest.NewRequest("GET", "/api/terminal/session", nil)
		for k, v := range map[string]string{"X-Callcenter": "1", "X-Callcenter-Seat": "callcenter:desk-2", "X-Callcenter-Staff": ccAgent,
			"X-Staff-Id": ccAgent, "X-Device-Kind": "callcenter", "X-Agent-Device-Id": "callcenter", "X-Forwarded-For": "callcenter"} {
			req.Header.Set(k, v)
		}
		if key != "" {
			req.Header.Set(deviceKeyHeader, key)
		}
		req.RemoteAddr = "callcenter"
		return req
	}
	for _, key := range []string{"", "callcenter", "qagt_callcenter", hashDeviceKey("callcenter")} {
		rec := httptest.NewRecorder()
		r.h.ServeHTTP(rec, spoof(key))
		if rec.Code != http.StatusUnauthorized || code(rec) != "device_required" {
			t.Errorf("key %q: %d %s", key, rec.Code, rec.Body)
		}
	}
	// With a real terminal key the request is that terminal's: nobody is logged in on it.
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, spoof(r.termKey))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"staff":null`) {
		t.Fatalf("a terminal with call-center headers: %d %s", rec.Code, rec.Body)
	}
	// The relay itself is not a route.
	for _, path := range []string{"/api/terminal/relay", "/api/relay", "/api/callcenter", "/api/terminal/orders/x/relay"} {
		if rec := call(r.h, "POST", path, r.termKey, `{}`); rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s: %d", path, rec.Code)
		}
	}
	// A call-center agent's PIN on a store terminal: wrong_pin (their hash, when main sends one, is never matched).
	pin, _ := bcrypt.GenerateFromPassword([]byte("4321"), bcrypt.MinCost)
	st := ccAgentStaff()
	st["pin_hash"] = string(pin)
	r.config.Store(ccConfig(t, 3, st))
	if _, err := r.s.terminalSvc.Store().Refresh(context.Background(), terminal.KindTerminalConfig); err != nil {
		t.Fatal(err)
	}
	if rec := call(r.h, "POST", "/api/terminal/login", r.termKey, `{"pin":"4321"}`); rec.Code != http.StatusUnauthorized || code(rec) != "wrong_pin" {
		t.Fatalf("a call-center PIN on a terminal: %d %s", rec.Code, rec.Body)
	}
	if rec := call(r.h, "POST", "/api/terminal/login", r.termKey, `{"pin":"1234"}`); rec.Code != 200 {
		t.Fatalf("the cashier logs in: %d %s", rec.Code, rec.Body)
	}
	if rec := call(r.h, "POST", "/api/terminal/manager-check", r.termKey, `{"pin":"4321"}`); rec.Code != http.StatusUnauthorized || code(rec) != "wrong_pin" {
		t.Fatalf("a call-center PIN as a manager: %d %s", rec.Code, rec.Body)
	}
}

// POST /api/terminal/orders/{id}/reprint, from a store terminal.
func TestTerminalReprintRoute(t *testing.T) {
	r := newCCRig(t)
	id := "0199c0de-0000-7000-8000-000000000001"
	if rec := call(r.h, "POST", "/api/terminal/orders/"+id+"/reprint", r.termKey, ""); rec.Code != http.StatusUnauthorized || code(rec) != "login_required" {
		t.Fatalf("nobody logged in: %d %s", rec.Code, rec.Body)
	}
	if rec := call(r.h, "POST", "/api/terminal/login", r.termKey, `{"pin":"1234"}`); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	if rec := call(r.h, "POST", "/api/terminal/orders/"+id+"/reprint", r.termKey, ""); rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"ok":true,"printed_on":"Front"}` {
		t.Fatalf("reprint: %d %s", rec.Code, rec.Body)
	}
	for want, err := range map[string]error{
		"404 not_found":            order.ErrNotFound,
		"503 printing_unavailable": orderApp.ErrPrintingUnavailable,
		"502 nothing_printed":      fmt.Errorf("%w: every printer is down", orderApp.ErrKitchenNotPrinted),
	} {
		r.floor.mu.Lock()
		r.floor.reprint = err
		r.floor.mu.Unlock()
		rec := call(r.h, "POST", "/api/terminal/orders/"+id+"/reprint", r.termKey, "")
		if got := fmt.Sprintf("%d %s", rec.Code, code(rec)); got != want {
			t.Errorf("%v: %s, want %s", err, got, want)
		}
	}
	if len(r.floor.reprints) != 1 {
		t.Fatalf("printed once: %v", r.floor.reprints)
	}
}

// keyedMain answers square.keyed_payment like main (after wait).
type keyedMain struct{ wait time.Duration }

func (m keyedMain) Send(ctx context.Context, typ string, _ any) (int, json.RawMessage, error) {
	if typ != squareterm.MsgKeyed {
		return 202, nil, nil
	}
	select {
	case <-time.After(m.wait):
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	}
	raw, _ := json.Marshal(map[string]any{"ok": true, "payment": map[string]any{"id": "pay_1", "status": "COMPLETED", "brand": "VISA", "last4": "1111", "entry": "keyed"}})
	return 200, raw, nil
}

// A typed card's charge runs for the call center too (a terminal key is not
// needed on the relay), under the keyed deadline.
func TestRelayKeyedPayment(t *testing.T) {
	r := newCCRig(t)
	r.s.square = squareterm.New(func() squareterm.Sender { return keyedMain{} }, zerolog.Nop())
	body := map[string]any{"nonce": "cnon:ok", "amount_cents": 1777, "tip_cents": 300, "idempotency_key": "sq-cc-0001", "reference_id": "ck-1"}
	got := r.relay(t, ccAgent, "POST", "square/keyed-payments", body)
	if got.Status != 200 || !strings.Contains(string(got.Body), `"id":"pay_1"`) {
		t.Fatalf("keyed payment: %d %s", got.Status, got.Body)
	}
	if got := r.relay(t, ccCashier, "POST", "square/keyed-payments", body); got.Status != http.StatusUnauthorized {
		t.Fatalf("a cashier's id on the relay: %d %s", got.Status, got.Body)
	}
	journal, err := r.s.terminalSvc.Journal(context.Background(), 20)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range journal {
		if e.Action == terminal.ActionSquareKeyed {
			found = e.DeviceID == "callcenter:desk-2" && strings.Contains(string(e.Detail), `"staff_id":"cc-1"`)
		}
	}
	if !found {
		t.Fatalf("journal square.keyed with the seat + the agent: %+v", journal)
	}
	// The deadline: 10 s, 30 s for the card charge (under main's 12 s / 35 s).
	if relayDeadline != 10*time.Second || relayDeadlineKeyed != 30*time.Second {
		t.Fatalf("deadlines %s / %s", relayDeadline, relayDeadlineKeyed)
	}
	// Past it the reply is 503 timeout (the same idempotency key again is safe).
	old := relayDeadlineKeyed
	relayDeadlineKeyed = 40 * time.Millisecond
	defer func() { relayDeadlineKeyed = old }()
	r.s.square = squareterm.New(func() squareterm.Sender { return keyedMain{wait: 5 * time.Second} }, zerolog.Nop())
	start := time.Now()
	late := r.relay(t, ccAgent, "POST", "square/keyed-payments", body)
	if late.Status != http.StatusServiceUnavailable || late.code() != "timeout" || time.Since(start) > 3*time.Second {
		t.Fatalf("past the deadline: %d %s after %s", late.Status, late.Body, time.Since(start))
	}
}

// arriveOnline stores a placed online pickup with two own lines (a $12.00
// pie with a $1.00 topping, two $3.00 wings) and tax: paid online by card,
// or to pay at pickup.
func (r *ccRig) arriveOnline(t *testing.T, card bool) string {
	t.Helper()
	ctx := context.Background()
	items := []order.LineItem{
		{ID: uuid.NewString(), Name: "Cheese Pizza", Size: "Large", Quantity: 1, UnitPrice: 1200, LineTotal: 1300,
			Children: []order.LineItem{{ID: uuid.NewString(), Name: "Pepperoni", Quantity: 1, UnitPrice: 100, LineTotal: 100}}, Routing: order.RoutingMatched},
		{ID: uuid.NewString(), Name: "Wings", Quantity: 2, UnitPrice: 300, LineTotal: 600, Routing: order.RoutingMatched},
	}
	snap := order.Snapshot{ID: uuid.NewString(), TenantID: uuid.NewString(), LocationID: uuid.NewString(), Source: "online",
		SourceEventID: "V-" + uuid.NewString()[:6], PlacedAt: time.Now().UTC(), Fulfillment: order.Fulfillment{Type: "pickup"},
		Customer: order.Customer{DisplayName: "Sam", Phone: "4105550100"}, Items: items,
		Money: order.Money{Subtotal: 1900, Tax: 190, Total: 2090}}
	if card {
		snap.Payments = []order.PaymentSummary{{Method: "card", AmountCents: 2090, Last4: "1881"}}
	}
	if _, err := r.repo.UpsertArrived(ctx, snap); err != nil {
		t.Fatal(err)
	}
	got, err := r.repo.ClaimAccept(ctx, uuid.MustParse(snap.ID), 20, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.repo.RecordAccepted(ctx, *got); err != nil {
		t.Fatal(err)
	}
	return snap.ID
}

type relayedOrder struct {
	Order struct {
		Order struct {
			Items []struct {
				Name     string `json:"name"`
				Quantity int    `json:"quantity"`
			} `json:"items"`
			Money struct {
				Total int64 `json:"total_cents"`
			} `json:"money"`
			Meta struct {
				Terminal struct {
					Cart struct {
						Lines []map[string]any `json:"lines"`
					} `json:"cart"`
				} `json:"terminal"`
			} `json:"source_metadata"`
		} `json:"order"`
		Status struct {
			CashDue int64 `json:"cash_due_cents"`
		} `json:"status"`
	} `json:"order"`
}

func (r *ccRig) order(t *testing.T, id string) relayedOrder {
	t.Helper()
	got := r.relay(t, ccAgent, "GET", "orders/"+id, nil)
	var v relayedOrder
	if err := json.Unmarshal(got.Body, &v); err != nil || got.Status != 200 {
		t.Fatalf("GET orders/%s: %d %s", id, got.Status, got.Body)
	}
	return v
}

func ccKey() string { return "cc-" + uuid.NewString() }

// An online / website order edited from the call center (contract 2.10aa +
// 2.10ab): its own lines are custom lines the app sends back unchanged.
func TestRelayOnlineOrderEditedFromTheCallCenter(t *testing.T) {
	r := newCCRig(t)
	soda := map[string]any{"item_id": "00000000-0000-0000-0000-0000000000a6", "qty": 1}
	put := func(id string, lines ...map[string]any) relayed {
		return r.relay(t, ccAgent, "PUT", "orders/"+id+"/cart", map[string]any{"client_key": ccKey(), "cart": map[string]any{"lines": lines}})
	}
	with := func(l map[string]any, k string, v any) map[string]any {
		out := map[string]any{}
		for kk, vv := range l {
			out[kk] = vv
		}
		out[k] = v
		return out
	}

	// Paid online by card.
	paid := r.arriveOnline(t, true)
	was := r.order(t, paid)
	own := was.Order.Order.Meta.Terminal.Cart.Lines
	if len(own) != 2 || own[0]["custom"] != true || own[1]["custom"] != true || own[1]["name"] != "Wings" {
		t.Fatalf("the order's own lines are custom lines on the terminal: %+v", own)
	}
	// (i) the same lines back: no open_price refusal, nothing changes.
	if got := put(paid, own...); got.Status != 200 || !strings.Contains(string(got.Body), `"changed":false`) {
		t.Fatalf("its own lines back unchanged: %d %s", got.Status, got.Body)
	}
	// (iii) removing or changing one of its lines: paid_locked; a line renamed or re-priced is a new custom line.
	for name, c := range map[string]struct {
		lines []map[string]any
		want  string
	}{
		"a line removed":     {[]map[string]any{own[0]}, "409 paid_locked"},
		"a quantity lowered": {[]map[string]any{own[0], with(own[1], "qty", 1)}, "409 paid_locked"},
		"a note changed":     {[]map[string]any{with(own[0], "note", "no pepperoni"), own[1]}, "409 paid_locked"},
		"a line renamed":     {[]map[string]any{own[0], with(own[1], "name", "Boneless wings")}, "403 not_allowed_for_callcenter:open_price"},
		"a line re-priced":   {[]map[string]any{own[0], with(own[1], "price_cents", 1)}, "403 not_allowed_for_callcenter:open_price"},
	} {
		got := put(paid, c.lines...)
		if res := fmt.Sprintf("%d %s", got.Status, got.code()); res != c.want {
			t.Errorf("%s: %s, want %s", name, res, c.want)
		}
		if c.want == "409 paid_locked" && !strings.Contains(string(got.Body), `"lines":["`) {
			t.Errorf("%s: no lines in %s", name, got.Body)
		}
	}
	// (v) a NEW custom line is still refused.
	if got := put(paid, own[0], own[1], map[string]any{"custom": true, "name": "Extra", "qty": 1, "price_cents": 100}); got.Status != http.StatusForbidden || got.code() != "not_allowed_for_callcenter:open_price" {
		t.Fatalf("a new custom line: %d %s", got.Status, got.Body)
	}
	if now := r.order(t, paid); now.Order.Order.Money.Total != was.Order.Order.Money.Total || len(now.Order.Order.Items) != 2 || now.Order.Status.CashDue != 0 {
		t.Fatalf("a refused change changes nothing: %+v", now.Order)
	}
	// (ii) adding a menu item: at our price, the balance is the extra.
	added := put(paid, own[0], own[1], soda)
	var av struct {
		Added   []string `json:"added"`
		Removed []string `json:"removed"`
		Balance int64    `json:"balance_cents"`
		Order   struct {
			Order struct {
				Money struct {
					Total int64 `json:"total_cents"`
				} `json:"money"`
			} `json:"order"`
		} `json:"order"`
	}
	if err := json.Unmarshal(added.Body, &av); err != nil || added.Status != 200 || len(av.Added) != 1 || av.Added[0] != "Soda" || len(av.Removed) != 0 {
		t.Fatalf("adding a soda to a paid online order: %d %s", added.Status, added.Body)
	}
	if extra := av.Order.Order.Money.Total - was.Order.Order.Money.Total; extra <= 0 || av.Balance != extra {
		t.Fatalf("the balance is the extra: total %d -> %d, balance %d", was.Order.Order.Money.Total, av.Order.Order.Money.Total, av.Balance)
	}
	// The added soda is paid for: locked like the rest; more can still be added.
	if got := put(paid, own[0], own[1]); got.Status != http.StatusConflict || got.code() != "paid_locked" {
		t.Fatalf("taking the added soda off again: %d %s", got.Status, got.Body)
	}

	// (iv) pay at pickup (nothing paid): cashier rules, the seat may remove a line.
	unpaid := r.arriveOnline(t, false)
	lines := r.order(t, unpaid).Order.Order.Meta.Terminal.Cart.Lines
	if got := put(unpaid, lines[0]); got.Status != 200 || !strings.Contains(string(got.Body), `"removed":["2× Wings"]`) {
		t.Fatalf("a line off an unpaid online order: %d %s", got.Status, got.Body)
	}
	if got := put(unpaid, lines[0], map[string]any{"custom": true, "name": "Extra", "qty": 1, "price_cents": 100}); got.code() != "not_allowed_for_callcenter:open_price" {
		t.Fatalf("a new custom line on an unpaid one: %d %s", got.Status, got.Body)
	}
	// Once a typed card is on it, its lines are locked too.
	if got := r.relay(t, ccAgent, "POST", "orders/"+unpaid+"/payments", map[string]any{"client_key": ccKey(), "tender": "manual_card", "amount_cents": 500, "last4": "1881"}); got.Status != 200 {
		t.Fatalf("a typed card on an online pickup: %d %s", got.Status, got.Body)
	}
	if got := put(unpaid, with(lines[0], "note", "well done")); got.Status != http.StatusConflict || got.code() != "paid_locked" {
		t.Fatalf("a change after a payment: %d %s", got.Status, got.Body)
	}
}

// The edit lock on a sent order, on the relay path: a seat's draft is hidden
// from the store's terminals; the store wins by opening the order; while it
// has it open the seat can't change, cancel, pay or re-time it.
func TestRelayEditLockTheStoreWins(t *testing.T) {
	r := newCCRig(t)
	if rec := call(r.h, "POST", "/api/terminal/login", r.termKey, `{"pin":"1234"}`); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	placed := r.relay(t, ccAgent, "POST", "orders", ccOrder(ccKey()))
	var pv struct {
		Order struct {
			Order struct {
				ID string `json:"id"`
			} `json:"order"`
		} `json:"order"`
	}
	if err := json.Unmarshal(placed.Body, &pv); err != nil || placed.Status != http.StatusCreated {
		t.Fatalf("place: %d %s", placed.Status, placed.Body)
	}
	x := pv.Order.Order.ID
	lock := map[string]any{"type": "pickup", "name": "Ada Test", "held": false, "order_id": x, "draft": map[string]any{"cc": map[string]any{"step": "menu"}}}
	if got := r.relay(t, ccAgent, "PUT", "drafts/d-cc-lock-01", lock); got.Status != 200 {
		t.Fatalf("the seat's lock: %d %s", got.Status, got.Body)
	}
	// The terminal (and the Pi web page) don't list it; the seat does.
	for name, rec := range map[string]*httptest.ResponseRecorder{"terminal": call(r.h, "GET", "/api/terminal/drafts", r.termKey, ""),
		"web": webCall(r.s, r.h, "GET", "/api/terminal/drafts", "")} {
		if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"drafts":[]}` {
			t.Fatalf("%s lists a seat's draft: %d %s", name, rec.Code, rec.Body)
		}
	}
	if got := r.relay(t, ccAgent, "GET", "drafts", nil); !strings.Contains(string(got.Body), `"id":"d-cc-lock-01"`) || !strings.Contains(string(got.Body), `"cc":{"step":"menu"}`) {
		t.Fatalf("the seat sees its lock: %s", got.Body)
	}
	// T1 opens X with its own draft: OK, the seat's is gone.
	mine := `{"type":"pickup","name":"Ada Test","held":false,"order_id":"` + x + `","draft":{"lines":[{"item_id":"x"}]}}`
	if rec := call(r.h, "PUT", "/api/terminal/drafts/d-till-edit1", r.termKey, mine); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"device_id":"term-1"`) {
		t.Fatalf("the store opens X: %d %s", rec.Code, rec.Body)
	}
	if got := r.relay(t, ccAgent, "GET", "drafts", nil); strings.Contains(string(got.Body), "d-cc-lock-01") || !strings.Contains(string(got.Body), "d-till-edit1") {
		t.Fatalf("the seat's draft is gone, the store's is there: %s", got.Body)
	}
	// The seat's late calls on X: open_elsewhere {holder}; X untouched.
	late := map[string]relayed{
		"cart":     r.relay(t, ccAgent, "PUT", "orders/"+x+"/cart", map[string]any{"client_key": ccKey(), "draft_id": "d-cc-lock-01", "cart": map[string]any{"lines": []any{}}}),
		"void":     r.relay(t, ccAgent, "POST", "orders/"+x+"/void", map[string]any{"reason": "customer called back"}),
		"payments": r.relay(t, ccAgent, "POST", "orders/"+x+"/payments", map[string]any{"client_key": ccKey(), "tender": "manual_card", "amount_cents": 100}),
		"time":     r.relay(t, ccAgent, "POST", "orders/"+x+"/time", map[string]any{"ready_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}),
		"lock":     r.relay(t, ccAgent, "PUT", "drafts/d-cc-lock-02", lock),
		"lock + take_over": r.relay(t, ccAgent, "PUT", "drafts/d-cc-lock-02", func() map[string]any {
			m := map[string]any{"take_over": true}
			for k, v := range lock {
				m[k] = v
			}
			return m
		}()),
	}
	for name, got := range late {
		if got.Status != http.StatusConflict || got.code() != "open_elsewhere" || !strings.Contains(string(got.Body), `"holder":{"device_id":"term-1"`) {
			t.Errorf("the seat's late %s: %d %s", name, got.Status, got.Body)
		}
	}
	rec := call(r.h, "GET", "/api/terminal/orders/"+x, r.termKey, "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"state":"accepted"`) || !strings.Contains(rec.Body.String(), `"payments":[]`) ||
		!strings.Contains(rec.Body.String(), `"total_cents":1377`) {
		t.Fatalf("X is untouched: %d %.300s", rec.Code, rec.Body)
	}
	// Reads stay open to the seat; once the store lets go, so do the writes.
	if got := r.relay(t, ccAgent, "GET", "orders/"+x, nil); got.Status != 200 {
		t.Fatalf("GET while the store has it open: %d", got.Status)
	}
	if rec := call(r.h, "DELETE", "/api/terminal/drafts/d-till-edit1", r.termKey, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("the store lets go: %d %s", rec.Code, rec.Body)
	}
	if got := r.relay(t, ccAgent, "PUT", "drafts/d-cc-lock-02", lock); got.Status != 200 {
		t.Fatalf("the seat locks X again: %d %s", got.Status, got.Body)
	}
	// The reverse by id: a seat can never take the store's draft over.
	if rec := call(r.h, "PUT", "/api/terminal/drafts/d-till-new01", r.termKey, `{"type":"counter","name":"Sam","held":false,"draft":{}}`); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	if got := r.relay(t, ccAgent, "PUT", "drafts/d-till-new01", map[string]any{"type": "counter", "take_over": true, "draft": map[string]any{}}); got.Status != http.StatusConflict || got.code() != "open_elsewhere" {
		t.Fatalf("a seat's take_over of the store's draft: %d %s", got.Status, got.Body)
	}
}

// A kept price from a seat is never a price (the relay path).
func TestRelayRungIsNeverAPrice(t *testing.T) {
	r := newCCRig(t)
	q := r.relay(t, ccAgent, "POST", "quote", map[string]any{"type": "pickup", "cart": map[string]any{"lines": []map[string]any{{"item_id": ccCheese, "size_id": ccXL, "qty": 1}}}})
	var qv struct {
		Quote struct {
			Lines []struct {
				Unit int64  `json:"unit_cents"`
				Name string `json:"name"`
			} `json:"lines"`
		} `json:"quote"`
	}
	if err := json.Unmarshal(q.Body, &qv); err != nil || q.Status != 200 || len(qv.Quote.Lines) != 1 || qv.Quote.Lines[0].Unit <= 0 {
		t.Fatalf("quote: %d %s", q.Status, q.Body)
	}
	unit := qv.Quote.Lines[0].Unit
	body := func(rung int64) map[string]any {
		return map[string]any{"client_key": ccKey(), "type": "pickup", "cart": map[string]any{"lines": []map[string]any{
			{"item_id": ccCheese, "size_id": ccXL, "qty": 1, "rung_cents": rung, "rung_name": qv.Quote.Lines[0].Name}}}}
	}
	for _, rung := range []int64{1, unit - 1, unit + 1} {
		if got := r.relay(t, ccAgent, "POST", "orders", body(rung)); got.Status != http.StatusForbidden || got.code() != "not_allowed_for_callcenter:price_change" {
			t.Errorf("rung %d (today's price is %d): %d %s", rung, unit, got.Status, got.Body)
		}
	}
	if got := r.relay(t, ccAgent, "POST", "orders", body(unit)); got.Status != http.StatusCreated {
		t.Fatalf("the quote's own price back: %d %s", got.Status, got.Body)
	}
	if got := r.relay(t, ccAgent, "GET", "orders", nil); strings.Count(string(got.Body), `"taken_by"`) != 1 {
		t.Fatalf("one order placed: %.200s", got.Body)
	}
}

// addressAt saves an address for a new customer through the relay and puts
// its point mi miles from the store (as a located delivery would).
func (r *ccRig) addressAt(t *testing.T, phone, line1 string, mi float64) map[string]any {
	t.Helper()
	ad := map[string]any{"line1": line1, "city": "Towson", "zip": "21204"}
	got := r.relay(t, ccAgent, "POST", "customers", map[string]any{"phone": phone, "first_name": "Ada", "last_name": "Test", "address": ad})
	var cv struct {
		Customer struct {
			ID string `json:"id"`
		} `json:"customer"`
		AddressID string `json:"address_id"`
	}
	if err := json.Unmarshal(got.Body, &cv); err != nil || got.Status != http.StatusCreated || cv.AddressID == "" {
		t.Fatalf("add a customer: %d %s", got.Status, got.Body)
	}
	if mi > 0 {
		if err := r.repo.SetCustomerAddressPoint(context.Background(), cv.AddressID, 39.4+mi/69.0, -76.6); err != nil {
			t.Fatal(err)
		}
	}
	return map[string]any{"id": cv.Customer.ID, "name": "Ada Test", "phone": phone, "address": ad}
}

// A call-center delivery outside the store's area (the owner, 2026-11-12):
// allowed with a warning, no manager's PIN - 409 out_of_area until the body
// says out_of_area_ack: true, then placed, marked and journaled.
func TestRelayOutOfArea(t *testing.T) {
	r := newCCRig(t)
	far := r.addressAt(t, "4105550101", "9 Far Rd", 5)
	near := r.addressAt(t, "4105550102", "7 Near Ln", 3.5)
	in := r.addressAt(t, "4105550103", "1 Test St", 2)
	fresh := r.addressAt(t, "4105550104", "5 New Rd", 0) // no point yet
	delivery := func(cust map[string]any, extra map[string]any) map[string]any {
		b := map[string]any{"client_key": ccKey(), "type": "delivery", "customer": cust,
			"cart": map[string]any{"lines": []map[string]any{{"item_id": ccCheese, "size_id": ccXL, "qty": 1}}}}
		for k, v := range extra {
			b[k] = v
		}
		return b
	}
	type placedOrder struct {
		Order struct {
			Order struct {
				ID        string `json:"id"`
				OutOfArea bool   `json:"out_of_area"`
			} `json:"order"`
		} `json:"order"`
	}
	idOf := func(got relayed) (string, bool) {
		var v placedOrder
		_ = json.Unmarshal(got.Body, &v)
		return v.Order.Order.ID, v.Order.Order.OutOfArea
	}
	// The lookup tells the app where each address is (what the warning is built from).
	if got := r.relay(t, ccAgent, "GET", "customers", nil, func(p map[string]any) { p["query"] = "phone=4105550101" }); !strings.Contains(string(got.Body), `"area":{"state":"out","distance_mi":5}`) {
		t.Fatalf("the lookup's area: %s", got.Body)
	}
	// The quote of a delivery says where the address is, and is never refused.
	q := r.relay(t, ccAgent, "POST", "quote", map[string]any{"type": "delivery", "customer": far, "cart": map[string]any{"lines": []map[string]any{{"item_id": ccCheese, "size_id": ccXL, "qty": 1}}}})
	if q.Status != 200 || !strings.Contains(string(q.Body), `"area":{"distance_mi":5,"state":"out"}`) && !strings.Contains(string(q.Body), `"area":{"state":"out","distance_mi":5}`) ||
		!strings.Contains(string(q.Body), `"quote":{`) {
		t.Fatalf("quote with the address: %d %s", q.Status, q.Body)
	}
	if q := r.relay(t, ccAgent, "POST", "quote", map[string]any{"type": "delivery", "cart": map[string]any{"lines": []any{}}}); strings.Contains(string(q.Body), `"area"`) {
		t.Fatalf("a quote without an address has no area: %s", q.Body)
	}
	// Out of the area without the ack: 409 with the numbers; nothing placed.
	for name, extra := range map[string]map[string]any{"no ack": nil, "ack false": {"out_of_area_ack": false}} {
		got := r.relay(t, ccAgent, "POST", "orders", delivery(far, extra))
		var ov struct {
			Code string `json:"code"`
			Area struct {
				State      string  `json:"state"`
				DistanceMi float64 `json:"distance_mi"`
			} `json:"area"`
			RadiusMi float64 `json:"radius_mi"`
			MarginMi float64 `json:"margin_mi"`
		}
		if err := json.Unmarshal(got.Body, &ov); err != nil || got.Status != http.StatusConflict || ov.Code != "out_of_area" || ov.Area.State != "out" ||
			ov.Area.DistanceMi != 5 || ov.RadiusMi != 3 || ov.MarginMi != 1 {
			t.Fatalf("%s: %d %s", name, got.Status, got.Body)
		}
	}
	if got := r.relay(t, ccAgent, "POST", "orders", delivery(far, map[string]any{"out_of_area_ack": "yes"})); got.Status != http.StatusBadRequest {
		t.Fatalf("an ack that isn't a boolean: %d %s", got.Status, got.Body)
	}
	if got := r.relay(t, ccAgent, "GET", "orders", nil); string(got.Body) != "[]" || len(r.floor.receipts) != 0 {
		t.Fatalf("nothing placed: %s", got.Body)
	}
	// With the ack: placed, marked, journaled under the agent, on the checks row.
	acked := r.relay(t, ccAgent, "POST", "orders", delivery(far, map[string]any{"out_of_area_ack": true}))
	farID, marked := idOf(acked)
	if acked.Status != http.StatusCreated || !marked {
		t.Fatalf("with the ack: %d %s", acked.Status, acked.Body)
	}
	journal, _ := r.s.terminalSvc.Journal(context.Background(), 50)
	var entry *terminal.Entry
	for i := range journal {
		if journal[i].Action == terminal.ActionOutOfArea {
			entry = &journal[i]
		}
	}
	if entry == nil || entry.StaffID != ccAgent || entry.DeviceID != "callcenter:desk-2" || entry.OrderID != farID ||
		!strings.Contains(string(entry.Detail), `"acked":true`) || !strings.Contains(string(entry.Detail), `"distance_mi":5`) {
		t.Fatalf("journal delivery.out_of_area: %+v", entry)
	}
	// In the area, near it (warn only) and with no point yet: placed as today, no ack, no mark.
	ids := map[string]string{}
	for name, cust := range map[string]map[string]any{"in": in, "near": near, "unknown": fresh} {
		got := r.relay(t, ccAgent, "POST", "orders", delivery(cust, nil))
		id, marked := idOf(got)
		if got.Status != http.StatusCreated || marked {
			t.Fatalf("%s: %d %s", name, got.Status, got.Body)
		}
		ids[name] = id
	}
	// A pickup needs no check, whatever address rides with the customer.
	if got := r.relay(t, ccAgent, "POST", "orders", map[string]any{"client_key": ccKey(), "type": "pickup", "customer": far,
		"cart": map[string]any{"lines": []map[string]any{{"item_id": ccCheese, "size_id": ccXL, "qty": 1}}}}); got.Status != http.StatusCreated {
		t.Fatalf("a pickup: %d %s", got.Status, got.Body)
	}
	// A change of address to one out of the area: 409 without the ack (nothing changes), marked with it;
	// back to an address in the area the mark comes off; the same address again asks nothing.
	change := func(id string, cust map[string]any, extra map[string]any) relayed {
		b := delivery(cust, extra)
		delete(b, "type")
		return r.relay(t, ccAgent, "PUT", "orders/"+id+"/cart", b)
	}
	if got := change(ids["in"], far, nil); got.Status != http.StatusConflict || got.code() != "out_of_area" {
		t.Fatalf("a new address out of the area, no ack: %d %s", got.Status, got.Body)
	}
	if got := r.relay(t, ccAgent, "GET", "orders/"+ids["in"], nil); !strings.Contains(string(got.Body), `"line1":"1 Test St"`) || strings.Contains(string(got.Body), `"out_of_area":true`) {
		t.Fatalf("the refused change changed nothing: %.300s", got.Body)
	}
	moved := change(ids["in"], far, map[string]any{"out_of_area_ack": true})
	if _, marked := idOf(moved); moved.Status != 200 || !marked || !strings.Contains(string(moved.Body), `"line1":"9 Far Rd"`) {
		t.Fatalf("the address changed with the ack: %d %s", moved.Status, moved.Body)
	}
	if same := change(ids["in"], far, nil); same.Status != 200 {
		t.Fatalf("the same out-of-area address again needs no new ack: %d %s", same.Status, same.Body)
	}
	back := change(ids["in"], in, nil)
	if _, marked := idOf(back); back.Status != 200 || marked {
		t.Fatalf("back inside the area: %d %s", back.Status, back.Body)
	}
	// A pickup made a delivery to an out-of-area address: the same rule.
	pick := r.relay(t, ccAgent, "POST", "orders", ccOrder(ccKey()))
	pid, _ := idOf(pick)
	toDelivery := delivery(far, nil)
	if got := r.relay(t, ccAgent, "PUT", "orders/"+pid+"/cart", toDelivery); got.Status != http.StatusConflict || got.code() != "out_of_area" {
		t.Fatalf("pickup -> delivery out of the area: %d %s", got.Status, got.Body)
	}
	toDelivery["out_of_area_ack"], toDelivery["client_key"] = true, ccKey()
	if got := r.relay(t, ccAgent, "PUT", "orders/"+pid+"/cart", toDelivery); got.Status != 200 || !strings.Contains(string(got.Body), `"out_of_area":true`) {
		t.Fatalf("pickup -> delivery with the ack: %d %s", got.Status, got.Body)
	}
	// The checks rows: the two out-of-area deliveries are marked for the store.
	checks := r.relay(t, ccAgent, "GET", "checks", nil, func(p map[string]any) { p["query"] = "batch=current&limit=50" })
	var cl struct {
		Checks []struct {
			OrderID   string `json:"order_id"`
			OutOfArea bool   `json:"out_of_area"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(checks.Body, &cl); err != nil || checks.Status != 200 {
		t.Fatalf("checks: %d %s", checks.Status, checks.Body)
	}
	for _, c := range cl.Checks {
		if want := c.OrderID == farID || c.OrderID == pid; c.OutOfArea != want {
			t.Errorf("check %s: out_of_area %v, want %v", c.OrderID, c.OutOfArea, want)
		}
	}
	// A store terminal's rule stays its own (a manager's PIN in the app): the Pi neither refuses nor marks, whatever out_of_area_ack says.
	if rec := call(r.h, "POST", "/api/terminal/login", r.termKey, `{"pin":"1234"}`); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	for _, ack := range []bool{false, true} {
		b := delivery(far, map[string]any{"out_of_area_ack": ack})
		raw, _ := json.Marshal(b)
		rec := call(r.h, "POST", "/api/terminal/orders", r.termKey, string(raw))
		if rec.Code != http.StatusCreated || strings.Contains(rec.Body.String(), `"out_of_area":true`) {
			t.Fatalf("a store terminal, ack %v: %d %s", ack, rec.Code, rec.Body)
		}
	}
}
