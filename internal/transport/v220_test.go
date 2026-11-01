package transport

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"quiccpos/agent/internal/application/devlog"
)

func TestTabletVersionRecorded(t *testing.T) {
	ctx := context.Background()
	s, h, key := diagServer(t)
	get := func(version string) {
		t.Helper()
		req := httptest.NewRequest("GET", "/api/diag", nil)
		req.Header.Set(deviceKeyHeader, key)
		if version != "" {
			req.Header.Set(quibaseVersionHeader, version)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("diag %d %s", rec.Code, rec.Body)
		}
	}
	get("1.7.0+1761400000")
	ds, _ := s.devices.List(ctx)
	if len(ds) != 1 || ds[0].AppVersion != "1.7.0+1761400000" || ds[0].AppVersionSeenAt == nil {
		t.Fatalf("stored: %+v", ds)
	}
	first := *ds[0].AppVersionSeenAt
	get("1.7.0+1761400000") // unchanged within 10 min: no write
	ds, _ = s.devices.List(ctx)
	if !ds[0].AppVersionSeenAt.Equal(first) {
		t.Fatal("an unchanged version was written again")
	}
	get("bad\x01version") // ignored
	get(strings.Repeat("9", 80))
	get("")
	ds, _ = s.devices.List(ctx)
	if ds[0].AppVersion != "1.7.0+1761400000" {
		t.Fatalf("junk header stored: %q", ds[0].AppVersion)
	}

	// /api/diag lists the tablet; on the event stream it counts as connected
	// and feeds floor.tablets.version.
	done := s.tabletOnStream("dev-1")
	rec := call(h, "GET", "/api/diag", key, "")
	var d struct {
		Tablets []TabletInfo `json:"tablets"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &d)
	if len(d.Tablets) != 1 || d.Tablets[0].DeviceID != "dev-1" || d.Tablets[0].Name != "Counter" ||
		d.Tablets[0].AppVersion != "1.7.0+1761400000" || !d.Tablets[0].Connected || d.Tablets[0].LastSeenAt == nil {
		t.Fatalf("diag tablets: %s", rec.Body)
	}
	if v := s.TabletVersions(); v["1.7.0+1761400000"] != 1 || len(v) != 1 {
		t.Fatalf("versions: %v", v)
	}
	done()
	if v := s.TabletVersions(); len(v) != 0 {
		t.Fatalf("versions after disconnect: %v", v)
	}
	// A new version is written at once (and the device list shows it).
	get("1.7.1+1761500000")
	ds, _ = s.devices.List(ctx)
	if ds[0].AppVersion != "1.7.1+1761500000" {
		t.Fatalf("new version: %q", ds[0].AppVersion)
	}
}

func TestV220DeviceRoutes(t *testing.T) {
	s, h, key := diagServer(t)
	ring := devlog.New(0)
	s.SetDevLog(ring)
	for _, rt := range []struct{ method, path, body string }{
		{"GET", "/api/settings/auto-accept", ""}, {"PUT", "/api/settings/auto-accept", `{"auto_accept":true}`},
		{"GET", "/api/dev/events", ""}, {"GET", "/api/deliveries/next-check", ""}, {"GET", "/api/settings/deliveries", ""},
		{"PUT", "/api/settings/auto-update", `{"auto_update":false}`},
	} {
		if rec := call(h, rt.method, rt.path, "", rt.body); rec.Code != 401 {
			t.Fatalf("%s %s without a key: %d", rt.method, rt.path, rec.Code)
		}
	}
	if rec := call(h, "PUT", "/api/settings/auto-accept", key, `{"auto_accept":true}`); rec.Code != 200 {
		t.Fatalf("tablet auto-accept: %d %s", rec.Code, rec.Body)
	}
	rec := call(h, "GET", "/api/settings/auto-accept", key, "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"auto_accept":true`) {
		t.Fatalf("read auto-accept: %d %s", rec.Code, rec.Body)
	}
	rec = call(h, "GET", "/api/dev/events?since=0", key, "")
	var ev struct {
		Events  []devlog.Event `json:"events"`
		LastSeq int64          `json:"last_seq"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &ev)
	if rec.Code != 200 || ev.LastSeq != 1 || len(ev.Events) != 1 || ev.Events[0].Text != "Auto-accept: off → on (tablet Counter)" {
		t.Fatalf("dev events: %d %s", rec.Code, rec.Body)
	}
	if rec = call(h, "GET", "/api/dev/events?since=1", key, ""); !strings.Contains(rec.Body.String(), `"events":[]`) {
		t.Fatalf("since=last: %s", rec.Body)
	}
	rec = call(h, "GET", "/api/deliveries/next-check", key, "")
	if rec.Code != 200 || rec.Body.String() != `{"driver_id":null,"rule":"detour","nearby_order_ids":[],"reason":""}`+"\n" {
		t.Fatalf("next-check: %d %s", rec.Code, rec.Body)
	}
	rec = call(h, "PUT", "/api/settings/deliveries", key, `{"nearby_rule":"bundle","nearby_distance_mi":2.5}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"nearby_rule":"bundle"`) ||
		!strings.Contains(rec.Body.String(), `"soon_ready_minutes":5`) || !strings.Contains(rec.Body.String(), `"delivery_radius_m":null`) {
		t.Fatalf("deliveries: %d %s", rec.Code, rec.Body)
	}
	if rec = call(h, "PUT", "/api/settings/deliveries", key, `{"nearby_rule":"nope"}`); rec.Code != 400 || code(rec) != "bad_request" {
		t.Fatalf("bad rule: %d %s", rec.Code, rec.Body)
	}
	// No main wired: a radius can't be set.
	if rec = call(h, "PUT", "/api/settings/deliveries", key, `{"delivery_radius_m":5000}`); rec.Code != 503 || code(rec) != "main_offline" {
		t.Fatalf("radius offline: %d %s", rec.Code, rec.Body)
	}
	// No update requester here: auto-update isn't available.
	if rec = call(h, "GET", "/api/settings/auto-update", key, ""); rec.Code != 503 {
		t.Fatalf("auto-update without updates: %d", rec.Code)
	}
}
