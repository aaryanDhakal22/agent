package storeupdate

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// rig is a fake store: devices, a manifest, caches, a root updater.
type rig struct {
	t        *testing.T
	mu       sync.Mutex
	now      time.Time
	devices  []Device
	manifest Manifest
	cached   map[string]*Release
	busy     string
	auto     bool
	ownSHA   string
	piCalls  []string
	piResult string
	piOK     bool
	events   []View
	notes    []string
	path     string
}

func newRig(t *testing.T) *rig {
	return &rig{t: t, now: time.Date(2026, 11, 13, 15, 0, 0, 0, time.UTC), auto: true, ownSHA: "aaa", piOK: true,
		cached: map[string]*Release{}, path: filepath.Join(t.TempDir(), "store-update.json")}
}

func (r *rig) coordinator(version string) *Coordinator {
	c := New(Deps{
		Path:    r.path,
		Version: version,
		OwnSHA:  func() string { r.mu.Lock(); defer r.mu.Unlock(); return r.ownSHA },
		Compat:  Compat{MinQuibase: "2.1.0", MinTerminal: "1.0.3"},
		Now:     func() time.Time { r.mu.Lock(); defer r.mu.Unlock(); return r.now },
		Logger:  zerolog.Nop(),
		Devices: func(context.Context) []Device {
			r.mu.Lock()
			defer r.mu.Unlock()
			return append([]Device(nil), r.devices...)
		},
		Manifest: func(context.Context, bool) (Manifest, bool) {
			r.mu.Lock()
			defer r.mu.Unlock()
			return r.manifest, r.manifest.AgentVersion != ""
		},
		Cached:    func(app string) *Release { r.mu.Lock(); defer r.mu.Unlock(); return r.cached[app] },
		CheckApps: func(context.Context) {},
		AutoOn:    func(context.Context) bool { return r.auto },
		Busy:      func(context.Context) string { r.mu.Lock(); defer r.mu.Unlock(); return r.busy },
		PiUpdate: func(_ context.Context, id, _ string) (bool, string) {
			r.piCalls = append(r.piCalls, id)
			if !r.piOK {
				return false, "update_in_progress"
			}
			return true, ""
		},
		PiResult: func(context.Context, string) string { return r.piResult },
		Publish: func(_ string, v any) {
			r.events = append(r.events, v.(View))
		},
		Notify: func(_ context.Context, msg string) { r.notes = append(r.notes, msg) },
	})
	c.Load()
	return c
}

func (r *rig) later(d time.Duration) { r.mu.Lock(); r.now = r.now.Add(d); r.mu.Unlock() }

func (r *rig) setVersion(id, v string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.devices {
		if r.devices[i].ID == id {
			r.devices[i].Version = v
		}
	}
}

func ready(t *testing.T, c *Coordinator, dev string, ok bool, why string) {
	t.Helper()
	if err := c.Ready(context.Background(), dev, c.View(nil).ID, ok, why); err != nil {
		t.Fatalf("ready %s: %v", dev, err)
	}
}

func phase(t *testing.T, c *Coordinator, want string) {
	t.Helper()
	if got := c.Phase(); got != want {
		t.Fatalf("phase = %s, want %s (reason %q)", got, want, c.View(nil).Reason)
	}
}

var ctx = context.Background()

