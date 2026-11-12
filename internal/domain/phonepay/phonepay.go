// Package phonepay is "Send to phone" (contract/agent.md §2.10z): a terminal
// sends a card charge to a cashier's phone, the phone charges it with
// Square's Point of Sale app (Tap to Pay on the phone) and reports Square's
// answer back to the Pi, which records it on the order as a square_app
// payment. This package holds the charge, its states and the pure rules.
package phonepay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Charge states. pending = on the phone, not started; in_app = the cashier
// pressed CHARGE and the Square app was opened; paid = recorded on the order;
// declined / cancelled / expired = nothing taken; unrecorded = Square took
// the money but the Pi couldn't record it (or the cashier says it went
// through and Square doesn't show it yet): "Record it" tries again.
const (
	StatusPending    = "pending"
	StatusInApp      = "in_app"
	StatusPaid       = "paid"
	StatusDeclined   = "declined"
	StatusCancelled  = "cancelled"
	StatusExpired    = "expired"
	StatusUnrecorded = "unrecorded"
)

// Open: a charge still waiting for Square's answer.
func Open(status string) bool { return status == StatusPending || status == StatusInApp }

// Final: a charge nothing more happens to by itself. A late "ok" from Square
// still reopens a declined / cancelled / expired one (money is never dropped).
func Final(status string) bool {
	switch status {
	case StatusPaid, StatusDeclined, StatusCancelled, StatusExpired:
		return true
	}
	return false
}

