package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"

	"github.com/google/uuid"
)

// 2.2.1 routes: device auth, and the error bodies the tablet matches on
// ({"error": code, "code": code}).
func TestNotDeliveredRoutes(t *testing.T) {
	s, h, key := diagServer(t)
	for _, path := range []string{"/api/orders/" + uuid.NewString() + "/not-delivered", "/api/orders/" + uuid.NewString() + "/release"} {
		if rec := call(h, "POST", path, "", `{"action":"retry"}`); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without a key: %d", path, rec.Code)
		}
	}
	snap := order.Snapshot{ID: uuid.NewString(), TenantID: uuid.NewString(), LocationID: uuid.NewString(), Source: "online",
		SourceEventID: "1", PlacedAt: time.Now().UTC(), Fulfillment: order.Fulfillment{Type: "delivery"}}
	if err := s.service.OnArrival(context.Background(), snap); err != nil {
		t.Fatal(err)
	}
	errBody := func(body []byte) (string, string) {
		var b struct{ Error, Code string }
		_ = json.Unmarshal(body, &b)
		return b.Error, b.Code
	}
	for _, tc := range []struct {
		path, body string
		status     int
		code       string
	}{
		{"/api/orders/" + snap.ID + "/not-delivered", `{"action":"lost"}`, 400, "bad_action"},
		{"/api/orders/" + snap.ID + "/not-delivered", ``, 400, "bad_action"},
		{"/api/orders/" + snap.ID + "/not-delivered", `{"action":"retry"}`, 409, "not_out_or_delivered"},
		{"/api/orders/" + uuid.NewString() + "/not-delivered", `{"action":"cancel"}`, 404, "not_found"},
		{"/api/orders/" + snap.ID + "/release", ``, 409, "not_came_back"},
		{"/api/orders/" + uuid.NewString() + "/release", ``, 404, "not_found"},
	} {
		rec := call(h, "POST", tc.path, key, tc.body)
		e, c := errBody(rec.Body.Bytes())
		if rec.Code != tc.status || c != tc.code || (tc.status == 409 || tc.code == "bad_action") && e != tc.code {
			t.Errorf("%s %s: %d %s", tc.path, tc.body, rec.Code, rec.Body)
		}
	}
}
