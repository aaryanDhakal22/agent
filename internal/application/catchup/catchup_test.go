package catchup

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"

	"github.com/rs/zerolog"
)

type arriver struct{ got []string }

func (a *arriver) OnArrival(_ context.Context, s order.Snapshot) error {
	a.got = append(a.got, s.ID)
	return nil
}

type batches struct{ opened time.Time }

func (b batches) OpenBatch(context.Context) (*order.Batch, error) {
	return &order.Batch{ID: 1, OpenedAt: b.opened}, nil
}

func TestCatchUpPagesReceivedOrdersSinceTheOpenBatch(t *testing.T) {
	now := time.Date(2026, 10, 16, 20, 0, 0, 0, time.UTC)
	opened := now.Add(-3 * time.Hour)
	var queries []url.Values
	total := 130
	fetch := func(_ context.Context, path, raw string) (int, []byte, error) {
		q, _ := url.ParseQuery(raw)
		queries = append(queries, q)
		if path != "/api/v1/orders" {
			return 404, nil, nil
		}
		var off int
		fmt.Sscan(q.Get("offset"), &off)
		var page []order.Snapshot
		for i := off; i < total && i < off+pageSize; i++ {
			page = append(page, order.Snapshot{ID: fmt.Sprint(i)})
		}
		b, _ := json.Marshal(page)
		return 200, b, nil
	}
	a := &arriver{}
	n, err := Run(context.Background(), fetch, a, batches{opened}, now, zerolog.Nop())
	if err != nil || n != total || len(a.got) != total {
		t.Fatalf("n=%d err=%v got=%d", n, err, len(a.got))
	}
	if len(queries) != 2 {
		t.Fatalf("pages = %d", len(queries))
	}
	q := queries[0]
	if q.Get("status") != "received" || q.Get("sort") != "asc" || q.Get("from") != opened.Format(time.RFC3339) {
		t.Fatalf("query = %v", q)
	}
}

func TestCatchUpLooksBackAtMost12h(t *testing.T) {
	now := time.Date(2026, 10, 16, 20, 0, 0, 0, time.UTC)
	var from string
	fetch := func(_ context.Context, _, raw string) (int, []byte, error) {
		q, _ := url.ParseQuery(raw)
		from = q.Get("from")
		return 200, []byte("[]"), nil
	}
	Run(context.Background(), fetch, &arriver{}, batches{now.Add(-72 * time.Hour)}, now, zerolog.Nop())
	if from != now.Add(-12*time.Hour).Format(time.RFC3339) {
		t.Fatalf("from = %s", from)
	}
}

func TestCatchUpSurfacesMainErrors(t *testing.T) {
	fetch := func(context.Context, string, string) (int, []byte, error) { return 503, nil, nil }
	if _, err := Run(context.Background(), fetch, &arriver{}, batches{}, time.Now(), zerolog.Nop()); err == nil {
		t.Fatal("a 503 must be reported")
	}
}
