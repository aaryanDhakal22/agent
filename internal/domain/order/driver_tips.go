package order

import (
	"errors"
	"strings"
	"time"
)

// Card tips the driver types before Close batch (contract/agent.md §2.10am,
// batch F, the owner 2026-11-14): a tippable card delivery has a tip state
// (none | tipped | no_tip), saved on the Pi at once and editable until
// Close batch. A typed tip counts as one of the driver's card tips on every
// report straight away (the handout drops at once); the press of Close
// batch puts it on the order (a counter card payment's tip payment + the
// Square tip adjustment, or a website card hold's ADD TIP).

// Tip states.
const (
	TipStateNone   = "none"
	TipStateTipped = "tipped"
	TipStateNoTip  = "no_tip"
)

// MaxTypedTipCents is the most one typed tip may be ($500, like ADD TIP).
const MaxTypedTipCents = 50_000

// Square states of a typed tip once Close batch applied it.
const (
	TipSquareSent          = "sent"           // a website card order: ADD TIP to main (main does Square)
	TipSquareAdjusted      = "adjusted"       // Square took the tip on the counter's card payment
	TipSquareNotAdjustable = "not_adjustable" // Square doesn't take a tip change on that payment
	TipSquareFailed        = "failed"         // main / Square unreachable or refused
)

// ErrTipBatchClosed: the delivery isn't on the open batch (closed batches
// stay read-only).
var ErrTipBatchClosed = errors.New("this delivery is on a closed batch: its tip can't change any more")

// DriverTip is one tippable card delivery's typed tip.
type DriverTip struct {
	OrderID  string `json:"order_id"`
	BatchID  int64  `json:"batch_id"`
	DriverID string `json:"driver_id"`
	State    string `json:"tip_state"`
	// TipCents: the typed tip (tipped only; 0 otherwise).
	TipCents int64 `json:"typed_tip_cents"`
	// AppliedCents: what the press of Close batch has put on the order so
	// far (the next press puts Pending()).
	AppliedCents int64      `json:"applied_cents"`
	SetAt        time.Time  `json:"set_at"`
	SetBy        string     `json:"set_by,omitempty"` // terminal:<name> | quibase | web
	AppliedAt    *time.Time `json:"applied_at,omitempty"`
	SquareState  string     `json:"square_state,omitempty"`
	SquareError  string     `json:"square_error,omitempty"`
	// SquarePaymentID: the counter card payment's Square id the tip went on.
	SquarePaymentID string `json:"square_payment_id,omitempty"`
}

// Target is the tip the order must end up with from this typing: the typed
// tip when tipped, else 0.
func (t DriverTip) Target() int64 {
	if t.State == TipStateTipped {
		return max(t.TipCents, 0)
	}
	return 0
}

// Pending is the part of the typed tip not on the order yet (negative = a
// tip lowered after a press applied it).
func (t DriverTip) Pending() int64 { return t.Target() - t.AppliedCents }

// ValidTipState: a state a manager may set.
func ValidTipState(s string) bool {
	return s == TipStateNone || s == TipStateTipped || s == TipStateNoTip
}

// cardTenders are the counter's card tenders (terminal.IsCard; the snapshot
// payment's tender, or its method on an older snapshot).
func cardPayment(p PaymentSummary) bool {
	t := strings.ToLower(p.Tender)
	if t == "" {
		t = strings.ToLower(p.Method)
	}
	return (t == "card" || t == "manual_card" || t == "square_app") && p.AmountCents > 0
}

// CardTippable: the delivery is paid by card in a way the driver's typed tip
// can go on (§2.10am): a counter order with a live card payment, or a
// website / channel card order main holds and hasn't charged. An online
// vendor order never (its tip is the vendor's); cash never.
func CardTippable(so StoredOrder) bool {
	if !so.IsDelivery() || so.Status.State == StateCancelled {
		return false
	}
	if so.Order.Source == SourceCounter {
		for _, p := range so.Order.Payments {
			if cardPayment(p) {
				return true
			}
		}
		return false
	}
	return so.IsChannel() && so.Order.CashDue() == 0 && !so.Status.TipLocked
}

// TipTakable: CardTippable, of the given (open) batch and with a driver who
// took it (out with him or delivered by him): the driver's tip pile and the
// manager's edit.
func TipTakable(so StoredOrder, openBatch int64) bool {
	st := so.Status
	return CardTippable(so) && st.BatchID == openBatch && st.DriverID != "" &&
		(st.State == StateOnDelivery || st.State == StateDelivered)
}

// ApplyTypedTips puts the typed tips on a report's lines (before Finish):
// a tippable line gets its tip state, and a typed tip not on the order yet
// counts as one of his card tips (tip, the slip's tip and, card, its total).
func ApplyTypedTips(rep *DriverReport, tips map[string]DriverTip) {
	for i := range rep.Lines {
		l := &rep.Lines[i]
		if !l.Tippable {
			continue
		}
		t, ok := tips[l.OrderID]
		if !ok || t.State == "" {
			l.TipState = TipStateNone
			continue
		}
		l.TipState = t.State
		l.TypedTipCents = t.Target()
		if extra := t.Pending(); extra != 0 {
			l.TipCents = max(l.TipCents+extra, 0)
			l.OrderTipCents = max(l.OrderTipCents+extra, 0)
			if !l.Cash {
				l.SlipTotalCents = max(l.SlipTotalCents+extra, 0)
			}
		}
	}
}

// OldDelivery (#27, §2.10am): a delivery of the batch still not delivered
// (cooking, ready, out, pooled or held after coming back; never unaccepted,
// scheduled or cancelled) whose kitchen time is at least after ago - every
// driver may claim it. after <= 0 = off.
func OldDelivery(so StoredOrder, batchID int64, now time.Time, after time.Duration) bool {
	st := so.Status
	if after <= 0 || !so.IsDelivery() || st.BatchID != batchID {
		return false
	}
	switch st.State {
	case StateAccepted, StateReady, StateOnDelivery:
	default:
		return false
	}
	return !now.Before(ClaimSince(so).Add(after))
}
