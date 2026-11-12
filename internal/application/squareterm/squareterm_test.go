package squareterm

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// The shared vector: the same file is the terminal's Kotlin test's
// (android/app/src/test/resources/square_session_vector.json).
func TestSharedSessionVector(t *testing.T) {
	raw, err := os.ReadFile("testdata/square_session_vector.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		DeviceKey      string `json:"device_key"`
		Info           string `json:"info"`
		KeyHex         string `json:"hkdf_key_hex"`
		AccessToken    string `json:"access_token"`
		Nonce          string `json:"nonce"`
		AccessTokenEnc string `json:"access_token_enc"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	if v.Info != SessionInfo {
		t.Fatalf("info %q, want %q", v.Info, SessionInfo)
	}
	if got := hex.EncodeToString(SessionKey(v.DeviceKey)); got != v.KeyHex {
		t.Fatalf("HKDF key %s, want %s", got, v.KeyHex)
	}
	nonce, _ := base64.StdEncoding.DecodeString(v.Nonce)
	ct, err := Seal(v.DeviceKey, nonce, v.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if got := base64.StdEncoding.EncodeToString(ct); got != v.AccessTokenEnc {
		t.Fatalf("sealed %s, want %s", got, v.AccessTokenEnc)
	}
	enc, _ := base64.StdEncoding.DecodeString(v.AccessTokenEnc)
	if plain, err := Open(v.DeviceKey, nonce, enc); err != nil || plain != v.AccessToken {
		t.Fatalf("open = %q, %v", plain, err)
	}
	// Another terminal's key can't open it.
	if _, err := Open(v.DeviceKey+"x", nonce, enc); err == nil {
		t.Fatal("another device key opened the session")
	}
}

func TestSealForUsesAFreshNonce(t *testing.T) {
	sess := Session{AccessToken: "EAAA-tok", LocationID: "L1", Environment: "sandbox"}
	a, err := SealFor(sess, "qagt_a")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := SealFor(sess, "qagt_a")
	if a.Nonce == b.Nonce || a.AccessTokenEnc == b.AccessTokenEnc {
		t.Fatal("two answers share a nonce")
	}
	n, _ := base64.StdEncoding.DecodeString(a.Nonce)
	ct, _ := base64.StdEncoding.DecodeString(a.AccessTokenEnc)
	if len(n) != 12 || len(ct) != len("EAAA-tok")+16 {
		t.Fatalf("nonce %d bytes, ciphertext %d", len(n), len(ct))
	}
	if p, err := Open("qagt_a", n, ct); err != nil || p != "EAAA-tok" {
		t.Fatalf("open = %q, %v", p, err)
	}
}

// fakeMain answers the RPCs.
type fakeMain struct {
	mu    sync.Mutex
	calls map[string]int
	fn    func(typ string, payload any) (int, any, error)
}

func (f *fakeMain) Send(_ context.Context, typ string, payload any) (int, json.RawMessage, error) {
	f.mu.Lock()
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[typ]++
	f.mu.Unlock()
	st, body, err := f.fn(typ, payload)
	if err != nil {
		return 0, nil, err
	}
	raw, _ := json.Marshal(body)
	return st, raw, nil
}

func (f *fakeMain) n(typ string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[typ]
}

func svc(f *fakeMain) *Service { return New(func() Sender { return f }, zerolog.Nop()) }

func TestSessionIsCachedInMemory(t *testing.T) {
	exp := time.Now().Add(30 * 24 * time.Hour).UTC()
	f := &fakeMain{fn: func(string, any) (int, any, error) {
		return 200, map[string]any{"ok": true, "access_token": "EAAA-1", "location_id": "L1", "environment": "fake", "expires_at": exp, "in_person": true}, nil
	}}
	s := svc(f)
	for i := 0; i < 3; i++ {
		got, err := s.Session(context.Background())
		if err != nil || got.AccessToken != "EAAA-1" || got.LocationID != "L1" {
			t.Fatalf("session = %+v, %v", got, err)
		}
	}
	if f.n(MsgSession) != 1 {
		t.Fatalf("main asked %d times, want 1 (cached)", f.n(MsgSession))
	}
	st := s.Status()
	if st == nil || !st.Linked || st.LocationID != "L1" || st.InPerson == nil || !*st.InPerson {
		t.Fatalf("status = %+v", st)
	}
	b, _ := json.Marshal(st)
	if strings.Contains(string(b), "EAAA") || strings.Contains(string(b), "token") {
		t.Fatalf("status leaks the token: %s", b)
	}
	s.now = func() time.Time { return time.Now().Add(sessionCacheFor + time.Minute) }
	if _, err := s.Session(context.Background()); err != nil || f.n(MsgSession) != 2 {
		t.Fatalf("after the cache: %v, %d asks", err, f.n(MsgSession))
	}
}

func TestAnswersSorted(t *testing.T) {
	for name, c := range map[string]struct {
		status int
		body   any
		err    error
		want   error
		code   string
		http   int
	}{
		"not linked":   {200, map[string]any{"ok": false, "code": "square_not_linked", "error": "no Square"}, nil, nil, "square_not_linked", 409},
		"reconnect":    {200, map[string]any{"ok": false, "code": "square_reconnect", "in_person": false}, nil, nil, "square_reconnect", 409},
		"older main":   {202, nil, nil, ErrUnsupported, "", 0},
		"main 500":     {500, map[string]any{"error": "handler failed"}, nil, ErrUnreachable, "", 0},
		"no answer":    {0, nil, errors.New("dial"), ErrUnreachable, "", 0},
		"unlinked Pi":  {401, nil, nil, ErrNotLinked, "", 0},
		"bad payload":  {400, map[string]any{"error": "bad rpc payload"}, nil, nil, "bad_request", 400},
		"session ok":   {200, map[string]any{"ok": true, "access_token": "t", "location_id": "L"}, nil, nil, "", 0},
		"empty answer": {200, map[string]any{"ok": true}, nil, nil, "square_not_linked", 409},
	} {
		f := &fakeMain{fn: func(string, any) (int, any, error) { return c.status, c.body, c.err }}
		_, err := svc(f).Session(context.Background())
		var ref *Refused
		switch {
		case c.code != "":
			if !errors.As(err, &ref) || ref.Code != c.code || ref.HTTPStatus() != c.http {
				t.Errorf("%s: %v, want refused %s (%d)", name, err, c.code, c.http)
			}
		case c.want != nil:
			if !errors.Is(err, c.want) {
				t.Errorf("%s: %v, want %v", name, err, c.want)
			}
		default:
			if err != nil {
				t.Errorf("%s: %v", name, err)
			}
		}
	}
	if _, err := New(nil, zerolog.Nop()).Session(context.Background()); !errors.Is(err, ErrNotLinked) {
		t.Errorf("no channel: %v", err)
	}
}

func TestKeyedAndRefund(t *testing.T) {
	var got map[string]any
	f := &fakeMain{fn: func(typ string, payload any) (int, any, error) {
		raw, _ := json.Marshal(payload)
		_ = json.Unmarshal(raw, &got)
		switch typ {
		case MsgKeyed:
			if got["nonce"] == "cnon:declined" {
				return 200, map[string]any{"ok": false, "code": "card_declined", "square_code": "CVV_FAILURE", "error": "CVV"}, nil
			}
			return 200, map[string]any{"ok": true, "payment": map[string]any{"id": "pay_1", "status": "COMPLETED", "brand": "VISA", "last4": "1111", "auth_code": "A1", "receipt_number": "pay1", "entry": "keyed"}}, nil
		case MsgRefund:
			return 200, map[string]any{"ok": true, "refund_id": "ref_1", "status": "PENDING"}, nil
		}
		return 202, nil, nil
	}}
	s := svc(f)
	p, err := s.Keyed(context.Background(), KeyedIn{Nonce: "cnon:ok", AmountCents: 1777, TipCents: 300, IdempotencyKey: "sq-1-k1", ReferenceID: "ck-1"})
	if err != nil || p.ID != "pay_1" || p.Last4 != "1111" || p.Entry != "keyed" {
		t.Fatalf("keyed = %+v, %v", p, err)
	}
	if got["amount_cents"].(float64) != 1777 || got["tip_cents"].(float64) != 300 || got["idempotency_key"] != "sq-1-k1" {
		t.Fatalf("sent %v", got)
	}
	_, err = s.Keyed(context.Background(), KeyedIn{Nonce: "cnon:declined", AmountCents: 100, IdempotencyKey: "k2"})
	var ref *Refused
	if !errors.As(err, &ref) || ref.HTTPStatus() != http.StatusPaymentRequired || ref.SquareCode != "CVV_FAILURE" {
		t.Fatalf("decline = %v", err)
	}
	for _, bad := range []KeyedIn{{AmountCents: 100, IdempotencyKey: "k"}, {Nonce: "n", IdempotencyKey: "k"},
		{Nonce: "n", AmountCents: 100}, {Nonce: "n", AmountCents: 100, IdempotencyKey: "0123456789012345678901234567890123456789012345"}} {
		if err := bad.Check(); !errors.As(err, &ref) || ref.HTTPStatus() != 400 {
			t.Errorf("%+v: %v", bad, err)
		}
	}
	r, err := s.Refund(context.Background(), "pay_1", 500, RefundKey("o1", "ck1"), "Wrong order")
	if err != nil || r.RefundID != "ref_1" || got["amount_cents"].(float64) != 500 {
		t.Fatalf("refund = %+v, %v (%v)", r, err, got)
	}
	k := RefundKey("o1", "ck1")
	if len(k) > 45 || k != RefundKey("o1", "ck1") || k == RefundKey("o1", "ck2") || k == RefundKey("o2", "ck1") {
		t.Fatalf("refund key %q", k)
	}
}