// The whole round: a new agent + Quibase + Centurion, the store quiet, every
// device ready → frozen → the Pi installs → the new agent boots → devices
// install → done.
func TestRoundAgentAndApps(t *testing.T) {
	r := newRig(t)
	r.devices = []Device{
		{ID: "t1", Name: "Counter tablet", Kind: KindTablet, Version: "2.1.1+100", Connected: true},
		{ID: "c1", Name: "Register 1", Kind: KindTerminal, Version: "1.0.5+100", Connected: true},
		{ID: "old", Name: "Old tablet", Kind: KindTablet, Version: "2.1.0+50", Connected: true}, // before the protocol
	}
	r.manifest = Manifest{AgentVersion: "2.5.4", AgentSHA: "bbb", MinQuibase: "2.1.0", MinTerminal: "1.0.3",
		Quibase: "2.1.2", Terminal: "1.0.6"}
	r.cached[AppQuibase] = &Release{VersionCode: 200, VersionName: "2.1.2"}
	r.cached[AppTerminal] = &Release{VersionCode: 200, VersionName: "1.0.6"}
	c := r.coordinator("2.5.3")

	r.busy = "an order is waiting to be accepted"
	c.Tick(ctx)
	phase(t, c, PhaseWaiting)
	if v := c.View(nil); !strings.Contains(v.Reason, "waiting to be accepted") || v.Agent.To != "2.5.4" ||
		v.Quibase.VersionName != "2.1.2" || v.Terminal.VersionName != "1.0.6" {
		t.Fatalf("waiting view: %+v", v)
	}

	r.busy = ""
	c.Tick(ctx)
	phase(t, c, PhasePreparing)
	v := c.View(&Device{ID: "t1", Kind: KindTablet, Version: "2.1.1+100"})
	if !v.Me.InRound || v.Me.Ready != nil || v.Me.Locked || v.Me.Behind {
		t.Fatalf("me while preparing: %+v", v.Me)
	}
	if err := c.Ready(ctx, "old", v.ID, true, ""); !errors.Is(err, ErrNotInRound) {
		t.Fatalf("an old-protocol tablet isn't waited for: %v", err)
	}
	ready(t, c, "t1", true, "")
	c.Tick(ctx)
	phase(t, c, PhasePreparing) // the terminal hasn't answered
	ready(t, c, "c1", false, "a cart is open")
	c.Tick(ctx)
	phase(t, c, PhasePreparing)
	ready(t, c, "c1", true, "")
	c.Tick(ctx)
	phase(t, c, PhasePiInstalling)
	if len(r.piCalls) != 1 {
		t.Fatalf("pi update calls: %v", r.piCalls)
	}
	if !c.Frozen() {
		t.Fatal("pi_installing is frozen for the devices")
	}
	// The old agent keeps running until the root updater stops it.
	r.piResult = "running"
	c.Tick(ctx)
	phase(t, c, PhasePiInstalling)

	// The new agent boots from the same state file.
	r.ownSHA = "bbb"
	c2 := r.coordinator("2.5.4")
	c2.Tick(ctx)
	phase(t, c2, PhaseDevicesInstalling)
	me := c2.View(&Device{ID: "t1", Kind: KindTablet, Version: "2.1.1+100"}).Me
	if !me.Behind {
		t.Fatalf("the tablet is behind the round's Quibase: %+v", me)
	}
	if sr := c2.View(nil).StoreRelease; sr.Quibase == nil || sr.Quibase.VersionName != "2.1.2" {
		t.Fatalf("the round's apps are the store's release now: %+v", sr)
	}
	r.setVersion("t1", "2.1.2+200")
	c2.Tick(ctx)
	phase(t, c2, PhaseDevicesInstalling)
	r.setVersion("c1", "1.0.6+200")
	c2.Tick(ctx)
	phase(t, c2, PhaseDone)
	// The old-protocol tablet is behind the store's release: it installs by itself.
	if me := c2.View(&Device{ID: "old", Kind: KindTablet, Version: "2.1.0+50"}).Me; !me.Behind || me.Locked {
		t.Fatalf("old tablet: %+v", me)
	}
	r.later(FinishedShown + time.Second)
	phase(t, c2, PhaseIdle)
	c2.Tick(ctx)
	phase(t, c2, PhaseIdle) // nothing left: no new round
	if len(r.notes) != 0 {
		t.Fatalf("no pages on a good round: %v", r.notes)
	}
}

// A device that doesn't get quiet in the window sends the round back to
// waiting for 5 minutes; nothing was frozen.
func TestRoundBusyDeviceRetries(t *testing.T) {
	r := newRig(t)
	r.devices = []Device{{ID: "c1", Name: "Register 1", Kind: KindTerminal, Version: "1.0.5+100", Connected: true}}
	r.manifest = Manifest{AgentVersion: "2.5.4", AgentSHA: "bbb"}
	c := r.coordinator("2.5.3")
	c.Tick(ctx)
	phase(t, c, PhasePreparing)
	ready(t, c, "c1", false, "a payment is open")
	r.later(PrepareWindow + time.Second)
	c.Tick(ctx)
	phase(t, c, PhaseWaiting)
	if v := c.View(nil); !strings.Contains(v.Reason, "Register 1 (a payment is open)") {
		t.Fatalf("reason: %q", v.Reason)
	}
	c.Tick(ctx)
	phase(t, c, PhaseWaiting) // retry later
	r.later(BusyRetry)
	c.Tick(ctx)
	phase(t, c, PhasePreparing)
	if len(r.piCalls) != 0 {
		t.Fatal("never froze")
	}
	// The Pi itself gets busy while preparing: back to waiting at once.
	r.busy = "printing"
	c.Tick(ctx)
	phase(t, c, PhaseWaiting)
}

