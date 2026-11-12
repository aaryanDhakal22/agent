package phonepay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	terminalApp "quiccpos/agent/internal/application/terminal"
	"quiccpos/agent/internal/domain/device"
	"quiccpos/agent/internal/domain/order"
	pp "quiccpos/agent/internal/domain/phonepay"
	"quiccpos/agent/internal/domain/terminal"
	"quiccpos/agent/internal/infra/database"
	"quiccpos/agent/internal/infra/database/repositories"
	"quiccpos/agent/internal/migrate"

	"github.com/rs/zerolog"
)

// ---- fakes -------------------------------------------------------------------

type fakeTerm struct {
	mu        sync.Mutex
	loggedIn  bool
	due       int64
	state     order.State
	payments  map[string]terminal.Payment // client_key -> payment
	recordErr error
	journal   []map[string]any
}

func (f *fakeTerm) CurrentStaff(a terminalApp.Actor) (terminal.Staff, error) {
	if !f.loggedIn {
		return terminal.Staff{}, terminal.ErrLoginRequired
	}
	return terminal.Staff{ID: "cash-1", FirstName: "Cara", LastName: "Smith", Role: terminal.RoleCashier}, nil
}

func (f *fakeTerm) RequireManager(context.Context, terminalApp.Actor, string, []string, string) (terminal.Staff, error) {
	return terminal.Staff{}, &terminal.ManagerRequired{Action: terminalApp.ActionPairPhone}
}

func (f *fakeTerm) Order(_ context.Context, id string) (*terminalApp.OrderView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id != orderID {
		return nil, order.ErrNotFound
	}
	n := int64(12)
	so := order.StoredOrder{Order: order.Snapshot{ID: id, Source: order.SourceCounter}}
	so.Status.State, so.Status.KitchenNumber, so.Status.CashDueCents = f.state, &n, f.due
	pays := []terminal.Payment{}
	for _, p := range f.payments {
		pays = append(pays, p)
	}
	return &terminalApp.OrderView{Order: so, Payments: pays}, nil
}

func (f *fakeTerm) RecordAs(_ context.Context, a terminalApp.Actor, staffID, orderID string, in terminalApp.PaymentIn) (*terminalApp.PaymentResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.recordErr != nil {
		return nil, f.recordErr
	}
	if p, ok := f.payments[in.ClientKey]; ok {
		return &terminalApp.PaymentResult{Payment: p, Duplicate: true}, nil
	}
	if in.AmountCents > f.due {
		return nil, terminal.ErrOverBalance
	}
	p := terminal.Payment{ID: fmt.Sprintf("pay-%d", len(f.payments)+1), OrderID: orderID, ClientKey: in.ClientKey, Tender: in.Tender,
		AmountCents: in.AmountCents, TipCents: in.TipCents, Ref: in.Ref, Last4: in.Last4, Brand: in.Brand, StaffID: staffID, DeviceID: a.DeviceID}
	f.payments[in.ClientKey] = p
	f.due -= in.AmountCents
	return &terminalApp.PaymentResult{Payment: p}, nil
}

func (f *fakeTerm) RecordPi(_ context.Context, _, action string, d map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d["action"] = action
	f.journal = append(f.journal, d)
}

func (f *fakeTerm) Store() *terminalApp.Store { return nil }

func (f *fakeTerm) statuses() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, j := range f.journal {
		out = append(out, j["status"].(string))
	}
	return out
}

type fakeDevices struct{ list []device.Device }

func (d *fakeDevices) List(context.Context) ([]device.Device, error) { return d.list, nil }

type fakeRPC struct {
	mu    sync.Mutex
	calls []string
	reply map[string]func(payload map[string]any) (int, any, error)
}

func (r *fakeRPC) Send(_ context.Context, msg string, payload any) (int, json.RawMessage, error) {
	r.mu.Lock()
	r.calls = append(r.calls, msg)
	fn := r.reply[msg]
	r.mu.Unlock()
	if fn == nil {
		return http.StatusAccepted, nil, nil // an older main
	}
	b, _ := json.Marshal(payload)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	st, v, err := fn(m)
	out, _ := json.Marshal(v)
	return st, out, err
}

