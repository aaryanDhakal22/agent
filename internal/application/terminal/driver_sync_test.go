package terminal

import (
	"errors"
	"sort"
	"testing"

	orderApp "quiccpos/agent/internal/application/order"
	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/terminal"
	"quiccpos/agent/internal/infra/ssebroker"
	"quiccpos/agent/internal/observability"

	"github.com/rs/zerolog"
)

// One driver identity (contract/agent.md §2.1f): the roster follows main's
// Store › Staff drivers.

// syncRig: a terminal service with the given staff config, the real floor
// service (temp SQLite) and no sync run yet.
func syncRig(t *testing.T, staff ...terminal.Staff) (*rig, *orderApp.Service) {
	t.Helper()
	r := newRig(t)
	r.main.set(PathTerminalConfig, staffConfig(t, 2, staff...))
	if _, err := r.svc.store.Refresh(r.ctx, terminal.KindTerminalConfig); err != nil {
		t.Fatal(err)
	}
	meters, _ := observability.NewMeters()
	floor := orderApp.NewService(r.repo, nil, nil, nil, ssebroker.New(), zerolog.Nop(), meters)
	r.svc.SetFloor(floor, r.repo)
	return r, floor
}

func setStaff(t *testing.T, r *rig, version int64, staff ...terminal.Staff) {
	t.Helper()
	r.main.set(PathTerminalConfig, staffConfig(t, version, staff...))
	if _, err := r.svc.store.Refresh(r.ctx, terminal.KindTerminalConfig); err != nil {
		t.Fatal(err)
	}
}

func rosterByName(t *testing.T, r *rig, floor *orderApp.Service) map[string]order.Driver {
	t.Helper()
	ds, err := floor.ListDrivers(r.ctx)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]order.Driver{}
	for _, d := range ds {
		if _, dup := out[d.Name]; dup {
			t.Fatalf("two drivers named %q: %+v", d.Name, ds)
		}
		out[d.Name] = d
	}
	return out
}

func boolPtr(b bool) *bool { return &b }

// Lombardi's prod on 2026-11-13: main's Store › Staff drivers are first
// names only (Abdul, Ahmadou, Antonio, Carlos, Juma) and the Pi's roster
// (made on Quibase) has the same five names, Antonio and Abdul clocked in.
// The first sync links 5 / 5 by name: same driver ids, nobody created or
// renamed, the clocked-in two stay in. A Pi-only driver stays, not in Staff.
func TestStaffSyncLinksProdFirstNameOnly(t *testing.T) {
	names := []string{"Abdul", "Ahmadou", "Antonio", "Carlos", "Juma"}
	var staff []terminal.Staff
	for i, n := range names {
		staff = append(staff, terminal.Staff{ID: "st-" + n, FirstName: n, Role: terminal.RoleDriver, PinHash: hash(t, "7"+string(rune('0'+i)))})
	}
	r, floor := syncRig(t, staff...)
	ids := map[string]string{}
	for _, n := range append(append([]string{}, names...), "Bob") {
		d, err := floor.CreateDriver(r.ctx, n)
		if err != nil {
			t.Fatal(err)
		}
		ids[n] = d.ID
	}
	for _, n := range []string{"Antonio", "Abdul"} {
		if _, err := floor.ClockIn(r.ctx, ids[n]); err != nil {
			t.Fatal(err)
		}
	}
	res, err := r.svc.SyncStaffDrivers(r.ctx)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(res.Linked)
	if len(res.Linked) != 5 || len(res.Created) != 0 || len(res.Renamed) != 0 || len(res.Retired) != 0 {
		t.Fatalf("prod: link 5 / 5 by name, nothing else: %+v", res)
	}
	got := rosterByName(t, r, floor)
	if len(got) != 6 {
		t.Fatalf("roster: %+v", got)
	}
	for _, n := range names {
		d := got[n]
		if d.ID != ids[n] || d.StaffID != "st-"+n || !d.InStaff {
			t.Fatalf("%s: linked to his staff id, same driver: %+v", n, d)
		}
		want := order.DriverOff
		if n == "Antonio" || n == "Abdul" {
			want = order.DriverAvailable
		}
		if d.Status != want {
			t.Fatalf("%s: status %s, want %s", n, d.Status, want)
		}
	}
	if b := got["Bob"]; b.InStaff || b.StaffID != "" {
		t.Fatalf("Bob is Pi-only (not in Staff): %+v", b)
	}
	// Juma logs in on a terminal: his driver is the one Quibase had, clocked
	// state and all.
	login, err := r.svc.Login(r.ctx, till2, "74")
	if err != nil || login.Driver == nil || login.Driver.ID != ids["Juma"] {
		t.Fatalf("Juma's login: %+v %v", login, err)
	}
	// A second pass changes nothing.
	again, err := r.svc.SyncStaffDrivers(r.ctx)
	if err != nil || len(again.Linked)+len(again.Created)+len(again.Renamed)+len(again.Reactivated)+len(again.Retired) != 0 {
		t.Fatalf("second sync: %+v %v", again, err)
	}
}

