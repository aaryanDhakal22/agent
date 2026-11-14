package squareterm

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// Batch J (contract 2.10x, main 2.5e): square.general_refund and
// square.card_payments over the channel.
func TestGeneralRefundAndCardPaymentsOverTheChannel(t *testing.T) {
	var answer func(typ string, payload any) (int, any, error)
	f := &fakeMain{fn: func(typ string, payload any) (int, any, error) { return answer(typ, payload) }}
	s := svc(f)
	ctx := context.Background()
	answer = func(typ string, payload any) (int, any, error) {
		p := payload.(map[string]any)
		if typ != MsgGeneralRefund || p["nonce"] != "cnon:x" || p["idempotency_key"] != GeneralRefundKey("g-1") || p["amount_cents"] != int64(500) {
			t.Fatalf("sent %s %v", typ, p)
		}
		return 200, map[string]any{"ok": true, "refund_id": "ref_1", "status": "PENDING", "brand": "VISA", "last4": "1111", "unlinked": true}, nil
	}
	res, err := s.GeneralRefund(ctx, "cnon:x", 500, GeneralRefundKey("g-1"), "why")
	if err != nil || res.RefundID != "ref_1" || res.Brand != "VISA" || res.Last4 != "1111" {
		t.Fatalf("refund %+v %v", res, err)
	}
	if k := GeneralRefundKey("g-1"); len(k) > 45 || k != GeneralRefundKey("g-1") || k == GeneralRefundKey("g-2") {
		t.Fatalf("key %q", k)
	}
	// Square refuses unlinked refunds for this store: a 402 the terminal turns into the fallback.
	answer = func(string, any) (int, any, error) {
		return 200, map[string]any{"ok": false, "code": CodeUnlinkedUnsupported, "error": "no"}, nil
	}
	var ref *Refused
	if _, err := s.GeneralRefund(ctx, "cnon:x", 500, "k", ""); !errors.As(err, &ref) || ref.HTTPStatus() != http.StatusPaymentRequired || ref.Code != CodeUnlinkedUnsupported {
		t.Fatalf("unsupported: %v", err)
	}
	// main's 500 (Square away): not known.
	answer = func(string, any) (int, any, error) { return 500, map[string]any{}, nil }
	if _, err := s.GeneralRefund(ctx, "cnon:x", 500, "k", ""); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("500: %v", err)
	}
	// An answer with no refund id is not a refund.
	answer = func(string, any) (int, any, error) { return 200, map[string]any{"ok": true}, nil }
	if _, err := s.GeneralRefund(ctx, "cnon:x", 500, "k", ""); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("no id: %v", err)
	}
	if _, err := s.GeneralRefund(ctx, "", 500, "k", ""); !errors.As(err, &ref) || ref.Code != "bad_request" {
		t.Fatalf("no nonce: %v", err)
	}
	// The fallback's list.
	answer = func(typ string, payload any) (int, any, error) {
		if typ != MsgCardPayments {
			t.Fatalf("sent %s", typ)
		}
		return 200, map[string]any{"ok": true, "payments": []map[string]any{{"square_payment_id": "pay_1", "amount_cents": 2000,
			"refundable_cents": 2000, "brand": "VISA", "last4": "1111", "entry": "keyed", "at": "2026-11-14T12:00:00Z"}}}, nil
	}
	list, err := s.CardPayments(ctx, "1111", 500)
	if err != nil || len(list) != 1 || list[0].SquarePaymentID != "pay_1" || list[0].RefundableCents != 2000 || list[0].At.IsZero() {
		t.Fatalf("list %+v %v", list, err)
	}
	if _, err := s.CardPayments(ctx, "11a1", 500); !errors.As(err, &ref) {
		t.Fatalf("bad last4: %v", err)
	}
}