const orderID = "0193a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b"

var till = terminalApp.Actor{DeviceID: "term-1", DeviceName: "Counter 1"}

type rig struct {
	svc  *Service
	term *fakeTerm
	rpc  *fakeRPC
	now  time.Time
	pub  []string
	mu   sync.Mutex
	ctx  context.Context
}

func newRig(t *testing.T, appEnv string) *rig {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agent.db")
	if err := migrate.Run(ctx, path); err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	r := &rig{ctx: ctx, now: time.Date(2026, 11, 12, 18, 0, 0, 0, time.UTC),
		term: &fakeTerm{loggedIn: true, due: 2000, state: order.StateAccepted, payments: map[string]terminal.Payment{}},
		rpc: &fakeRPC{reply: map[string]func(map[string]any) (int, any, error){
			MsgPosConfig: func(map[string]any) (int, any, error) {
				return 200, map[string]any{"app_id": "sq0idp-x", "environment": "production", "callback_url": "https://b.example/pay/back", "linked": true}, nil
			},
		}}}
	devs := &fakeDevices{list: []device.Device{{ID: "ph-1", Name: "Ali's phone", Kind: device.KindPhone}, {ID: "ph-2", Name: "Sam's phone", Kind: device.KindPhone},
		{ID: "tab-1", Name: "Kitchen", Kind: device.KindTablet}}}
	r.svc = New(Deps{Repo: repositories.NewPhoneChargeRepository(db), Term: r.term, Devices: devs, RPC: func() Sender { return r.rpc },
		Publish: func(typ string, v any) { r.mu.Lock(); r.pub = append(r.pub, typ); r.mu.Unlock() },
		AppEnv: appEnv, Logger: zerolog.Nop(), Now: func() time.Time { return r.now }})
	return r
}

func (r *rig) found(tip int64) {
	r.rpc.mu.Lock()
	defer r.rpc.mu.Unlock()
	r.rpc.reply[MsgPosLookup] = func(p map[string]any) (int, any, error) {
		return 200, map[string]any{"found": true, "payment": map[string]any{"id": "sqpay-1", "transaction_id": "tx-1", "status": "COMPLETED",
			"brand": "VISA", "last4": "1111", "entry": "CONTACTLESS", "amount_cents": 1500 + tip, "tip_cents": tip, "total_cents": 1500 + tip}}, nil
	}
}

func (r *rig) notFound() {
	r.rpc.mu.Lock()
	defer r.rpc.mu.Unlock()
	r.rpc.reply[MsgPosLookup] = func(map[string]any) (int, any, error) { return 200, map[string]any{"found": false, "code": "not_found"}, nil }
}

func (r *rig) create(t *testing.T, key, phone string, amount, tip int64) pp.Charge {
	t.Helper()
	c, dup, err := r.svc.Create(r.ctx, till, orderID, CreateIn{ClientKey: key, PhoneID: phone, AmountCents: amount, TipCents: tip})
	if err != nil || dup {
		t.Fatalf("create: %+v dup=%v err=%v", c, dup, err)
	}
	return c
}

func refusalCode(err error) string {
	var ref *Refusal
	if errors.As(err, &ref) {
		return ref.Code
	}
	return fmt.Sprint(err)
}

// ---- tests -------------------------------------------------------------------

