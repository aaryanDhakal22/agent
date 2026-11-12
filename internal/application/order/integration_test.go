package order

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
	"quiccpos/agent/internal/infra/database"
	"quiccpos/agent/internal/infra/database/repositories"
	"quiccpos/agent/internal/infra/ssebroker"
	"quiccpos/agent/internal/migrate"
	"quiccpos/agent/internal/observability"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
)

// 2.1.0 store hub surface through the service with the real SQLite repo.

type printCall struct {
	OrderID  string
	Mode     printer.Mode
	Number   int64
	Promised *time.Time // the snapshot's promised_at as printed
}

type printLog struct {
	mu    sync.Mutex
	calls []printCall
}

func (p *printLog) all() []printCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]printCall(nil), p.calls...)
}

type rpcCall struct {
	Type    string
	Payload map[string]any
}

type rpcLog struct {
	mu    sync.Mutex
	calls []rpcCall
	reply string
}

func (r *rpcLog) send(_ context.Context, typ string, payload any) (int, json.RawMessage, error) {
	raw, _ := json.Marshal(payload)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, rpcCall{Type: typ, Payload: m})
	reply := r.reply
	if reply == "" {
		reply = `{"ok":true}`
	}
	return 200, json.RawMessage(reply), nil
}

func (r *rpcLog) of(typ string) []rpcCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []rpcCall
	for _, c := range r.calls {
		if c.Type == typ {
			out = append(out, c)
		}
	}
	return out
}

func newIntegService(t *testing.T) (*Service, *repositories.OrderRepository, chan ssebroker.Event, *printLog, *rpcLog) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agent.db")
	if err := migrate.Run(ctx, path); err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	repo := repositories.NewOrderRepository(db, zerolog.Nop())
	meters, _ := observability.NewMeters()
	broker := ssebroker.New()
	events, _ := broker.Subscribe()
	log := &printLog{}
	print := func(_ context.Context, snap order.Snapshot, mode printer.Mode) (printer.Outcome, error) {
		log.mu.Lock()
		defer log.mu.Unlock()
		n := int64(0)
		if snap.KitchenNumber != nil {
			n = *snap.KitchenNumber
		}
		log.calls = append(log.calls, printCall{OrderID: snap.ID, Mode: mode, Number: n, Promised: snap.Fulfillment.PromisedAt})
		printed := []printer.TicketResult{{Kind: printer.TicketStation}}
		switch mode {
		case printer.ModeFutureEarly, printer.ModeFutureBoth: // 2.4.0: the switches' early tickets; 2.4.3 PRINT NOW
			printed = append(printed, printer.TicketResult{Kind: printer.TicketReceipt})
		case printer.ModeFutureReceipt:
			printed = []printer.TicketResult{{Kind: printer.TicketReceipt}}
		}
		return printer.Outcome{Printed: printed}, nil
	}
	svc := NewService(repo, repositories.NewSettingsRepository(db, zerolog.Nop()), print, nil, broker, zerolog.Nop(), meters)
	svc.legacyArrival = true // the kept accept path (§2.10aa: real arrivals are placed)
	noAutoAssign(t, repo)
	svc.LoadIntegrationSettings(ctx)
	rpc := &rpcLog{}
	svc.SetGeo(GeoDeps{Send: rpc.send, Linked: func() bool { return true }})
	tenant, location := uuid.NewString(), uuid.NewString()
	svc.SetStoreIDs(func() (string, string) { return tenant, location })
	return svc, repo, events, log, rpc
}

func storedOf(t *testing.T, svc *Service, id string) order.StoredOrder {
	t.Helper()
	so, err := svc.GetByID(context.Background(), uuid.MustParse(id))
	if err != nil {
		t.Fatal(err)
	}
	return *so
}

