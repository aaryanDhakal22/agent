package floorsync

import (
	"bytes"
	"context"
	"net/http"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/terminal"
	"quiccpos/agent/internal/observability"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// M-2 (contract 2.10s §3): a money event is never marked synced on a
// refusal - an invalid / not_found answer or a 400 / 413 - and an event of
// the same subject waits behind it from its first refusal.

func TestKeepRefusedCoversEveryMoneyEvent(t *testing.T) {
	for _, typ := range []string{"order.placed", "order.payment", "order.changed", "order.void", "customer.upsert",
		"drawer.opened", "drawer.closed", "drawer.movement", "z.report", "journal.entries"} {
		for _, res := range []string{"invalid", "not_found"} {
			if !KeepRefused(typ, res) {
				t.Errorf("%s %s must be kept", typ, res)
			}
		}
		if KeepRefused(typ, "stale") || KeepRefused(typ, "duplicate") || KeepRefused(typ, "applied") {
			t.Errorf("%s: only refusals are kept", typ)
		}
		if ClassOf(typ) != "money" {
			t.Errorf("%s class", typ)
		}
	}
	for _, typ := range []string{"order.status", "drivers.roster", "driver.shift", "delivery.run", "batch.closed", "order.tip"} {
		if KeepRefused(typ, "invalid") || ClassOf(typ) != "floor" {
			t.Errorf("%s is floor bookkeeping", typ)
		}
	}
}

func TestBadRequestOnAMoneyEventKeepsAndParksIt(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusRequestEntityTooLarge} {
		r, m := setup(t)
		ctx := context.Background()
		if err := r.SetSyncParkSettings(ctx, 2, 24); err != nil {
			t.Fatal(err)
		}
		c := terminal.Customer{ID: uuid.NewString(), Phone: "4105550101", FirstName: "Ada"}
		if err := r.SaveTerminalCustomer(ctx, c, map[string]any{"customer_id": c.ID}, nil, time.Now()); err != nil {
			t.Fatal(err)
		}
		evs, _ := r.ListUnsyncedEvents(ctx, 10)
		bad := evs[0].ID
		journalEvent(t, r)
		ans := &answers{by: map[string]string{}}
		picky := func(ctx context.Context, body []byte) (int, []byte, error) {
			if bytes.Contains(body, []byte(bad)) {
				return status, []byte(`{"error":"bad"}`), nil
			}
			return ans.post(ctx, body)
		}
		s := New(r, picky, zerolog.Nop(), m)
		if err := s.Flush(ctx); err == nil {
			t.Fatalf("%d: a kept money event reports an error", status)
		}
		if n, _ := r.CountUnsyncedEvents(ctx); n != 1 {
			t.Fatalf("%d: the customer stays queued, the journal syncs: unsynced %d", status, n)
		}
		if n, _ := r.CountParkedEvents(ctx); n != 0 {
			t.Fatalf("%d: parked after one refusal", status)
		}
		s.Flush(ctx) // second refusal: parked
		if n, _ := r.CountParkedEvents(ctx); n != 1 {
			t.Fatalf("%d: parked = %d", status, n)
		}
		v, _ := r.ParkedEvents(ctx, 10)
		if v.Parked[0].EventID != bad || v.Parked[0].Result == "" {
			t.Fatalf("%d: dead letter %+v", status, v.Parked[0])
		}
	}
}

func TestLaterEventsWaitBehindARefusedOne(t *testing.T) {
	r, m := setup(t)
	ctx := context.Background()
	c := terminal.Customer{ID: uuid.NewString(), Phone: "4105550102", FirstName: "Ada"}
	if err := r.SaveTerminalCustomer(ctx, c, map[string]any{"customer_id": c.ID}, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	c.FirstName = "Bea"
	if err := r.SaveTerminalCustomer(ctx, c, map[string]any{"customer_id": c.ID}, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	evs, _ := r.ListUnsyncedEvents(ctx, 10)
	if len(evs) != 2 {
		t.Fatalf("events %d", len(evs))
	}
	first := evs[0].ID
	// main refuses the first; the second (same subject) comes back
	// unapplied (stale: main applies per subject in order) - it must not be
	// marked synced and dropped. (One main applied in the same batch stays
	// applied: main dedupes by event id, so it can't be replayed.)
	ans := &answers{by: map[string]string{first: "invalid", evs[1].ID: "stale"}}
	s := New(r, ans.post, zerolog.Nop(), m)
	s.Flush(ctx)
	if n, _ := r.CountUnsyncedEvents(ctx); n != 2 {
		t.Fatalf("both stay queued: %d", n)
	}
	// The next flushes send only the refused one (the other is held).
	s.Flush(ctx)
	held, _ := r.ListUnsyncedEvents(ctx, 10)
	if len(held) != 1 || held[0].ID != first {
		t.Fatalf("only the refused event goes out while it is refused: %+v", held)
	}
	// main takes it: both sync, in their order.
	ans.mu.Lock()
	ans.by = map[string]string{}
	ans.order = nil
	ans.names = nil
	ans.mu.Unlock()
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if n, _ := r.CountUnsyncedEvents(ctx); n != 0 {
		t.Fatalf("unsynced %d", n)
	}
	if len(ans.order) != 2 || ans.order[0] != first || ans.order[1] != evs[1].ID {
		t.Fatalf("order kept: %v", ans.order)
	}
}

func TestOldestAgeAndClassMetrics(t *testing.T) {
	r, m := setup(t)
	ctx := context.Background()
	reader := sdkmetric.NewManualReader()
	cm, err := observability.NewCounterMeters(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	if err != nil {
		t.Fatal(err)
	}
	journalEvent(t, r)
	seed(t, r, 1)
	s := New(r, func(context.Context, []byte) (int, []byte, error) { return 0, nil, context.DeadlineExceeded }, zerolog.Nop(), m)
	s.SetCounterMeters(cm)
	s.now = func() time.Time { return time.Now().Add(time.Minute) }
	s.Flush(ctx)
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatal(err)
	}
	ages := map[string]float64{}
	sent := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, mt := range sm.Metrics {
			switch d := mt.Data.(type) {
			case metricdata.Gauge[float64]:
				if mt.Name == "floor.sync.oldest_age" {
					for _, p := range d.DataPoints {
						c, _ := p.Attributes.Value("class")
						ages[c.AsString()] = p.Value
					}
				}
			case metricdata.Sum[int64]:
				if mt.Name == "floor.sync.events.sent" {
					for _, p := range d.DataPoints {
						c, _ := p.Attributes.Value("class")
						sent[c.AsString()] += p.Value
					}
				}
			}
		}
	}
	if ages["money"] < 50 || ages["floor"] < 50 {
		t.Fatalf("oldest ages %v", ages)
	}
	if sent["money"] != 1 || sent["floor"] != 1 {
		t.Fatalf("sent by class %v", sent)
	}
}
