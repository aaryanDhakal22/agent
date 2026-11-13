package transport

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"quiccpos/agent/internal/application/appupdate"
)

// Terminals report their app-update state (contract 2.10w "From home", 2.5.3); the Pi keeps the last one
// per terminal, shows it in Terminals() (agent.health's terminals[]) and wakes the heartbeat on a change.
// main's terminals.update_now caches the newest release first, announces it and answers per terminal.
func TestTerminalAppStateAndUpdateNow(t *testing.T) {
	s, h, tabletKey, termKey, _ := setupServer(t)
	woke := 0
	s.OnTerminalAppState(func() { woke++ })
	body := `{"version_name":"1.0.5","version_code":1762900000,"state":"waiting_for_idle","reason":"a cart is open",
		"target_code":1762990000,"android":"11 (API 30)","permissions":{"install_packages":true}}`
	if rec := call(h, "POST", "/api/terminal/app/state", tabletKey, body); rec.Code != http.StatusForbidden {
		t.Fatalf("a tablet: %d", rec.Code)
	}
	if rec := call(h, "POST", "/api/terminal/app/state", termKey, `{"state":"bored"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("a bad state: %d", rec.Code)
	}
	if rec := call(h, "POST", "/api/terminal/app/state", termKey, body); rec.Code != http.StatusNoContent {
		t.Fatalf("report: %d %s", rec.Code, rec.Body)
	}
	call(h, "POST", "/api/terminal/app/state", termKey, body) // the same state again: no second wake
	if woke != 1 {
		t.Fatalf("heartbeat woken %d times", woke)
	}
	terms := s.Terminals(context.Background())
	if len(terms) != 1 || terms[0].Update == nil || terms[0].Update.State != "waiting_for_idle" ||
		terms[0].Update.Android != "11 (API 30)" || !terms[0].Update.Permissions["install_packages"] || terms[0].Update.ReceivedAt.IsZero() {
		t.Fatalf("terminals: %+v", terms)
	}
	// The terminal gauges (2.5.5): no X-Terminal-Version here, so the version comes from the app state.
	if apps := s.TerminalApps(context.Background()); len(apps) != 1 || apps[0].Device != "Counter 1" || apps[0].Connected ||
		apps[0].State != "waiting_for_idle" || apps[0].AppVersion != "1.0.5+1762900000" {
		t.Fatalf("terminal apps: %+v", apps)
	}

	// Update now: no release anywhere → no_release, nothing announced.
	var announced []appupdate.Release
	var cur *appupdate.Release
	fn := s.TerminalsUpdateNow(func(context.Context) error { return errors.New("main unreachable") },
		func() *appupdate.Release { return cur }, func(r appupdate.Release) { announced = append(announced, r) })
	out, _ := fn(context.Background(), nil)
	res := out.(UpdateNowResult)
	if res.OK || res.Code != "no_release" || res.CheckError == "" || len(announced) != 0 || len(res.Terminals) != 1 {
		t.Fatalf("no release: %+v", res)
	}
	// The check caches one: announced, the terminal (not on /api/events here) not told.
	fn = s.TerminalsUpdateNow(func(context.Context) error {
		cur = &appupdate.Release{App: "terminal", VersionCode: 1762990000, VersionName: "1.0.6"}
		return nil
	}, func() *appupdate.Release { return cur }, func(r appupdate.Release) { announced = append(announced, r) })
	out, _ = fn(context.Background(), nil)
	res = out.(UpdateNowResult)
	if !res.OK || res.Release == nil || res.Release.VersionCode != 1762990000 || len(announced) != 1 ||
		res.Terminals[0].Told || res.Terminals[0].UpToDate || res.Terminals[0].Name != "Counter 1" {
		t.Fatalf("update now: %+v", res)
	}
	raw, _ := json.Marshal(res)
	if !json.Valid(raw) {
		t.Fatal("reply must be JSON")
	}
}
