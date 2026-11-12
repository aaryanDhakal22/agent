package terminal

import (
	"context"
	"errors"
	"testing"
	"time"

	orderApp "quiccpos/agent/internal/application/order"
	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/terminal"
)

// The Dispatch / Pickup bars (contract §2.10v): a staff login, the
// tablet's floor work, a journal row each.

type fakeDispatch struct {
	sent     []string
	markRdy  bool
	returned string
	readyAt  time.Time
	err      error
}

func (f *fakeDispatch) ListDrivers(context.Context) ([]order.Driver, error) {
	return []order.Driver{{ID: "d1", Name: "Ali", Status: order.DriverAvailable}, {ID: "d2", Name: "Bo", Status: order.DriverOff},
		{ID: "d3", Name: "Cy", Status: order.DriverOut}}, nil
}

func (f *fakeDispatch) SendOut(_ context.Context, driverID string, req orderApp.SendOutRequest) ([]order.StoredOrder, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.sent, f.markRdy = req.Include, req.MarkReady
	var out []order.StoredOrder
	for _, id := range req.Include {
		so := order.StoredOrder{Order: order.Snapshot{ID: id}}
		so.Status.DriverID, so.Status.DriverName = driverID, "Ali"
		out = append(out, so)
	}
	return out, nil
}

func (f *fakeDispatch) ReturnDriver(_ context.Context, driverID string) ([]order.StoredOrder, []string, error) {
	f.returned = driverID
	return nil, nil, f.err
}

func (f *fakeDispatch) SetReadyAt(_ context.Context, id string, at time.Time) (*order.StoredOrder, *time.Time, error) {
	if f.err != nil {
		return nil, nil, f.err
	}
	f.readyAt = at
	prev := at.Add(-10 * time.Minute)
	so := &order.StoredOrder{Order: order.Snapshot{ID: id}}
	so.Status.EstimatedReadyAt = &at
	return so, &prev, nil
}

func TestDispatchBars(t *testing.T) {
	r := newRig(t)
	fd := &fakeDispatch{}
	r.svc.SetDispatch(fd)
	id := "0190b6d2-0000-7000-8000-000000000001"
	// No login: 401 for every one of them.
	if _, err := r.svc.Drivers(r.ctx, till); !errors.Is(err, terminal.ErrLoginRequired) {
		t.Fatalf("drivers without login: %v", err)
	}
	if _, err := r.svc.SendOut(r.ctx, till, SendOutIn{DriverID: "d1", OrderIDs: []string{id}}); !errors.Is(err, terminal.ErrLoginRequired) {
		t.Fatalf("send-out without login: %v", err)
	}
	r.login(t, till, "1234")
	ds, err := r.svc.Drivers(r.ctx, till)
	if err != nil || len(ds) != 2 || ds[0].ID != "d1" || ds[1].ID != "d3" {
		t.Fatalf("clocked-in drivers %+v %v", ds, err)
	}
	if _, err := r.svc.SendOut(r.ctx, till, SendOutIn{OrderIDs: []string{id}}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("no driver: %v", err)
	}
	if _, err := r.svc.SendOut(r.ctx, till, SendOutIn{DriverID: "d1", OrderIDs: []string{"nope"}}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("bad id: %v", err)
	}
	sent, err := r.svc.SendOut(r.ctx, till, SendOutIn{DriverID: "d1", OrderIDs: []string{id}, MarkReady: true})
	if err != nil || len(sent) != 1 || len(fd.sent) != 1 || !fd.markRdy {
		t.Fatalf("send-out %+v %v", sent, err)
	}
	if j := journalOf(t, r, ActionSendOut); len(j) != 1 || j[0]["driver_id"] != "d1" || j[0]["_staff"] != "cash-1" {
		t.Fatalf("journal %v", j)
	}
	// The floor's own refusal comes back as it is (the tablet's codes).
	fd.err = &order.ErrNotReady{IDs: []string{id}}
	var nr *order.ErrNotReady
	if _, err := r.svc.SendOut(r.ctx, till, SendOutIn{DriverID: "d1", OrderIDs: []string{id}}); !errors.As(err, &nr) {
		t.Fatalf("not ready: %v", err)
	}
	fd.err = nil
	if res, err := r.svc.DriverReturned(r.ctx, till, "d3"); err != nil || fd.returned != "d3" || res.Orders == nil || res.AutoAssigned == nil {
		t.Fatalf("returned %+v %v", res, err)
	}
	// The time: required, within a day, journaled, announced.
	if _, err := r.svc.ChangeTime(r.ctx, till, id, TimeIn{}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("no ready_at: %v", err)
	}
	far := time.Now().Add(48 * time.Hour)
	if _, err := r.svc.ChangeTime(r.ctx, till, id, TimeIn{ReadyAt: &far}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("2 days ahead: %v", err)
	}
	at := time.Now().Add(25 * time.Minute)
	res, err := r.svc.ChangeTime(r.ctx, till, id, TimeIn{ReadyAt: &at})
	if err != nil || !res.To.Equal(at.UTC().Truncate(time.Second)) || res.From == nil || !fd.readyAt.Equal(res.To) {
		t.Fatalf("time %+v %v", res, err)
	}
	if j := journalOf(t, r, ActionOrderTime); len(j) != 1 || j[0]["from"] == nil {
		t.Fatalf("order.time journal %v", j)
	}
	if r.pub.count(EventOrderTime) != 1 || len(journalOf(t, r, ActionDriverBack)) != 1 {
		t.Fatalf("events %v", r.pub.events)
	}
	// A terminal can't post the Pi's own actions.
	for _, a := range []string{ActionSendOut, ActionDriverBack, ActionOrderTime, terminal.ActionMenuGoneLine, terminal.ActionPriceKept, terminal.ActionSessionEnded} {
		if terminal.CheckPostedAction(a) == nil {
			t.Fatalf("%s is posted by a terminal", a)
		}
	}
}
