package order

import (
	"context"
	"errors"
	"path/filepath"
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
	agentrpc "quiccpos/agent/internal/rpc"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
)

// 2.5.0 website orders: ADD TIP, CANCEL ORDER (VOID slip), the online
// ordering proxies.

type webPrintLog struct {
	mu    sync.Mutex
	modes []printer.Mode
}

func (p *webPrintLog) all() []printer.Mode {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]printer.Mode(nil), p.modes...)
}

func newWebService(t *testing.T) (*Service, *webPrintLog, chan ssebroker.Event) {
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
	log := &webPrintLog{}
	print := func(_ context.Context, _ order.Snapshot, m printer.Mode) (printer.Outcome, error) {
		log.mu.Lock()
		log.modes = append(log.modes, m)
		log.mu.Unlock()
		return printer.Outcome{Printed: []printer.TicketResult{{Kind: printer.TicketStation}}}, nil
	}
	svc := NewService(repo, repositories.NewSettingsRepository(db, zerolog.Nop()), print, nil, broker, zerolog.Nop(), meters)
	noAutoAssign(t, repo)
	return svc, log, events
}

func webOrder(t *testing.T, svc *Service, card bool, accept bool) string {
	t.Helper()
	ctx := context.Background()
	s := order.Snapshot{ID: uuid.NewString(), TenantID: uuid.NewString(), LocationID: uuid.NewString(), Source: "web",
		SourceEventID: "w1", PlacedAt: time.Now().UTC(), Fulfillment: order.Fulfillment{Type: "pickup"},
		Money: order.Money{Total: 2500, Tip: 200}}
	if card {
		s.Payments = []order.PaymentSummary{{Method: "card", AmountCents: 2500}}
	}
	if err := svc.OnArrival(ctx, s); err != nil {
		t.Fatal(err)
	}
	if accept {
		p := 15
		if _, err := svc.Accept(ctx, uuid.MustParse(s.ID), &p); err != nil {
			t.Fatal(err)
		}
	}
	return s.ID
}

func TestSetTipService(t *testing.T) {
	ctx := context.Background()
	svc, _, events := newWebService(t)
	id := webOrder(t, svc, true, true)
	drain(events)
	so, err := svc.SetTip(ctx, id, 500)
	if err != nil || so.Status.AddedTipCents != 500 {
		t.Fatalf("%v %+v", err, so)
	}
	if _, ok := eventOf(events, "order.updated"); !ok {
		t.Fatal("no order.updated")
	}
	for _, bad := range []int64{-1, order.MaxAddedTipCents + 1} {
		if _, err := svc.SetTip(ctx, id, bad); !errors.Is(err, ErrBadRequest) {
			t.Errorf("tip %d: %v", bad, err)
		}
	}
	if _, err := svc.SetTip(ctx, webOrder(t, svc, false, true), 100); !errors.Is(err, order.ErrNotTippable) {
		t.Errorf("cash: %v", err)
	}
}

func TestCancelOrderPrintsVoidOnlyWhenPrinted(t *testing.T) {
	ctx := context.Background()
	svc, log, events := newWebService(t)
	accepted := webOrder(t, svc, true, true)
	waiting := webOrder(t, svc, true, false)
	drain(events)
	if _, err := svc.CancelOrder(ctx, accepted, "nope", ""); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("bad reason: %v", err)
	}
	long := make([]rune, order.MaxCancelNoteRunes+1)
	for i := range long {
		long[i] = 'x'
	}
	if _, err := svc.CancelOrder(ctx, accepted, order.CancelOther, string(long)); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("long note: %v", err)
	}
	so, err := svc.CancelOrder(ctx, accepted, order.CancelCustomerAsked, "changed their mind")
	if err != nil || so.Status.State != order.StateCancelled || so.Status.CancelReason != order.CancelCustomerAsked {
		t.Fatalf("%v %+v", err, so)
	}
	svc.WaitBackground()
	if m := log.all(); len(m) != 2 || m[0] != printer.ModeAccept || m[1] != printer.ModeVoid {
		t.Fatalf("prints = %v, want accept then void", m)
	}
	if _, err := svc.CancelOrder(ctx, waiting, order.CancelOther, ""); err != nil {
		t.Fatal(err)
	}
	svc.WaitBackground()
	if m := log.all(); len(m) != 2 {
		t.Fatalf("a never-printed order gets no VOID: %v", m)
	}
	if _, err := svc.CancelOrder(ctx, waiting, order.CancelOther, ""); !errors.Is(err, order.ErrNotCancellable) {
		t.Fatalf("twice: %v", err)
	}
}

func TestOnlineProxies(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newWebService(t)
	meters, _ := observability.NewMeters()
	ex := &exchanger{status: 200, reply: `{"state":"paused","pause":{"preset":"30m","note":"Oven"}}`}
	reg := agentrpc.NewRegistry(ex, zerolog.Nop(), meters)
	svc.SetGeo(GeoDeps{Send: reg.Send, Linked: func() bool { return true }})

	got, err := svc.PauseOnline(ctx, "30m", " Oven ")
	if err != nil || string(got) != ex.reply {
		t.Fatalf("pause: %v %s", err, got)
	}
	if e := ex.got[0]; e.Type != MsgOnlinePause || string(e.Payload) != `{"note":"Oven","preset":"30m"}` {
		t.Fatalf("sent %s %s", e.Type, e.Payload)
	}
	if _, err := svc.PauseOnline(ctx, "forever", ""); !errors.Is(err, ErrBadRequest) || len(ex.got) != 1 {
		t.Fatalf("bad preset: %v", err)
	}
	in := false
	if _, err := svc.SetStock(ctx, StockChange{Kind: "item", ID: "abc", InStock: &in, Hold: true}); err != nil {
		t.Fatal(err)
	}
	if e := ex.got[1]; e.Type != MsgMenuStockSet || string(e.Payload) != `{"hold":true,"id":"abc","in_stock":false,"kind":"item"}` {
		t.Fatalf("stock sent %s %s", e.Type, e.Payload)
	}
	if _, err := svc.SetStock(ctx, StockChange{Kind: "size", ID: "abc", InStock: &in}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("bad kind: %v", err)
	}
	for _, f := range []func(context.Context) error{
		func(c context.Context) error { _, err := svc.OnlineStatus(c); return err },
		func(c context.Context) error { _, err := svc.ResumeOnline(c); return err },
		func(c context.Context) error { _, err := svc.MenuStock(c); return err },
	} {
		if err := f(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if types := []string{ex.got[2].Type, ex.got[3].Type, ex.got[4].Type}; types[0] != MsgOnlineStatus || types[1] != MsgOnlineResume || types[2] != MsgMenuStock {
		t.Fatalf("types %v", types)
	}
	// main's 400 / an older main / no answer / not linked.
	ex.status = 400
	if _, err := svc.OnlineStatus(ctx); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("400: %v", err)
	}
	ex.status = 202
	if _, err := svc.OnlineStatus(ctx); !errors.Is(err, ErrMainUnsupported) {
		t.Fatalf("202: %v", err)
	}
	ex.status, ex.err = 200, errors.New("dial tcp: refused")
	if _, err := svc.OnlineStatus(ctx); !errors.Is(err, ErrMainUnreachable) {
		t.Fatalf("offline: %v", err)
	}
	svc.SetGeo(GeoDeps{Send: reg.Send, Linked: func() bool { return false }})
	if _, err := svc.OnlineStatus(ctx); !errors.Is(err, ErrMainUnreachable) {
		t.Fatalf("unlinked: %v", err)
	}
}
