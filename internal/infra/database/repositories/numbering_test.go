package repositories

import (
	"context"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/terminal"
)

// Ticket numbers restart once a business day (contract 2.10ae, the owner
// 2026-11-13): a close marks them, the first number of a new business day
// after a close starts both sequences again; a second close the same day
// carries on.

type numberDay struct {
	day   string
	start int64
}

func withDay(r *OrderRepository, d *numberDay) {
	r.SetNumbering(func(time.Time) order.Numbering { return order.Numbering{Day: d.day, KitchenStart: d.start} })
}

func kitchenNo(t *testing.T, r *OrderRepository) int64 {
	t.Helper()
	so := accept(t, r, arrive(t, r, "pickup"))
	if so.Status.KitchenNumber == nil {
		t.Fatal("no kitchen number")
	}
	return *so.Status.KitchenNumber
}

func counterNo(t *testing.T, r *OrderRepository, key string, start int64) int64 {
	t.Helper()
	so, _ := counterOrder(t, r, key, start, 500, pay(terminal.TenderCash, 500))
	return *so.Status.KitchenNumber
}

func closeDay(t *testing.T, r *OrderRepository) {
	t.Helper()
	if _, _, err := r.CloseBatch(context.Background(), true); err != nil {
		t.Fatal(err)
	}
}

func TestNumbersCarryOnAcrossClosesTheSameDay(t *testing.T) {
	r := newFloorRepo(t)
	d := &numberDay{day: "2026-11-13", start: 1100}
	withDay(r, d)
	if k := kitchenNo(t, r); k != 1100 {
		t.Fatalf("first kitchen # %d", k)
	}
	if c := counterNo(t, r, "n-1", 1); c != 1 {
		t.Fatalf("first counter # %d", c)
	}
	closeDay(t, r)
	if k, c := kitchenNo(t, r), counterNo(t, r, "n-2", 1); k != 1101 || c != 2 {
		t.Fatalf("after the first close the same day: kitchen %d counter %d (want 1101, 2)", k, c)
	}
	closeDay(t, r)
	closeDay(t, r) // an empty third close
	if k, c := kitchenNo(t, r), counterNo(t, r, "n-3", 1); k != 1102 || c != 3 {
		t.Fatalf("after the third close the same day: kitchen %d counter %d (want 1102, 3)", k, c)
	}
}

func TestNumbersRestartOnTheNextBusinessDay(t *testing.T) {
	r := newFloorRepo(t)
	d := &numberDay{day: "2026-11-13", start: 1100}
	withDay(r, d)
	kitchenNo(t, r)
	kitchenNo(t, r)
	counterNo(t, r, "r-1", 1)
	closeDay(t, r)
	d.day = "2026-11-14"
	if c, k := counterNo(t, r, "r-2", 1), kitchenNo(t, r); c != 1 || k != 1100 {
		t.Fatalf("next day after a close: counter %d kitchen %d (want 1, 1100)", c, k)
	}
	// Later the same day (no close): carries on; a close and more orders the
	// same day: still carries on.
	if k := kitchenNo(t, r); k != 1101 {
		t.Fatalf("kitchen # %d", k)
	}
	closeDay(t, r)
	if c := counterNo(t, r, "r-3", 1); c != 2 {
		t.Fatalf("counter # %d", c)
	}
}

func TestNumbersNeverRestartWithoutAClose(t *testing.T) {
	r := newFloorRepo(t)
	d := &numberDay{day: "2026-11-13", start: 1100}
	withDay(r, d)
	kitchenNo(t, r)
	counterNo(t, r, "w-1", 1)
	d.day = "2026-11-14" // the batch was never closed: one batch, no repeats
	if k, c := kitchenNo(t, r), counterNo(t, r, "w-2", 1); k != 1101 || c != 2 {
		t.Fatalf("a new day without a close: kitchen %d counter %d (want 1101, 2)", k, c)
	}
	// A close, then orders the same (new) day: carry on; the restart waits
	// for the next day.
	closeDay(t, r)
	if k := kitchenNo(t, r); k != 1102 {
		t.Fatalf("kitchen # %d", k)
	}
	closeDay(t, r)
	d.day = "2026-11-15"
	if k := kitchenNo(t, r); k != 1100 {
		t.Fatalf("next day after a close: kitchen %d", k)
	}
}

func TestNumbersCustomStarts(t *testing.T) {
	r := newFloorRepo(t)
	d := &numberDay{day: "2026-11-13", start: 500}
	withDay(r, d)
	// A fresh Pi's stored kitchen_next is 1100 (the old fixed start): it
	// carries on until the first daily restart, then main's kitchen_start.
	if k := kitchenNo(t, r); k != 1100 {
		t.Fatalf("before any restart: %d", k)
	}
	if c := counterNo(t, r, "c-1", 20); c != 20 {
		t.Fatalf("counter start 20: %d", c)
	}
	closeDay(t, r)
	d.day = "2026-11-14"
	if k, c := kitchenNo(t, r), counterNo(t, r, "c-2", 30); k != 500 || c != 30 {
		t.Fatalf("custom starts after the restart: kitchen %d counter %d (want 500, 30)", k, c)
	}
	if k, c := kitchenNo(t, r), counterNo(t, r, "c-3", 30); k != 501 || c != 31 {
		t.Fatalf("then: kitchen %d counter %d", k, c)
	}
}

func TestNumbersStateRoundTripsThroughRestore(t *testing.T) {
	r := newFloorRepo(t)
	d := &numberDay{day: "2026-11-13", start: 1100}
	withDay(r, d)
	kitchenNo(t, r)
	counterNo(t, r, "s-1", 1)
	closeDay(t, r)
	st, err := r.AgentState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.NumbersDay != "2026-11-13" || !st.NumbersClosed || st.KitchenNext != 1101 {
		t.Fatalf("state: day %q closed %v kitchen_next %d", st.NumbersDay, st.NumbersClosed, st.KitchenNext)
	}
}