// A staff driver nobody made on Quibase appears on the roster (off) before
// any terminal login; his name follows main; switched off in main he leaves
// the roster once he is off, switched on again he is the same driver;
// Quibase can't rename or remove him, and an older Quibase adding his name
// gets him back instead of a second driver.
func TestStaffSyncCreatesRenamesRetires(t *testing.T) {
	ali := terminal.Staff{ID: "st-ali", FirstName: "Ali", LastName: "Khan", Role: terminal.RoleDriver, PinHash: hash(t, "77")}
	r, floor := syncRig(t, ali)
	if _, err := r.svc.SyncStaffDrivers(r.ctx); err != nil {
		t.Fatal(err)
	}
	d, ok := rosterByName(t, r, floor)["Ali K."]
	if !ok || d.StaffID != "st-ali" || d.Status != order.DriverOff {
		t.Fatalf("Ali K. on the roster, off, in Staff: %+v", rosterByName(t, r, floor))
	}
	// Quibase: no rename, no bin; the device stays editable.
	if err := floor.RenameDriver(r.ctx, d.ID, "Al"); !errors.Is(err, order.ErrStaffDriverName) {
		t.Fatalf("rename: %v", err)
	}
	if err := floor.RemoveDriver(r.ctx, d.ID); !errors.Is(err, order.ErrStaffDriverRemove) {
		t.Fatalf("remove: %v", err)
	}
	// An older Quibase's ADD with his name: him, nothing new.
	got, existed, err := floor.AddDriver(r.ctx, "ali k.")
	if err != nil || !existed || got.ID != d.ID {
		t.Fatalf("add his name: %+v %v %v", got, existed, err)
	}
	// A Pi-only driver: added, renamed and removed as before.
	bob, existed, err := floor.AddDriver(r.ctx, "Bob")
	if err != nil || existed || bob.InStaff {
		t.Fatalf("add Bob: %+v %v %v", bob, existed, err)
	}
	if err := floor.RenameDriver(r.ctx, bob.ID, "Bobby"); err != nil {
		t.Fatal(err)
	}
	if err := floor.RemoveDriver(r.ctx, bob.ID); err != nil {
		t.Fatal(err)
	}

	// Renamed in main → the roster follows (same driver).
	ali.FirstName = "Alistair"
	setStaff(t, r, 3, ali)
	if _, err := r.svc.SyncStaffDrivers(r.ctx); err != nil {
		t.Fatal(err)
	}
	if got := rosterByName(t, r, floor)["Alistair K."]; got.ID != d.ID {
		t.Fatalf("renamed: %+v", rosterByName(t, r, floor))
	}

	// Switched off in main while clocked in: stays until he is off.
	if _, err := floor.ClockIn(r.ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	ali.Active = boolPtr(false)
	setStaff(t, r, 4, ali)
	if res, err := r.svc.SyncStaffDrivers(r.ctx); err != nil || len(res.Retired) != 0 {
		t.Fatalf("clocked in: kept %+v %v", res, err)
	}
	if _, ok := rosterByName(t, r, floor)["Alistair K."]; !ok {
		t.Fatal("left the roster while clocked in")
	}
	if _, err := floor.ClockOut(r.ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	if res, err := r.svc.SyncStaffDrivers(r.ctx); err != nil || len(res.Retired) != 1 {
		t.Fatalf("off: retired %+v %v", res, err)
	}
	if _, ok := rosterByName(t, r, floor)["Alistair K."]; ok {
		t.Fatal("still on the roster after he was switched off and clocked out")
	}

	// Switched on again: the same driver, off.
	ali.Active = nil
	setStaff(t, r, 5, ali)
	res, err := r.svc.SyncStaffDrivers(r.ctx)
	if err != nil || len(res.Reactivated) != 1 || len(res.Created) != 0 {
		t.Fatalf("back: %+v %v", res, err)
	}
	if got := rosterByName(t, r, floor)["Alistair K."]; got.ID != d.ID || got.Status != order.DriverOff {
		t.Fatalf("back as the same driver: %+v", got)
	}

	// The role changed to cashier in main: off the roster too.
	ali.Role = terminal.RoleCashier
	setStaff(t, r, 6, ali)
	if res, err := r.svc.SyncStaffDrivers(r.ctx); err != nil || len(res.Retired) != 1 {
		t.Fatalf("no longer a driver: %+v %v", res, err)
	}
}

// Names a roster made on Quibase may have: "Ali Khan" (full) and a unique
// first name link; two Pi drivers with the first name don't (a new one).
func TestStaffSyncNameRules(t *testing.T) {
	r, floor := syncRig(t,
		terminal.Staff{ID: "st-ali", FirstName: "Ali", LastName: "Khan", Role: terminal.RoleDriver, PinHash: hash(t, "71")},
		terminal.Staff{ID: "st-sana", FirstName: "Sana", LastName: "Rao", Role: terminal.RoleDriver, PinHash: hash(t, "72")},
		terminal.Staff{ID: "st-omar", FirstName: "Omar", LastName: "Tee", Role: terminal.RoleDriver, PinHash: hash(t, "73")})
	full, _ := floor.CreateDriver(r.ctx, "Ali Khan")
	first, _ := floor.CreateDriver(r.ctx, "Sana")
	floor.CreateDriver(r.ctx, "Omar")
	floor.CreateDriver(r.ctx, "omar")
	res, err := r.svc.SyncStaffDrivers(r.ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]order.Driver{}
	ds, _ := floor.ListDrivers(r.ctx)
	for _, d := range ds {
		if d.StaffID != "" {
			got[d.StaffID] = d
		}
	}
	if got["st-ali"].ID != full.ID || got["st-ali"].Name != "Ali K." {
		t.Fatalf("Ali Khan linked by his full name, renamed as main names him: %+v", got["st-ali"])
	}
	if got["st-sana"].ID != first.ID || got["st-sana"].Name != "Sana R." {
		t.Fatalf("Sana linked by her unique first name: %+v", got["st-sana"])
	}
	if o := got["st-omar"]; o.Name != "Omar T." || len(res.Created) != 1 {
		t.Fatalf("two Omars on the roster: a new 'Omar T.': %+v %+v", o, res)
	}
}
