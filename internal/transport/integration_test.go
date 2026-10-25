package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// integrationKey pairs an integration device (the store hub) on the server's
// device repo and returns its key.
func integrationKey(t *testing.T, s *Server) string {
	t.Helper()
	key, _ := newDeviceKey()
	if err := s.devices.Create(context.Background(), "hub-1", "Store hub", "integration", hashDeviceKey(key), key[:11]); err != nil {
		t.Fatal(err)
	}
	return key
}

// An integration key reaches /api/integration/* and the read-only floor
// routes only; a tablet key (or no key) never reaches /api/integration/*.
func TestIntegrationKeyGuarding(t *testing.T) {
	s, h, tablet := diagServer(t)
	hub := integrationKey(t, s)

	// Tablet: 403 on every integration route, floor routes as before.
	for _, rt := range []struct{ method, path string }{
		{"GET", "/api/integration/config"},
		{"PUT", "/api/integration/status"},
		{"PUT", "/api/integration/orders/1234"},
		{"POST", "/api/integration/orders/1234/void"},
		{"PATCH", "/api/integration/online-orders/0198b4a6-0000-7000-8000-000000000000"},
		{"GET", "/api/integration/events"},
	} {
		if rec := call(h, rt.method, rt.path, tablet, "{}"); rec.Code != http.StatusForbidden || code(rec) != "integration_required" {
			t.Errorf("tablet %s %s = %d %s, want 403 integration_required", rt.method, rt.path, rec.Code, rec.Body)
		}
		if rec := call(h, rt.method, rt.path, "", "{}"); rec.Code != http.StatusUnauthorized {
			t.Errorf("no key %s %s = %d, want 401", rt.method, rt.path, rec.Code)
		}
	}
	if rec := call(h, "GET", "/api/drivers", tablet, ""); rec.Code != http.StatusOK {
		t.Fatalf("tablet floor route = %d", rec.Code)
	}

	// Hub: the integration routes work…
	if rec := call(h, "GET", "/api/integration/config", hub, ""); rec.Code != http.StatusOK {
		t.Fatalf("hub config = %d %s", rec.Code, rec.Body)
	} else {
		var cfg map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &cfg)
		for _, k := range []string{"location", "features", "tickets", "times", "drivers", "printers", "agent_version"} {
			if _, ok := cfg[k]; !ok {
				t.Errorf("config lacks %q: %s", k, rec.Body)
			}
		}
	}
	if rec := call(h, "PUT", "/api/integration/status", hub, `{"ok":true,"insert_mode":"shadow","problems":[]}`); rec.Code != http.StatusNoContent {
		t.Fatalf("hub status = %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "GET", "/api/integration/events?after=0", hub, ""); rec.Code != http.StatusOK {
		t.Fatalf("hub events = %d %s", rec.Code, rec.Body)
	}
	// … the read-only floor routes too …
	for _, p := range []string{"/api/floor", "/api/drivers", "/api/settings/features", "/api/settings/tickets", "/api/settings/times", "/api/settings/driver-reset"} {
		if rec := call(h, "GET", p, hub, ""); rec.Code != http.StatusOK {
			t.Errorf("hub GET %s = %d %s", p, rec.Code, rec.Body)
		}
	}
	// … and nothing else.
	for _, rt := range []struct{ method, path, body string }{
		{"GET", "/api/orders", ""},
		{"GET", "/api/diag", ""},
		{"POST", "/api/drivers", `{"name":"X"}`},
		{"PUT", "/api/settings/features", `{"pos_pull":false}`},
		{"POST", "/api/pairing/code", ""},
		{"POST", "/api/batch/close", `{}`},
	} {
		if rec := call(h, rt.method, rt.path, hub, rt.body); rec.Code != http.StatusForbidden || code(rec) != "integration_only" {
			t.Errorf("hub %s %s = %d %s, want 403 integration_only", rt.method, rt.path, rec.Code, rec.Body)
		}
	}
	// Web-UI-only routes still want a session.
	if rec := call(h, "GET", "/api/devices", hub, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("hub GET /api/devices = %d, want 401", rec.Code)
	}
}