func TestCreateRules(t *testing.T) {
	r := newRig(t, "prod")
	sub, stop := r.svc.Subscribe("ph-1")
	defer stop()
	c := r.create(t, "ph-key-0001", "ph-1", 1500, 200)
	if c.Status != pp.StatusPending || c.Number != 12 || c.TotalCents() != 1700 || c.StaffID != "cash-1" || c.Note != "Order #12 "+c.Code {
		t.Fatalf("charge: %+v", c)
	}
	if got := <-sub; got.ID != c.ID {
		t.Fatalf("the phone got %+v", got)
	}
	if !c.ExpiresAt.Equal(r.now.Add(180 * time.Second)) {
		t.Fatalf("expires %v (default 180 s)", c.ExpiresAt)
	}
	// The same key + body = the same charge; another body = key_reused.
	again, dup, err := r.svc.Create(r.ctx, till, orderID, CreateIn{ClientKey: "ph-key-0001", PhoneID: "ph-1", AmountCents: 1500, TipCents: 200})
	if err != nil || !dup || again.ID != c.ID {
		t.Fatalf("replay: %+v %v %v", again, dup, err)
	}
	_, _, err = r.svc.Create(r.ctx, till, orderID, CreateIn{ClientKey: "ph-key-0001", PhoneID: "ph-1", AmountCents: 1400})
	if refusalCode(err) != "key_reused" {
		t.Fatalf("key reused: %v", err)
	}
	for _, tc := range []struct {
		in   CreateIn
		code string
	}{
		{CreateIn{ClientKey: "ph-key-0002", PhoneID: "ph-1", AmountCents: 500}, "phone_busy"},
		{CreateIn{ClientKey: "ph-key-0003", PhoneID: "ph-2", AmountCents: 2001}, "over_balance"},
		{CreateIn{ClientKey: "ph-key-0004", PhoneID: "tab-1", AmountCents: 500}, "unknown_phone"},
		{CreateIn{ClientKey: "ph-key-0005", PhoneID: "nope", AmountCents: 500}, "unknown_phone"},
		{CreateIn{ClientKey: "short", PhoneID: "ph-2", AmountCents: 500}, "bad_request"},
		{CreateIn{ClientKey: "", PhoneID: "ph-2", AmountCents: 500}, "client_key_required"},
		{CreateIn{ClientKey: "ph-key-0006", PhoneID: "ph-2", AmountCents: 0}, "bad_request"},
	} {
		if _, _, err := r.svc.Create(r.ctx, till, orderID, tc.in); refusalCode(err) != tc.code {
			t.Errorf("%+v: %v, want %s", tc.in, err, tc.code)
		}
	}
	r.term.loggedIn = false
	if _, _, err := r.svc.Create(r.ctx, till, orderID, CreateIn{ClientKey: "ph-key-0007", PhoneID: "ph-2", AmountCents: 100}); !errors.Is(err, terminal.ErrLoginRequired) {
		t.Fatalf("logged out: %v", err)
	}
	phones, err := r.svc.Phones(r.ctx)
	if err != nil || len(phones) != 2 || phones[0].BusyChargeID != c.ID || !phones[0].Online || phones[1].Online {
		t.Fatalf("phones: %+v %v", phones, err)
	}
}

func TestPaidOnceWithCardDetails(t *testing.T) {
	r := newRig(t, "prod")
	r.found(250) // the customer added $2.50 on Square's screen
	c := r.create(t, "ph-key-0101", "ph-1", 1500, 0)
	r.term.loggedIn = false // Cara's terminal locked meanwhile: the payment is still hers
	if got, err := r.svc.Open(r.ctx, "ph-1", c.ID); err != nil || got.Status != pp.StatusInApp || got.OpenedAt == nil {
		t.Fatalf("open: %+v %v", got, err)
	}
	if _, err := r.svc.Open(r.ctx, "ph-2", c.ID); refusalCode(err) != "not_found" {
		t.Fatalf("another phone's charge: %v", err)
	}
	ok := pp.Result{Status: "ok", TransactionID: "tx-1", ClientTransactionID: "ctx-1"}
	got, dup, err := r.svc.Result(r.ctx, "ph-1", c.ID, ok)
	if err != nil || dup || got.Status != pp.StatusPaid || got.Brand != "visa" || got.Last4 != "1111" || got.Entry != "contactless" ||
		got.SquarePaymentID != "sqpay-1" || got.ExtraTipCents != 250 || got.PaymentID == "" {
		t.Fatalf("paid: %+v dup=%v %v", got, dup, err)
	}
	p := r.term.payments[c.ClientKey]
	if p.Tender != terminal.TenderSquareApp || p.AmountCents != 1500 || p.TipCents != 250 || p.Ref != "sqpay-1" || p.StaffID != "cash-1" ||
		p.DeviceID != "term-1" || p.Brand != "visa" {
		t.Fatalf("payment: %+v", p)
	}
	// Square's callback replayed (a reload of /pay/back): nothing twice.
	again, dup, err := r.svc.Result(r.ctx, "ph-1", c.ID, ok)
	if err != nil || !dup || again.PaymentID != got.PaymentID || len(r.term.payments) != 1 {
		t.Fatalf("replay: %+v %v %v (%d payments)", again, dup, err, len(r.term.payments))
	}
	if _, _, err := r.svc.Result(r.ctx, "ph-1", c.ID, pp.Result{Status: "cancelled"}); refusalCode(err) != "charge_final" {
		t.Fatalf("cancel after paid: %v", err)
	}
	if _, err := r.svc.Cancel(r.ctx, "", c.ID, "terminal"); refusalCode(err) != "charge_final" {
		t.Fatalf("terminal cancel after paid: %v", err)
	}
	if st := r.term.statuses(); fmt.Sprint(st) != "[sent opened recorded]" {
		t.Fatalf("journal: %v", st)
	}
	// The phone is free again.
	r.term.due = 500
	r.term.loggedIn = true
	r.create(t, "ph-key-0102", "ph-1", 500, 0)
}

