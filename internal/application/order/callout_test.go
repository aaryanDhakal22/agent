package order

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"

	"github.com/google/uuid"
)

// 00020 through the service: SSE order.changed, a check shown later as new
// (with the chime), void tickets only behind the POS switch, OK, finishing,
// TEST CALLOUT, settings validation.

func kitchenLine(ref, name string, kitchen bool) order.ExternalItem {
	return order.ExternalItem{Ref: ref, Name: name, Quantity: 1, UnitPriceCents: 500, LineTotalCents: 500, PrinterID: "p-pizza", Kitchen: kitchen}
}

func TestTerminalChangeRingsAndVoidPrintsOnlyWithTheSwitch(t *testing.T) {
	ctx := context.Background()
	svc, _, events, prints, _ := newIntegService(t)
	e := extOrder("pickup", "h1")
	e.Items = []order.ExternalItem{kitchenLine("1", "Cheese Steak", true), kitchenLine("2", "Fries", true)}
	res := mustUpsert(t, svc, "158", e)
	data, ok := eventOf(events, "order.added")
	var added struct {
		Chime bool `json:"chime"`
	}
	_ = json.Unmarshal(data, &added)
	if !ok || !added.Chime {
		t.Fatalf("a takeout check on Kitchen must chime: %s", data)
	}
	drain(events)

	// Garlic Knots added at the terminal: order.changed with the bar text.
	e.Hash = "h2"
	e.Items = append(e.Items, kitchenLine("3", "Garlic Knots", true))
	mustUpsert(t, svc, "158", e)
	data, ok = eventOf(events, "order.changed")
	var ch struct {
		Order  order.StoredOrder `json:"order"`
		Change order.PosChange   `json:"change"`
	}
	_ = json.Unmarshal(data, &ch)
	if !ok || ch.Change.Text != "#99 changed: +1 Garlic Knots" || ch.Order.Status.PosChange == nil {
		t.Fatalf("order.changed = %s", data)
	}

	// Fries voided, the takeout switch off: no void ticket.
	before := len(prints.all())
	e.Hash = "h3"
	e.Items = []order.ExternalItem{kitchenLine("1", "Cheese Steak", true), kitchenLine("3", "Garlic Knots", true)}
	mustUpsert(t, svc, "158", e)
	if n := len(prints.all()); n != before {
		t.Fatalf("void printed with pos_takeout off: %+v", prints.all())
	}
	// Switch on: the next void prints a VOID ticket.
	if _, err := svc.UpdateIntegrationSettings(ctx, func(s *order.IntegrationSettings) error { s.Tickets.PosTakeout = true; return nil }); err != nil {
		t.Fatal(err)
	}
	e.Hash = "h4"
	e.Items = []order.ExternalItem{kitchenLine("1", "Cheese Steak", true)}
	mustUpsert(t, svc, "158", e)
	calls := prints.all()
	if len(calls) != before+1 || calls[len(calls)-1].Mode != printer.ModeVoid || calls[len(calls)-1].Number != 99 {
		t.Fatalf("void ticket = %+v", calls)
	}

	// OK clears it for every tablet; a second OK has nothing to clear.
	drain(events)
	so, err := svc.AckPosChange(ctx, res.OrderID, "")
	if err != nil || so.Status.PosChange == nil || so.Status.PosChange.Pending() {
		t.Fatalf("ack = %+v %v", so, err)
	}
	if _, ok := eventOf(events, "order.updated"); !ok {
		t.Fatal("no order.updated after OK")
	}
	if _, err := svc.AckPosChange(ctx, res.OrderID, ""); !errors.Is(err, ErrNoChange) {
		t.Fatalf("second ack = %v", err)
	}
}