// A disconnected device is excused; no device at all freezes at once.
func TestRoundNoDevicesAndDisconnects(t *testing.T) {
	r := newRig(t)
	r.manifest = Manifest{AgentVersion: "2.5.4", AgentSHA: "bbb"}
	c := r.coordinator("2.5.3")
	c.Tick(ctx)
	phase(t, c, PhasePiInstalling)

	r2 := newRig(t)
	r2.devices = []Device{{ID: "t1", Name: "T", Kind: KindTablet, Version: "2.1.1+1", Connected: true}}
	r2.manifest = Manifest{AgentVersion: "2.5.4", AgentSHA: "bbb"}
	c2 := r2.coordinator("2.5.3")
	c2.Tick(ctx)
	phase(t, c2, PhasePreparing)
	r2.devices[0].Connected = false
	c2.Tick(ctx)
	phase(t, c2, PhasePiInstalling)
}

// The Pi's update fails (rolled back): the round is cancelled before any
// device installed, the developer is paged, the same build isn't retried.
func TestRoundPiUpdateFails(t *testing.T) {
	r := newRig(t)
	r.devices = []Device{{ID: "t1", Name: "T", Kind: KindTablet, Version: "2.1.1+100", Connected: false}}
	r.manifest = Manifest{AgentVersion: "2.5.4", AgentSHA: "bbb", Quibase: "2.1.2"}
	r.cached[AppQuibase] = &Release{VersionCode: 200, VersionName: "2.1.2"}
	c := r.coordinator("2.5.3")
	c.Tick(ctx)
	phase(t, c, PhasePiInstalling)
	r.piResult = "rolled_back"
	c2 := r.coordinator("2.5.3") // the old agent is back
	c2.Tick(ctx)
	phase(t, c2, PhaseCancelled)
	if v := c2.View(nil); v.Reason != "pi_update_failed" || v.StoreRelease.Quibase != nil {
		t.Fatalf("cancelled view: %+v", v)
	}
	if me := c2.View(&Device{ID: "t1", Kind: KindTablet, Version: "2.1.1+100"}).Me; me.Behind {
		t.Fatal("no device installs after a failed Pi update")
	}
	if len(r.notes) != 1 || !strings.Contains(r.notes[0], "didn't work") {
		t.Fatalf("notes: %v", r.notes)
	}
	failedID := c2.View(nil).ID
	r.later(time.Hour)
	c2.Tick(ctx)
	if v := c2.View(nil); v.ID != failedID || v.Phase != PhaseIdle {
		t.Fatalf("neither the failed build nor its apps go out again: %+v", v)
	}
	r.manifest = Manifest{AgentVersion: "2.5.5", AgentSHA: "ccc", Quibase: "2.1.2"}
	c2.Tick(ctx)
	if v := c2.View(nil); v.Agent == nil || v.Agent.To != "2.5.5" {
		t.Fatalf("a newer build starts a round: %+v", v)
	}
}

// The manifest names an app version the Pi hasn't cached: the round waits
// for it (the Pi isn't updated alone).
func TestRoundWaitsForNamedApp(t *testing.T) {
	r := newRig(t)
	r.devices = []Device{{ID: "t1", Name: "T", Kind: KindTablet, Version: "2.1.1+100", Connected: true}}
	r.manifest = Manifest{AgentVersion: "2.5.4", AgentSHA: "bbb", Quibase: "2.1.2"}
	r.cached[AppQuibase] = &Release{VersionCode: 100, VersionName: "2.1.1"}
	c := r.coordinator("2.5.3")
	c.Tick(ctx)
	phase(t, c, PhaseWaiting)
	if v := c.View(nil); !strings.Contains(v.Reason, "waiting_for_apps: quibase 2.1.2") {
		t.Fatalf("reason %q", v.Reason)
	}
	// A newer APK than the manifest names is ignored too.
	r.cached[AppQuibase] = &Release{VersionCode: 300, VersionName: "2.1.3"}
	r.later(AppsCheckEvery)
	c.Tick(ctx)
	phase(t, c, PhaseWaiting)
	r.cached[AppQuibase] = &Release{VersionCode: 200, VersionName: "2.1.2"}
	r.later(AppsCheckEvery)
	c.Tick(ctx)
	phase(t, c, PhasePreparing)
	if v := c.View(nil); v.Quibase == nil || v.Quibase.VersionCode != 200 {
		t.Fatalf("target %+v", v.Quibase)
	}
}