func TestDeclinedCancelledAndWords(t *testing.T) {
	r := newRig(t, "prod")
	c := r.create(t, "ph-key-0201", "ph-1", 1000, 0)
	got, _, err := r.svc.Result(r.ctx, "ph-1", c.ID, pp.Result{Status: "error", ErrorCode: "com.squareup.pos.ERROR_NO_NETWORK"})
	if err != nil || got.Status != pp.StatusDeclined || got.Message != "The phone has no internet: nothing was charged." {
		t.Fatalf("declined: %+v %v", got, err)
	}
	// Android's cancel code (with or without the ERROR_ prefix) = cancelled.
	c2 := r.create(t, "ph-key-0202", "ph-1", 1000, 0)
	got, _, err = r.svc.Result(r.ctx, "ph-1", c2.ID, pp.Result{Status: "error", ErrorCode: "com.squareup.pos.ERROR_TRANSACTION_CANCELED"})
	if err != nil || got.Status != pp.StatusCancelled {
		t.Fatalf("cancelled: %+v %v", got, err)
	}
	c3 := r.create(t, "ph-key-0203", "ph-1", 1000, 0)
	got, _, err = r.svc.Result(r.ctx, "ph-1", c3.ID, pp.Result{Status: "error", ErrorCode: "payment_canceled"}) // iOS
	if err != nil || got.Status != pp.StatusCancelled {
		t.Fatalf("ios cancelled: %+v %v", got, err)
	}
	if _, _, err := r.svc.Result(r.ctx, "ph-1", c3.ID, pp.Result{Status: "ok"}); refusalCode(err) != "bad_request" {
		t.Fatalf("ok without an id: %v", err)
	}
	if len(r.term.payments) != 0 {
		t.Fatalf("nothing is recorded: %+v", r.term.payments)
	}
}

func TestExpiredThenALateTapIsRecorded(t *testing.T) {
	r := newRig(t, "prod")
	r.notFound() // main can't find it: recorded from Square's word alone
	c := r.create(t, "ph-key-0301", "ph-1", 1200, 300)
	r.now = r.now.Add(179 * time.Second)
	r.svc.Sweep(r.ctx)
	if got, _ := r.svc.Get(r.ctx, c.ID); got.Status != pp.StatusPending {
		t.Fatalf("not yet: %s", got.Status)
	}
	r.now = r.now.Add(2 * time.Second)
	r.svc.Sweep(r.ctx)
	got, _ := r.svc.Get(r.ctx, c.ID)
	if got.Status != pp.StatusExpired || got.FinishedAt == nil {
		t.Fatalf("expired: %+v", got)
	}
	got, _, err := r.svc.Result(r.ctx, "ph-1", c.ID, pp.Result{Status: "ok", TransactionID: "tx-late"})
	if err != nil || got.Status != pp.StatusPaid {
		t.Fatalf("late tap: %+v %v", got, err)
	}
	p := r.term.payments[c.ClientKey]
	if p.AmountCents != 1200 || p.TipCents != 300 || p.Ref != "tx-late" || p.Last4 != "" {
		t.Fatalf("late payment: %+v", p)
	}
}