func feedOf(t *testing.T, svc *Service) []order.FeedEvent {
	t.Helper()
	page, err := svc.Feed(context.Background(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	return page.Events
}

func TestFeedLongPoll(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _, _ := newIntegService(t)
	page, err := svc.Feed(ctx, 0, 0)
	if err != nil || len(page.Events) != 0 || page.Next != 0 {
		t.Fatalf("empty feed = %+v %v", page, err)
	}
	go func() {
		time.Sleep(150 * time.Millisecond)
		svc.UpdateIntegrationSettings(ctx, func(s *order.IntegrationSettings) error { s.Times.DefaultDeliveryMinutes = 40; return nil })
	}()
	start := time.Now()
	page, err = svc.Feed(ctx, 0, 5*time.Second)
	if err != nil || len(page.Events) != 1 || page.Events[0].Type != order.FeedSettingsChanged || page.Next != 1 {
		t.Fatalf("long poll = %+v %v", page, err)
	}
	if waited := time.Since(start); waited > 900*time.Millisecond {
		t.Fatalf("woke after %v (should be right after the write)", waited)
	}
	// Nothing new: waits out the time and answers with the same cursor.
	start = time.Now()
	if page, _ = svc.Feed(ctx, 1, 300*time.Millisecond); len(page.Events) != 0 || page.Next != 1 || time.Since(start) < 250*time.Millisecond {
		t.Fatalf("idle poll = %+v after %v", page, time.Since(start))
	}
	// A cursor from another Pi's feed (past the newest): starts over.
	if page, _ = svc.Feed(ctx, 500, 0); len(page.Events) != 1 || page.Next != 1 {
		t.Fatalf("reset cursor = %+v", page)
	}
	// A cancelled request returns at once.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if page, _ = svc.Feed(cctx, 1, 5*time.Second); len(page.Events) != 0 {
		t.Fatalf("cancelled = %+v", page)
	}
}

func TestFutureOrderScheduleRaiseAccept(t *testing.T) {
	ctx := context.Background()
	svc, _, events, prints, _ := newIntegService(t)
	promised := time.Now().Add(2 * time.Hour).UTC()
	s := order.Snapshot{ID: uuid.NewString(), TenantID: uuid.NewString(), LocationID: uuid.NewString(), Source: "online",
		SourceEventID: "9", Fulfillment: order.Fulfillment{Type: "delivery", PromisedAt: &promised, Address: &order.Address{Line1: "1 Main"}}}
	if err := svc.OnArrival(ctx, s); err != nil {
		t.Fatal(err)
	}
	so := storedOf(t, svc, s.ID)
	if !so.Status.Future {
		t.Fatal("not future")
	}
	// ASAP orders are not future; future orders off → ASAP.
	asap := readyDelivery(t, svc, time.Minute)
	if _, err := svc.Schedule(ctx, asap, 20); !errors.Is(err, order.ErrInvalidState) {
		t.Fatalf("schedule accepted order = %v", err)
	}
	if _, err := svc.Schedule(ctx, s.ID, 0); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("lead 0 = %v", err)
	}
	drain(events)
	got, err := svc.Schedule(ctx, s.ID, 20)
	if err != nil || got.Status.State != order.StateScheduled || !got.Status.Future || *got.Status.ScheduledLeadMinutes != 20 ||
		!got.Status.RingAt.Equal(promised.Add(-20*time.Minute).Truncate(time.Millisecond)) {
		t.Fatalf("schedule = %+v %v", got, err)
	}
	if got, err = svc.Schedule(ctx, s.ID, 30); err != nil || *got.Status.ScheduledLeadMinutes != 30 {
		t.Fatalf("edit lead = %+v %v", got, err)
	}
	if len(prints.all()) != 1 { // only the ASAP order printed
		t.Fatal("scheduling printed")
	}
	// Out of pools and AUTO while scheduled.
	ann, _ := svc.CreateDriver(ctx, "Ann")
	svc.ClockIn(ctx, ann.ID)
	if _, err := svc.Pool(ctx, []string{s.ID}, ann.ID); !errors.Is(err, order.ErrInvalidState) {
		t.Fatalf("pool scheduled = %v", err)
	}
	plan, err := svc.PlanDeliveries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(plan)
	if strings.Contains(string(raw), s.ID) {
		t.Fatal("AUTO planned a scheduled order")
	}
	// Future orders off: an ASAP-style order again (no schedule).
	svc.UpdateIntegrationSettings(ctx, func(x *order.IntegrationSettings) error { x.Features.FutureOrders = false; return nil })
	s2 := s
	s2.ID = uuid.NewString()
	svc.OnArrival(ctx, s2)
	if _, err := svc.Schedule(ctx, s2.ID, 20); !errors.Is(err, ErrNotFuture) {
		t.Fatalf("future off = %v", err)
	}
	svc.UpdateIntegrationSettings(ctx, func(x *order.IntegrationSettings) error { x.Features.FutureOrders = true; return nil })
	if _, err := svc.Schedule(ctx, s2.ID, 10); err != nil { // future again: it waits too (rings later)
		t.Fatal(err)
	}

	// At ring_at it rings again (rerung), then escalates after 2 minutes.
	ring := promised.Add(-30 * time.Minute)
	if n, _ := svc.RaiseDue(ctx, ring.Add(-time.Second)); n != 0 {
		t.Fatal("rang early")
	}
	drain(events)
	if n, err := svc.RaiseDue(ctx, ring.Add(time.Second)); n != 1 || err != nil {
		t.Fatalf("raise = %d %v", n, err)
	}
	if data, ok := eventOf(events, "order.arrived"); !ok || !strings.Contains(string(data), `"rerung":true`) {
		t.Fatalf("ring = %s", data)
	}
	if n, _ := svc.EscalateOverdue(ctx, ring.Add(time.Minute), 2*time.Minute); n != 0 {
		t.Fatal("escalated before 2 min")
	}
	if n, _ := svc.EscalateOverdue(ctx, ring.Add(2*time.Minute+2*time.Second), 2*time.Minute); n != 1 {
		t.Fatal("no escalation after 2 min")
	}
	// Second confirm: no time dialog — prep = the lead; prints.
	acc, err := svc.Accept(ctx, uuid.MustParse(s.ID), nil)
	if err != nil || *acc.Status.PrepMinutes != 30 || acc.Status.KitchenNumber == nil {
		t.Fatalf("accept = %+v %v", acc, err)
	}
	if p := prints.all(); len(p) != 2 || p[1].OrderID != s.ID || p[1].Mode != printer.ModeAccept {
		t.Fatalf("prints = %+v", p)
	}
}

