package order

import (
	"context"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
)

type resetRepo struct {
	*fakeRepo
	reset order.DriverReset
	runs  []time.Time
}

func (r *resetRepo) GetDriverReset(context.Context) (order.DriverReset, error) { return r.reset, nil }
func (r *resetRepo) MarkDriverReset(_ context.Context, at time.Time) error {
	r.reset.LastRun = &at
	return nil
}
func (r *resetRepo) SetDriverResetTime(_ context.Context, hhmm string) error {
	r.reset.Time = hhmm
	return nil
}
func (r *resetRepo) AutoClockOut(_ context.Context, at time.Time) (int, []order.StoredOrder, error) {
	r.runs = append(r.runs, at)
	r.reset.LastRun = &at
	return 0, nil, nil
}

func TestDriverResetRunsOncePerDayAtLocalTime(t *testing.T) {
	svc, base, _, _ := newTestService(t, false)
	ny, _ := time.LoadLocation("America/New_York")
	at := func(h, m int) time.Time { return time.Date(2026, 10, 16, h, m, 0, 0, ny) }
	yesterday := at(10, 0).AddDate(0, 0, -1)
	repo := &resetRepo{fakeRepo: base, reset: order.DriverReset{Time: "10:00", LastRun: &yesterday}}
	svc.repo = repo

	for _, now := range []time.Time{at(9, 59), at(10, 0), at(10, 1), at(23, 0)} {
		if _, err := svc.DriverResetDue(context.Background(), now.UTC(), ny); err != nil {
			t.Fatal(err)
		}
	}
	if len(repo.runs) != 1 || !repo.runs[0].Equal(at(10, 0)) {
		t.Fatalf("want exactly one run at 10:00 New York, got %v", repo.runs)
	}
	// Next day 10:00 runs again.
	svc.DriverResetDue(context.Background(), at(10, 0).AddDate(0, 0, 1).UTC(), ny)
	if len(repo.runs) != 2 {
		t.Fatalf("runs = %d", len(repo.runs))
	}
	// A bad setting never resets.
	repo.reset.Time = "25:99"
	svc.DriverResetDue(context.Background(), at(11, 0).AddDate(0, 0, 2).UTC(), ny)
	if len(repo.runs) != 2 {
		t.Fatal("bad reset time must not run")
	}
}

func TestFirstResetCheckAfterUpgradeDoesNotClockAnyoneOut(t *testing.T) {
	svc, base, _, _ := newTestService(t, false)
	repo := &resetRepo{fakeRepo: base, reset: order.DriverReset{Time: "10:00"}} // never ran
	svc.repo = repo
	ny, _ := time.LoadLocation("America/New_York")
	afternoon := time.Date(2026, 10, 16, 15, 0, 0, 0, ny)
	if n, err := svc.DriverResetDue(context.Background(), afternoon, ny); err != nil || n != 0 {
		t.Fatalf("first check: %d %v", n, err)
	}
	if len(repo.runs) != 0 || repo.reset.LastRun == nil {
		t.Fatalf("must only start the clock: runs=%v last=%v", repo.runs, repo.reset.LastRun)
	}
	// Tomorrow 10:00 it runs normally.
	svc.DriverResetDue(context.Background(), afternoon.AddDate(0, 0, 1).Add(-5*time.Hour), ny)
	if len(repo.runs) != 1 {
		t.Fatalf("runs = %d", len(repo.runs))
	}
}

// The clock starts at the latest scheduled reset, not "now": a driver who
// clocked in after 10:00 (e.g. in the setup wizard) must not count as having
// crossed a reset.
func TestFirstResetCheckStampsTheScheduledTimeNotNow(t *testing.T) {
	svc, base, _, _ := newTestService(t, false)
	repo := &resetRepo{fakeRepo: base, reset: order.DriverReset{Time: "10:00"}}
	svc.repo = repo
	ny, _ := time.LoadLocation("America/New_York")
	afternoon := time.Date(2026, 10, 16, 15, 0, 0, 0, ny)
	svc.DriverResetDue(context.Background(), afternoon, ny)
	if want := time.Date(2026, 10, 16, 10, 0, 0, 0, ny); repo.reset.LastRun == nil || !repo.reset.LastRun.Equal(want) {
		t.Fatalf("LastRun = %v, want %v", repo.reset.LastRun, want)
	}
}

func TestSavingTheResetTimeStampsTheLatestScheduledReset(t *testing.T) {
	svc, base, _, _ := newTestService(t, false)
	repo := &resetRepo{fakeRepo: base, reset: order.DriverReset{Time: "10:00"}}
	svc.repo = repo
	ny, _ := time.LoadLocation("America/New_York")
	svc.SetStoreTZ(func() *time.Location { return ny })

	if err := svc.SetDriverResetTime(context.Background(), "04:00"); err != nil {
		t.Fatal(err)
	}
	last := *repo.reset.LastRun
	now := time.Now().In(ny)
	if last.After(now) || now.Sub(last) > 24*time.Hour || last.In(ny).Hour() != 4 || last.In(ny).Minute() != 0 {
		t.Fatalf("LastRun = %v, want the latest 04:00 New York before %v", last.In(ny), now)
	}
}

func TestLastScheduledReset(t *testing.T) {
	ny, _ := time.LoadLocation("America/New_York")
	hm, _ := time.Parse("15:04", "10:00")
	for _, tc := range []struct{ now, want time.Time }{
		{time.Date(2026, 10, 16, 15, 0, 0, 0, ny), time.Date(2026, 10, 16, 10, 0, 0, 0, ny)},
		{time.Date(2026, 10, 16, 10, 0, 0, 0, ny), time.Date(2026, 10, 16, 10, 0, 0, 0, ny)},
		{time.Date(2026, 10, 16, 9, 59, 0, 0, ny), time.Date(2026, 10, 15, 10, 0, 0, 0, ny)},
	} {
		if got := lastScheduledReset(tc.now.UTC(), hm, ny); !got.Equal(tc.want) {
			t.Errorf("lastScheduledReset(%v) = %v, want %v", tc.now, got, tc.want)
		}
	}
}