func TestUnrecordedThenRecordIt(t *testing.T) {
	r := newRig(t, "prod")
	r.found(0)
	c := r.create(t, "ph-key-0401", "ph-1", 1500, 0)
	r.term.recordErr = terminal.ErrOverBalance // paid in cash meanwhile
	got, _, err := r.svc.Result(r.ctx, "ph-1", c.ID, pp.Result{Status: "ok", TransactionID: "tx-1"})
	if err != nil || got.Status != pp.StatusUnrecorded || got.TransactionID != "tx-1" {
		t.Fatalf("unrecorded: %+v %v", got, err)
	}
	if p := r.svc.Problems(r.ctx); len(p) != 1 || p[0] != "phone_charge_unrecorded" {
		t.Fatalf("problems: %v", p)
	}
	if _, err := r.svc.Cancel(r.ctx, "", c.ID, "terminal"); refusalCode(err) != "charge_final" {
		t.Fatalf("cancel of money Square took: %v", err)
	}
	// The phone isn't blocked by it.
	r.term.recordErr = nil
	if phones, _ := r.svc.Phones(r.ctx); phones[0].BusyChargeID != "" {
		t.Fatalf("busy: %+v", phones[0])
	}
	got, err = r.svc.Record(r.ctx, c.ID)
	if err != nil || got.Status != pp.StatusPaid || len(r.term.payments) != 1 {
		t.Fatalf("record it: %+v %v", got, err)
	}
	if p := r.svc.Problems(r.ctx); len(p) != 0 {
		t.Fatalf("problems after: %v", p)
	}
}

func TestWentThroughLooksItUp(t *testing.T) {
	r := newRig(t, "prod")
	r.notFound()
	c := r.create(t, "ph-key-0501", "ph-1", 1500, 0)
	got, _, err := r.svc.Result(r.ctx, "ph-1", c.ID, pp.Result{Status: "went_through"})
	if err != nil || got.Status != pp.StatusUnrecorded || len(r.term.payments) != 0 {
		t.Fatalf("not in Square yet: %+v %v", got, err)
	}
	if _, err := r.svc.Record(r.ctx, c.ID); refusalCode(err) != "not_found_in_square" {
		t.Fatalf("record, not found: %v", err)
	}
	var asked map[string]any
	r.rpc.mu.Lock()
	r.rpc.reply[MsgPosLookup] = func(p map[string]any) (int, any, error) {
		asked = p
		return 200, map[string]any{"found": true, "payment": map[string]any{"id": "sqpay-9", "transaction_id": "tx-9", "brand": "MASTERCARD", "last4": "4444", "entry": "EMV"}}, nil
	}
	r.rpc.mu.Unlock()
	got, err = r.svc.Record(r.ctx, c.ID)
	if err != nil || got.Status != pp.StatusPaid || got.TransactionID != "tx-9" || got.Brand != "mastercard" || got.Entry != "chip" {
		t.Fatalf("record it: %+v %v", got, err)
	}
	if asked["code"] != c.Code || asked["total_cents"].(float64) != 1500 {
		t.Fatalf("looked up by %v", asked)
	}
	// Square unavailable (an older main) = 409 square_unavailable.
	r.term.due = 1500
	c2 := r.create(t, "ph-key-0502", "ph-1", 700, 0)
	r.rpc.mu.Lock()
	delete(r.rpc.reply, MsgPosLookup)
	r.rpc.mu.Unlock()
	if _, err := r.svc.Record(r.ctx, c2.ID); refusalCode(err) != "square_unavailable" {
		t.Fatalf("unavailable: %v", err)
	}
	// Cancelled by the terminal, then the phone's late cancel = the same.
	if got, err := r.svc.Cancel(r.ctx, "", c2.ID, "terminal"); err != nil || got.Status != pp.StatusCancelled {
		t.Fatalf("cancel: %+v %v", got, err)
	}
	if got, _, err := r.svc.Result(r.ctx, "ph-1", c2.ID, pp.Result{Status: "cancelled"}); err != nil || got.Status != pp.StatusCancelled {
		t.Fatalf("late cancel: %+v %v", got, err)
	}
}

