package terminal

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"quiccpos/agent/internal/application/squareterm"
	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/terminal"
	"quiccpos/agent/internal/safego"

	"github.com/google/uuid"
)

// Card tips the driver types before Close batch (contract/agent.md §2.10am,
// batch F, the owner 2026-11-14): "A card tip typed for a delivery shows on
// the driver's report right away. It is saved but not closed, so it stays
// editable from Quibase until Close batch ... The tip goes onto the Square
// charge when Close batch is pressed."
//
// The driver types it on his terminal's Tips tab (Tip / NoTip, TipsView); a
// manager corrects it on Quibase / the web UI (ManagerTip). It counts on
// every driver report at once (application/order applyTypedTips). The press
// of Close batch (ApplyDriverTips, hooked by cmd/main.go) puts each one on
// its order: a counter card payment's tip payment (2.10p) + the Square tip
// adjustment through main (square.tip_adjust, in the background), or a
// website card hold's ADD TIP (also at his clock-out: main captures then).

// DriverTipRepo stores the typed tips (repositories.OrderRepository).
type DriverTipRepo interface {
	DriverTipsOfBatch(ctx context.Context, batchID int64) (map[string]order.DriverTip, error)
	SaveDriverTip(ctx context.Context, orderID, state string, cents int64, setBy string) (*order.StoredOrder, order.DriverTip, order.DriverTip, error)
	PendingDriverTips(ctx context.Context, driverID string) ([]order.DriverTip, error)
	DriverTipApplied(ctx context.Context, orderID string, applied int64, squareState, squareErr, squarePaymentID string) error
	DriverTipSquare(ctx context.Context, orderID, state, errText string) error
}

// SquareTipAdjuster sets a Square payment's whole tip through main
// (squareterm.Service.TipAdjust).
type SquareTipAdjuster interface {
	TipAdjust(ctx context.Context, squarePaymentID string, tipCents int64, orderID string) (string, error)
}

// SetDriverTips wires the typed tips (cmd/main.go).
func (s *Service) SetDriverTips(r DriverTipRepo) { s.drv.tips = r }

// Who changed a tip (DriverTip.SetBy, the SSE event's by).
const (
	TipByTerminal = "terminal"
	TipByQuibase  = "quibase"
	TipByWeb      = "web"
)

// typedTips is the open batch's typed tips (empty without a store).
func (s *Service) typedTips(ctx context.Context) map[string]order.DriverTip {
	if s.drv.tips == nil {
		return nil
	}
	m, err := s.drv.tips.DriverTipsOfBatch(ctx, 0)
	if err != nil {
		s.logger.Warn().Ctx(ctx).Err(err).Msg("read the typed driver tips failed")
		return nil
	}
	return m
}

// ticketWith is ticketOf with the typed tip: its state, amount, and the tip
// so far (a typed tip not on the order yet counts).
func ticketWith(so order.StoredOrder, tips map[string]order.DriverTip) DriverTicket {
	t := ticketOf(so)
	if t.TipKind != "card" {
		return t
	}
	if tp, ok := tips[so.Order.ID]; ok && tp.State != "" {
		t.TipState, t.TypedTipCents = tp.State, tp.Target()
		t.TipCents = max(t.TipCents+tp.Pending(), 0)
	}
	return t
}

// TipsView is GET /api/terminal/driver/tips: his tip pile (card deliveries
// with nothing typed yet, oldest first), the ones typed (newest first) and
// his report so far.
type TipsView struct {
	Driver *DriverPublic        `json:"driver"`
	Pile   []DriverTicket       `json:"pile"`
	Typed  []DriverTicket       `json:"typed"`
	Report *order.DriverReport `json:"report"`
}

// Tips is his Tips tab.
func (s *Service) Tips(ctx context.Context, a Actor) (*TipsView, error) {
	_, d, err := s.driverOf(ctx, a)
	if err != nil {
		return nil, err
	}
	v := &TipsView{Driver: driverPublic(*d), Pile: []DriverTicket{}, Typed: []DriverTicket{}}
	mine, err := s.mine(ctx, d)
	if err != nil {
		return nil, err
	}
	all := append(append([]DriverTicket{}, mine.Out...), mine.Delivered...)
	for _, t := range all {
		switch t.TipState {
		case order.TipStateNone:
			v.Pile = append(v.Pile, t)
		case order.TipStateTipped, order.TipStateNoTip:
			v.Typed = append(v.Typed, t)
		}
	}
	at := func(t DriverTicket) time.Time {
		switch {
		case t.DeliveredAt != nil:
			return *t.DeliveredAt
		case t.OutAt != nil:
			return *t.OutAt
		}
		return time.Time{}
	}
	sort.SliceStable(v.Pile, func(i, j int) bool { return at(v.Pile[i]).Before(at(v.Pile[j])) })
	sort.SliceStable(v.Typed, func(i, j int) bool { return at(v.Typed[i]).After(at(v.Typed[j])) })
	if v.Report, err = s.drv.floor.DriverReport(ctx, d.ID); err != nil {
		return nil, err
	}
	return v, nil
}

