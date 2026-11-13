package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"quiccpos/agent/internal/application/storeupdate"

	"github.com/rs/zerolog"
)

// storeRoundServer: setupServer + a coordinator whose round has a terminal
// update and the terminal connected, so it can answer ready.
func storeRoundServer(t *testing.T) (s *Server, h http.Handler, tabletKey, termKey string, c *storeupdate.Coordinator) {
	t.Helper()
	s, h, tabletKey, termKey, _ = setupServer(t)
	c = storeupdate.New(storeupdate.Deps{
		Path:    filepath.Join(t.TempDir(), "store-update.json"),
		Settle:  -1,
		Version: "2.5.3",
		Compat:  storeupdate.Compat{MinQuibase: "2.1.0", MinTerminal: "1.0.3"},
		Logger:  zerolog.Nop(),
		Devices: func(context.Context) []storeupdate.Device {
			return []storeupdate.Device{{ID: "term-1", Name: "Counter 1", Kind: storeupdate.KindTerminal, Version: "1.0.5+100", Connected: true}}
		},
		Cached: func(app string) *storeupdate.Release {
			if app == storeupdate.AppTerminal {
				return &storeupdate.Release{VersionCode: 200, VersionName: "1.0.6"}
			}
			return &storeupdate.Release{VersionCode: 300, VersionName: "2.1.1"}
		},
	})
	s.SetStoreUpdate(c)
	return s, h, tabletKey, termKey, c
}

func keyed(h http.Handler, method, path, key, header, version, body string) *httptest.ResponseRecorder {
	req, _ := http.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set(deviceKeyHeader, key)
	if version != "" {
		req.Header.Set(header, version)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// Below the floor: 426 app_update_required everywhere but the update routes.
func TestAppGateLocksOldApps(t *testing.T) {
	_, h, tabletKey, termKey, _ := storeRoundServer(t)
	rec := keyed(h, "GET", "/api/floor", tabletKey, quibaseVersionHeader, "2.0.5+1", "")
	if rec.Code != http.StatusUpgradeRequired {
		t.Fatalf("old tablet on /api/floor: %d %s", rec.Code, rec.Body)
	}
	var body struct {
		Code    string               `json:"code"`
		Min     string               `json:"min_version"`
		Release *storeupdate.Release `json:"release"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Code != "app_update_required" || body.Min != "2.1.0" || body.Release == nil || body.Release.VersionName != "2.1.1" {
		t.Fatalf("426 body: %s", rec.Body)
	}
	for _, p := range []string{"/api/store-update", "/api/app/latest"} {
		if rec := keyed(h, "GET", p, tabletKey, quibaseVersionHeader, "2.0.5+1", ""); rec.Code == http.StatusUpgradeRequired {
			t.Fatalf("%s must stay open to a locked tablet", p)
		}
	}
	var view storeupdate.View
	rec = keyed(h, "GET", "/api/store-update", tabletKey, quibaseVersionHeader, "2.0.5+1", "")
	_ = json.Unmarshal(rec.Body.Bytes(), &view)
	if rec.Code != 200 || view.Me == nil || !view.Me.Locked || view.Min.Quibase != "2.1.0" {
		t.Fatalf("store-update for a locked tablet: %d %s", rec.Code, rec.Body)
	}
	// At the floor, and with no version at all: open.
	if rec := keyed(h, "GET", "/api/floor", tabletKey, quibaseVersionHeader, "2.1.0+1", ""); rec.Code == http.StatusUpgradeRequired {
		t.Fatal("a tablet at the floor is not locked")
	}
	// A terminal below its floor: its /api/terminal/* routes too, not its app routes.
	if rec := keyed(h, "GET", "/api/terminal/status", termKey, terminalVersionHeader, "1.0.2+1", ""); rec.Code != http.StatusUpgradeRequired {
		t.Fatalf("old terminal on /api/terminal/status: %d", rec.Code)
	}
	if rec := keyed(h, "GET", "/api/terminal/app/latest", termKey, terminalVersionHeader, "1.0.2+1", ""); rec.Code == http.StatusUpgradeRequired {
		t.Fatal("the terminal's app routes stay open")
	}
	if rec := keyed(h, "GET", "/api/store-update", termKey, terminalVersionHeader, "1.0.2+1", ""); rec.Code != 200 {
		t.Fatalf("a terminal key reads /api/store-update: %d %s", rec.Code, rec.Body)
	}
}

// The ready answer: 409 outside preparing, 204 inside, 400 on a bad body.
func TestStoreUpdateReadyRoute(t *testing.T) {
	_, h, tabletKey, termKey, c := storeRoundServer(t)
	if rec := keyed(h, "POST", "/api/store-update/ready", termKey, terminalVersionHeader, "1.0.5+100", `{"id":"su-x","ready":true}`); rec.Code != http.StatusConflict {
		t.Fatalf("idle: %d %s", rec.Code, rec.Body)
	}
	c.Tick(context.Background()) // the terminal is behind the cached 1.0.6: a round
	v := c.View(nil)
	if v.Phase != storeupdate.PhasePreparing {
		t.Fatalf("phase %s (%s)", v.Phase, v.Reason)
	}
	if rec := keyed(h, "POST", "/api/store-update/ready", termKey, terminalVersionHeader, "1.0.5+100", `{"id":"`+v.ID+`"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("no ready field: %d", rec.Code)
	}
	if rec := keyed(h, "POST", "/api/store-update/ready", tabletKey, quibaseVersionHeader, "2.1.1+300", `{"id":"`+v.ID+`","ready":true}`); rec.Code != http.StatusConflict {
		t.Fatalf("a device the round doesn't wait for: %d", rec.Code)
	}
	if rec := keyed(h, "POST", "/api/store-update/ready", termKey, terminalVersionHeader, "1.0.5+100", `{"id":"`+v.ID+`","ready":true}`); rec.Code != http.StatusNoContent {
		t.Fatalf("ready: %d %s", rec.Code, rec.Body)
	}
	c.Tick(context.Background())
	if p := c.Phase(); p != storeupdate.PhaseDevicesInstalling {
		t.Fatalf("after ready: %s", p)
	}
	var view storeupdate.View
	rec := keyed(h, "GET", "/api/store-update", termKey, terminalVersionHeader, "1.0.5+100", "")
	_ = json.Unmarshal(rec.Body.Bytes(), &view)
	if view.Me == nil || !view.Me.Behind || view.Terminal == nil || view.Terminal.VersionCode != 200 {
		t.Fatalf("devices_installing view: %s", rec.Body)
	}
}

// Writes in flight are counted (the round's quiet check); GETs aren't.
func TestWritesInFlight(t *testing.T) {
	s := &Server{}
	seen := int64(-1)
	h := s.countWrites(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen = s.WritesInFlight() }))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/api/orders/x/accept", nil))
	if seen != 1 || s.WritesInFlight() != 0 {
		t.Fatalf("POST: seen %d, after %d", seen, s.WritesInFlight())
	}
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/floor", nil))
	if seen != 0 {
		t.Fatalf("GET counted: %d", seen)
	}
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/api/store-update/ready", nil))
	if seen != 0 {
		t.Fatalf("the ready answer counted: %d", seen)
	}
}
