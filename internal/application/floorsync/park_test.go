package floorsync

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/terminal"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
)

// The sync-stall safety rule (contract 2.4e) against a real SQLite outbox:
// an event main refuses for good is parked after the refusals threshold, the
// rest of the queue moves on, events about the same subject wait behind it,
// a retry puts it back in its place, and the owner is paged at most once an
// hour.

type answers struct {
	mu    sync.Mutex
	by    map[string]string // event id -> result (default applied)
	order []string          // every event id main applied, in order
	names []string          // first_name of every applied customer.upsert, in order
}

func (a *answers) post(_ context.Context, body []byte) (int, []byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var env envelope
	var p struct {
		Events []struct {
			EventID   string `json:"event_id"`
			FirstName string `json:"first_name"`
		} `json:"events"`
	}
	json.Unmarshal(body, &env)
	json.Unmarshal(env.Payload, &p)
	var res []Result
	for _, e := range p.Events {
		r := a.by[e.EventID]
		if r == "" {
			r = "applied"
			a.order = append(a.order, e.EventID)
			if e.FirstName != "" {
				a.names = append(a.names, e.FirstName)
			}
		}
		res = append(res, Result{EventID: e.EventID, Result: r})
	}
	inner, _ := json.Marshal(map[string]any{"results": res})
	out, _ := json.Marshal(envelope{Payload: inner})
	return http.StatusOK, out, nil
}

type pages struct {
	mu   sync.Mutex
	msgs []string
}

func (p *pages) Send(_ context.Context, msg, _ string) error {
	p.mu.Lock()
	p.msgs = append(p.msgs, msg)
	p.mu.Unlock()
	return nil
}

// journalEvent writes a journal.entries event (no subject: never held).
func journalEvent(t *testing.T, r interface {
	AddJournal(context.Context, []terminal.Entry) error
}) {
	t.Helper()
	if err := r.AddJournal(context.Background(), []terminal.Entry{{DeviceID: "t", Action: "no_sale"}}); err != nil {
		t.Fatal(err)
	}
}

