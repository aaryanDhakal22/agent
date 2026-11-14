package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/terminal"
	"quiccpos/agent/internal/infra/database"
	"quiccpos/agent/internal/migrate"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
)

// Ticket numbers, the owner's final rule (contract 2.10ae, 2026-11-14): ONE
// sequence for every order from main's counter_start (default 100), every
// Close batch restarts it, a future order holds a random 5-digit placeholder
// until it is placed. Each test here failed on the 2026-11-13 code (two
// sequences, 1100+ online numbers, the business-day restart).

func withStart(r *OrderRepository, start int64) {
	r.SetNumbering(func(time.Time) order.Numbering { return order.Numbering{Start: start} })
}

// onlineNo places an online / website order (the claim takes its number).
func onlineNo(t *testing.T, r *OrderRepository) int64 {
	t.Helper()
	so := accept(t, r, arrive(t, r, "pickup"))
	if so.Status.KitchenNumber == nil || so.Status.NumberPlaceholder {
		t.Fatalf("no ticket number: %+v", so.Status)
	}
	return *so.Status.KitchenNumber
}

// counterNo places a terminal order with the store's start (0 = the
// repository's numbering, as the terminal service passes Config.CounterStart).
func counterNo(t *testing.T, r *OrderRepository, key string, start int64) int64 {
	t.Helper()
	so, _ := counterOrder(t, r, key, start, 500, pay(terminal.TenderCash, 500))
	return *so.Status.KitchenNumber
}

func closeBatch(t *testing.T, r *OrderRepository) {
	t.Helper()
	if _, _, err := r.CloseBatch(context.Background(), true); err != nil {
		t.Fatal(err)
	}
}

func floorNext(t *testing.T, r *OrderRepository) int64 {
	t.Helper()
	fs, err := r.FloorSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return fs.KitchenNext
}

// scheduleFuture stores an online order and schedules it (a future order's
// first confirm: it gets its placeholder).
func scheduleFuture(t *testing.T, r *OrderRepository) order.StoredOrder {
	t.Helper()
	so := arrive(t, r, "delivery")
	got, err := r.Schedule(context.Background(), so.Order.ID, 30, time.Now().Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return *got
}

// fire raises every due future order (its ring time) and places so.
func fire(t *testing.T, r *OrderRepository, so order.StoredOrder) order.StoredOrder {
	t.Helper()
	if _, err := r.RaiseDue(context.Background(), time.Now().Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	return accept(t, r, so)
}

func TestNumbersStartAt100AndCounterAndOnlineInterleave(t *testing.T) {
	r := newFloorRepo(t) // no numbering wired: the default start, 100
	if n := floorNext(t, r); n != 100 {
		t.Fatalf("a fresh Pi's next number = %d, want 100", n)
	}
	got := []int64{
		counterNo(t, r, "i-1", 0),
		onlineNo(t, r),
		counterNo(t, r, "i-2", 0),
		onlineNo(t, r),
		onlineNo(t, r),
		counterNo(t, r, "i-3", 0),
	}
	want := []int64{100, 101, 102, 103, 104, 105}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("one sequence for counter + online: got %v, want %v", got, want)
		}
	}
	if n := floorNext(t, r); n != 106 {
		t.Fatalf("/api/floor kitchen_next = %d, want 106", n)
	}
}

func TestCloseBatchRestartsTheNumbersAt100(t *testing.T) {
	r := newFloorRepo(t)
	withStart(r, 100)
	onlineNo(t, r)
	counterNo(t, r, "c-1", 0)
	if n := onlineNo(t, r); n != 102 {
		t.Fatalf("third order %d", n)
	}
	closeBatch(t, r)
	if n := floorNext(t, r); n != 100 {
		t.Fatalf("after Close batch the next number = %d, want 100", n)
	}
	if c, o := counterNo(t, r, "c-2", 0), onlineNo(t, r); c != 100 || o != 101 {
		t.Fatalf("after Close batch: counter %d online %d, want 100, 101", c, o)
	}
}

func TestASecondCloseTheSameDayRestartsAgain(t *testing.T) {
	r := newFloorRepo(t)
	withStart(r, 100)
	onlineNo(t, r)
	onlineNo(t, r)
	closeBatch(t, r)
	if n := counterNo(t, r, "s-1", 0); n != 100 {
		t.Fatalf("after the first close: %d", n)
	}
	onlineNo(t, r)
	closeBatch(t, r) // the same day, minutes later
	if o, c := onlineNo(t, r), counterNo(t, r, "s-2", 0); o != 100 || c != 101 {
		t.Fatalf("after a second close the same day: online %d counter %d, want 100, 101", o, c)
	}
	closeBatch(t, r)
	closeBatch(t, r) // empty closes too
	if n := onlineNo(t, r); n != 100 {
		t.Fatalf("after empty closes: %d", n)
	}
}

