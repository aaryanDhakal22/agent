package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	orderApp "quiccpos/agent/internal/application/order"
	"quiccpos/agent/internal/domain/order"

	"github.com/google/uuid"
)

// 2.2.2 POST /api/orders/{id}/geo/search: device auth, the RPC sent to main
// and every error body the tablet matches on.
func TestGeoSearchRoute(t *testing.T) {
	s, h, key := diagServer(t)
	snap := order.Snapshot{ID: uuid.NewString(), TenantID: uuid.NewString(), LocationID: uuid.NewString(), Source: "online",
		SourceEventID: "1", PlacedAt: time.Now().UTC(), Fulfillment: order.Fulfillment{Type: "delivery"}}
	if err := s.service.OnArrival(context.Background(), snap); err != nil {
		t.Fatal(err)
	}
	path := "/api/orders/" + snap.ID + "/geo/search"
	if rec := call(h, "POST", path, "", `{"query":"1613 Wadsworth Way"}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("without a key: %d", rec.Code)
	}
	// No main wired yet.
	if rec := call(h, "POST", path, key, `{"query":"1613 Wadsworth Way"}`); rec.Code != 503 || code(rec) != "main_offline" {
		t.Fatalf("offline: %d %s", rec.Code, rec.Body)
	}

	var mu sync.Mutex
	var sent []map[string]any
	status, reply := 200, `{"ok":true}`
	s.service.SetGeo(orderApp.GeoDeps{Linked: func() bool { return true },
		Send: func(_ context.Context, typ string, payload any) (int, json.RawMessage, error) {
			mu.Lock()
			defer mu.Unlock()
			raw, _ := json.Marshal(payload)
			var m map[string]any
			_ = json.Unmarshal(raw, &m)
			m["_type"] = typ
			sent = append(sent, m)
			return status, json.RawMessage(reply), nil
		}})

	rec := call(h, "POST", path, key, `{"query":"  1613  Wadsworth Way "}`)
	if rec.Code != http.StatusAccepted || strings.TrimSpace(rec.Body.String()) != "{}" {
		t.Fatalf("search: %d %s", rec.Code, rec.Body)
	}
	if len(sent) != 1 || sent[0]["_type"] != "geo.search" || sent[0]["order_id"] != snap.ID || sent[0]["query"] != "1613 Wadsworth Way" {
		t.Fatalf("rpc = %+v", sent)
	}

	errBody := func(body []byte) (string, string) {
		var b struct{ Error, Code string }
		_ = json.Unmarshal(body, &b)
		return b.Error, b.Code
	}
	for _, tc := range []struct {
		name, path, body string
		mainStatus       int
		mainReply        string
		status           int
		code             string
	}{
		{"empty", path, `{"query":"   "}`, 200, `{"ok":true}`, 400, "bad_query"},
		{"missing", path, `{}`, 200, `{"ok":true}`, 400, "bad_query"},
		{"too long", path, `{"query":"` + strings.Repeat("a", order.MaxGeoSearchQuery+1) + `"}`, 200, `{"ok":true}`, 400, "bad_query"},
		{"unknown order", "/api/orders/" + uuid.NewString() + "/geo/search", `{"query":"1 Main St"}`, 200, `{"ok":true}`, 404, "not_found"},
		{"main has no order", path, `{"query":"1 Main St"}`, 200, `{"ok":false,"code":"not_found"}`, 404, "not_found"},
		{"too soon", path, `{"query":"1 Main St"}`, 200, `{"ok":false,"code":"too_soon","retry_after_ms":2000}`, 429, "too_soon"},
		{"older main", path, `{"query":"1 Main St"}`, 202, ``, 501, "main_unsupported"},
		{"main refused the query", path, `{"query":"1 Main St"}`, 400, `{"error":"bad payload"}`, 400, "bad_query"},
		{"main error", path, `{"query":"1 Main St"}`, 500, `{"error":"handler failed"}`, 503, "main_offline"},
	} {
		mu.Lock()
		status, reply = tc.mainStatus, tc.mainReply
		mu.Unlock()
		rec := call(h, "POST", tc.path, key, tc.body)
		e, c := errBody(rec.Body.Bytes())
		if rec.Code != tc.status || c != tc.code || (tc.code == "bad_query" && e != "bad_query") {
			t.Errorf("%s: %d %s", tc.name, rec.Code, rec.Body)
		}
	}
	// 200 characters (not bytes) is fine.
	mu.Lock()
	status, reply = 200, `{"ok":true}`
	mu.Unlock()
	if rec := call(h, "POST", path, key, `{"query":"`+strings.Repeat("é", order.MaxGeoSearchQuery)+`"}`); rec.Code != http.StatusAccepted {
		t.Errorf("200 characters: %d %s", rec.Code, rec.Body)
	}
}