func TestGeoCheckChoose(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _, rpc := newIntegService(t)
	id := readyDelivery(t, svc, time.Minute)
	cands := []order.GeoCandidate{}
	for i, l := range []string{"931 Taylor Ave", "931 W Taylor Ave", "913 Taylor Ave", "931 Tyler Ave"} {
		cands = append(cands, order.GeoCandidate{Label: l + ", Towson MD", Line1: l, City: "Towson", State: "MD", PostalCode: "21286",
			Lat: 39.4 + float64(i)/100, Lng: -76.6})
	}
	so, err := svc.OnGeo(ctx, order.GeoUpdate{OrderID: id, State: order.GeoCheck, Candidates: cands})
	if err != nil || so.Geo.State != order.GeoCheck || len(so.Geo.Candidates) != 4 || so.Geo.Located() {
		t.Fatalf("check = %+v %v", so.Geo, err)
	}
	if planOrder(*so).Located {
		t.Fatal("AUTO must treat check as not located")
	}
	if err := svc.ChooseCandidate(ctx, id, 4); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("index 4 = %v", err)
	}
	if err := svc.ChooseCandidate(ctx, id, 1); err != nil {
		t.Fatal(err)
	}
	if c := rpc.of(MsgGeoChoose); len(c) != 1 || c[0].Payload["order_id"] != id || c[0].Payload["index"] != float64(1) {
		t.Fatalf("geo.choose = %+v", c)
	}
	// main's answer: located, pinned, with the chosen address.
	lat, lng := 39.41, -76.6
	addr := &order.ConfirmedAddress{Line1: "931 W Taylor Ave", City: "Towson", State: "MD", PostalCode: "21286"}
	upd := order.GeoUpdate{OrderID: id, State: order.GeoLocated, Lat: &lat, Lng: &lng, Quality: "pinned", ConfirmedAddress: addr, Confirmed: true}
	if so, err = svc.OnGeo(ctx, upd); err != nil || so.Geo.ConfirmedAddress == nil || len(so.Geo.Candidates) != 0 {
		t.Fatalf("located = %+v %v", so.Geo, err)
	}
	if err := svc.ChooseCandidate(ctx, id, 0); !errors.Is(err, order.ErrInvalidState) {
		t.Fatalf("choose when located = %v", err)
	}
}