func TestSquareModes(t *testing.T) {
	r := newRig(t, "prod")
	if sq := r.svc.Square(r.ctx); sq.Mode != ModeSquare || sq.AppID != "sq0idp-x" || sq.CallbackURL == "" {
		t.Fatalf("production: %+v", sq)
	}
	for _, tc := range []struct {
		env, mode, reason string
		reply             func(map[string]any) (int, any, error)
	}{
		{"prod", ModeOff, "main_unsupported", nil},
		{"dev", ModeFake, "main_unsupported", nil},
		{"staging", ModeFake, "no_sandbox", func(map[string]any) (int, any, error) {
			return 200, map[string]any{"app_id": "sandbox-x", "environment": "sandbox", "linked": true}, nil
		}},
		{"prod", ModeOff, "no_callback_url", func(map[string]any) (int, any, error) {
			return 200, map[string]any{"app_id": "sq0idp-x", "environment": "production", "linked": true}, nil
		}},
		{"prod", ModeOff, "square_not_linked", func(map[string]any) (int, any, error) {
			return 200, map[string]any{"app_id": "sq0idp-x", "environment": "production", "callback_url": "https://x/pay/back"}, nil
		}},
		{"prod", ModeOff, "main_unreachable", func(map[string]any) (int, any, error) { return 0, nil, errors.New("down") }},
	} {
		r := newRig(t, tc.env)
		r.rpc.reply[MsgPosConfig] = tc.reply
		if sq := r.svc.Square(r.ctx); sq.Mode != tc.mode || sq.Reason != tc.reason {
			t.Errorf("%s %s: %+v", tc.env, tc.reason, sq)
		}
		if tc.mode == ModeOff {
			if _, _, err := r.svc.Create(r.ctx, till, orderID, CreateIn{ClientKey: "ph-key-0601", PhoneID: "ph-1", AmountCents: 100}); refusalCode(err) != "square_off" {
				t.Errorf("%s: create while off: %v", tc.reason, err)
			}
		}
	}
	if newRig(t, "prod").svc.FakeAllowed() || !newRig(t, "staging").svc.FakeAllowed() {
		t.Fatal("the fake Square never runs on prod")
	}
}

func TestFakeChargeFallsBack(t *testing.T) {
	r := newRig(t, "dev")
	tx, client, viaMain := r.svc.FakeCharge(r.ctx, 1500, "Order #12 QP-1")
	if viaMain || tx == "" || client == "" {
		t.Fatalf("made up: %s %s %v", tx, client, viaMain)
	}
	r.rpc.reply[MsgPosFakeCharge] = func(p map[string]any) (int, any, error) {
		return 200, map[string]any{"ok": true, "transaction_id": "fakeorder-1", "client_transaction_id": "c-1"}, nil
	}
	if tx, _, viaMain := r.svc.FakeCharge(r.ctx, 1500, "x"); !viaMain || tx != "fakeorder-1" {
		t.Fatalf("via main: %s %v", tx, viaMain)
	}
}

func TestResultNormalize(t *testing.T) {
	for _, tc := range []struct {
		in   pp.Result
		want string
		bad  bool
	}{
		{pp.Result{Status: "ok", ClientTransactionID: "offline-1"}, "ok", false},
		{pp.Result{Status: "error", ErrorCode: "TRANSACTION_CANCELED"}, "cancelled", false},
		{pp.Result{Status: "error", ErrorCode: "com.squareup.pos.ERROR_USER_NOT_LOGGED_IN"}, "error", false},
		{pp.Result{Status: "maybe"}, "", true},
	} {
		got, err := tc.in.Normalize()
		if (err != nil) != tc.bad || (!tc.bad && got.Status != tc.want) {
			t.Errorf("%+v: %+v %v", tc.in, got, err)
		}
	}
	if w := pp.WordsFor("com.squareup.pos.ERROR_USER_NOT_LOGGED_IN"); w != "Square isn't signed in on this phone." {
		t.Fatal(w)
	}
}
