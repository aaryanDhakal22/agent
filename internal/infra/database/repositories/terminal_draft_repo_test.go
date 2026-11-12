package repositories

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/terminal"
	"quiccpos/agent/internal/infra/database"
	"quiccpos/agent/internal/migrate"

	"github.com/rs/zerolog"
)

// Shared drafts live in SQLite: they survive a Pi restart, stay per store
// and keep their holder (contract §2.10i).
func TestTerminalDraftsSurviveARestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agent.db")
	if err := migrate.Run(ctx, path); err != nil {
		t.Fatal(err)
	}
	open := func() (*OrderRepository, func()) {
		db, err := database.Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		return NewOrderRepository(db, zerolog.Nop()), func() { db.Close() }
	}
	r, closeDB := open()
	by := terminal.Holder{DeviceID: "dev-1", DeviceName: "Till 1", StaffID: "s-1", StaffName: "Cara S."}
	now := time.Now().UTC().Truncate(time.Millisecond)
	in := terminal.Draft{ID: "d-keep-0001", LocationID: "loc-a", Type: "pickup", Name: "Sam", Draft: json.RawMessage(`{"lines":[]}`)}
	if _, err := r.PutTerminalDraft(ctx, in, terminal.DraftPut{By: by}, now); err != nil {
		t.Fatal(err)
	}
	closeDB()

	r, closeDB = open()
	defer closeDB()
	list, err := r.TerminalDrafts(ctx, "loc-a")
	if err != nil || len(list) != 1 {
		t.Fatalf("after restart: %+v %v", list, err)
	}
	d := list[0]
	if d.Holder == nil || d.Holder.DeviceID != "dev-1" || d.StaffName != "Cara S." || d.Type != "pickup" || !d.CreatedAt.Equal(now) {
		t.Fatalf("draft: %+v", d)
	}
	if other, _ := r.TerminalDrafts(ctx, "loc-b"); len(other) != 0 {
		t.Fatal("another store sees none")
	}
	// Still open on dev-1: dev-2 is told who has it.
	if _, err := r.PutTerminalDraft(ctx, in, terminal.DraftPut{By: terminal.Holder{DeviceID: "dev-2"}}, now); err == nil {
		t.Fatal("open elsewhere")
	}
	if ok, err := r.DeleteTerminalDraft(ctx, in.ID, "loc-a", "dev-1"); err != nil || !ok {
		t.Fatalf("delete: %v %v", ok, err)
	}
}

