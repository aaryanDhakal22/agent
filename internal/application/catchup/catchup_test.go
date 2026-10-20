package catchup

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"

	"github.com/rs/zerolog"
)

var now = time.Date(2026, 10, 19, 21, 30, 0, 0, time.FixedZone("EDT", -4*3600))

func at(ago time.Duration) string {
	return now.Add(-ago).Format("2006-01-02T15:04:05-0700")
}

type recovered struct {
	id    int
	print bool
}

type fakeService struct {
	mu      sync.Mutex
	present map[int]bool
	calls   []recovered
	fail    map[int]bool
}

func (f *fakeService) Recover(_ context.Context, o order.OrderRequest, print bool) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail[o.OrderID] {
		return false, errors.New("db down")
	}
	if f.present[o.OrderID] {
		return false, nil
	}
	f.present[o.OrderID] = true
	f.calls = append(f.calls, recovered{o.OrderID, print})
	return true, nil
}

func mainServer(t *testing.T, orders []order.OrderRequest, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/orders" || r.URL.Query().Get("num") != "100" {
			t.Errorf("unexpected request %s", r.URL)
		}
		if r.Header.Get("X-API-Key") != "k" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(orders)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newRec(url string, svc Recoverer) *Reconciler {
	r := New(url+"/", "k", svc, zerolog.Nop())
	r.Now = func() time.Time { return now }
	return r
}

func TestPassSortsOrdersByAge(t *testing.T) {
	orders := []order.OrderRequest{
		{OrderID: 1778641409, SubmittedDate: "2026-05-12T23:03:29-0400"}, // old test order
		{OrderID: 107, SubmittedDate: at(2 * time.Minute)},               // missed, fresh → print
		{OrderID: 106, SubmittedDate: at(10 * time.Minute)},              // already here
		{OrderID: 105, SubmittedDate: at(60 * time.Minute)},              // boundary → print
		{OrderID: 104, SubmittedDate: at(61 * time.Minute)},              // tablet only
		{OrderID: 103, SubmittedDate: at(11 * time.Hour)},                // tablet only
		{OrderID: 102, SubmittedDate: at(13 * time.Hour)},                // too old
		{OrderID: 101, SubmittedDate: "yesterday-ish"},                   // unparseable
	}
	svc := &fakeService{present: map[int]bool{106: true}}
	res, err := newRec(mainServer(t, orders, 200).URL, svc).Pass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []recovered{{107, true}, {105, true}, {104, false}, {103, false}}
	if len(svc.calls) != len(want) {
		t.Fatalf("calls = %v, want %v", svc.calls, want)
	}
	for i := range want {
		if svc.calls[i] != want[i] {
			t.Fatalf("calls = %v, want %v", svc.calls, want)
		}
	}
	exp := Result{Fetched: 8, Present: 1, Printed: 2, TabletOnly: 2, TooOld: 2, Unparseable: 1}
	if res != exp {
		t.Fatalf("result = %+v, want %+v", res, exp)
	}

	// A second pass finds everything already stored: nothing recovered twice.
	res, err = newRec(mainServer(t, orders, 200).URL, svc).Pass(context.Background())
	if err != nil || res.Recovered() != 0 || res.Present != 5 {
		t.Fatalf("second pass = %+v, %v", res, err)
	}
}

func TestPassFetchErrorsLeaveNothingDone(t *testing.T) {
	svc := &fakeService{present: map[int]bool{}}
	for _, status := range []int{http.StatusInternalServerError, http.StatusServiceUnavailable} {
		_, err := newRec(mainServer(t, nil, status).URL, svc).Pass(context.Background())
		if err == nil {
			t.Fatalf("HTTP %d: want an error", status)
		}
	}
	r := New(mainServer(t, nil, 200).URL, "wrong", svc, zerolog.Nop())
	if _, err := r.Pass(context.Background()); err == nil {
		t.Fatal("bad key: want an error")
	}
	if len(svc.calls) != 0 {
		t.Fatalf("recovered on error: %v", svc.calls)
	}
}

func TestPassCountsRecoverFailures(t *testing.T) {
	orders := []order.OrderRequest{
		{OrderID: 2, SubmittedDate: at(time.Minute)},
		{OrderID: 1, SubmittedDate: at(time.Minute)},
	}
	svc := &fakeService{present: map[int]bool{}, fail: map[int]bool{2: true}}
	res, err := newRec(mainServer(t, orders, 200).URL, svc).Pass(context.Background())
	if err != nil || res.Failed != 1 || res.Printed != 1 {
		t.Fatalf("result = %+v, %v", res, err)
	}
}

func TestRunPassesOnStartAndTrigger(t *testing.T) {
	var mu sync.Mutex
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		_, _ = w.Write([]byte("[]"))
	}))
	defer srv.Close()
	r := newRec(srv.URL, &fakeService{present: map[int]bool{}})
	r.Interval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)

	count := func() int { mu.Lock(); defer mu.Unlock(); return hits }
	waitFor(t, func() bool { return count() == 1 })
	r.Trigger()
	r.Trigger() // collapses into the pending one
	waitFor(t, func() bool { return count() >= 2 })
	time.Sleep(100 * time.Millisecond)
	if c := count(); c > 3 {
		t.Fatalf("triggers did not collapse: %d passes", c)
	}
}

func TestParseSubmitted(t *testing.T) {
	cases := map[string]time.Time{
		"2026-10-19T21:15:00-0400":  time.Date(2026, 10, 20, 1, 15, 0, 0, time.UTC),
		"2026-10-19T21:15:00Z":      time.Date(2026, 10, 19, 21, 15, 0, 0, time.UTC),
		"2026-10-19T21:15:00-04:00": time.Date(2026, 10, 20, 1, 15, 0, 0, time.UTC),
		"2026-04-22 12:00:00":       time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC),
		" 2026-04-22T12:00:00 ":     time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC),
	}
	for in, want := range cases {
		got, err := ParseSubmitted(in)
		if err != nil || !got.Equal(want) {
			t.Errorf("ParseSubmitted(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "19/10/2026", "now"} {
		if _, err := ParseSubmitted(bad); err == nil {
			t.Errorf("ParseSubmitted(%q): want an error", bad)
		}
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}
