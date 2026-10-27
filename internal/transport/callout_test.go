package transport

import (
	"encoding/json"
	"net/http"
	"testing"
)

// 00020 routes: finishing + callout settings, GET /api/callout with the
// hub's report, TEST CALLOUT, change OK; the hub reads both settings.
func TestCalloutRoutes(t *testing.T) {
	s, h, tablet := diagServer(t)
	hub := integrationKey(t, s)

	if rec := call(h, "GET", "/api/settings/finishing", tablet, ""); rec.Code != http.StatusOK || rec.Body.String() != "{\"online_pickup_minutes\":30}\n" {
		t.Fatalf("finishing = %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "PUT", "/api/settings/finishing", tablet, `{"online_pickup_minutes":300}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad finishing = %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "PUT", "/api/settings/callout", tablet, `{"voice":"kokoro:bf_emma","music_dip":40}`); rec.Code != http.StatusOK {
		t.Fatalf("callout put = %d %s", rec.Code, rec.Body)
	}
	var set map[string]any
	rec := call(h, "GET", "/api/settings/callout", hub, "")
	_ = json.Unmarshal(rec.Body.Bytes(), &set)
	if rec.Code != http.StatusOK || set["voice"] != "kokoro:bf_emma" || set["music_dip"] != 40.0 || set["enabled"] != true || set["volume"] != 90.0 {
		t.Fatalf("hub reads callout = %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "PUT", "/api/settings/callout", hub, `{"enabled":false}`); rec.Code != http.StatusForbidden {
		t.Fatalf("hub may not write settings: %d", rec.Code)
	}

	// The hub reports its voices in its status; the tablet reads them.
	if rec := call(h, "PUT", "/api/integration/status", hub, `{"ok":true,"insert_mode":"off","problems":[],
		"callout":{"voices":[{"id":"kokoro:af_heart","name":"Heart","ready":999,"total":999}],"default_voice":"kokoro:af_heart"}}`); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	var view struct {
		Settings     map[string]any `json:"settings"`
		HubConnected bool           `json:"hub_connected"`
		Hub          struct {
			Voices []map[string]any `json:"voices"`
		} `json:"hub"`
	}
	rec = call(h, "GET", "/api/callout", tablet, "")
	_ = json.Unmarshal(rec.Body.Bytes(), &view)
	if rec.Code != http.StatusOK || !view.HubConnected || len(view.Hub.Voices) != 1 || view.Settings["voice"] != "kokoro:bf_emma" {
		t.Fatalf("callout view = %d %s", rec.Code, rec.Body)
	}

	if rec := call(h, "POST", "/api/callout/test", tablet, `{"number":102}`); rec.Code != http.StatusAccepted {
		t.Fatalf("test = %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "POST", "/api/callout/test", tablet, `{"number":0}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad test = %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "POST", "/api/orders/0198b4a6-0000-7000-8000-000000000000/change/ack", tablet, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("ack unknown = %d %s", rec.Code, rec.Body)
	}
	var cfg map[string]any
	rec = call(h, "GET", "/api/integration/config", hub, "")
	_ = json.Unmarshal(rec.Body.Bytes(), &cfg)
	if cfg["callout"] == nil || cfg["finishing"] == nil {
		t.Fatalf("config lacks callout/finishing: %s", rec.Body)
	}
}