// Without a version in the manifest (staging), a newer cached APK is a
// devices-only round: no Pi update.
func TestRoundAppsOnly(t *testing.T) {
	r := newRig(t)
	r.devices = []Device{
		{ID: "c1", Name: "Register 1", Kind: KindTerminal, Version: "1.0.5+100", Connected: true},
		{ID: "c2", Name: "Register 2", Kind: KindTerminal, Version: "1.0.5+100", Connected: false},
	}
	r.cached[AppTerminal] = &Release{VersionCode: 150, VersionName: "1.0.5"}
	c := r.coordinator("2.5.3")
	c.Tick(ctx)
	phase(t, c, PhasePreparing)
	ready(t, c, "c1", true, "")
	c.Tick(ctx)
	phase(t, c, PhaseDevicesInstalling)
	if len(r.piCalls) != 0 {
		t.Fatal("an apps-only round never updates the Pi")
	}
	r.setVersion("c1", "1.0.5+150")
	c.Tick(ctx)
	phase(t, c, PhaseDone) // c2 was off: it catches up by itself
	if me := c.View(&Device{ID: "c2", Kind: KindTerminal, Version: "1.0.5+100"}).Me; !me.Behind || me.InRound {
		t.Fatalf("c2: %+v", me)
	}
	// Cached = what the store runs: nothing more.
	r.later(time.Hour)
	c.Tick(ctx)
	phase(t, c, PhaseIdle)
}

// Nobody is behind the cached APK: the store is on it, no round.
func TestNoRoundWhenNobodyBehind(t *testing.T) {
	r := newRig(t)
	r.devices = []Device{{ID: "t1", Name: "T", Kind: KindTablet, Version: "2.1.1+100", Connected: true}}
	r.cached[AppQuibase] = &Release{VersionCode: 100, VersionName: "2.1.1"}
	c := r.coordinator("2.5.3")
	c.Tick(ctx)
	phase(t, c, PhaseIdle)
	if sr := c.View(nil).StoreRelease.Quibase; sr == nil || sr.VersionCode != 100 {
		t.Fatalf("store release adopted: %+v", sr)
	}
}

// auto_update off: no round by itself; a nudge starts one.
func TestAutoOffNudge(t *testing.T) {
	r := newRig(t)
	r.auto = false
	r.manifest = Manifest{AgentVersion: "2.5.4", AgentSHA: "bbb"}
	r.devices = []Device{{ID: "t1", Name: "T", Kind: KindTablet, Version: "2.1.1+1", Connected: true}}
	c := r.coordinator("2.5.3")
	c.Tick(ctx)
	phase(t, c, PhaseIdle)
	rep := c.Nudge(ctx, "console", "0f3c8a52-1d2e-4c7a-9e1b-6a5d4c3b2a10")
	if !rep.Accepted || rep.State != "waiting_for_idle" || rep.ID != "0f3c8a52-1d2e-4c7a-9e1b-6a5d4c3b2a10" {
		t.Fatalf("nudge: %+v", rep)
	}
	phase(t, c, PhasePreparing)
	// Nothing to do: up to date.
	r2 := newRig(t)
	if rep := r2.coordinator("2.5.3").Nudge(ctx, "console", ""); rep.State != "up_to_date" {
		t.Fatalf("nothing behind: %+v", rep)
	}
}

// Stragglers: the round ends after the install window, the Pi stays new,
// the developer is paged.
func TestRoundStragglers(t *testing.T) {
	r := newRig(t)
	r.devices = []Device{{ID: "c1", Name: "Register 1", Kind: KindTerminal, Version: "1.0.5+100", Connected: true}}
	r.cached[AppTerminal] = &Release{VersionCode: 150, VersionName: "1.0.5"}
	c := r.coordinator("2.5.3")
	c.Tick(ctx)
	ready(t, c, "c1", true, "")
	c.Tick(ctx)
	phase(t, c, PhaseDevicesInstalling)
	r.later(InstallWindow + time.Second)
	c.Tick(ctx)
	phase(t, c, PhaseDone)
	if v := c.View(nil); len(v.Stragglers) != 1 || v.Stragglers[0] != "Register 1" {
		t.Fatalf("stragglers %+v", v.Stragglers)
	}
	if len(r.notes) != 1 {
		t.Fatalf("notes %v", r.notes)
	}
}

