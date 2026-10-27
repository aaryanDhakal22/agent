package transport

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

// 2.3.2 PREVIEW on this tablet: tablet asks -> the hub uploads the WAV (its
// key only) -> the tablet fetches it; errors, size cap, unknown ids.
func TestCalloutPreviewRoutes(t *testing.T) {
	s, h, tablet := diagServer(t)
	hub := integrationKey(t, s)
	wav := append([]byte("RIFF\x00\x00\x00\x00WAVE"), make([]byte, 600)...)
	put := func(key, id, ctype string, body []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest("PUT", "/api/integration/callout-audio/"+id, bytes.NewReader(body))
		req.Header.Set(deviceKeyHeader, key)
		req.Header.Set("Content-Type", ctype)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// No hub yet: 503 hub_not_connected; the view says this Pi can preview.
	if rec := call(h, "POST", "/api/callout/preview", tablet, `{"number":122}`); rec.Code != http.StatusServiceUnavailable || code(rec) != "hub_not_connected" {
		t.Fatalf("no hub = %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "GET", "/api/callout", tablet, ""); !strings.Contains(rec.Body.String(), `"preview":true`) {
		t.Fatalf("view = %s", rec.Body)
	}
	// An older hub (no preview in its report): hub_update_needed.
	call(h, "PUT", "/api/integration/status", hub, `{"ok":true,"insert_mode":"off","problems":[],"callout":{"installed":true}}`)
	if rec := call(h, "POST", "/api/callout/preview", tablet, `{"number":122}`); rec.Code != http.StatusServiceUnavailable || code(rec) != "hub_update_needed" {
		t.Fatalf("older hub = %d %s", rec.Code, rec.Body)
	}
	call(h, "PUT", "/api/integration/status", hub, `{"ok":true,"insert_mode":"off","problems":[],"callout":{"installed":true,"preview":true}}`)

	// Auth: no key 401, the hub may not ask or fetch, a tablet may not upload.
	if rec := call(h, "POST", "/api/callout/preview", "", `{"number":122}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no key = %d", rec.Code)
	}
	if rec := call(h, "POST", "/api/callout/preview", hub, `{"number":122}`); rec.Code != http.StatusForbidden {
		t.Fatalf("hub asks = %d", rec.Code)
	}
	if rec := call(h, "POST", "/api/callout/preview", tablet, `{"number":10000}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad number = %d", rec.Code)
	}

	rec := call(h, "POST", "/api/callout/preview", tablet, `{"number":122,"voice":"kokoro:am_michael"}`)
	var asked struct{ ID, Status string }
	_ = json.Unmarshal(rec.Body.Bytes(), &asked)
	if rec.Code != http.StatusAccepted || asked.Status != "pending" || asked.ID == "" {
		t.Fatalf("ask = %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "GET", "/api/callout/preview/"+asked.ID, tablet, ""); rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), "pending") {
		t.Fatalf("pending = %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "GET", "/api/callout/preview/"+asked.ID+"?wait=99", tablet, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad wait = %d", rec.Code)
	}
	if rec := call(h, "GET", "/api/callout/preview/"+asked.ID, hub, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("hub fetch = %d", rec.Code)
	}

	if rec := put(tablet, asked.ID, "audio/wav", wav); rec.Code != http.StatusForbidden {
		t.Fatalf("tablet upload = %d", rec.Code)
	}
	if rec := put(hub, "0198b4a6-0000-7000-8000-000000000000", "audio/wav", wav); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown id = %d %s", rec.Code, rec.Body)
	}
	if rec := put(hub, asked.ID, "audio/wav", []byte(strings.Repeat("x", 100))); rec.Code != http.StatusBadRequest || code(rec) != "not_wav" {
		t.Fatalf("not wav = %d %s", rec.Code, rec.Body)
	}
	big := append(append([]byte{}, wav...), make([]byte, 2<<20)...)
	if rec := put(hub, asked.ID, "audio/wav", big); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("too large = %d %s", rec.Code, rec.Body)
	}
	if rec := put(hub, asked.ID, "audio/wav", wav); rec.Code != http.StatusNoContent {
		t.Fatalf("upload = %d %s", rec.Code, rec.Body)
	}
	rec = call(h, "GET", "/api/callout/preview/"+asked.ID+"?wait=2", tablet, "")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "audio/wav" || !bytes.Equal(rec.Body.Bytes(), wav) {
		t.Fatalf("audio = %d %s %d bytes", rec.Code, rec.Header().Get("Content-Type"), rec.Body.Len())
	}

	// The hub reports a failure: 503 with its reason.
	rec = call(h, "POST", "/api/callout/preview", tablet, `{"number":7}`)
	_ = json.Unmarshal(rec.Body.Bytes(), &asked)
	if rec := put(hub, asked.ID, "application/json", []byte(`{"error":"voice is not installed on this box","code":"not_installed"}`)); rec.Code != http.StatusNoContent {
		t.Fatalf("fail = %d %s", rec.Code, rec.Body)
	}
	rec = call(h, "GET", "/api/callout/preview/"+asked.ID, tablet, "")
	if rec.Code != http.StatusServiceUnavailable || code(rec) != "not_installed" || !strings.Contains(rec.Body.String(), "not installed") {
		t.Fatalf("failed = %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, "GET", "/api/callout/preview/nope", tablet, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown = %d", rec.Code)
	}
}

// The global body cap stays 1 MiB everywhere but the preview upload.
func TestCapBodies(t *testing.T) {
	var got int
	h := capBodies(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = len(b)
	}))
	body := make([]byte, 1<<20+500)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("PUT", "/api/integration/status", bytes.NewReader(body)))
	if got != 1<<20 {
		t.Fatalf("status body read %d", got)
	}
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("PUT", calloutAudioPath+"x", bytes.NewReader(body)))
	if got != len(body) {
		t.Fatalf("audio body read %d", got)
	}
}