// Charge is one card charge sent to a phone.
type Charge struct {
	ID          string `json:"id"`
	ClientKey   string `json:"client_key"`
	RequestHash string `json:"-"`
	OrderID     string `json:"order_id"`
	Number      int64  `json:"number"`
	PhoneID     string `json:"phone_id"`
	PhoneName   string `json:"phone_name"`
	DeviceID    string `json:"device_id"` // the terminal that sent it
	DeviceName  string `json:"device_name"`
	StaffID     string `json:"staff_id"` // who was logged in there: the payment is theirs
	StaffName   string `json:"staff_name"`
	AmountCents int64  `json:"amount_cents"`
	TipCents    int64  `json:"tip_cents"`
	// Code is the short code on Square's note ("Order #12 · QP-4F2A9C"), so a
	// payment whose callback was lost can be found in Square.
	Code      string     `json:"code"`
	Note      string     `json:"note"`
	Status    string     `json:"status"`
	Message   string     `json:"message,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	ExpiresAt time.Time  `json:"expires_at"`
	OpenedAt  *time.Time `json:"opened_at,omitempty"`
	// FinishedAt: when it became paid / declined / cancelled / expired.
	FinishedAt          *time.Time `json:"finished_at,omitempty"`
	TransactionID       string     `json:"transaction_id,omitempty"`
	ClientTransactionID string     `json:"client_transaction_id,omitempty"`
	ErrorCode           string     `json:"error_code,omitempty"`
	PaymentID           string     `json:"payment_id,omitempty"` // the order_payments row
	SquarePaymentID     string     `json:"square_payment_id,omitempty"`
	Brand               string     `json:"brand,omitempty"`
	Last4               string     `json:"last4,omitempty"`
	// Entry: how Square read the card (contactless = Tap to Pay or a tap on
	// a reader, chip, swiped, keyed; "" = unknown), from the lookup.
	Entry string `json:"entry,omitempty"`
	// ExtraTipCents: a tip the customer added on Square's own screen (the
	// Square app's tipping), learned from the lookup; recorded with TipCents.
	ExtraTipCents int64 `json:"extra_tip_cents,omitempty"`
}

// TotalCents is what Square is asked to charge: the amount + the tip
// Cash Out added (the driver tip), like the reader's charge.
func (c Charge) TotalCents() int64 { return c.AmountCents + c.TipCents }

// Errors.
var (
	ErrNotFound     = errors.New("phone charge not found")
	ErrFinal        = errors.New("the charge is already finished")
	ErrPhoneBusy    = errors.New("that phone already has a charge waiting")
	ErrUnknownPhone = errors.New("no such phone")
	ErrKeyReused    = errors.New("client_key reused with another body")
)

// Repo stores charges.
type Repo interface {
	// Create inserts c; a charge with the same client_key returns that one
	// (dup true) and nothing is written.
	Create(ctx context.Context, c Charge) (got Charge, dup bool, err error)
	Get(ctx context.Context, id string) (Charge, error)
	// ByKey is the charge a client_key made (ErrNotFound when none).
	ByKey(ctx context.Context, key string) (Charge, error)
	Update(ctx context.Context, c Charge) error
	// OpenFor lists a phone's open (pending / in_app / unrecorded) charges,
	// oldest first; phoneID "" = every phone's.
	OpenFor(ctx context.Context, phoneID string) ([]Charge, error)
	// ForOrder lists an order's charges, newest first.
	ForOrder(ctx context.Context, orderID string) ([]Charge, error)
	// Recent lists the newest charges (the Pi page), at most limit.
	Recent(ctx context.Context, limit int) ([]Charge, error)
}

// Fingerprint of a create request (the client_key rule, contract 2.10s §1).
func Fingerprint(orderID, phoneID string, amount, tip int64) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%d|%d", orderID, phoneID, amount, tip)))
	return hex.EncodeToString(h[:])
}

// CodeOf is the short code for a charge id (its last 6 hex digits, upper
// case, "QP-" in front).
func CodeOf(id string) string {
	s := strings.ToUpper(strings.ReplaceAll(id, "-", ""))
	if len(s) > 6 {
		s = s[len(s)-6:]
	}
	return "QP-" + s
}

// NoteOf is the note Square shows and keeps on the payment.
func NoteOf(number int64, code string) string {
	if number > 0 {
		return fmt.Sprintf("Order #%d %s", number, code)
	}
	return "Order " + code
}

// Result is what the phone reports from Square's callback.
type Result struct {
	// Status: ok (Square says paid) | error (Square refused / failed) |
	// cancelled (the cashier or customer cancelled in Square) | "went_through"
	// (no callback: the cashier says Square showed it approved).
	Status              string `json:"status"`
	TransactionID       string `json:"transaction_id"`
	ClientTransactionID string `json:"client_transaction_id"`
	ErrorCode           string `json:"error_code"`
}

// Result statuses.
const (
	ResultOK          = "ok"
	ResultError       = "error"
	ResultCancelled   = "cancelled"
	ResultWentThrough = "went_through"
)

// cancelled: Square's "nobody paid, nothing went wrong" answers. Android
// sends com.squareup.pos.ERROR_TRANSACTION_CANCELED (the docs' table drops the
// ERROR_ prefix, so match the end), iOS payment_canceled.
func cancelled(code string) bool {
	return strings.HasSuffix(code, "TRANSACTION_CANCELED") || code == "payment_canceled"
}

// Normalize checks a result and folds Square's cancel codes into cancelled.
func (r Result) Normalize() (Result, error) {
	r.Status = strings.TrimSpace(r.Status)
	r.TransactionID = strings.TrimSpace(r.TransactionID)
	r.ClientTransactionID = strings.TrimSpace(r.ClientTransactionID)
	r.ErrorCode = strings.TrimSpace(r.ErrorCode)
	if len(r.TransactionID) > 120 || len(r.ClientTransactionID) > 120 || len(r.ErrorCode) > 120 {
		return r, fmt.Errorf("ids and codes are at most 120 characters")
	}
	switch r.Status {
	case ResultOK:
		// Offline mode answers a client transaction id only (Tap to Pay is
		// never offline, a reader charge may be).
		if r.TransactionID == "" && r.ClientTransactionID == "" {
			return r, fmt.Errorf("an ok result needs Square's transaction id")
		}
	case ResultError:
		if cancelled(r.ErrorCode) {
			r.Status = ResultCancelled
		}
	case ResultCancelled, ResultWentThrough:
	default:
		return r, fmt.Errorf("status is ok, error, cancelled or went_through")
	}
	return r, nil
}

// errorWords: plain words for Square's error codes, by the code's end
// (Android com.squareup.pos.ERROR_<X>, iOS snake_case).
var errorWords = []struct{ suffix, words string }{
	{"NO_NETWORK", "The phone has no internet: nothing was charged."},
	{"no_network_connection", "The phone has no internet: nothing was charged."},
	{"USER_NOT_LOGGED_IN", "Square isn't signed in on this phone."},
	{"not_logged_in", "Square isn't signed in on this phone."},
	{"NO_EMPLOYEE_LOGGED_IN", "Nobody is signed in to Square on this phone."},
	{"USER_NOT_ACTIVATED", "This Square account can't take cards yet."},
	{"user_not_active", "This Square account can't take cards yet."},
	{"ILLEGAL_LOCATION_ID", "Square is signed in to another location."},
	{"UNSUPPORTED_API_VERSION", "Update the Square app on this phone."},
	{"UNSUPPORTED_WEB_API_VERSION", "Update the Square app on this phone."},
	{"unsupported_api_version", "Update the Square app on this phone."},
	{"UNAUTHORIZED_CLIENT_ID", "Square doesn't know this app: check the Square app settings."},
	{"client_not_authorized_for_user", "Square doesn't know this app: check the Square app settings."},
	{"TRANSACTION_ALREADY_IN_PROGRESS", "Square is busy with another charge on this phone."},
	{"could_not_perform", "Square is busy with another charge on this phone."},
	{"DISABLED", "Square's Point of Sale API is turned off for this account."},
	{"amount_too_small", "The amount is too small for Square."},
	{"amount_too_large", "The amount is too large for Square."},
	{"currency_code_mismatch", "Square is signed in to an account with another currency."},
	{"INSUFFICIENT_CARD_BALANCE", "The card doesn't have enough on it."},
}

// WordsFor is a declined charge's message.
func WordsFor(code string) string {
	for _, w := range errorWords {
		if strings.HasSuffix(code, w.suffix) {
			return w.words
		}
	}
	if code == "" {
		return "Square didn't take the payment: nothing was charged."
	}
	return "Square didn't take the payment (" + code + "): nothing was charged."
}

// Timeout bounds (seconds) of the store setting phone_charge_timeout_seconds.
const (
	DefaultTimeoutSeconds = 180
	MinTimeoutSeconds     = 30
	MaxTimeoutSeconds     = 1800
)