func TestLockedFloor(t *testing.T) {
	c := newRig(t).coordinator("2.5.3")
	for _, tc := range []struct {
		kind, version string
		locked        bool
	}{
		{KindTablet, "2.0.5+1", true},
		{KindTablet, "2.1.0+1", false},
		{KindTablet, "2.1.0", false},
		{KindTablet, "", false},          // unknown is never locked
		{KindTablet, "dev-build", false}, // not a version
		{KindTerminal, "1.0.2+9", true},
		{KindTerminal, "1.0.3+9", false},
		{"integration", "0.0.1", false},
	} {
		if got, _ := c.Locked(tc.kind, tc.version); got != tc.locked {
			t.Errorf("Locked(%s, %q) = %v", tc.kind, tc.version, got)
		}
	}
	if Floors().MinQuibase == "" || Floors().MinTerminal == "" {
		t.Fatal("compat.json has both floors")
	}
}

func TestReadyRefusals(t *testing.T) {
	c := newRig(t).coordinator("2.5.3")
	if err := c.Ready(ctx, "x", "su-nope", true, ""); !errors.Is(err, ErrNotPreparing) {
		t.Fatalf("idle: %v", err)
	}
}

func TestPublishedOncePerChange(t *testing.T) {
	r := newRig(t)
	r.manifest = Manifest{AgentVersion: "2.5.4", AgentSHA: "bbb"}
	r.devices = []Device{{ID: "t1", Name: "T", Kind: KindTablet, Version: "2.1.1+1", Connected: true}}
	c := r.coordinator("2.5.3")
	c.Tick(ctx)
	c.Tick(ctx)
	c.Tick(ctx)
	if len(r.events) != 1 || r.events[0].Phase != PhasePreparing || r.events[0].Me != nil {
		t.Fatalf("events %+v", r.events)
	}
}

func TestVersionHelpers(t *testing.T) {
	if !behind("2.1.1+100", &Release{VersionCode: 200, VersionName: "2.1.1"}) {
		t.Fatal("code decides")
	}
	if behind("2.1.2", &Release{VersionCode: 200, VersionName: "2.1.1"}) {
		t.Fatal("name decides without a code")
	}
	if !onProtocol(Device{Kind: KindTablet, Version: "2.1.1+5"}) || onProtocol(Device{Kind: KindTablet, Version: "2.1.0+5"}) {
		t.Fatal("protocol versions")
	}
	if !newer("2.5.4", "2.5.3") || newer("2.5.3", "2.5.3") || !newer("staging-latest-abc", "2.5.3") {
		t.Fatal("newer")
	}
}

func TestStatePath(t *testing.T) {
	if got := StatePath("/var/lib/quicc-agent/agent.db"); got != "/var/lib/quicc-agent/agent.store-update.json" {
		t.Fatal(got)
	}
	if got := StatePath("sql/agent-lombardis.db"); got != "sql/agent-lombardis.store-update.json" {
		t.Fatal(got)
	}
}

// A dev agent (no root updater) never plans its own update: no round that
// would be refused every 30 s.
func TestNoPiUpdateWhereUnsupported(t *testing.T) {
	r := newRig(t)
	r.manifest = Manifest{AgentVersion: "2.5.4", AgentSHA: "bbb"}
	c := r.coordinator("2.5.3")
	c.d.PiSupported = func() bool { return false }
	c.Tick(ctx)
	phase(t, c, PhaseIdle)
	// A refused hand-over waits before the next round.
	r2 := newRig(t)
	r2.piOK = false
	r2.manifest = Manifest{AgentVersion: "2.5.4", AgentSHA: "bbb"}
	c2 := r2.coordinator("2.5.3")
	c2.Tick(ctx)
	phase(t, c2, PhaseCancelled)
	id := c2.View(nil).ID
	c2.Tick(ctx)
	if c2.View(nil).ID != id {
		t.Fatal("a new round right after a refusal")
	}
	r2.later(BusyRetry + time.Second)
	c2.Tick(ctx)
	if c2.View(nil).ID == id {
		t.Fatal("tried again after the wait")
	}
}
