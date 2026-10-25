package order

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
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
	OrderID string
	Mode    printer.Mode
	Number  int64
}

type printLog struct {
	mu     sync.Mutex
	calls  []printCall
	insert bool // the Outcome's InsertExpected on an accept print
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
		log.calls = append(log.calls, printCall{OrderID: snap.ID, Mode: mode, Number: n})
		return printer.Outcome{Printed: []printer.TicketResult{{Kind: printer.TicketStation}},
			InsertExpected: log.insert && mode == printer.ModeAccept}, nil
	}
	svc := NewService(repo, repositories.NewSettingsRepository(db, zerolog.Nop()), print, nil, broker, zerolog.Nop(), meters)
	svc.LoadIntegrationSettings(ctx)
	rpc := &rpcLog{}
	svc.SetGeo(GeoDeps{Send: rpc.send, Linked: func() bool { return true }})
	tenant, location := uuid.NewString(), uuid.NewString()
	svc.SetStoreIDs(func() (string, string) { return tenant, location })
	return svc, repo, events, log, rpc
}

func extOrder(ftype, hash string) order.ExternalOrder {
	sent := time.Now().Add(-2 * time.Minute).UTC()
	e := order.ExternalOrder{Source: "softtouch", Hash: hash, DisplayNumber: 99, CheckType: ftype, SentAt: &sent, PlacedAt: &sent,
		Customer: order.ExternalCustomer{DisplayName: "Pat Doe", Phone: "4105550100"},
		Items: []order.ExternalItem{{Name: "14\" Cheese Pizza", Quantity: 1, UnitPriceCents: 1199, LineTotalCents: 1199, PrinterID: "p-pizza",
			Children: []order.ExternalItem{{Name: "Pineapple", Quantity: 1, UnitPriceCents: 150, LineTotalCents: 150}}}},
		Money:        order.ExternalMoney{SubtotalCents: 1349, TaxCents: 81, TotalCents: 1430},
		CashDueCents: 1430}
	if ftype == "delivery" {
		e.Address = &order.Address{Line1: "931 Taylor Ave", City: "Towson", State: "MD", PostalCode: "21286"}
	}
	return e
}