// The store wins an order's edit lock (contract 2.10ab): a call-center
// seat's draft of a sent order is its lock (origin callcenter, kept across a
// restart); a store terminal that opens the order displaces it in the same
// transaction; a seat is refused while a store terminal has the order open.
func TestCallcenterDraftIsALockTheStoreDisplaces(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agent.db")
	if err := migrate.Run(ctx, path); err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := NewOrderRepository(db, zerolog.Nop())
	now := time.Now().UTC().Truncate(time.Millisecond)
	seat := terminal.Holder{DeviceID: "callcenter:desk-1", DeviceName: "Elevate desk 1", StaffID: "cc-1", StaffName: "Elena V."}
	seat2 := terminal.Holder{DeviceID: "callcenter:desk-2", DeviceName: "Elevate desk 2", StaffID: "cc-1", StaffName: "Elena V."}
	till := terminal.Holder{DeviceID: "dev-1", DeviceName: "Till 1", StaffID: "s-1", StaffName: "Cara S."}
	const x, y = "0199c0de-0000-7000-8000-00000000000a", "0199c0de-0000-7000-8000-00000000000b"
	draft := func(id, orderID, body string) terminal.Draft {
		return terminal.Draft{ID: id, LocationID: "loc-a", Type: "pickup", OrderID: orderID, Draft: json.RawMessage(body)}
	}
	// The seat locks X (its own body), and puts a draft of Y aside (held).
	res, err := r.PutTerminalDraft(ctx, draft("d-seat-000x", x, `{"cc":1}`), terminal.DraftPut{By: seat}, now)
	if err != nil || !res.Draft.FromSeat() || len(res.Displaced) != 0 {
		t.Fatalf("the seat's lock: %+v %v", res, err)
	}
	if _, err := r.PutTerminalDraft(ctx, draft("d-seat-000y", y, `{"cc":2}`), terminal.DraftPut{By: seat, Held: true}, now); err != nil {
		t.Fatal(err)
	}
	list, _ := r.TerminalDrafts(ctx, "loc-a")
	if len(list) != 2 || !list[0].FromSeat() || !list[1].FromSeat() || list[1].Holder != nil {
		t.Fatalf("both are the call center's: %+v", list)
	}
	// Another seat's own draft of X is not refused (seats take over by id as terminals do).
	if _, err := r.PutTerminalDraft(ctx, draft("d-seat2-00x", x, `{"cc":3}`), terminal.DraftPut{By: seat2}, now); err != nil {
		t.Fatalf("a second seat on X: %v", err)
	}
	// The store opens X with its own draft (no take_over): both seat drafts of X go, Y's stays.
	res, err = r.PutTerminalDraft(ctx, draft("d-till-000x", x, `{"lines":[1]}`), terminal.DraftPut{By: till}, now)
	if err != nil || res.Draft.FromSeat() || res.Taken != nil || len(res.Displaced) != 2 || res.Displaced[0].ID != "d-seat-000x" ||
		res.Displaced[0].Holder == nil || res.Displaced[0].Holder.DeviceID != seat.DeviceID || res.Displaced[1].ID != "d-seat2-00x" {
		t.Fatalf("the store opens X: %+v %v", res, err)
	}
	list, _ = r.TerminalDrafts(ctx, "loc-a")
	if len(list) != 2 || list[0].ID != "d-seat-000y" || list[1].ID != "d-till-000x" || list[1].FromSeat() || string(list[1].Draft) != `{"lines":[1]}` {
		t.Fatalf("after the store opened X: %+v", list)
	}
	// A seat can't lock X now: with its old id, a new one, or take_over.
	var oe *terminal.OpenElsewhere
	for _, c := range []struct {
		id       string
		takeOver bool
	}{{"d-seat-000x", false}, {"d-seat-new1", false}, {"d-seat-new1", true}, {"d-till-000x", true}} {
		_, err := r.PutTerminalDraft(ctx, draft(c.id, x, `{"cc":9}`), terminal.DraftPut{By: seat, TakeOver: c.takeOver}, now)
		if !errors.As(err, &oe) || oe.Holder.DeviceID != till.DeviceID || oe.TakeOverRefused != c.takeOver {
			t.Fatalf("seat PUT %s take_over=%v: %v", c.id, c.takeOver, err)
		}
	}
	// The store's own held draft of X locks nobody: the seat may lock X again.
	if _, err := r.PutTerminalDraft(ctx, draft("d-till-000x", x, `{"lines":[1]}`), terminal.DraftPut{By: till, Held: true}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := r.PutTerminalDraft(ctx, draft("d-seat-new1", x, `{"cc":9}`), terminal.DraftPut{By: seat}, now); err != nil {
		t.Fatalf("X put aside by the store: %v", err)
	}
	// The store takes the seat's held draft of Y over by id: the stored body is the terminal's, the draft the store's.
	res, err = r.PutTerminalDraft(ctx, draft("d-seat-000y", y, `{"lines":[2]}`), terminal.DraftPut{By: till, TakeOver: true}, now)
	if err != nil || res.Draft.FromSeat() || string(res.Draft.Draft) != `{"lines":[2]}` || res.Draft.Holder == nil || res.Draft.Holder.DeviceID != till.DeviceID {
		t.Fatalf("take-over by id: %+v %v", res, err)
	}
	// A store draft with no order never displaces anything.
	if res, err := r.PutTerminalDraft(ctx, draft("d-till-new1", "", `{}`), terminal.DraftPut{By: till}, now); err != nil || len(res.Displaced) != 0 {
		t.Fatalf("a new order's draft: %+v %v", res, err)
	}
}