func TestParkAfterRefusalsQueueMovesOn(t *testing.T) {
	r, m := setup(t)
	ctx := context.Background()
	if err := r.SetSyncParkSettings(ctx, 3, 24); err != nil {
		t.Fatal(err)
	}
	// 60 refused customers (more than one batch: the old stall) + 5 good events.
	ans := &answers{by: map[string]string{}}
	for i := 0; i < 60; i++ {
		c := terminal.Customer{ID: uuid.NewString(), Phone: fmt.Sprintf("410555%04d", i), FirstName: "Ada"}
		if err := r.SaveTerminalCustomer(ctx, c, map[string]any{"customer_id": c.ID}, nil, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	bad, _ := r.ListUnsyncedEvents(ctx, 100)
	for _, e := range bad {
		ans.by[e.ID] = "invalid"
	}
	for i := 0; i < 5; i++ {
		journalEvent(t, r)
	}
	pg := &pages{}
	s := New(r, ans.post, zerolog.Nop(), m)
	s.SetPager(pg)
	parkedN := 0
	s.OnParked(func(n int) { parkedN += n })
	for i := 0; i < 2; i++ { // refusals 1 and 2: kept, nothing else gets through
		if err := s.Flush(ctx); err == nil {
			t.Fatalf("flush %d: kept events must report an error", i)
		}
	}
	if len(ans.order) != 0 {
		t.Fatalf("nothing behind a full batch of refused events gets through yet: %d", len(ans.order))
	}
	// Third refusal: the first batch parks; the rest is refused once (kept).
	s.Flush(ctx)
	if parkedN != 50 {
		t.Fatalf("parked %d, want the first 50", parkedN)
	}
	for i := 0; i < 3; i++ {
		s.Flush(ctx)
	}
	v, err := r.ParkedEvents(ctx, 500)
	if err != nil {
		t.Fatal(err)
	}
	if v.Count != 60 || len(v.Parked) != 60 || v.Backlog != 0 || v.Held != 0 {
		t.Fatalf("parked view: count %d listed %d backlog %d held %d", v.Count, len(v.Parked), v.Backlog, v.Held)
	}
	if got := len(ans.order); got < 5 {
		t.Fatalf("the good events must sync past the parked ones: %d applied", got)
	}
	if v.Parked[0].Type != "customer.upsert" || v.Parked[0].Result != "invalid" || v.Parked[0].Refusals != 3 ||
		!strings.HasPrefix(v.Parked[0].Subject, "customer:") {
		t.Fatalf("dead letter %+v", v.Parked[0])
	}
	// One Pushover with the count, then quiet for the hour (kept in settings).
	s.alertParked(ctx)
	if len(pg.msgs) != 1 || !strings.Contains(pg.msgs[0], "60 parked in all") {
		t.Fatalf("pages %v", pg.msgs)
	}
	journal, _ := r.Journal(ctx, 500)
	parkedRows := 0
	for _, e := range journal {
		if e.Action == terminal.ActionSyncParked {
			parkedRows++
		}
	}
	if parkedRows != 60 {
		t.Fatalf("journal sync.parked rows = %d", parkedRows)
	}
	// main takes them now: a retry of all puts them back and they sync.
	ans.mu.Lock()
	ans.by = map[string]string{}
	ans.mu.Unlock()
	n, err := r.RetryParked(ctx, nil, "web", time.Now())
	if err != nil || n != 60 {
		t.Fatalf("retry all: %d %v", n, err)
	}
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if v, _ := r.ParkedEvents(ctx, 10); v.Count != 0 || v.Backlog != 0 {
		t.Fatalf("after retry: %+v", v)
	}
	if _, err := r.RetryParked(ctx, nil, "web", time.Now()); err == nil {
		t.Fatal("nothing parked: ErrNotParked")
	}
}

func TestParkAfterHoursAndAlertOncePerHour(t *testing.T) {
	r, m := setup(t)
	ctx := context.Background()
	if err := r.SetSyncParkSettings(ctx, 1000, 2); err != nil {
		t.Fatal(err)
	}
	ans := &answers{by: map[string]string{}}
	c := terminal.Customer{ID: uuid.NewString(), Phone: "4105550100", FirstName: "Ada"}
	if err := r.SaveTerminalCustomer(ctx, c, map[string]any{"customer_id": c.ID}, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	evs, _ := r.ListUnsyncedEvents(ctx, 10)
	ans.by[evs[0].ID] = "invalid"
	clock := time.Now().Add(-5 * time.Hour)
	pg := &pages{}
	s := New(r, ans.post, zerolog.Nop(), m)
	s.SetPager(pg)
	s.now = func() time.Time { return clock }
	s.Flush(ctx)
	clock = clock.Add(time.Hour)
	s.Flush(ctx)
	if n, _ := r.CountParkedEvents(ctx); n != 0 {
		t.Fatal("1 h after the first refusal: still kept")
	}
	clock = clock.Add(time.Hour + time.Minute)
	if err := s.Flush(ctx); err != nil {
		t.Fatalf("parked on the third try: no error left, got %v", err)
	}
	if n, _ := r.CountParkedEvents(ctx); n != 1 {
		t.Fatal("2 h after the first refusal: parked")
	}
	s.alertParked(ctx)
	// Another one parks 10 minutes later: no second page within the hour,
	// it rides on the next one with its count.
	c2 := terminal.Customer{ID: uuid.NewString(), Phone: "4105550101", FirstName: "Bea"}
	r.SaveTerminalCustomer(ctx, c2, map[string]any{"customer_id": c2.ID}, nil, clock)
	evs, _ = r.ListUnsyncedEvents(ctx, 10)
	for _, e := range evs { // the park's own journal.entries event may be first
		if e.Type == terminal.EventCustomerUpsert {
			ans.by[e.ID] = "invalid"
		}
	}
	r.SetSyncParkSettings(ctx, 1, 2)
	clock = clock.Add(10 * time.Minute)
	s.Flush(ctx)
	s.alertParked(ctx)
	if len(pg.msgs) != 1 {
		t.Fatalf("one page an hour: %v", pg.msgs)
	}
	clock = clock.Add(time.Hour)
	s.alertParked(ctx)
	if len(pg.msgs) != 2 || !strings.Contains(pg.msgs[1], "refused 1 event") || !strings.Contains(pg.msgs[1], "2 parked in all") {
		t.Fatalf("second page after the hour: %v", pg.msgs)
	}
	// A new Sender (Pi restart) remembers the last page.
	s2 := New(r, ans.post, zerolog.Nop(), m)
	s2.SetPager(pg)
	s2.now = func() time.Time { return clock.Add(time.Minute) }
	s2.unalerted = 1
	s2.alertParked(ctx)
	if len(pg.msgs) != 2 {
		t.Fatal("a restart must not page again within the hour")
	}
}

// TestParkedSubjectHoldsItsLaterEvents is the order rule: a parked event
// holds the later events about the same subject (an order's payment behind
// its order.placed, a drawer's movement behind its opening), never anything
// else; a retry sends them in their old order.
func TestParkedSubjectHoldsItsLaterEvents(t *testing.T) {
	r, m := setup(t)
	ctx := context.Background()
	r.SetSyncParkSettings(ctx, 1, 24)
	ans := &answers{by: map[string]string{}}
	custA := terminal.Customer{ID: uuid.NewString(), Phone: "4105550200", FirstName: "A"}
	custB := terminal.Customer{ID: uuid.NewString(), Phone: "4105550201", FirstName: "B"}
	save := func(c terminal.Customer, name string) {
		c.FirstName = name
		if err := r.SaveTerminalCustomer(ctx, c, map[string]any{"customer_id": c.ID, "first_name": name}, nil, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	save(custA, "A1")
	evs, _ := r.ListUnsyncedEvents(ctx, 10)
	a1 := evs[0].ID
	ans.mu.Lock()
	ans.by[a1] = "invalid" // parks at once (threshold 1)
	ans.mu.Unlock()
	s := New(r, ans.post, zerolog.Nop(), m)
	s.Flush(ctx)
	save(custA, "A2") // about A: waits behind A1
	save(custB, "B1") // about B: goes
	journalEvent(t, r) // no subject: goes
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	ans.mu.Lock()
	sent := append([]string(nil), ans.names...)
	ans.mu.Unlock()
	if contains(sent, "A2") {
		t.Fatal("A's later event must wait behind its parked event")
	}
	if !contains(sent, "B1") {
		t.Fatal("another subject's event must not wait")
	}
	if v, _ := r.ParkedEvents(ctx, 10); v.Count != 1 || v.Held != 1 {
		t.Fatalf("parked 1 held 1: %+v", v)
	}
	// Retry: A1 first, then A2.
	ans.mu.Lock()
	delete(ans.by, a1)
	ans.mu.Unlock()
	if n, err := r.RetryParked(ctx, []string{a1}, "web", time.Now()); err != nil || n != 1 {
		t.Fatalf("retry: %d %v", n, err)
	}
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	ans.mu.Lock()
	defer ans.mu.Unlock()
	ia, ib := index(ans.names, "A1"), index(ans.names, "A2")
	if ia < 0 || ib < 0 || ia > ib {
		t.Fatalf("A1 must go before A2: %v", ans.names)
	}
}

func contains(xs []string, x string) bool { return index(xs, x) >= 0 }
func index(xs []string, x string) int {
	for i, v := range xs {
		if v == x {
			return i
		}
	}
	return -1
}

var _ = order.DefaultSyncParkHours
