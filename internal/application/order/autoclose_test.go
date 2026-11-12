package order

import (
	"context"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
)

// The batch auto-close (contract/agent.md §2.10c) against a real SQLite DB
// with an injected clock: "now" is handed in, so nothing sleeps. The batch
// opens at the real time; the injected now is later than that.

func TestAutoCloseOncePerSlotAndRestartSafe(t *testing.T) {
	ctx := context.Background()
	svc, repo, _ := newPoolService(t)
	ny, _ := time.LoadLocation("America/New_York")
	readyDelivery(t, svc, time.Minute)
	first, _ := repo.OpenBatch(ctx)

	now := time.Now().Add(2 * time.Hour) // the batch is 2 h old
	at := now.Add(-10 * time.Minute).In(ny).Format("15:04")

	// Off / a bad setting: nothing.
	for _, v := range []string{"", "25:99", "4am"} {
		if res, err := svc.AutoCloseDue(ctx, now, ny, v); err != nil || res.Outcome != AutoCloseOff {
			t.Fatalf("%q: %+v %v", v, res, err)
		}
	}
	// Not yet: the slot is still ahead (yesterday's slot is past the window).
	early := now.Add(-20 * time.Minute)
	if res, err := svc.AutoCloseDue(ctx, early, ny, at); err != nil || res.Outcome != AutoCloseMissed {
		t.Fatalf("before today's slot (yesterday's missed): %+v %v", res, err)
	}

	res, err := svc.AutoCloseDue(ctx, now, ny, at)
	if err != nil || res.Outcome != AutoCloseClosed || res.Orders != 1 || res.BatchID != first.ID {
		t.Fatalf("close: %+v %v", res, err)
	}
	svc.WaitBackground()
	next, _ := repo.OpenBatch(ctx)
	if next.ID == first.ID {
		t.Fatal("a new batch must be open")
	}
	// The order was auto-finished like a forced Close batch.
	list, _ := repo.ListByBatch(ctx, first.ID, 0, 10)
	if len(list) != 1 || list[0].Status.State != order.StateDelivered {
		t.Fatalf("leftover auto-finished: %+v", list)
	}

	// The same slot never closes twice, also after a "restart" (a new
	// service on the same DB).
	readyDelivery(t, svc, time.Minute)
	for _, s := range []*Service{svc, NewService(repo, svc.settings, svc.print, nil, nil, svc.logger, svc.meters)} {
		if res, err := s.AutoCloseDue(ctx, now.Add(5*time.Minute), ny, at); err != nil || res.Outcome != AutoCloseNotDue {
			t.Fatalf("same slot: %+v %v", res, err)
		}
	}

	// The next day: the new batch (opened at the real time, a day before)
	// closes again.
	res, err = svc.AutoCloseDue(ctx, now.Add(24*time.Hour), ny, at)
	if err != nil || res.Outcome != AutoCloseClosed || res.BatchID != next.ID {
		t.Fatalf("next day: %+v %v", res, err)
	}
	svc.WaitBackground()
	// The day after: an empty batch is left alone (and the slot is done).
	if res, err := svc.AutoCloseDue(ctx, now.Add(48*time.Hour), ny, at); err != nil || res.Outcome != AutoCloseEmpty {
		t.Fatalf("empty batch: %+v %v", res, err)
	}
	if res, _ := svc.AutoCloseDue(ctx, now.Add(48*time.Hour+time.Minute), ny, at); res.Outcome != AutoCloseNotDue {
		t.Fatalf("empty slot handled: %+v", res)
	}
}

func TestAutoCloseSkipsARecentBatchAndAMissedSlot(t *testing.T) {
	ctx := context.Background()
	svc, repo, _ := newPoolService(t)
	readyDelivery(t, svc, time.Minute)
	b, _ := repo.OpenBatch(ctx)
	utc := time.UTC

	// Someone closed by hand half an hour ago: the batch is too young.
	now := time.Now().Add(30 * time.Minute)
	at := now.Add(-5 * time.Minute).UTC().Format("15:04")
	if res, err := svc.AutoCloseDue(ctx, now, utc, at); err != nil || res.Outcome != AutoCloseRecent {
		t.Fatalf("recent: %+v %v", res, err)
	}
	// ... and that slot is done for the day even once the batch is older.
	if res, _ := svc.AutoCloseDue(ctx, now.Add(time.Hour), utc, at); res.Outcome != AutoCloseNotDue {
		t.Fatalf("after a recent skip: %+v", res)
	}

	// A Pi that boots 3 h after the slot never closes mid-service.
	later := time.Now().Add(26 * time.Hour)
	at = later.Add(-3 * time.Hour).UTC().Format("15:04")
	if res, err := svc.AutoCloseDue(ctx, later, utc, at); err != nil || res.Outcome != AutoCloseMissed {
		t.Fatalf("missed: %+v %v", res, err)
	}
	if cur, _ := repo.OpenBatch(ctx); cur.ID != b.ID {
		t.Fatal("nothing may close")
	}
}
