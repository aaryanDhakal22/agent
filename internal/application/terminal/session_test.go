package terminal

import (
	"errors"
	"testing"

	"quiccpos/agent/internal/domain/terminal"
)

// A staff member main switched off, took off or gave a new PIN is logged
// out on every terminal as soon as the new config is in use: SSE
// terminal.session.ended per session, journal session.ended, and the next
// call with it is 401 login_required (contract §2.10u).
func TestConfigEndsSessionsAtOnce(t *testing.T) {
	r := newRig(t)
	till2, till3, till4 := Actor{DeviceID: "dev-2"}, Actor{DeviceID: "dev-3"}, Actor{DeviceID: "dev-4"}
	off := false
	cfg2 := staffConfig(t, 2, terminal.Staff{ID: "cash-3", FirstName: "Dee", Role: terminal.RoleCashier, PinHash: hash(t, "4321")})
	r.main.set(PathTerminalConfig, cfg2)
	if _, err := r.svc.Store().Refresh(r.ctx, terminal.KindTerminalConfig); err != nil {
		t.Fatal(err)
	}
	r.login(t, till, "1234")  // cash-1: switched off below
	r.login(t, till2, "55")   // cash-2: a new PIN below
	r.login(t, till3, "9876") // mgr-1: untouched
	r.login(t, till4, "4321") // cash-3: taken off the list below
	// main's next config: the same hashes but for the changes below.
	cfg := map[string]any{}
	for k, v := range cfg2 {
		cfg[k] = v
	}
	cfg["version"] = 3
	var staff []terminal.Staff
	for _, st := range cfg2["staff"].([]terminal.Staff) {
		if st.ID != "cash-3" {
			staff = append(staff, st)
		}
	}
	cfg["staff"] = staff
	for i := range staff {
		switch staff[i].ID {
		case "cash-1":
			staff[i].Active = &off
		case "cash-2":
			staff[i].PinHash = hash(t, "66")
		}
	}
	r.main.set(PathTerminalConfig, cfg)
	if changed, err := r.svc.Store().Refresh(r.ctx, terminal.KindTerminalConfig); err != nil || !changed {
		t.Fatalf("config refresh: %v %v", changed, err)
	}
	if n := r.pub.count(EventSessionEnded); n != 3 {
		t.Fatalf("session.ended events %d", n)
	}
	got := map[string]string{}
	for _, e := range journalOf(t, r, terminal.ActionSessionEnded) {
		got[e["_staff"].(string)] = e["reason"].(string)
	}
	want := map[string]string{"cash-1": terminal.SessionDeactivated, "cash-2": terminal.SessionPinChanged, "cash-3": terminal.SessionRemoved}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("ended %v, want %v", got, want)
		}
	}
	for _, a := range []Actor{till, till2, till4} {
		if _, _, err := r.svc.staffOf(a); !errors.Is(err, terminal.ErrLoginRequired) {
			t.Fatalf("%s still logged in: %v", a.DeviceID, err)
		}
		if v := r.svc.Session(a); v.Staff != nil {
			t.Fatalf("%s session %+v", a.DeviceID, v.Staff)
		}
	}
	if st, _, err := r.svc.staffOf(till3); err != nil || st.ID != "mgr-1" {
		t.Fatalf("the manager's session ended: %v", err)
	}
	// The new PIN logs in; the old one doesn't.
	if _, err := r.svc.Login(r.ctx, till2, "55"); !errors.Is(err, terminal.ErrWrongPIN) {
		t.Fatalf("old PIN: %v", err)
	}
	r.login(t, till2, "66")
	// The same config again ends nothing more.
	if changed, _ := r.svc.Store().Refresh(r.ctx, terminal.KindTerminalConfig); changed || r.pub.count(EventSessionEnded) != 3 {
		t.Fatalf("a same config ended more sessions")
	}
	if e, ok := r.pub.last(EventSessionEnded).(endedSession); !ok || e.DeviceID == "" || e.Reason == "" {
		t.Fatalf("payload %+v", r.pub.last(EventSessionEnded))
	}
}
