package floorsync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/infra/database"
	"quiccpos/agent/internal/infra/database/repositories"
	"quiccpos/agent/internal/migrate"
	"quiccpos/agent/internal/observability"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
)

func setup(t *testing.T) (*repositories.OrderRepository, observability.Meters) {
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
	m, err := observability.NewMeters()
	if err != nil {
		t.Fatal(err)
	}
	return repositories.NewOrderRepository(db, zerolog.Nop()), m
}

// seed produces n floor events (driver roster changes).
func seed(t *testing.T, r *repositories.OrderRepository, n int) {
	for i := 0; i < n; i++ {
		if _, err := r.CreateDriver(context.Background(), "D"+uuid.NewString()[:4]); err != nil {
			t.Fatal(err)
		}
	}
}

// mainStub answers like main: one result per event, all "applied".
func mainStub(posts *[][]json.RawMessage) Poster {
	return func(_ context.Context, body []byte) (int, []byte, error) {
		var env envelope
		var p struct {
			Events []json.RawMessage `json:"events"`
		}
		json.Unmarshal(body, &env)
		json.Unmarshal(env.Payload, &p)
		*posts = append(*posts, p.Events)
		var res []Result
		for _, e := range p.Events {
			var ev struct {
				EventID string `json:"event_id"`
			}
			json.Unmarshal(e, &ev)
			res = append(res, Result{EventID: ev.EventID, Result: "applied"})
		}
		inner, _ := json.Marshal(map[string]any{"results": res})
		out, _ := json.Marshal(envelope{Type: "floor.events.result", ID: env.ID, Payload: inner})
		return http.StatusOK, out, nil
	}
}

func TestFlushSendsInOrderAndBatches(t *testing.T) {
	r, m := setup(t)
	seed(t, r, 60)
	var posts [][]json.RawMessage
	s := New(r, mainStub(&posts), zerolog.Nop(), m)
	if err := s.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(posts) != 2 || len(posts[0]) != batchSize || len(posts[1]) != 10 {
		t.Fatalf("want 50 + 10, got %d posts", len(posts))
	}
	if n, _ := r.CountUnsyncedEvents(context.Background()); n != 0 {
		t.Fatalf("unsynced after flush = %d", n)
	}
	// Each roster event lists one more driver than the previous: oldest first.
	var first, last order.DriversRosterEvent
	json.Unmarshal(posts[0][0], &first)
	json.Unmarshal(posts[1][9], &last)
	if len(first.Drivers) != 1 || len(last.Drivers) != 60 {
		t.Fatalf("order: first=%d last=%d", len(first.Drivers), len(last.Drivers))
	}
}

func TestFlushKeepsEventsOnFailure(t *testing.T) {
	r, m := setup(t)
	seed(t, r, 3)
	down := func(context.Context, []byte) (int, []byte, error) { return 0, nil, errors.New("connection refused") }
	s := New(r, down, zerolog.Nop(), m)
	if err := s.Flush(context.Background()); err == nil {
		t.Fatal("want an error while main is down")
	}
	evs, _ := r.ListUnsyncedEvents(context.Background(), 10)
	if len(evs) != 3 || evs[0].Attempts != 1 {
		t.Fatalf("events must stay unsynced with attempts counted: %+v", evs)
	}
	if d := s.nextBackoff(errors.New("x")); d != minBackoff {
		t.Fatalf("first backoff = %v", d)
	}
	if d := s.nextBackoff(errors.New("x")); d != 2*minBackoff {
		t.Fatalf("second backoff = %v", d)
	}

	// main comes back → everything drains.
	var posts [][]json.RawMessage
	s.post = mainStub(&posts)
	if err := s.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n, _ := r.CountUnsyncedEvents(context.Background()); n != 0 {
		t.Fatalf("unsynced = %d", n)
	}
}

func TestOldMainIsUnsupported(t *testing.T) {
	r, m := setup(t)
	seed(t, r, 1)
	old := func(context.Context, []byte) (int, []byte, error) { return http.StatusAccepted, nil, nil }
	s := New(r, old, zerolog.Nop(), m)
	err := s.Flush(context.Background())
	if _, ok := err.(unsupportedError); !ok {
		t.Fatalf("want unsupportedError, got %v", err)
	}
	if d := s.nextBackoff(err); d != unsupportedBackoff {
		t.Fatalf("backoff = %v", d)
	}
	if n, _ := r.CountUnsyncedEvents(context.Background()); n != 1 {
		t.Fatal("events must wait for a main that understands them")
	}
}

func TestPartialAckRetriesTheRest(t *testing.T) {
	r, m := setup(t)
	seed(t, r, 2)
	half := func(_ context.Context, body []byte) (int, []byte, error) {
		var env envelope
		var p struct {
			Events []struct {
				EventID string `json:"event_id"`
			} `json:"events"`
		}
		json.Unmarshal(body, &env)
		json.Unmarshal(env.Payload, &p)
		inner, _ := json.Marshal(map[string]any{"results": []Result{{EventID: p.Events[0].EventID, Result: "not_found"}}})
		out, _ := json.Marshal(envelope{Payload: inner})
		return http.StatusOK, out, nil
	}
	s := New(r, half, zerolog.Nop(), m)
	if err := s.Flush(context.Background()); err == nil {
		t.Fatal("partial ack should report an error")
	}
	if n, _ := r.CountUnsyncedEvents(context.Background()); n != 1 {
		t.Fatalf("unsynced = %d, want 1", n)
	}
}

func TestWakeTriggersImmediateFlush(t *testing.T) {
	r, m := setup(t)
	var posts [][]json.RawMessage
	s := New(r, mainStub(&posts), zerolog.Nop(), m)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	time.Sleep(50 * time.Millisecond)
	seed(t, r, 1)
	s.Wake()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if n, _ := r.CountUnsyncedEvents(context.Background()); n == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("wake did not flush within 2s")
}

func TestPoisonEventIsIsolated(t *testing.T) {
	r, m := setup(t)
	seed(t, r, 3)
	evs, _ := r.ListUnsyncedEvents(context.Background(), 10)
	bad := evs[1].ID
	var posts [][]json.RawMessage
	ok := mainStub(&posts)
	picky := func(ctx context.Context, body []byte) (int, []byte, error) {
		if bytes.Contains(body, []byte(bad)) {
			return http.StatusBadRequest, []byte(`{"error":"bad"}`), nil
		}
		return ok(ctx, body)
	}
	s := New(r, picky, zerolog.Nop(), m)
	if err := s.Flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if n, _ := r.CountUnsyncedEvents(context.Background()); n != 0 {
		t.Fatalf("the good events must sync and the bad one be dropped, unsynced = %d", n)
	}
	if len(posts) != 2 {
		t.Fatalf("good events sent one by one: %d posts", len(posts))
	}
}
