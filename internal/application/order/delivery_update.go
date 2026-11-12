package order

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
	"quiccpos/agent/internal/safego"

	"github.com/google/uuid"
)

// DefaultDeliveryUpdateWait is how long a changed delivery waits for the
// difference to be charged before its UPDATED pair prints anyway (the
// difference left to collect at the door). AGENT_DELIVERY_UPDATE_WAIT changes
// it (SetDeliveryUpdateWait).
const DefaultDeliveryUpdateWait = 90 * time.Second

// deliveryUpdates are the UPDATED delivery copies waiting after a change
// (the owner's choice A, 2026-11-12): ONE pair per change, showing the final
// state. A change of a sent delivery arms it; a payment on that order prints
// it at once (after the charge, with the payment on it); with no payment it
// prints when the wait runs out. Another change while it waits starts the
// wait again: still one pair.
type deliveryUpdates struct {
	mu      sync.Mutex
	wait    time.Duration
	pending map[string]*pendingUpdate
}

type pendingUpdate struct {
	so    order.StoredOrder
	timer *time.Timer
}

// SetDeliveryUpdateWait sets the wait (≤ 0 = the default).
func (s *Service) SetDeliveryUpdateWait(d time.Duration) {
	s.upd.mu.Lock()
	s.upd.wait = d
	s.upd.mu.Unlock()
}

// wantsDeliveryUpdate: a delivery that is still to be (or being) delivered.
func wantsDeliveryUpdate(so order.StoredOrder) bool {
	if !strings.EqualFold(so.Order.Fulfillment.Type, "delivery") {
		return false
	}
	switch so.Status.State {
	case order.StateCancelled, order.StateDelivered:
		return false
	}
	return true
}

// deferDeliveryUpdate arms (or re-arms) the order's UPDATED pair after a
// change of its lines or type.
func (s *Service) deferDeliveryUpdate(ctx context.Context, so order.StoredOrder) {
	if !wantsDeliveryUpdate(so) {
		return
	}
	id := so.Order.ID
	bg := context.WithoutCancel(ctx)
	s.upd.mu.Lock()
	defer s.upd.mu.Unlock()
	if s.upd.pending == nil {
		s.upd.pending = map[string]*pendingUpdate{}
	}
	if old := s.upd.pending[id]; old != nil {
		old.timer.Stop()
	}
	wait := s.upd.wait
	if wait <= 0 {
		wait = DefaultDeliveryUpdateWait
	}
	p := &pendingUpdate{so: so}
	p.timer = time.AfterFunc(wait, func() {
		defer safego.Recover("order.deliveryUpdate")
		s.upd.mu.Lock()
		if s.upd.pending[id] != p { // printed by a payment, or re-armed by another change
			s.upd.mu.Unlock()
			return
		}
		delete(s.upd.pending, id)
		s.upd.mu.Unlock()
		final := p.so
		if uid, err := uuid.Parse(id); err == nil {
			if cur, err := s.repo.GetByID(bg, uid); err == nil && cur != nil {
				final = *cur
			}
		}
		if wantsDeliveryUpdate(final) {
			s.printDeliveryUpdate(bg, final)
		}
	})
	s.upd.pending[id] = p
}

// DeliveryUpdatePending says whether a changed delivery's UPDATED pair is
// still waiting (tests, diagnostics).
func (s *Service) DeliveryUpdatePending(orderID string) bool {
	s.upd.mu.Lock()
	defer s.upd.mu.Unlock()
	return s.upd.pending[orderID] != nil
}

// PrintDeliveryUpdate prints a sent delivery's Merchant + Customer Copy again,
// marked UPDATED / ACTUALIZADO, on the receipt printer (background) after a
// payment on it (the terminal service: a charge, a tip afterwards). A change
// waiting for this payment prints now, as that one pair (choice A). Called
// once per real payment (never on a duplicate); nothing for a pickup, a
// cancelled or a delivered order.
func (s *Service) PrintDeliveryUpdate(ctx context.Context, so order.StoredOrder) {
	s.upd.mu.Lock()
	if p := s.upd.pending[so.Order.ID]; p != nil {
		p.timer.Stop()
		delete(s.upd.pending, so.Order.ID)
	}
	s.upd.mu.Unlock()
	if !wantsDeliveryUpdate(so) {
		return
	}
	s.printDeliveryUpdate(ctx, so)
}

func (s *Service) printDeliveryUpdate(ctx context.Context, so order.StoredOrder) {
	snap := printable(so)
	s.background(ctx, func(ctx context.Context) {
		out, err := s.print(ctx, snap, printer.ModeDeliveryUpdate)
		if errors.Is(err, printer.ErrNoTickets) {
			return
		}
		s.alertPrintIssues(ctx, so, out, false)
		if err != nil {
			s.logger.Warn().Ctx(ctx).Err(err).Str("order_id", so.Order.ID).Msg("Updated delivery copies not printed")
			return
		}
		s.printEvents(label(so)+" delivery copies (updated)", out)
	})
}
