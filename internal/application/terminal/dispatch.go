package terminal

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	orderApp "quiccpos/agent/internal/application/order"
	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/terminal"

	"github.com/google/uuid"
)

// The terminal's Dispatch and Pickup bars (contract/agent.md §2.10v,
// 2026-11-12): the clocked-in drivers, Send Out, a driver back, and a
// pickup's ready time / a delivery's time moved. Each needs a staff member
// logged in on the terminal and is journaled under them; the floor work is
// the tablet's own (orderApp.Service), so main, the tablets and the hub see
// the same events as from Quibase.

// Dispatch is what the bars need from the floor (orderApp.Service).
type Dispatch interface {
	ListDrivers(ctx context.Context) ([]order.Driver, error)
	SendOut(ctx context.Context, driverID string, req orderApp.SendOutRequest) ([]order.StoredOrder, error)
	ReturnDriver(ctx context.Context, driverID string) ([]order.StoredOrder, []string, error)
	SetReadyAt(ctx context.Context, id string, at time.Time) (*order.StoredOrder, *time.Time, error)
}

// SetDispatch wires the bars' floor (cmd/main.go).
func (s *Service) SetDispatch(d Dispatch) { s.dispatch = d }

// Journal actions of the bars (Pi only).
const (
	ActionSendOut       = terminal.ActionSendOut
	ActionDriverBack    = terminal.ActionDriverBack
	ActionOrderTime     = terminal.ActionOrderTime
	EventOrderTime      = "terminal.order.time"
	MaxSendOut          = 30
	maxReadyAhead       = 24 * time.Hour
	errDispatchOffLabel = "dispatch isn't running"
)

// ErrDispatchOff: the bars' floor isn't wired (tests, a Pi mid-start).
var ErrDispatchOff = errors.New(errDispatchOffLabel)

func (s *Service) dispatchStaff(a Actor) (terminal.Staff, error) {
	st, _, err := s.staffOf(a)
	if err != nil {
		return st, err
	}
	if s.dispatch == nil {
		return st, ErrDispatchOff
	}
	return st, nil
}

// Drivers is GET /api/terminal/drivers: the clocked-in drivers (available
// or out) in queue order.
func (s *Service) Drivers(ctx context.Context, a Actor) ([]order.Driver, error) {
	if _, err := s.dispatchStaff(a); err != nil {
		return nil, err
	}
	all, err := s.dispatch.ListDrivers(ctx)
	if err != nil {
		return nil, err
	}
	out := []order.Driver{}
	for _, d := range all {
		if d.Status != order.DriverOff {
			out = append(out, d)
		}
	}
	return out, nil
}

// SendOutIn is POST /api/terminal/deliveries/send-out.
type SendOutIn struct {
	DriverID  string   `json:"driver_id"`
	OrderIDs  []string `json:"order_ids"`
	MarkReady bool     `json:"mark_ready"`
}

// SendOut sends a driver out with the given deliveries (pooled with him
// first) plus whatever his pool already held, as the tablet's SEND OUT.
func (s *Service) SendOut(ctx context.Context, a Actor, in SendOutIn) ([]order.StoredOrder, error) {
	st, err := s.dispatchStaff(a)
	if err != nil {
		return nil, err
	}
	in.DriverID = strings.TrimSpace(in.DriverID)
	if in.DriverID == "" {
		return nil, badRequest("driver_id is required")
	}
	if len(in.OrderIDs) > MaxSendOut {
		return nil, badRequest(fmt.Sprintf("at most %d orders at once", MaxSendOut))
	}
	for _, id := range in.OrderIDs {
		if _, err := uuid.Parse(id); err != nil {
			return nil, badRequest("order_ids are order ids")
		}
	}
	sent, err := s.dispatch.SendOut(ctx, in.DriverID, orderApp.SendOutRequest{MarkReady: in.MarkReady, Include: in.OrderIDs})
	if err != nil {
		return nil, err
	}
	ids, nums := make([]string, 0, len(sent)), make([]int64, 0, len(sent))
	name := ""
	for _, so := range sent {
		ids = append(ids, so.Order.ID)
		nums = append(nums, numberOf(so))
		name = so.Status.DriverName
	}
	s.journal(ctx, terminal.Entry{StaffID: st.ID, DeviceID: a.DeviceID, Action: ActionSendOut,
		Detail: detail(map[string]any{"driver_id": in.DriverID, "driver_name": name, "order_ids": ids, "numbers": nums, "mark_ready": in.MarkReady})})
	s.logger.Info().Ctx(ctx).Str("driver_id", in.DriverID).Int("orders", len(sent)).Str("staff_id", st.ID).
		Str("device_id", a.DeviceID).Msg("Driver sent out from a terminal")
	return sent, nil
}