func mustUpsert(t *testing.T, svc *Service, ref string, e order.ExternalOrder) *UpsertResult {
	t.Helper()
	res, err := svc.UpsertExternal(context.Background(), ref, e)
	if err != nil {
		t.Fatal(err)
	}
	svc.WaitBackground()
	return res
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

func TestExternalOrderLifecycle(t *testing.T) {
	ctx := context.Background()
	svc, repo, events, prints, rpc := newIntegService(t)
	drain(events)

	res := mustUpsert(t, svc, "1276237", extOrder("delivery", "h1"))
	if !res.Created || res.Changed {
		t.Fatalf("create = %+v", res)
	}
	id, err := uuid.Parse(res.OrderID)
	if err != nil || id.Version() != 7 {
		t.Fatalf("order id %q is not a UUIDv7", res.OrderID)
	}
	data, ok := eventOf(events, "order.added")
	if !ok || !strings.Contains(string(data), `"chime":true`) || !strings.Contains(string(data), `"display_number":99`) {
		t.Fatalf("order.added = %s", data)
	}
	so := storedOf(t, svc, res.OrderID)
	if so.Status.State != order.StateAccepted || !so.Status.LocalOnly || so.Status.KitchenNumber != nil || *so.Status.PrepMinutes != 35 ||
		so.Order.Items[0].Routing != order.RoutingMatched || so.Order.Items[0].Children[0].Name != "Pineapple" {
		t.Fatalf("stored = %+v", so)
	}
	if len(prints.all()) != 0 {
		t.Fatal("POS switches are off: nothing prints")
	}
	// main is asked to locate it with the address only.
	loc := rpc.of(MsgGeoLocate)
	if len(loc) != 1 || loc[0].Payload["order_id"] != res.OrderID {
		t.Fatalf("geo.locate = %+v", loc)
	}
	raw, _ := json.Marshal(loc[0].Payload)
	if strings.Contains(string(raw), "Pat") || strings.Contains(string(raw), "4105550100") || !strings.Contains(string(raw), "931 Taylor Ave") {
		t.Fatalf("geo.locate payload = %s", raw)
	}

	// Same hash: nothing.
	drain(events)
	if res2 := mustUpsert(t, svc, "1276237", extOrder("delivery", "h1")); res2.Created || res2.Changed || res2.OrderID != res.OrderID {
		t.Fatalf("same hash = %+v", res2)
	}
	if _, ok := eventOf(events, "order.updated"); ok {
		t.Fatal("same hash published an update")
	}
	// Changed + paid: updated, no reprint, no re-locate (same address).
	e := extOrder("delivery", "h2")
	e.Paid, e.CashDueCents, e.TipCents = true, 0, 300
	if res2 := mustUpsert(t, svc, "1276237", e); !res2.Changed {
		t.Fatalf("changed = %+v", res2)
	}
	if _, ok := eventOf(events, "order.updated"); !ok {
		t.Fatal("no order.updated")
	}
	so = storedOf(t, svc, res.OrderID)
	if !so.Status.Paid || so.Status.CashDueCents != 0 || so.Status.TipCents != 300 || so.Order.Money.Tip != 300 || len(prints.all()) != 0 || len(rpc.of(MsgGeoLocate)) != 1 {
		t.Fatalf("after change: %+v prints=%d", so.Status, len(prints.all()))
	}
	// New address: located again.
	e.Hash, e.Address = "h3", &order.Address{Line1: "1 York Rd", City: "Towson", State: "MD"}
	mustUpsert(t, svc, "1276237", e)
	if len(rpc.of(MsgGeoLocate)) != 2 {
		t.Fatal("address change not re-located")
	}

	// A counter check with no kitchen item: stored hidden, no chime.
	drain(events)
	hidden := extOrder("dine_in", "c1")
	hidden.BoardHidden = true
	mustUpsert(t, svc, "500", hidden)
	if data, _ := eventOf(events, "order.added"); !strings.Contains(string(data), `"chime":false`) || !strings.Contains(string(data), `"board_hidden":true`) {
		t.Fatalf("hidden order.added = %s", data)
	}

	// Void: cancelled, out of its pool, main forgets the point.
	ann, _ := svc.CreateDriver(ctx, "Ann", "")
	svc.ClockIn(ctx, ann.ID)
	if _, err := svc.Pool(ctx, []string{res.OrderID}, ann.ID); err != nil {
		t.Fatal(err)
	}
	so2, err := svc.VoidExternal(ctx, "1276237")
	svc.WaitBackground()
	if err != nil || so2.Status.State != order.StateCancelled || so2.Status.PoolDriverID != "" {
		t.Fatalf("void = %+v %v", so2, err)
	}
	if f := rpc.of(MsgGeoForget); len(f) != 1 || f[0].Payload["order_id"] != res.OrderID {
		t.Fatalf("geo.forget = %+v", f)
	}
	types := []string{}
	for _, ev := range feedOf(t, svc) {
		types = append(types, ev.Type)
	}
	if strings.Join(types, ",") != "order.cancelled" {
		t.Fatalf("feed = %v", types)
	}

	// Never synced to main as an order.
	evs, _ := repo.ListUnsyncedEvents(ctx, 1000)
	for _, ev := range evs {
		if ev.OrderID == res.OrderID || strings.Contains(string(ev.Payload), res.OrderID) {
			t.Fatalf("POS order in main's outbox: %s %s", ev.Type, ev.Payload)
		}
	}

	// Pull-in off: 409.
	if _, err := svc.UpdateIntegrationSettings(ctx, func(s *order.IntegrationSettings) error { s.Features.PosPull = false; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpsertExternal(ctx, "77", extOrder("pickup", "x")); !errors.Is(err, ErrPosPullOff) {
		t.Fatalf("pull off = %v", err)
	}
	for _, bad := range []func(*order.ExternalOrder){
		func(e *order.ExternalOrder) { e.CheckType = "drive_thru" },
		func(e *order.ExternalOrder) { e.Hash = "" },
		func(e *order.ExternalOrder) { e.Source = "other" },
	} {
		e := extOrder("pickup", "y")
		bad(&e)
		svc.UpdateIntegrationSettings(ctx, func(s *order.IntegrationSettings) error { s.Features.PosPull = true; return nil })
		if _, err := svc.UpsertExternal(ctx, "78", e); !errors.Is(err, ErrBadRequest) {
			t.Fatalf("bad body = %v", err)
		}
	}
}

func TestPOSPrintsPerSwitch(t *testing.T) {
	ctx := context.Background()
	svc, _, _, prints, _ := newIntegService(t)
	if _, err := svc.UpdateIntegrationSettings(ctx, func(s *order.IntegrationSettings) error { s.Tickets.PosTakeout = true; return nil }); err != nil {
		t.Fatal(err)
	}
	mustUpsert(t, svc, "1", extOrder("delivery", "a")) // delivery switch off
	res := mustUpsert(t, svc, "2", extOrder("pickup", "b"))
	got := prints.all()
	if len(got) != 1 || got[0].OrderID != res.OrderID || got[0].Mode != printer.ModeAccept || got[0].Number != 99 {
		t.Fatalf("prints = %+v", got)
	}
	// A changed check is not printed again.
	mustUpsert(t, svc, "2", extOrder("pickup", "b2"))
	if len(prints.all()) != 1 {
		t.Fatal("reprinted on change")
	}
	// Reprint and Resend work for POS orders (the check number on the paper).
	if _, err := svc.Reprint(ctx, uuid.MustParse(res.OrderID)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.ResendKitchen(ctx, uuid.MustParse(res.OrderID)); err != nil {
		t.Fatal(err)
	}
	got = prints.all()
	if len(got) != 3 || got[1].Mode != printer.ModeReceipt || got[2].Mode != printer.ModeKitchen || got[2].Number != 99 {
		t.Fatalf("reprint/resend = %+v", got)
	}
}

func TestPOSDriverMirror(t *testing.T) {
	ctx := context.Background()
	svc, repo, events, _, _ := newIntegService(t)
	ann, _ := svc.CreateDriver(ctx, "Ann", "12")
	bob, _ := svc.CreateDriver(ctx, "Bob", "13") // stays off
	cat, _ := svc.CreateDriver(ctx, "Cat", "14")
	svc.ClockIn(ctx, ann.ID)
	svc.ClockIn(ctx, cat.ID)
	_ = bob
	// Cat goes out with an online order; Ann has one pooled.
	catRun := readyDelivery(t, svc, time.Minute)
	if _, err := svc.AssignDriver(ctx, cat.ID, []string{catRun}); err != nil {
		t.Fatal(err)
	}
	pooled := readyDelivery(t, svc, time.Minute)
	if _, err := svc.Pool(ctx, []string{pooled}, ann.ID); err != nil {
		t.Fatal(err)
	}
	feedBefore := len(feedOf(t, svc))

	// Available driver: cooking → ready → sent out alone; their pool unassigned.
	e := extOrder("delivery", "a")
	num := "12"
	e.DriverPosNumber = &num
	a := mustUpsert(t, svc, "100", e)
	so := storedOf(t, svc, a.OrderID)
	if so.Status.State != order.StateOnDelivery || so.Status.DriverID != ann.ID || so.Status.PosDriverNote != "" {
		t.Fatalf("mirror available = %+v", so.Status)
	}
	if p := storedOf(t, svc, pooled); p.Status.PoolDriverID != "" {
		t.Fatal("the driver's other pooled order stayed with an out driver")
	}
	ds, _ := svc.ListDrivers(ctx)
	for _, d := range ds {
		if d.ID == ann.ID && d.Status != order.DriverOut {
			t.Fatalf("Ann = %s", d.Status)
		}
	}
	var types []string
	for _, ev := range feedOf(t, svc)[feedBefore:] {
		types = append(types, ev.Type)
	}
	if strings.Join(types, ",") != "order.ready,run.sent" {
		t.Fatalf("feed = %v", types)
	}
	// The same number again (unchanged hash or not) is not mirrored twice.
	e.Hash = "a2"
	mustUpsert(t, svc, "100", e)

	// Driver already out: joins the run, no new run.sent.
	n14 := "14"
	b := extOrder("delivery", "b")
	b.DriverPosNumber = &n14
	before := len(feedOf(t, svc))
	bres := mustUpsert(t, svc, "101", b)
	if so := storedOf(t, svc, bres.OrderID); so.Status.State != order.StateOnDelivery || so.Status.DriverID != cat.ID {
		t.Fatalf("mirror out = %+v", so.Status)
	}
	for _, ev := range feedOf(t, svc)[before:] {
		if ev.Type == order.FeedRunSent {
			t.Fatal("joining a run wrote run.sent")
		}
	}
	// Cat returns: both delivered.
	if out, err := svc.DriverReturned(ctx, cat.ID); err != nil || len(out) != 2 {
		t.Fatalf("returned = %d %v", len(out), err)
	}

	// Off / unknown driver: a note + a warn toast, the order stays.
	drain(events)
	for _, tc := range []struct{ num, want string }{{"13", "not clocked in"}, {"99", "no driver has that number"}} {
		c := extOrder("delivery", "c"+tc.num)
		n := tc.num
		c.DriverPosNumber = &n
		cres := mustUpsert(t, svc, "2"+tc.num, c)
		so := storedOf(t, svc, cres.OrderID)
		if so.Status.State != order.StateAccepted || !strings.Contains(so.Status.PosDriverNote, tc.want) || !strings.HasPrefix(so.Status.PosDriverNote, "POS says #"+tc.num) {
			t.Fatalf("%s: %+v", tc.num, so.Status)
		}
		if data, ok := eventOf(events, "integration.toast"); !ok || !strings.Contains(string(data), `"severity":"warn"`) {
			t.Fatalf("%s: toast = %s", tc.num, data)
		}
	}
	// Main never hears of POS orders, only anonymous stops.
	evs, _ := repo.ListUnsyncedEvents(ctx, 1000)
	for _, ev := range evs {
		if ev.Type == order.FloorEventOrderStatus {
			var st order.OrderStatusEvent
			_ = json.Unmarshal(ev.Payload, &st)
			if st.AgentStatus != nil && st.AgentStatus.LocalOnly {
				t.Fatalf("local order synced: %s", ev.Payload)
			}
		}
		if ev.Type == order.FloorEventDeliveryRun && strings.Contains(string(ev.Payload), a.OrderID) {
			t.Fatalf("delivery.run names a POS order: %s", ev.Payload)
		}
	}
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

func TestOnlineAcceptFeedAndPatch(t *testing.T) {
	ctx := context.Background()
	svc, _, events, _, _ := newIntegService(t)
	s := order.Snapshot{ID: uuid.NewString(), TenantID: uuid.NewString(), LocationID: uuid.NewString(), Source: "online",
		SourceEventID: "1", Fulfillment: order.Fulfillment{Type: "pickup"}, Money: order.Money{Total: 2000}}
	if err := svc.OnArrival(ctx, s); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Accept(ctx, uuid.MustParse(s.ID), nil); err != nil {
		t.Fatal(err)
	}
	feed := feedOf(t, svc)
	if len(feed) != 1 || feed[0].Type != order.FeedOrderAccepted {
		t.Fatalf("feed = %+v", feed)
	}
	var acc order.OrderAcceptedData
	if err := json.Unmarshal(feed[0].Data, &acc); err != nil || acc.Order.Order.ID != s.ID || acc.Order.Status.CashDueCents != 2000 {
		t.Fatalf("order.accepted = %s", feed[0].Data)
	}
	drain(events)
	paid, check := true, int64(4412)
	so, err := svc.PatchOnline(ctx, s.ID, order.OnlinePatch{Paid: &paid, PosCheckNumber: &check, SetFlag: true,
		Flag: &order.Flag{Text: "Settle manually: unmapped item X", Severity: "warn"}})
	if err != nil || !so.Status.Paid || *so.Status.PosCheckNumber != 4412 || so.Status.Flag == nil || so.Status.Flag.Severity != "warn" {
		t.Fatalf("patch = %+v %v", so, err)
	}
	if _, ok := eventOf(events, "order.updated"); !ok {
		t.Fatal("no order.updated")
	}
	if so, _ = svc.PatchOnline(ctx, s.ID, order.OnlinePatch{SetFlag: true}); so.Status.Flag != nil || !so.Status.Paid {
		t.Fatalf("clear flag = %+v", so.Status)
	}
	if _, err := svc.PatchOnline(ctx, s.ID, order.OnlinePatch{SetFlag: true, Flag: &order.Flag{Text: "x", Severity: "loud"}}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("bad severity = %v", err)
	}
	if _, err := svc.PatchOnline(ctx, uuid.NewString(), order.OnlinePatch{Paid: &paid}); !errors.Is(err, order.ErrNotFound) {
		t.Fatalf("unknown = %v", err)
	}
	pos := mustUpsert(t, svc, "1", extOrder("pickup", "a"))
	if _, err := svc.PatchOnline(ctx, pos.OrderID, order.OnlinePatch{Paid: &paid}); !errors.Is(err, order.ErrInvalidState) {
		t.Fatalf("POS patch = %v", err)
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
	ann, _ := svc.CreateDriver(ctx, "Ann", "")
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
	// Second confirm: no time dialog — prep = the lead; prints; order.accepted.
	acc, err := svc.Accept(ctx, uuid.MustParse(s.ID), nil)
	if err != nil || *acc.Status.PrepMinutes != 30 || acc.Status.KitchenNumber == nil {
		t.Fatalf("accept = %+v %v", acc, err)
	}
	if p := prints.all(); len(p) != 2 || p[1].OrderID != s.ID || p[1].Mode != printer.ModeAccept {
		t.Fatalf("prints = %+v", p)
	}
	found := false
	for _, ev := range feedOf(t, svc) {
		found = found || (ev.Type == order.FeedOrderAccepted && strings.Contains(string(ev.Data), s.ID))
	}
	if !found {
		t.Fatal("no order.accepted at the second confirm")
	}
}

func TestGeoCheckChooseAndAddressConfirmed(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _, rpc := newIntegService(t)
	res := mustUpsert(t, svc, "300", extOrder("delivery", "a"))
	cands := []order.GeoCandidate{}
	for i, l := range []string{"931 Taylor Ave", "931 W Taylor Ave", "913 Taylor Ave", "931 Tyler Ave"} {
		cands = append(cands, order.GeoCandidate{Label: l + ", Towson MD", Line1: l, City: "Towson", State: "MD", PostalCode: "21286",
			Lat: 39.4 + float64(i)/100, Lng: -76.6})
	}
	so, err := svc.OnGeo(ctx, order.GeoUpdate{OrderID: res.OrderID, State: order.GeoCheck, Candidates: cands})
	if err != nil || so.Geo.State != order.GeoCheck || len(so.Geo.Candidates) != 3 || so.Geo.Located() {
		t.Fatalf("check = %+v %v", so.Geo, err)
	}
	if planOrder(*so).Located {
		t.Fatal("AUTO must treat check as not located")
	}
	if err := svc.ChooseCandidate(ctx, res.OrderID, 3); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("index 3 = %v", err)
	}
	if err := svc.ChooseCandidate(ctx, res.OrderID, 1); err != nil {
		t.Fatal(err)
	}
	if c := rpc.of(MsgGeoChoose); len(c) != 1 || c[0].Payload["order_id"] != res.OrderID || c[0].Payload["index"] != float64(1) {
		t.Fatalf("geo.choose = %+v", c)
	}
	// main's answer: located, pinned, with the chosen address.
	lat, lng := 39.41, -76.6
	addr := &order.ConfirmedAddress{Line1: "931 W Taylor Ave", City: "Towson", State: "MD", PostalCode: "21286"}
	upd := order.GeoUpdate{OrderID: res.OrderID, State: order.GeoLocated, Lat: &lat, Lng: &lng, Quality: "pinned", ConfirmedAddress: addr, Confirmed: true}
	if so, err = svc.OnGeo(ctx, upd); err != nil || so.Geo.ConfirmedAddress == nil || len(so.Geo.Candidates) != 0 {
		t.Fatalf("located = %+v %v", so.Geo, err)
	}
	if err := svc.ChooseCandidate(ctx, res.OrderID, 0); !errors.Is(err, order.ErrInvalidState) {
		t.Fatalf("choose when located = %v", err)
	}
	svc.OnGeo(ctx, upd) // main re-pushes (reconnect): no second event
	lat2 := 39.42
	svc.OnGeo(ctx, order.GeoUpdate{OrderID: res.OrderID, State: order.GeoLocated, Lat: &lat2, Lng: &lng, Quality: "pinned", Confirmed: true}) // a pin
	// A geo_cache hit comes back pinned too, but not flagged confirmed: no
	// staff action, no event (here a new POS delivery at a pinned address).
	cached := mustUpsert(t, svc, "302", extOrder("delivery", "c"))
	lat3 := 39.43
	if so, err := svc.OnGeo(ctx, order.GeoUpdate{OrderID: cached.OrderID, State: order.GeoLocated, Lat: &lat3, Lng: &lng, Quality: "pinned"}); err != nil || !so.Geo.Located() {
		t.Fatalf("cache hit = %+v %v", so, err)
	}
	// Nor does a cached chosen candidate (confirmed_address, not confirmed).
	svc.OnGeo(ctx, order.GeoUpdate{OrderID: cached.OrderID, State: order.GeoLocated, Lat: &lat3, Lng: &lng, Quality: "pinned", ConfirmedAddress: addr})
	var got []order.AddressConfirmedData
	for _, ev := range feedOf(t, svc) {
		if ev.Type == order.FeedAddressConfirmed {
			var d order.AddressConfirmedData
			_ = json.Unmarshal(ev.Data, &d)
			got = append(got, d)
		}
	}
	if len(got) != 2 || got[0].How != "candidate" || *got[0].Address != *addr || got[0].Lat != lat || *got[0].ExtRef != "300" ||
		got[1].How != "pin" || got[1].Address != nil || got[1].Lat != lat2 {
		t.Fatalf("address.confirmed = %+v", got)
	}
	// A pending/unavailable POS delivery is re-located on reconnect.
	other := mustUpsert(t, svc, "301", extOrder("delivery", "b"))
	before := len(rpc.of(MsgGeoLocate))
	svc.RelocatePOS(ctx)
	svc.WaitBackground()
	after := rpc.of(MsgGeoLocate)
	if len(after) != before+1 || after[len(after)-1].Payload["order_id"] != other.OrderID {
		t.Fatalf("relocate = %d -> %d", before, len(after))
	}
}

func TestIntegrationStatusAndInsertLive(t *testing.T) {
	ctx := context.Background()
	svc, _, events, _, _ := newIntegService(t)
	if v := svc.IntegrationView(time.Now()); v.Connected || v.LastSeenAt != nil || len(v.Problems) != 0 || v.InsertMode != "off" {
		t.Fatalf("no hub yet = %+v", v)
	}
	drain(events)
	if err := svc.SetIntegrationStatus(ctx, order.IntegrationStatus{OK: true, InsertMode: "live",
		Problems: []order.Problem{{Code: "quicc_batch", Text: "Quicc batch not open in SoftTouch", Severity: "serious"}}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := eventOf(events, "integration.status"); !ok {
		t.Fatal("no integration.status")
	}
	if svc.InsertLive() {
		t.Fatal("insert live with features.order_insert off")
	}
	svc.UpdateIntegrationSettings(ctx, func(s *order.IntegrationSettings) error { s.Features.OrderInsert = true; return nil })
	if !svc.InsertLive() {
		t.Fatal("insert not live")
	}
	v := svc.IntegrationView(time.Now())
	if !v.Connected || !v.OK || len(v.Problems) != 1 || v.Problems[0].Severity != "serious" {
		t.Fatalf("view = %+v", v)
	}
	// Silent for over 60 s: link down, serious, insert not live; published once.
	later := time.Now().Add(61 * time.Second)
	v = svc.IntegrationView(later)
	if v.Connected || v.InsertLive || len(v.Problems) != 1 || v.Problems[0].Text != "POS link down" || v.Problems[0].Severity != "serious" {
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
	if v := svc.IntegrationView(time.Now()); v.InsertMode != "live" || v.LastSeenAt == nil {
		t.Fatalf("reloaded = %+v", v)
	}
}

func TestSettingsChangedEverywhere(t *testing.T) {
	ctx := context.Background()
	svc, _, events, _, _ := newIntegService(t)
	drain(events)
	set, err := svc.UpdateIntegrationSettings(ctx, func(s *order.IntegrationSettings) error {
		s.Tickets.PosDelivery, s.Times.DefaultPickupMinutes = true, 20
		return nil
	})
	if err != nil || !set.Tickets.PosDelivery || set.Times.DefaultPickupMinutes != 20 {
		t.Fatalf("set = %+v %v", set, err)
	}
	if data, ok := eventOf(events, "settings.updated"); !ok || !strings.Contains(string(data), `"pos_delivery":true`) {
		t.Fatalf("settings.updated = %s", data)
	}
	f, err := svc.Floor(ctx)
	if err != nil || f.Settings != set {
		t.Fatalf("floor settings = %+v %v", f.Settings, err)
	}
	feed := feedOf(t, svc)
	if len(feed) != 1 || feed[0].Type != order.FeedSettingsChanged {
		t.Fatalf("feed = %+v", feed)
	}
	if _, err := svc.UpdateIntegrationSettings(ctx, func(s *order.IntegrationSettings) error { s.Times.DefaultDeliveryMinutes = 0; return nil }); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("bad time = %v", err)
	}
	if svc.IntegrationSettings() != set {
		t.Fatal("a refused change leaked into the settings")
	}
}

func TestCloseBatchForgetsPOSDeliveries(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _, rpc := newIntegService(t)
	res := mustUpsert(t, svc, "1", extOrder("delivery", "a"))
	mustUpsert(t, svc, "2", extOrder("pickup", "b"))
	if _, _, err := svc.CloseBatch(ctx, true); err != nil {
		t.Fatal(err)
	}
	svc.WaitBackground()
	if f := rpc.of(MsgGeoForget); len(f) != 1 || f[0].Payload["order_id"] != res.OrderID {
		t.Fatalf("geo.forget = %+v", f)
	}
	so := storedOf(t, svc, res.OrderID)
	if so.Order.Customer.DisplayName != "" || so.Order.Fulfillment.Address != nil || so.Status.State != order.StateCancelled {
		t.Fatalf("after close = %+v", so)
	}
	types := []string{}
	for _, ev := range feedOf(t, svc) {
		types = append(types, ev.Type)
	}
	if strings.Join(types, ",") != "order.cancelled,order.cancelled,batch.closed" {
		t.Fatalf("feed = %v", types)
	}
}

// order.accepted carries insert_expected from the accept's own print outcome.
func TestOrderAcceptedInsertExpected(t *testing.T) {
	ctx := context.Background()
	svc, _, _, prints, _ := newIntegService(t)
	accepted := func(insert bool) order.OrderAcceptedData {
		t.Helper()
		prints.mu.Lock()
		prints.insert = insert
		prints.mu.Unlock()
		s := order.Snapshot{ID: uuid.NewString(), TenantID: uuid.NewString(), LocationID: uuid.NewString(), Source: "online",
			SourceEventID: uuid.NewString()[:6], Fulfillment: order.Fulfillment{Type: "pickup"}, Money: order.Money{Total: 2000}}
		if err := svc.OnArrival(ctx, s); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Accept(ctx, uuid.MustParse(s.ID), nil); err != nil {
			t.Fatal(err)
		}
		for _, ev := range feedOf(t, svc) {
			var d order.OrderAcceptedData
			if ev.Type == order.FeedOrderAccepted && json.Unmarshal(ev.Data, &d) == nil && d.Order.Order.ID == s.ID {
				if !strings.Contains(string(ev.Data), `"insert_expected":`) {
					t.Fatalf("no insert_expected key: %s", ev.Data)
				}
				return d
			}
		}
		t.Fatal("no order.accepted")
		return order.OrderAcceptedData{}
	}
	if d := accepted(true); !d.InsertExpected {
		t.Fatal("insert_expected = false for a live-insert print")
	}
	if d := accepted(false); d.InsertExpected {
		t.Fatal("insert_expected = true for a receipt-only print")
	}
}

// A POS check that becomes a delivery is located; one that stops being a
// delivery is forgotten by main and leaves its pool.
func TestPOSTypeChangeLocatesAndForgets(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _, rpc := newIntegService(t)
	res := mustUpsert(t, svc, "400", extOrder("pickup", "a"))
	if len(rpc.of(MsgGeoLocate)) != 0 {
		t.Fatal("a pickup was located")
	}
	mustUpsert(t, svc, "400", extOrder("delivery", "b"))
	if c := rpc.of(MsgGeoLocate); len(c) != 1 || c[0].Payload["order_id"] != res.OrderID {
		t.Fatalf("geo.locate = %+v", c)
	}
	lat, lng := 39.4, -76.6
	if _, err := svc.OnGeo(ctx, order.GeoUpdate{OrderID: res.OrderID, State: order.GeoLocated, Lat: &lat, Lng: &lng, Quality: "exact"}); err != nil {
		t.Fatal(err)
	}
	ann, _ := svc.CreateDriver(ctx, "Ann", "")
	svc.ClockIn(ctx, ann.ID)
	if _, err := svc.Pool(ctx, []string{res.OrderID}, ann.ID); err != nil {
		t.Fatal(err)
	}
	mustUpsert(t, svc, "400", extOrder("pickup", "c"))
	if c := rpc.of(MsgGeoForget); len(c) != 1 || c[0].Payload["order_id"] != res.OrderID {
		t.Fatalf("geo.forget = %+v", c)
	}
	so := storedOf(t, svc, res.OrderID)
	if so.Status.PoolDriverID != "" || so.Geo != nil || so.IsDelivery() {
		t.Fatalf("after pickup = pool %q geo %+v", so.Status.PoolDriverID, so.Geo)
	}
	if len(rpc.of(MsgGeoLocate)) != 1 {
		t.Fatal("located again")
	}
	// Address-only change of a delivery still re-locates (unchanged rule).
	other := mustUpsert(t, svc, "401", extOrder("delivery", "a"))
	moved := extOrder("delivery", "b")
	moved.Address.Line1 = "1 Other St"
	mustUpsert(t, svc, "401", moved)
	if c := rpc.of(MsgGeoLocate); len(c) != 3 || c[2].Payload["order_id"] != other.OrderID {
		t.Fatalf("geo.locate = %d", len(c))
	}
}

// Auto-accept on: a future order is scheduled on arrival with the default
// time for its type (nothing prints), and auto-accepted when it rings again.
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
	if so.Status.State != order.StateScheduled || so.Status.ScheduledLeadMinutes == nil || *so.Status.ScheduledLeadMinutes != 35 ||
		!so.Status.RingAt.Equal(promised.Add(-35*time.Minute).Truncate(time.Millisecond)) {
		t.Fatalf("auto-scheduled = %+v", so.Status)
	}
	if len(prints.all()) != 0 {
		t.Fatal("a future order printed on arrival")
	}
	if _, ok := eventOf(events, "order.arrived"); ok {
		t.Fatal("a future order rang under auto-accept")
	}
	for _, ev := range feedOf(t, svc) {
		if ev.Type == order.FeedOrderAccepted {
			t.Fatal("order.accepted before the ring")
		}
	}
	// An ASAP order is still accepted on arrival.
	asap := order.Snapshot{ID: uuid.NewString(), TenantID: uuid.NewString(), LocationID: uuid.NewString(), Source: "online",
		SourceEventID: "f2", Fulfillment: order.Fulfillment{Type: "pickup"}, Money: order.Money{Total: 900}}
	if err := svc.OnArrival(ctx, asap); err != nil {
		t.Fatal(err)
	}
	if got := storedOf(t, svc, asap.ID); got.Status.State != order.StateAccepted {
		t.Fatalf("asap = %s", got.Status.State)
	}
	// At ring_at it is accepted (prep = its lead) and printed.
	if n, err := svc.RaiseDue(ctx, promised.Add(-35*time.Minute).Add(time.Second)); n != 1 || err != nil {
		t.Fatalf("raise = %d %v", n, err)
	}
	got := storedOf(t, svc, s.ID)
	if got.Status.State != order.StateAccepted || *got.Status.PrepMinutes != 35 || got.Status.KitchenNumber == nil {
		t.Fatalf("at ring = %+v", got.Status)
	}
	if p := prints.all(); len(p) != 2 || p[1].OrderID != s.ID || p[1].Mode != printer.ModeAccept {
		t.Fatalf("prints = %+v", p)
	}
	n := 0
	for _, ev := range feedOf(t, svc) {
		if ev.Type == order.FeedOrderAccepted {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("order.accepted events = %d", n)
	}
}