func TestIntegrationStatusAndLinkDown(t *testing.T) {
	ctx := context.Background()
	svc, _, events, _, _ := newIntegService(t)
	if v := svc.IntegrationView(time.Now()); v.Connected || v.LastSeenAt != nil || len(v.Problems) != 0 || v.InsertMode != "off" {
		t.Fatalf("no hub yet = %+v", v)
	}
	drain(events)
	if err := svc.SetIntegrationStatus(ctx, order.IntegrationStatus{OK: true, InsertMode: "off",
		Problems: []order.Problem{{Code: "speaker", Text: "No speaker", Severity: "serious"}}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := eventOf(events, "integration.status"); !ok {
		t.Fatal("no integration.status")
	}
	v := svc.IntegrationView(time.Now())
	if !v.Connected || !v.OK || len(v.Problems) != 1 || v.Problems[0].Severity != "serious" {
		t.Fatalf("view = %+v", v)
	}
	// Silent for over 60 s: link down, serious; published once.
	later := time.Now().Add(61 * time.Second)
	v = svc.IntegrationView(later)
	if v.Connected || len(v.Problems) != 1 || v.Problems[0].Code != "pos_link_down" || v.Problems[0].Text != "Hub link down" || v.Problems[0].Severity != "serious" {
		t.Fatalf("stale = %+v", v)
	}
	drain(events)
	svc.CheckIntegrationLink(later)
	svc.CheckIntegrationLink(later.Add(time.Minute))
	n := 0
	for {
		if _, ok := eventOf(events, "integration.status"); !ok {
			break
		}
		n++
	}
	if n != 1 {
		t.Fatalf("link-down published %d times", n)
	}
	if err := svc.SetIntegrationStatus(ctx, order.IntegrationStatus{InsertMode: "turbo"}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("bad mode = %v", err)
	}
	// The last status survives a restart (but reads disconnected once old).
	svc.integ.hub = order.IntegrationStatus{}
	svc.LoadIntegrationSettings(ctx)
	if v := svc.IntegrationView(time.Now()); v.InsertMode != "off" || v.LastSeenAt == nil {
		t.Fatalf("reloaded = %+v", v)
	}
}

func TestSettingsChangedEverywhere(t *testing.T) {
	ctx := context.Background()
	svc, _, events, _, _ := newIntegService(t)
	drain(events)
	set, err := svc.UpdateIntegrationSettings(ctx, func(s *order.IntegrationSettings) error {
		s.Tickets.FutureKitchen, s.Times.DefaultPickupMinutes = true, 20
		return nil
	})
	if err != nil || !set.Tickets.FutureKitchen || set.Times.DefaultPickupMinutes != 20 {
		t.Fatalf("set = %+v %v", set, err)
	}
	if data, ok := eventOf(events, "settings.updated"); !ok || !strings.Contains(string(data), `"future_kitchen":true`) {
		t.Fatalf("settings.updated = %s", data)
	}
	f, err := svc.Floor(ctx)
	if err != nil || !reflect.DeepEqual(f.Settings, set) {
		t.Fatalf("floor settings = %+v %v", f.Settings, err)
	}
	feed := feedOf(t, svc)
	if len(feed) != 1 || feed[0].Type != order.FeedSettingsChanged {
		t.Fatalf("feed = %+v", feed)
	}
	if _, err := svc.UpdateIntegrationSettings(ctx, func(s *order.IntegrationSettings) error { s.Times.DefaultDeliveryMinutes = 0; return nil }); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("bad time = %v", err)
	}
	if !reflect.DeepEqual(svc.IntegrationSettings(), set) {
		t.Fatal("a refused change leaked into the settings")
	}
}

// Auto-accept on: a future order is scheduled on arrival with its default
// lead (a delivery: the delivery target; nothing prints), and auto-accepted
// when it rings again.
func TestAutoAcceptSchedulesFutureOrders(t *testing.T) {
	ctx := context.Background()
	svc, _, events, prints, _ := newIntegService(t)
	if err := svc.settings.SetAutoAccept(ctx, true); err != nil {
		t.Fatal(err)
	}
	promised := time.Now().Add(3 * time.Hour).UTC()
	s := order.Snapshot{ID: uuid.NewString(), TenantID: uuid.NewString(), LocationID: uuid.NewString(), Source: "online",
		SourceEventID: "f1", Fulfillment: order.Fulfillment{Type: "delivery", PromisedAt: &promised, Address: &order.Address{Line1: "1 Main"}}}
	drain(events)
	if err := svc.OnArrival(ctx, s); err != nil {
		t.Fatal(err)
	}
	so := storedOf(t, svc, s.ID)
	// 2.4.0: a future delivery's lead is the store's delivery target (45).
	if so.Status.State != order.StateScheduled || so.Status.ScheduledLeadMinutes == nil || *so.Status.ScheduledLeadMinutes != 45 ||
		!so.Status.RingAt.Equal(promised.Add(-45*time.Minute).Truncate(time.Millisecond)) {
		t.Fatalf("auto-scheduled = %+v", so.Status)
	}
	if len(prints.all()) != 0 {
		t.Fatal("a future order printed on arrival")
	}
	if _, ok := eventOf(events, "order.arrived"); ok {
		t.Fatal("a future order rang under auto-accept")
	}
	// An ASAP order is still accepted on arrival.
	asap := order.Snapshot{ID: uuid.NewString(), TenantID: uuid.NewString(), LocationID: uuid.NewString(), Source: "online",
		SourceEventID: "f2", Fulfillment: order.Fulfillment{Type: "pickup"}, Money: order.Money{Total: 900}}
	if err := svc.OnArrival(ctx, asap); err != nil {
		t.Fatal(err)
	}
	if got := storedOf(t, svc, asap.ID); got.Status.State != order.StateAccepted || !got.Status.AutoAccepted {
		t.Fatalf("asap = %s auto %v", got.Status.State, got.Status.AutoAccepted)
	}
	// At ring_at it is accepted (prep = its lead) and printed.
	if n, err := svc.RaiseDue(ctx, promised.Add(-45*time.Minute).Add(time.Second)); n != 1 || err != nil {
		t.Fatalf("raise = %d %v", n, err)
	}
	got := storedOf(t, svc, s.ID)
	if got.Status.State != order.StateAccepted || *got.Status.PrepMinutes != 45 || got.Status.KitchenNumber == nil ||
		!got.Status.AutoAccepted {
		t.Fatalf("at ring = %+v", got.Status)
	}
	// §2.10aa: placed like a called-in order (stations + a delivery's copies).
	if p := prints.all(); len(p) != 2 || p[1].OrderID != s.ID || p[1].Mode != printer.ModePlaced {
		t.Fatalf("prints = %+v", p)
	}
}

// 2.4.0 (owner): with the future switches on, a future order's first ACCEPT
// prints its early tickets (once - a time change doesn't reprint); its
// CONFIRM then prints the kitchen tickets only (the receipt is out). A
// future delivery's default lead is the store's delivery target.
func TestFutureOrderEarlyTickets(t *testing.T) {
	ctx := context.Background()
	svc, _, _, prints, _ := newIntegService(t)
	svc.UpdateIntegrationSettings(ctx, func(x *order.IntegrationSettings) error {
		x.Tickets.FutureKitchen, x.Tickets.FutureReceipt = true, true
		return nil
	})
	promised := time.Now().Add(2 * time.Hour).UTC()
	s := order.Snapshot{ID: uuid.NewString(), TenantID: uuid.NewString(), LocationID: uuid.NewString(), Source: "online",
		SourceEventID: "19", Fulfillment: order.Fulfillment{Type: "delivery", PromisedAt: &promised, Address: &order.Address{Line1: "1 Main"}}}
	if err := svc.OnArrival(ctx, s); err != nil {
		t.Fatal(err)
	}
	if lead := svc.FutureLead(ctx, storedOf(t, svc, s.ID)); lead != int(order.DefaultDeliveryPrefs.DeliveryTargetMinutes) {
		t.Fatalf("delivery lead = %d", lead)
	}
	if _, err := svc.Schedule(ctx, s.ID, 45); err != nil {
		t.Fatal(err)
	}
	svc.WaitBackground()
	if _, err := svc.Schedule(ctx, s.ID, 60); err != nil { // a new time: nothing reprints
		t.Fatal(err)
	}
	svc.WaitBackground()
	p := prints.all()
	if len(p) != 1 || p[0].Mode != printer.ModeFutureEarly {
		t.Fatalf("prints = %+v", p)
	}
	so := storedOf(t, svc, s.ID)
	if !so.Status.EarlyKitchen || !so.Status.EarlyReceipt {
		t.Fatalf("early flags = %+v", so.Status)
	}
	if _, err := svc.RaiseDue(ctx, promised.Add(-59*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Accept(ctx, uuid.MustParse(s.ID), nil); err != nil {
		t.Fatal(err)
	}
	if p := prints.all(); len(p) != 2 || p[1].Mode != printer.ModeAcceptKitchen {
		t.Fatalf("confirm prints = %+v", p)
	}

	// Switches off: nothing early.
	svc.UpdateIntegrationSettings(ctx, func(x *order.IntegrationSettings) error {
		x.Tickets.FutureKitchen, x.Tickets.FutureReceipt = false, false
		return nil
	})
	s2 := s
	s2.ID = uuid.NewString()
	svc.OnArrival(ctx, s2)
	if _, err := svc.Schedule(ctx, s2.ID, 30); err != nil {
		t.Fatal(err)
	}
	svc.WaitBackground()
	if p := prints.all(); len(p) != 2 {
		t.Fatalf("switches off printed: %+v", p)
	}
}
