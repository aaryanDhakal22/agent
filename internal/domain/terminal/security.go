package terminal

import (
	"context"
	"fmt"
	"time"
)

// Pi journal actions of the store-side security round (contract §2.10y
// "Pairing window", §2.10s §2 "PIN guard"). The Pi writes them itself; a
// terminal can't post them.
const (
	// ActionPairingWindow: a manager opened / closed the admin code's
	// pairing window, or a pairing used it up {event: opened | closed |
	// used, by, minutes?}.
	ActionPairingWindow = "pairing.window"
	// ActionPinLocked: the store's wrong PINs reached the daily ceiling;
	// every PIN check is refused until a manager unlocks {misses_24h,
	// daily_ceiling}.
	ActionPinLocked = "pin.locked"
	// ActionPinUnlocked: a manager unlocked the PIN checks {by}.
	ActionPinUnlocked = "pin.unlocked"
	// ActionAdminCodeCheck: a device checked the admin code (admin /
	// developer mode) {ok} (never the code).
	ActionAdminCodeCheck = "admin_code.check"
)

// The manager needs of the security round.
const (
	// NeedPairingWindow: opening the pairing window from a terminal.
	NeedPairingWindow = "pairing_window"
)

func init() {
	for _, a := range []string{ActionPairingWindow, ActionPinLocked, ActionPinUnlocked, ActionAdminCodeCheck} {
		reserved[a] = true
	}
}

// SessionIdle: a staff session the Pi ended because its terminal made no
// call for the auto-lock time + a margin (ActionSessionEnded's reason).
const SessionIdle = "idle"

// PinLocked: the store's wrong PINs reached the daily ceiling; every PIN
// check is refused until a manager unlocks them (403 pin_locked).
type PinLocked struct{ At time.Time }

func (e *PinLocked) Error() string {
	return "PIN logins are locked after too many wrong PINs. A manager unlocks them on the Pi page or in Quibase."
}

// A manager's PIN is exactly ManagerPINDigits digits (the owner, the
// security round): a shorter PIN that is a manager's is a wrong PIN on the
// Pi (counted like one), with these words.
const ManagerPINDigits = 4

var (
	// ErrManagerPINShort is ErrWrongPIN at a login / manager-check.
	ErrManagerPINShort = fmt.Errorf("%w: a manager's PIN is 4 digits - set a new one on the Staff page", ErrWrongPIN)
	// ErrManagerPINShortInline is ErrManagerPINWrong for a manager_pin
	// sent with a request.
	ErrManagerPINShortInline = fmt.Errorf("%w: a manager's PIN is 4 digits - set a new one on the Staff page", ErrManagerPINWrong)
)

type voidApproverKey struct{}

// WithVoidApprover carries the manager who approved a void into the
// repository's transaction (the order.void event's approved_by).
func WithVoidApprover(ctx context.Context, staffID string) context.Context {
	if staffID == "" {
		return ctx
	}
	return context.WithValue(ctx, voidApproverKey{}, staffID)
}

// VoidApprover is the manager WithVoidApprover put on ctx ("" = none).
func VoidApprover(ctx context.Context) string {
	id, _ := ctx.Value(voidApproverKey{}).(string)
	return id
}