// driverSetTip saves his tip state on one of his card deliveries.
func (s *Service) driverSetTip(ctx context.Context, a Actor, id, state string, cents int64) (*TipResult, error) {
	st, d, err := s.driverOf(ctx, a)
	if err != nil {
		return nil, err
	}
	if _, err := s.his(ctx, d, id); err != nil {
		return nil, err
	}
	so, saved, prev, err := s.saveTip(ctx, id, state, cents, TipByTerminal+":"+d.Name)
	if err != nil {
		return nil, err
	}
	s.journal(ctx, terminal.Entry{StaffID: st.ID, DeviceID: a.DeviceID, Action: terminal.ActionDriverTip, OrderID: id,
		Detail: detail(map[string]any{"driver_id": d.ID, "tip_cents": saved.Target(), "prev_cents": prev.Target(),
			"tip_state": saved.State, "kind": "card"})})
	s.publish(EventDriverTip, map[string]any{"staff_id": st.ID, "driver_id": d.ID, "order_id": id, "tip_cents": saved.Target(),
		"tip_state": saved.State, "kind": "card", "by": TipByTerminal})
	return &TipResult{Ticket: ticketWith(*so, map[string]order.DriverTip{id: saved}), Kind: "card", TipState: saved.State}, nil
}

func (s *Service) saveTip(ctx context.Context, id, state string, cents int64, by string) (*order.StoredOrder, order.DriverTip, order.DriverTip, error) {
	if s.drv.tips == nil {
		return nil, order.DriverTip{}, order.DriverTip{}, errNoFloor
	}
	if _, err := uuid.Parse(id); err != nil {
		return nil, order.DriverTip{}, order.DriverTip{}, order.ErrNotFound
	}
	return s.drv.tips.SaveDriverTip(ctx, id, state, cents, by)
}

// ManagerTipIn is PUT /api/orders/{id}/driver-tip.
type ManagerTipIn struct {
	TipState string `json:"tip_state"`
	TipCents *int64 `json:"tip_cents"`
}

// ManagerTipResult answers it: the tip saved and the driver's report after it.
type ManagerTipResult struct {
	OrderID       string              `json:"order_id"`
	DriverID      string              `json:"driver_id"`
	TipState      string              `json:"tip_state"`
	TypedTipCents int64               `json:"typed_tip_cents"`
	Report        *order.DriverReport `json:"report"`
}

// ManagerTip is a manager's correction from Quibase / the web UI (by =
// TipByQuibase | TipByWeb, deviceID = the tablet's): any state, until Close
// batch. Journaled under the device.
func (s *Service) ManagerTip(ctx context.Context, by, deviceID, id string, in ManagerTipIn) (*ManagerTipResult, error) {
	if !order.ValidTipState(in.TipState) {
		return nil, badRequest("tip_state is tipped, no_tip or none")
	}
	var cents int64
	if in.TipState == order.TipStateTipped {
		if in.TipCents == nil || *in.TipCents < 1 || *in.TipCents > order.MaxTypedTipCents {
			return nil, badRequest(fmt.Sprintf("tip_cents is 1-%d for a tip", order.MaxTypedTipCents))
		}
		cents = *in.TipCents
	}
	so, saved, prev, err := s.saveTip(ctx, id, in.TipState, cents, by)
	if err != nil {
		return nil, err
	}
	driverID := so.Status.DriverID
	s.journal(ctx, terminal.Entry{DeviceID: deviceID, Action: terminal.ActionDriverTip, OrderID: id,
		Detail: detail(map[string]any{"driver_id": driverID, "tip_cents": saved.Target(), "prev_cents": prev.Target(),
			"tip_state": saved.State, "kind": "card", "by": by})})
	s.publish(EventDriverTip, map[string]any{"driver_id": driverID, "order_id": id, "tip_cents": saved.Target(),
		"tip_state": saved.State, "kind": "card", "by": by})
	out := &ManagerTipResult{OrderID: id, DriverID: driverID, TipState: saved.State, TypedTipCents: saved.Target()}
	if s.drv.floor != nil {
		if rep, err := s.drv.floor.DriverReport(ctx, driverID); err == nil {
			out.Report = rep
		}
	}
	return out, nil
}