func TestPairKind(t *testing.T) {
	s, h, _ := diagServer(t)
	pair := func(body string) (int, map[string]any) {
		code, _, _ := s.pairing.issue(time.Now())
		rec := call(h, "POST", "/api/pair", "", `{"code":"`+code+`",`+body+`}`)
		var m map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &m)
		return rec.Code, m
	}
	if st, m := pair(`"name":"Hub","kind":"robot"`); st != http.StatusBadRequest {
		t.Fatalf("bad kind = %d %v", st, m)
	}
	st, m := pair(`"name":"Hub","kind":"integration"`)
	if st != http.StatusCreated || m["kind"] != "integration" || m["device_key"] == nil || m["device_key"] != m["key"] {
		t.Fatalf("integration pair = %d %v", st, m)
	}
	if rec := call(h, "GET", "/api/integration/config", m["device_key"].(string), ""); rec.Code != http.StatusOK {
		t.Fatalf("paired hub key = %d", rec.Code)
	}
	st, m = pair(`"name":"Tab"`)
	if st != http.StatusCreated || m["kind"] != "tablet" {
		t.Fatalf("default kind = %d %v", st, m)
	}
	if rec := call(h, "GET", "/api/integration/config", m["key"].(string), ""); rec.Code != http.StatusForbidden {
		t.Fatalf("tablet key on integration = %d", rec.Code)
	}
}

func TestSettingsRoutes(t *testing.T) {
	_, h, tablet := diagServer(t)
	rec := call(h, "GET", "/api/settings/features", tablet, "")
	if rec.Code != 200 || rec.Body.String() != `{"pos_pull":true,"order_insert":false,"driver_writeback":true,"address_heal":true,"future_orders":true,"paid_wave":true}`+"\n" {
		t.Fatalf("features = %d %s", rec.Code, rec.Body)
	}
	rec = call(h, "PUT", "/api/settings/features", tablet, `{"order_insert":true}`)
	if rec.Code != 200 || rec.Body.String() != `{"pos_pull":true,"order_insert":true,"driver_writeback":true,"address_heal":true,"future_orders":true,"paid_wave":true}`+"\n" {
		t.Fatalf("partial PUT = %d %s", rec.Code, rec.Body)
	}
	if rec = call(h, "PUT", "/api/settings/features", tablet, `{"order_insert":"yes"}`); rec.Code != 400 {
		t.Fatalf("bad type = %d", rec.Code)
	}
	if rec = call(h, "GET", "/api/settings/tickets", tablet, ""); rec.Code != 200 ||
		rec.Body.String() != `{"pos_delivery":false,"pos_takeout":false,"pos_counter":false,"online_cash_pickup":true,"online_cash_delivery":true,"online_card":true}`+"\n" {
		t.Fatalf("tickets = %d %s", rec.Code, rec.Body)
	}
	if rec = call(h, "PUT", "/api/settings/times", tablet, `{"default_pickup_minutes":0}`); rec.Code != 400 {
		t.Fatalf("pickup 0 = %d", rec.Code)
	}
	if rec = call(h, "PUT", "/api/settings/times", tablet, `{"default_delivery_minutes":241}`); rec.Code != 400 {
		t.Fatalf("delivery 241 = %d", rec.Code)
	}
	rec = call(h, "PUT", "/api/settings/times", tablet, `{"default_delivery_minutes":40}`)
	if rec.Code != 200 || rec.Body.String() != `{"default_pickup_minutes":15,"default_delivery_minutes":40}`+"\n" {
		t.Fatalf("times = %d %s", rec.Code, rec.Body)
	}
	var floor struct {
		Settings struct {
			Features map[string]bool  `json:"features"`
			Times    map[string]int64 `json:"times"`
		} `json:"settings"`
		Integration struct {
			Connected  bool   `json:"connected"`
			InsertMode string `json:"insert_mode"`
		} `json:"integration"`
	}
	rec = call(h, "GET", "/api/floor", tablet, "")
	if err := json.Unmarshal(rec.Body.Bytes(), &floor); err != nil || !floor.Settings.Features["order_insert"] ||
		floor.Settings.Times["default_delivery_minutes"] != 40 || floor.Integration.Connected || floor.Integration.InsertMode != "off" {
		t.Fatalf("floor = %s", rec.Body)
	}
}