func TestSmallCheckShownLaterChimesAsNew(t *testing.T) {
	svc, _, events, _, _ := newIntegService(t)
	e := extOrder("dine_in", "h1")
	e.Items = []order.ExternalItem{kitchenLine("1", "Ranch", false)}
	e.BoardHidden = true
	mustUpsert(t, svc, "77", e)
	data, _ := eventOf(events, "order.added")
	var added struct {
		Chime bool `json:"chime"`
	}
	_ = json.Unmarshal(data, &added)
	if added.Chime {
		t.Fatal("a hidden check chimed")
	}
	e.Hash, e.BoardHidden = "h2", false
	e.Items = append(e.Items, kitchenLine("2", "Large Pizza", true))
	mustUpsert(t, svc, "77", e)
	data, ok := eventOf(events, "order.added")
	_ = json.Unmarshal(data, &added)
	if !ok || !added.Chime {
		t.Fatalf("grown check not published as new with the chime: %s", data)
	}
}

func TestFinishDueUsesTheSetting(t *testing.T) {
	ctx := context.Background()
	svc, repo, events, _, _ := newIntegService(t)
	snap := order.Snapshot{ID: uuid.NewString(), TenantID: uuid.NewString(), LocationID: uuid.NewString(), Source: "test",
		SourceEventID: "p1", PlacedAt: time.Now().UTC(), Fulfillment: order.Fulfillment{Type: "pickup"}}
	if _, err := repo.UpsertArrived(ctx, snap); err != nil {
		t.Fatal(err)
	}
	got, _ := repo.ClaimAccept(ctx, uuid.MustParse(snap.ID), 20, false)
	_ = repo.RecordAccepted(ctx, *got, false)
	if _, err := svc.Ready(ctx, snap.ID); err != nil {
		t.Fatal(err)
	}
	if n, _ := svc.FinishDue(ctx, time.Now()); n != 0 {
		t.Fatal("finished before its time")
	}
	if n, _ := svc.FinishDue(ctx, time.Now().Add(31*time.Minute)); n != 1 {
		t.Fatal("not finished after 30 min (default)")
	}
	if _, ok := eventOf(events, "order.updated"); !ok {
		t.Fatal("finish not published")
	}
	// 0 = never.
	if _, err := svc.UpdateIntegrationSettings(ctx, func(s *order.IntegrationSettings) error { s.Finishing.OnlinePickupMinutes = 0; return nil }); err != nil {
		t.Fatal(err)
	}
	if n, err := svc.FinishDue(ctx, time.Now().Add(10*time.Hour)); n != 0 || err != nil {
		t.Fatalf("finish with 0 = %d %v", n, err)
	}
}

func TestCalloutTestAndSettingsValidation(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _, _ := newIntegService(t)
	for _, bad := range []struct {
		kind   string
		number int
		voice  string
	}{{"test", 0, ""}, {"test", 10000, ""}, {"shout", 5, ""}, {"preview", 5, "../etc"}} {
		if _, err := svc.CalloutTest(ctx, bad.kind, bad.number, bad.voice); !errors.Is(err, ErrBadRequest) {
			t.Fatalf("%+v: err = %v", bad, err)
		}
	}
	res, err := svc.CalloutTest(ctx, "", 213, "kokoro:af_bella")
	if err != nil || res.ID == "" || res.HubConnected {
		t.Fatalf("test = %+v %v", res, err)
	}
	evs := feedOf(t, svc)
	last := evs[len(evs)-1]
	var d order.CalloutTestData
	_ = json.Unmarshal(last.Data, &d)
	if last.Type != order.FeedCalloutTest || d.Number != 213 || d.Kind != order.CalloutTest || d.Voice != "kokoro:af_bella" {
		t.Fatalf("feed = %s %s", last.Type, last.Data)
	}
	for _, change := range []func(*order.IntegrationSettings){
		func(s *order.IntegrationSettings) { s.Callout.Volume = 101 },
		func(s *order.IntegrationSettings) { s.Callout.MusicDip = -1 },
		func(s *order.IntegrationSettings) { s.Callout.RestoreSeconds = 11 },
		func(s *order.IntegrationSettings) { s.Finishing.OnlinePickupMinutes = 241 },
	} {
		if _, err := svc.UpdateIntegrationSettings(ctx, func(s *order.IntegrationSettings) error { change(s); return nil }); !errors.Is(err, ErrBadRequest) {
			t.Fatalf("bad settings saved: %v", err)
		}
	}
	if got := svc.IntegrationSettings().Callout; got != order.DefaultCallout() {
		t.Fatalf("callout changed by a refused save: %+v", got)
	}
}