// --- the press of Close batch ------------------------------------------------------------

// EventDriverTipsApplied: a press of Close batch put typed tips on their orders.
const EventDriverTipsApplied = "driver.tips_applied"

// ApplyDriverTips puts the open batch's typed tips that aren't on their
// orders yet there (contract 2.10am). driverID "" = every driver's;
// channelOnly = only website card orders (a clock-out: main captures his
// holds). Never fails the caller: an order that can't take its tip is
// logged, journaled and paged, and stays pending (counted on the report).
func (s *Service) ApplyDriverTips(ctx context.Context, driverID string, channelOnly bool) int {
	if s.drv.tips == nil || s.drv.floor == nil {
		return 0
	}
	pend, err := s.drv.tips.PendingDriverTips(ctx, driverID)
	if err != nil {
		s.logger.Warn().Ctx(ctx).Err(err).Msg("read the typed driver tips to apply failed")
		return 0
	}
	var done []string
	for _, t := range pend {
		so, err := s.drv.floor.GetOrder(ctx, t.OrderID)
		if err != nil {
			s.logger.Warn().Ctx(ctx).Err(err).Str("order_id", t.OrderID).Msg("typed tip: order gone")
			continue
		}
		counter := so.Order.Source == order.SourceCounter
		if channelOnly && counter {
			continue
		}
		if err := s.applyOne(ctx, *so, t, counter); err != nil {
			s.logger.Warn().Ctx(ctx).Err(err).Str("order_id", t.OrderID).Int64("tip_cents", t.Target()).Msg("typed tip not applied")
			s.page(ctx, fmt.Sprintf("Card tip %s on #%s could NOT be added: %v", dollars(t.Target()), tipNumber(*so), err))
			continue
		}
		done = append(done, t.OrderID)
	}
	if len(done) > 0 {
		s.publish(EventDriverTipsApplied, map[string]any{"orders": done})
		s.logger.Info().Ctx(ctx).Int("orders", len(done)).Bool("clock_out", channelOnly).Msg("Typed driver tips put on their orders")
	}
	return len(done)
}

func (s *Service) applyOne(ctx context.Context, so order.StoredOrder, t order.DriverTip, counter bool) error {
	target, delta := t.Target(), t.Pending()
	if !counter {
		// A website card hold: ADD TIP (order.tip to main, before batch.closed).
		if _, err := s.drv.floor.SetTip(ctx, so.Order.ID, max(so.Status.AddedTipCents+delta, 0)); err != nil {
			return err
		}
		if err := s.drv.tips.DriverTipApplied(ctx, so.Order.ID, target, order.TipSquareSent, "", ""); err != nil {
			return err
		}
		s.journalApplied(ctx, so, t, "channel")
		return nil
	}
	pay, whole, err := s.counterTip(ctx, so, t)
	if err != nil {
		return err
	}
	if err := s.drv.tips.DriverTipApplied(ctx, so.Order.ID, target, "", "", pay.SquarePaymentID); err != nil {
		return err
	}
	s.journalApplied(ctx, so, t, "counter")
	s.squareTip(ctx, so, pay, whole, target)
	return nil
}

func (s *Service) journalApplied(ctx context.Context, so order.StoredOrder, t order.DriverTip, kind string) {
	s.journal(ctx, terminal.Entry{DeviceID: "pi", Action: terminal.ActionDriverTip, OrderID: so.Order.ID,
		Detail: detail(map[string]any{"driver_id": so.Status.DriverID, "applied": true, "from_cents": t.AppliedCents,
			"to_cents": t.Target(), "tip_state": t.State, "kind": kind})})
}

var errNoCardPayment = errors.New("no card payment to put the tip on")

