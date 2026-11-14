package squareterm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"
	"time"
)

// A general refund to a card (batch J, the owner 2026-11-14; contract/agent.md
// §2.10x, main §2.5e): money back on a card with no order. The unlinked way
// hands main the token Square's card field gave the terminal
// (square.general_refund: Square's unlinked refund); the fallback lists the
// store's recent card payments with the card's last 4
// (square.card_payments) and refunds the one the manager picks with the
// ordinary square.refund. The token is never stored or logged.

// The agent → main RPCs.
const (
	MsgGeneralRefund = "square.general_refund"
	MsgCardPayments  = "square.card_payments"
)

// CodeUnlinkedUnsupported: Square makes no unlinked refunds for this store
// (the terminal offers the fallback).
const CodeUnlinkedUnsupported = "unlinked_unsupported"

// GeneralRefundResult is Square's unlinked refund.
type GeneralRefundResult struct {
	RefundID string
	Status   string
	Brand    string // Square's (VISA, MASTERCARD...)
	Last4    string
}

// GeneralRefundKey is the Square idempotency key of a general card refund:
// one per client_key (≤ 45 characters), so a retry after no answer is the
// same Square refund.
func GeneralRefundKey(clientKey string) string {
	sum := sha256.Sum256([]byte("general-refund\x00" + clientKey))
	return "gr" + hex.EncodeToString(sum[:])[:40]
}

// GeneralRefund has main give cents back to the card the nonce names
// (Square's unlinked refund). Recorded by the caller only on success.
func (s *Service) GeneralRefund(ctx context.Context, nonce string, cents int64, key, reason string) (GeneralRefundResult, error) {
	nonce = strings.TrimSpace(nonce)
	switch {
	case nonce == "" || len(nonce) > 512:
		return GeneralRefundResult{}, &Refused{Code: "bad_request", Message: "card_nonce (Square's card field token) is required"}
	case cents < 1 || cents > 1_000_000:
		return GeneralRefundResult{}, &Refused{Code: "bad_request", Message: "amount_cents is 1-1000000"}
	}
	r, err := s.send(ctx, MsgGeneralRefund, map[string]any{"nonce": nonce, "amount_cents": cents, "idempotency_key": key, "reason": reason})
	if err != nil {
		return GeneralRefundResult{}, err
	}
	if r.RefundID == "" {
		return GeneralRefundResult{}, ErrUnreachable
	}
	return GeneralRefundResult{RefundID: r.RefundID, Status: r.Status, Brand: r.Brand, Last4: r.Last4}, nil
}

// CardPayment is one of the store's recent Square card payments
// (square.card_payments).
type CardPayment struct {
	SquarePaymentID string    `json:"square_payment_id"`
	At              time.Time `json:"at"`
	AmountCents     int64     `json:"amount_cents"`
	RefundableCents int64     `json:"refundable_cents"`
	Brand           string    `json:"brand"`
	Last4           string    `json:"last4"`
	Entry           string    `json:"entry"`
	ReceiptNumber   string    `json:"receipt_number,omitempty"`
}

var last4Re = regexp.MustCompile(`^[0-9]{4}$`)

// CardPayments are the store's Square card payments of the last 30 days
// with last4 that still hold cents (newest first, ≤ 20).
func (s *Service) CardPayments(ctx context.Context, last4 string, cents int64) ([]CardPayment, error) {
	last4 = strings.TrimSpace(last4)
	switch {
	case !last4Re.MatchString(last4):
		return nil, &Refused{Code: "bad_request", Message: "last4 is the card's last 4 digits"}
	case cents < 1 || cents > 1_100_000:
		return nil, &Refused{Code: "bad_request", Message: "amount_cents is 1-1100000"}
	}
	r, err := s.send(ctx, MsgCardPayments, map[string]any{"last4": last4, "amount_cents": cents})
	if err != nil {
		return nil, err
	}
	out := []CardPayment{}
	if len(r.Payments) > 0 && string(r.Payments) != "null" {
		if json.Unmarshal(r.Payments, &out) != nil {
			return nil, ErrUnreachable
		}
	}
	return out, nil
}
