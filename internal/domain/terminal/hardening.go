package terminal

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// The hardening round (contract/agent.md §2.10s, HARDENING-PLAN A1).

// ErrClientKeyRequired: a keyed route without its client_key (400
// client_key_required).
var ErrClientKeyRequired = errors.New("client_key is required (one per tap)")

// KeyReused: the client_key was used before with another body (409
// key_reused): nothing is stored.
type KeyReused struct {
	ClientKey string
	OrderID   string
}

func (e *KeyReused) Error() string {
	return fmt.Sprintf("client_key %q was used before with another request", e.ClientKey)
}

// PaidFirst: a void / waste of an order that still holds money (409
// refund_first {paid_cents}): refund it first under the refund rules.
type PaidFirst struct{ PaidCents int64 }

func (e *PaidFirst) Error() string {
	return fmt.Sprintf("the order holds %d cents: refund it first, then void it", e.PaidCents)
}

// Is lets errors.Is(err, ErrRefundFirst) match (one 409 code).
func (e *PaidFirst) Is(target error) bool { return target == ErrRefundFirst }

// Fingerprint is a keyed request's fingerprint: sha256 of its JSON encoding
// (the caller passes the request without client_key / manager_pin). Go's
// encoding is deterministic for structs and maps (sorted keys).
func Fingerprint(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// SameRequest: a stored fingerprint matches (an empty stored one is a row
// from before 00033: answered as the same request).
func SameRequest(stored, now string) bool { return stored == "" || now == "" || stored == now }

// Waste (the owner, 2026-11-12): a made order nobody paid for is thrown away
// with a reason from the store's list (main's terminal setting
// waste_reasons, the "X" rule).
const (
	// WasteReason is the cancel reason a wasted order is stored with (its
	// reason text is in the journal and the cancel_reason column).
	WasteReasonPrefix = "waste: "
	// ActionVoidWaste journals a waste (Pi action; the Z / till Waste section).
	ActionVoidWaste = "void.waste"
	// ActionUnsettled journals the unsettled checks of a close.
	ActionUnsettled = "batch.unsettled"
	// ActionPairingCode journals a pairing code minted (X-1).
	ActionPairingCode = "pairing.code"
	// ActionPinThrottle journals a PIN lockout (X-2).
	ActionPinLockout = "pin.lockout"
)

// DefaultWasteReasons are the owner's defaults (contract 2.10s §5).
var DefaultWasteReasons = []string{"Never picked up", "Made wrong / remade", "Customer complaint", "Burnt / dropped"}

// MaxWasteReasons / MaxWasteReasonLen bound main's list.
const (
	MaxWasteReasons   = 20
	MaxWasteReasonLen = 40
)

// WasteReasons is the store's list: main's waste_reasons (1-20 entries of
// 1-40 characters after trimming; bad entries skipped), else the defaults.
func (c *Config) WasteReasons() []string {
	var out []string
	if c != nil {
		for _, r := range c.parsed.WasteReasons {
			r = strings.TrimSpace(r)
			if r == "" || utf8.RuneCountInString(r) > MaxWasteReasonLen {
				continue
			}
			out = append(out, r)
			if len(out) == MaxWasteReasons {
				break
			}
		}
	}
	if len(out) == 0 {
		return append([]string(nil), DefaultWasteReasons...)
	}
	return out
}

// WasteReasonOf finds reason in the store's list (exact text after
// trimming; "" = not on it).
func (c *Config) WasteReasonOf(reason string) string {
	reason = strings.TrimSpace(reason)
	for _, r := range c.WasteReasons() {
		if r == reason {
			return r
		}
	}
	return ""
}