func TestTheStartIsMainsCounterStart(t *testing.T) {
	r := newFloorRepo(t)
	withStart(r, 250)
	if o, c := onlineNo(t, r), counterNo(t, r, "k-1", 0); o != 250 || c != 251 {
		t.Fatalf("counter_start 250: online %d counter %d", o, c)
	}
	closeBatch(t, r)
	withStart(r, 0) // missing / bad = 100
	if n := onlineNo(t, r); n != 100 {
		t.Fatalf("unset start after a close: %d", n)
	}
}

func TestFutureOrderHoldsAPlaceholderAcrossACloseThenTakesTheNextNumber(t *testing.T) {
	r := newFloorRepo(t)
	withStart(r, 100)
	ctx := context.Background()
	fut := scheduleFuture(t, r)
	ph := fut.Status.KitchenNumber
	if ph == nil || !fut.Status.NumberPlaceholder || *ph < 10_000 || *ph > 99_999 {
		t.Fatalf("a scheduled future order holds a 5-digit placeholder: %+v", fut.Status)
	}
	// main's mirror gets the placeholder with the scheduled event.
	if ev := lastStatusEvent(t, r, fut.Order.ID); ev["status"] != "scheduled" || int64(ev["kitchen_number"].(float64)) != *ph {
		t.Fatalf("scheduled event: %v", ev)
	}
	// The placeholder is not a sequence number: today's orders count on.
	if n := onlineNo(t, r); n != 100 {
		t.Fatalf("placeholder took a sequence number: %d", n)
	}
	// A lead edit keeps it.
	if again, err := r.Schedule(ctx, fut.Order.ID, 45, time.Now().Add(2*time.Hour)); err != nil || *again.Status.KitchenNumber != *ph {
		t.Fatalf("lead edit changed the placeholder: %+v %v", again.Status.KitchenNumber, err)
	}
	// Carried across a Close batch with its placeholder.
	closeBatch(t, r)
	moved, err := r.GetByID(ctx, uuid.MustParse(fut.Order.ID))
	if err != nil || moved.Status.State != order.StateScheduled || moved.Status.KitchenNumber == nil ||
		*moved.Status.KitchenNumber != *ph || !moved.Status.NumberPlaceholder {
		t.Fatalf("after the close the future order keeps its placeholder: %+v %v", moved.Status, err)
	}
	counterNo(t, r, "f-1", 0) // 100 in the new batch
	counterNo(t, r, "f-2", 0) // 101
	// It fires (RaiseDue → placed): the next number, the placeholder gone.
	if _, err := r.RaiseDue(ctx, time.Now().Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	placed := accept(t, r, *moved)
	if placed.Status.KitchenNumber == nil || *placed.Status.KitchenNumber != 102 || placed.Status.NumberPlaceholder {
		t.Fatalf("placed future order: %+v, want 102 and no placeholder", placed.Status)
	}
	// main's mirror: the accepted event carries the real number.
	if ev := lastStatusEvent(t, r, fut.Order.ID); ev["status"] != "accepted" || int64(ev["kitchen_number"].(float64)) != 102 {
		t.Fatalf("accepted event: %v", ev)
	}
	if n := counterNo(t, r, "f-3", 0); n != 103 {
		t.Fatalf("after the fire: %d", n)
	}
	// A revert keeps the real number (a failed print retried).
	if err := r.RevertAccept(ctx, uuid.MustParse(fut.Order.ID)); err != nil {
		t.Fatal(err)
	}
	again := accept(t, r, placed)
	if *again.Status.KitchenNumber != 102 || again.Status.NumberPlaceholder {
		t.Fatalf("re-accept: %+v", again.Status)
	}
}

func TestTwoOpenFutureOrdersNeverShareAPlaceholder(t *testing.T) {
	r := newFloorRepo(t)
	// A random source that always draws the same number: the second order
	// must still get another one.
	r.placeholderRand = func(int64) int64 { return 4_321 } // 14321
	a, b := scheduleFuture(t, r), scheduleFuture(t, r)
	if *a.Status.KitchenNumber != 14_321 || *b.Status.KitchenNumber == 14_321 {
		t.Fatalf("placeholders %d and %d", *a.Status.KitchenNumber, *b.Status.KitchenNumber)
	}
	// Once the first is placed, its old placeholder is free again.
	fire(t, r, a)
	c := scheduleFuture(t, r)
	if *c.Status.KitchenNumber != 14_321 {
		t.Fatalf("a placed order's placeholder is free again: got %d", *c.Status.KitchenNumber)
	}
	// Many at once: all different, all 5 digits.
	r.placeholderRand = nil
	seen := map[int64]bool{*b.Status.KitchenNumber: true, *c.Status.KitchenNumber: true}
	for range 40 {
		so := scheduleFuture(t, r)
		n := *so.Status.KitchenNumber
		if seen[n] || n < 10_000 || n > 99_999 {
			t.Fatalf("placeholder %d repeated or not 5 digits", n)
		}
		seen[n] = true
	}
}

func TestARestoredPiKeepsTheSequence(t *testing.T) {
	ctx := context.Background()
	a := newFloorRepo(t)
	withStart(a, 100)
	counterNo(t, a, "r-1", 0) // 100
	onlineNo(t, a)            // 101
	counterNo(t, a, "r-2", 0) // 102
	onlineNo(t, a)            // 103
	fut := scheduleFuture(t, a)
	st, err := a.AgentState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !st.OneSequence || st.CounterNext != 104 || st.KitchenNext != 0 {
		t.Fatalf("agent.state: one_sequence %v counter_next %d kitchen_next %d", st.OneSequence, st.CounterNext, st.KitchenNext)
	}
	raw, _ := json.Marshal(st)
	var stWire order.AgentState
	_ = json.Unmarshal(raw, &stWire)
	// main lost the state's counter (an older report): the restored orders'
	// numbers still keep the sequence going.
	stWire.CounterNext = 0
	open, err := a.ListByBatch(ctx, st.Batch.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	data := order.RestoreData{State: &stWire}
	for _, so := range open {
		b, _ := json.Marshal(so.Status)
		var wire order.Status
		_ = json.Unmarshal(b, &wire)
		data.Orders = append(data.Orders, order.RestoreOrder{Order: so.Order, AgentStatus: &wire})
	}
	b := newFloorRepo(t)
	withStart(b, 100)
	res, err := b.Restore(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	if res.KitchenNext != 104 {
		t.Fatalf("restore's next number = %d, want 104", res.KitchenNext)
	}
	if o, c := onlineNo(t, b), counterNo(t, b, "r-3", 0); o != 104 || c != 105 {
		t.Fatalf("after the restore: online %d counter %d, want 104, 105", o, c)
	}
	back, err := b.GetByID(ctx, uuid.MustParse(fut.Order.ID))
	if err != nil || !back.Status.NumberPlaceholder || *back.Status.KitchenNumber != *fut.Status.KitchenNumber {
		t.Fatalf("restored future order keeps its placeholder: %+v %v", back.Status, err)
	}
	if placed := fire(t, b, *back); *placed.Status.KitchenNumber != 106 {
		t.Fatalf("restored future order fires with %d, want 106", *placed.Status.KitchenNumber)
	}
}

// 00047 must honour a close made on the 2026-11-13 binary (numbers_closed =
// 1): the update round runs right after Close batch, so a store's first
// 2.5.9 start comes with that close pending. Lombardi's 2026-11-14: the
// numbers carried on (~170 -> 270) instead of restarting at 100.
func TestMigration00047RestartsAfterACloseOnTheOldBinary(t *testing.T) {
	for _, tc := range []struct {
		closed int
		want   int64
	}{{1, 100}, {0, 171}} {
		ctx := context.Background()
		path := filepath.Join(t.TempDir(), "agent.db")
		if err := migrate.RunTo(ctx, path, 46); err != nil {
			t.Fatal(err)
		}
		raw, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := raw.Exec(`UPDATE settings SET counter_next = 171, kitchen_next = 1140, numbers_closed = ?, numbers_day = '2026-11-13' WHERE id = 1`, tc.closed); err != nil {
			t.Fatal(err)
		}
		raw.Close()
		if err := migrate.Run(ctx, path); err != nil {
			t.Fatal(err)
		}
		db, err := database.Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		r := NewOrderRepository(db, zerolog.Nop())
		withStart(r, 100)
		if got := onlineNo(t, r); got != tc.want {
			t.Errorf("numbers_closed=%d: next number = %d, want %d", tc.closed, got, tc.want)
		}
		db.Close()
	}
}