// DriverReturnedResult is POST /api/terminal/drivers/{id}/returned's answer.
type DriverReturnedResult struct {
	Orders       []order.StoredOrder `json:"orders"`
	AutoAssigned []string            `json:"auto_assigned_order_ids"`
}

// DriverReturned: the driver is back (his run delivered, back of the queue).
func (s *Service) DriverReturned(ctx context.Context, a Actor, driverID string) (*DriverReturnedResult, error) {
	st, err := s.dispatchStaff(a)
	if err != nil {
		return nil, err
	}
	done, assigned, err := s.dispatch.ReturnDriver(ctx, driverID)
	if err != nil {
		return nil, err
	}
	if done == nil {
		done = []order.StoredOrder{}
	}
	if assigned == nil {
		assigned = []string{}
	}
	ids := make([]string, 0, len(done))
	for _, so := range done {
		ids = append(ids, so.Order.ID)
	}
	s.journal(ctx, terminal.Entry{StaffID: st.ID, DeviceID: a.DeviceID, Action: ActionDriverBack,
		Detail: detail(map[string]any{"driver_id": driverID, "order_ids": ids})})
	return &DriverReturnedResult{Orders: done, AutoAssigned: assigned}, nil
}

// TimeIn is POST /api/terminal/orders/{id}/time.
type TimeIn struct {
	ReadyAt *time.Time `json:"ready_at"`
}

// TimeResult is its answer.
type TimeResult struct {
	Order order.StoredOrder `json:"order"`
	From  *time.Time        `json:"from"`
	To    time.Time         `json:"to"`
}

// ChangeTime moves an order's ready time (a pickup's ready time, a
// delivery's time): any source, while it is cooking or ready. Not before it
// was placed, at most 24 h ahead.
func (s *Service) ChangeTime(ctx context.Context, a Actor, id string, in TimeIn) (*TimeResult, error) {
	st, err := s.dispatchStaff(a)
	if err != nil {
		return nil, err
	}
	if _, err := uuid.Parse(id); err != nil {
		return nil, order.ErrNotFound
	}
	if in.ReadyAt == nil {
		return nil, badRequest("ready_at is required (RFC 3339)")
	}
	at := in.ReadyAt.UTC().Truncate(time.Second)
	now := s.now()
	if at.After(now.Add(maxReadyAhead)) {
		return nil, badRequest("ready_at is at most 24 hours ahead")
	}
	if at.Before(now.Add(-maxReadyAhead)) {
		return nil, badRequest("ready_at is too far back")
	}
	so, prev, err := s.dispatch.SetReadyAt(ctx, id, at)
	if err != nil {
		return nil, err
	}
	d := map[string]any{"number": numberOf(*so), "to": at, "type": so.Order.Fulfillment.Type}
	if prev != nil {
		d["from"] = prev.UTC()
	}
	s.journal(ctx, terminal.Entry{StaffID: st.ID, DeviceID: a.DeviceID, Action: ActionOrderTime, OrderID: id, Detail: detail(d)})
	s.publish(EventOrderTime, map[string]any{"order": so, "from": prev, "to": at, "device_id": a.DeviceID})
	return &TimeResult{Order: *so, From: prev, To: at}, nil
}