// counterTip writes the typed tip's difference as a 2.10p tip payment on the
// order's largest live card payment; whole = that payment's tip after it.
func (s *Service) counterTip(ctx context.Context, so order.StoredOrder, t order.DriverTip) (terminal.Payment, int64, error) {
	id := so.Order.ID
	pays, err := s.repo.OrderPayments(ctx, id)
	if err != nil {
		return terminal.Payment{}, 0, err
	}
	var pick *terminal.Payment
	for i := range pays {
		p := pays[i]
		if p.Refund || !terminal.IsCard(p.Tender) || p.AmountCents <= 0 {
			continue
		}
		if pick == nil || p.AmountCents > pick.AmountCents {
			pick = &pays[i]
		}
	}
	if pick == nil {
		return terminal.Payment{}, 0, errNoCardPayment
	}
	orig := *pick
	_, now, _ := tipOf(pays, orig.ID)
	whole := max(now+t.Pending(), 0)
	if whole == now {
		return orig, whole, nil
	}
	key := fmt.Sprintf("dtip:%s:%d:%d", id, t.AppliedCents, t.Target())
	in := PaymentTipIn{TipCents: whole, ClientKey: key}
	hash := tipFingerprint(orig.ID, in)
	at := s.now().UTC().Truncate(time.Millisecond)
	cfg, _ := s.store.Config()
	so2, got, dup, err := s.repo.AddCounterPayment(ctx, id, key, hash, func(cur order.StoredOrder, paid []terminal.Payment) ([]terminal.Payment, []terminal.Entry, error) {
		_, from, _ := tipOf(paid, orig.ID)
		delta := whole - from
		if delta == 0 {
			return nil, nil, errNoTipChange
		}
		p := terminal.Payment{ID: uuid.Must(uuid.NewV7()).String(), OrderID: id, ClientKey: key, RequestHash: hash, At: at, Tender: orig.Tender,
			TipCents: max(delta, -delta), Refund: delta < 0, Ref: orig.ID, Last4: orig.Last4, Brand: orig.Brand, DeviceID: "pi"}
		if p.Refund {
			p.Reason = "tip lowered"
		}
		e := terminal.Entry{DeviceID: "pi", Action: terminal.ActionPaymentTip, OrderID: id,
			Detail: detail(map[string]any{"payment_id": orig.ID, "from_cents": from, "to_cents": whole, "tip_payment_id": p.ID,
				"by": "driver_tip"})}
		p.DrawerStart = cfg.DrawerStartCents()
		return []terminal.Payment{p}, []terminal.Entry{e}, nil
	})
	if errors.Is(err, errNoTipChange) {
		return orig, whole, nil
	}
	if err != nil {
		return terminal.Payment{}, 0, err
	}
	if !dup {
		s.orders.CounterOrderChanged(ctx, so2, nil, false)
		s.publish(EventOrderPaid, map[string]any{"order": so2, "payment": got})
	}
	return orig, whole, nil
}

// squareTip asks main, in the background, to put the payment's whole tip on
// its Square charge; the outcome is kept on the typed tip and anything but
// adjusted pages the floor (the driver is owed the tip either way).
func (s *Service) squareTip(ctx context.Context, so order.StoredOrder, pay terminal.Payment, whole, typed int64) {
	id, num := so.Order.ID, tipNumber(so)
	record := func(ctx context.Context, state, why string) {
		if err := s.drv.tips.DriverTipSquare(ctx, id, state, why); err != nil {
			s.logger.Warn().Ctx(ctx).Err(err).Str("order_id", id).Msg("typed tip: square state not saved")
		}
		if state != order.TipSquareAdjusted {
			s.page(ctx, fmt.Sprintf("Card tip %s on #%s is NOT on the Square charge: %s - add it in Square", dollars(typed), num, why))
		}
	}
	adj, ok := s.square.(SquareTipAdjuster)
	if pay.SquarePaymentID == "" || !ok {
		why := "no Square payment id (charged on the Square app)"
		if pay.SquarePaymentID != "" {
			why = "this Pi can't reach Square"
		}
		record(ctx, order.TipSquareNotAdjustable, why)
		return
	}
	bg := context.WithoutCancel(ctx)
	safego.Go("driver-tip-square", func() {
		_, err := adj.TipAdjust(bg, pay.SquarePaymentID, whole, id)
		var ref *squareterm.Refused
		switch {
		case err == nil:
			record(bg, order.TipSquareAdjusted, "")
		case errors.As(err, &ref) && ref.Code == "not_adjustable":
			record(bg, order.TipSquareNotAdjustable, "Square completed the payment and takes no tip change")
		default:
			record(bg, order.TipSquareFailed, err.Error())
		}
	})
}

func tipNumber(so order.StoredOrder) string {
	if n := so.Number(); n != nil {
		return fmt.Sprint(*n)
	}
	return so.Order.ID[:8]
}

func dollars(c int64) string { return fmt.Sprintf("$%d.%02d", c/100, c%100) }
