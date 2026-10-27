package update

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

type memStore struct {
	mu       sync.Mutex
	job      string
	notified string
	lastOK   time.Time
}

func (m *memStore) GetUpdateState(context.Context) (string, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.job, m.notified, nil
}
func (m *memStore) SetUpdateJob(_ context.Context, j string) error {
	m.mu.Lock()
	m.job = j
	m.mu.Unlock()
	return nil
}
func (m *memStore) GetUpdateLastSuccess(context.Context) (time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastOK, nil
}
func (m *memStore) SetUpdateLastSuccess(_ context.Context, at time.Time) error {
	m.mu.Lock()
	m.lastOK = at
	m.mu.Unlock()
	return nil
}
func (m *memStore) SetUpdateNotified(_ context.Context, k string) error {
	m.mu.Lock()
	m.notified = k
	m.mu.Unlock()
	return nil
}

type rig struct {
	t        *testing.T
	dir      string
	store    *memStore
	total    int64
	active   int64
	now      time.Time
	reports  []map[string]any
	pages    []string
	finished []string // OnFinished: "<action>:<state>"
	failSend bool
}

func newRig(t *testing.T) *rig {
	return &rig{t: t, dir: t.TempDir(), store: &memStore{}, now: time.Date(2026, 10, 21, 23, 0, 0, 0, time.UTC)}
}

func (g *rig) requester(supported bool) *Requester {
	r := New(Config{Dir: g.dir, Supported: supported, Version: "2.0.5", Commit: "abc", Env: "staging",
		StoreName: func() string { return "Lombardi's Pizza" }, Now: func() time.Time { return g.now },
		OnFinished: func(_ context.Context, s Status) { g.finished = append(g.finished, s.Action+":"+s.State) }},
		func(context.Context) (int64, int64, error) { return g.total, g.active, nil },
		g.store,
		func(_ context.Context, msgType string, payload any) error {
			if g.failSend {
				return errors.New("offline")
			}
			if msgType != MsgStatus {
				g.t.Fatalf("report type %q", msgType)
			}
			b, _ := json.Marshal(payload)
			var m map[string]any
			_ = json.Unmarshal(b, &m)
			g.reports = append(g.reports, m)
			return nil
		},
		func(_ context.Context, msg, _ string) error { g.pages = append(g.pages, msg); return nil },
		nil, zerolog.Nop())
	r.Load(context.Background())
	return r
}

func (g *rig) writeStatus(s Status) {
	b, _ := json.Marshal(s)
	if err := os.WriteFile(filepath.Join(g.dir, StatusFile), b, 0o644); err != nil {
		g.t.Fatal(err)
	}
}

func (g *rig) request() map[string]string {
	b, err := os.ReadFile(filepath.Join(g.dir, RequestFile))
	if err != nil {
		return nil
	}
	var m map[string]string
	_ = json.Unmarshal(b, &m)
	return m
}

func TestNotSupported(t *testing.T) {
	g := newRig(t)
	r := g.requester(false)
	rep := r.Request(context.Background(), ActionUpdate, false, "pi-web", "")
	if rep.Accepted || rep.Code != CodeNotSupported {
		t.Fatalf("got %+v", rep)
	}
	v := r.View(context.Background())
	if v.CanUpdateNow || v.BlockedReason != CodeNotSupported || v.State != StateIdle || v.Pending != nil || v.Last != nil {
		t.Fatalf("view %+v", v)
	}
}

func TestGateAfterClose(t *testing.T) {
	g := newRig(t)
	r := g.requester(true)
	g.total = 3 // orders in the open batch, none active
	if rep := r.Request(context.Background(), ActionUpdate, false, "admin", "u1"); rep.Accepted || rep.Code != CodeNotAfterClose {
		t.Fatalf("got %+v", rep)
	}
	if v := r.View(context.Background()); v.CanUpdateNow || v.BlockedReason != CodeNotAfterClose {
		t.Fatalf("view %+v", v)
	}
	g.total = 0
	rep := r.Request(context.Background(), ActionUpdate, false, "pi-web", "u1")
	if !rep.Accepted || rep.State != StateRequested || rep.ID != "u1" {
		t.Fatalf("got %+v", rep)
	}
	req := g.request()
	if req["id"] != "u1" || req["action"] != "update" || req["requested_by"] != "pi-web" || req["at"] == "" {
		t.Fatalf("request file %v", req)
	}
	st, _ := os.Stat(filepath.Join(g.dir, RequestFile))
	if st.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v", st.Mode())
	}
	// A second request while this one is pending is refused; a retry of the
	// same id gets the current answer.
	if rep := r.Request(context.Background(), ActionRollback, false, "admin", "u2"); rep.Accepted || rep.Code != CodeInProgress {
		t.Fatalf("got %+v", rep)
	}
	if rep := r.Request(context.Background(), ActionUpdate, false, "admin", "u1"); !rep.Accepted || rep.State != StateRequested {
		t.Fatalf("retry %+v", rep)
	}
	v := r.View(context.Background())
	if v.State != StateRequested || v.Pending == nil || v.Pending.ID != "u1" || v.BlockedReason != CodeInProgress {
		t.Fatalf("view %+v", v)
	}
	// Bad ids never reach the root side.
	g2 := newRig(t)
	if rep := g2.requester(true).Request(context.Background(), ActionUpdate, false, "admin", "../../x"); rep.Accepted {
		t.Fatalf("bad id accepted %+v", rep)
	}
}

func TestForcedWaitsForIdle(t *testing.T) {
	g := newRig(t)
	r := g.requester(true)
	g.total, g.active = 5, 2
	rep := r.Request(context.Background(), ActionUpdate, true, "admin", "f1")
	if !rep.Accepted || rep.State != StateWaiting {
		t.Fatalf("got %+v", rep)
	}
	if g.request() != nil {
		t.Fatal("request written while orders are open")
	}
	r.Tick(context.Background())
	if len(g.reports) != 1 || g.reports[0]["state"] != StateWaiting || g.reports[0]["force"] != true || g.reports[0]["requested_by"] != "admin" {
		t.Fatalf("reports %v", g.reports)
	}
	// The pending request survives a restart.
	r2 := g.requester(true)
	if v := r2.View(context.Background()); v.State != StateWaiting || v.Pending == nil || !v.Pending.Force {
		t.Fatalf("after restart %+v", v)
	}
	// Still busy: nothing written.
	g.now = g.now.Add(31 * time.Second)
	r2.Tick(context.Background())
	if g.request() != nil {
		t.Fatal("written while busy")
	}
	// Idle (orders remain in the batch, none active) → written within 30 s.
	g.active = 0
	g.now = g.now.Add(31 * time.Second)
	r2.Tick(context.Background())
	if req := g.request(); req == nil || req["id"] != "f1" {
		t.Fatalf("request %v", req)
	}
	if v := r2.View(context.Background()); v.State != StateRequested {
		t.Fatalf("state %s", v.State)
	}
	last := g.reports[len(g.reports)-1]
	if last["state"] != StateRequested {
		t.Fatalf("reports %v", g.reports)
	}
}

func TestRollbackGate(t *testing.T) {
	g := newRig(t)
	r := g.requester(true)
	g.total, g.active = 4, 1
	if rep := r.Request(context.Background(), ActionRollback, false, "admin", "r1"); rep.Accepted || rep.Code != CodeOrdersOpen {
		t.Fatalf("got %+v", rep)
	}
	g.active = 0 // orders in the batch but all done: rollback allowed (no after-close rule)
	if rep := r.Request(context.Background(), ActionRollback, false, "admin", "r1"); !rep.Accepted || rep.State != StateRequested {
		t.Fatalf("got %+v", rep)
	}
	if g.request()["action"] != "rollback" {
		t.Fatalf("request %v", g.request())
	}
}

func TestStatusTransitionsReportAndPageOnce(t *testing.T) {
	g := newRig(t)
	r := g.requester(true)
	if rep := r.Request(context.Background(), ActionUpdate, false, "console", "s1"); !rep.Accepted {
		t.Fatalf("got %+v", rep)
	}
	r.Tick(context.Background())
	g.writeStatus(Status{ID: "s1", Action: "update", RequestedBy: "console", State: StateRunning, FromVersion: "2.0.5",
		StartedAt: g.now.Format(time.RFC3339)})
	g.now = g.now.Add(10 * time.Second)
	r.Tick(context.Background())
	r.Tick(context.Background()) // unchanged → no second report
	if n := len(g.reports); n != 2 || g.reports[1]["state"] != StateRunning {
		t.Fatalf("reports %v", g.reports)
	}
	if v := r.View(context.Background()); v.State != StateRunning || v.Pending == nil {
		t.Fatalf("view %+v", v)
	}

	// The update restarts the agent: the new process finds the finished state.
	g.writeStatus(Status{ID: "s1", Action: "update", RequestedBy: "console", State: StateOK, FromVersion: "2.0.5",
		ToVersion: "2.0.6", StartedAt: g.now.Format(time.RFC3339), FinishedAt: g.now.Add(time.Minute).Format(time.RFC3339)})
	g.now = g.now.Add(2 * time.Minute)
	r2 := g.requester(true)
	r2.Tick(context.Background())
	if last := g.reports[len(g.reports)-1]; last["state"] != StateOK || last["to_version"] != "2.0.6" || last["requested_by"] != "console" {
		t.Fatalf("reports %v", g.reports)
	}
	if len(g.pages) != 1 || g.pages[0] != "Lombardi's Pizza Pi updated 2.0.5 → 2.0.6" {
		t.Fatalf("pages %v", g.pages)
	}
	v := r2.View(context.Background())
	if v.State != StateOK || v.Last == nil || v.Last.ToVersion != "2.0.6" || v.Pending != nil || !v.CanUpdateNow {
		t.Fatalf("view %+v", v)
	}
	// Another restart: main hears it again (re-reported), but no second page.
	r3 := g.requester(true)
	r3.Tick(context.Background())
	r3.Tick(context.Background())
	if len(g.pages) != 1 {
		t.Fatalf("paged twice: %v", g.pages)
	}
	// After 15 min the headline is idle again; last stays.
	g.now = g.now.Add(20 * time.Minute)
	if v := r3.View(context.Background()); v.State != StateIdle || v.Last == nil {
		t.Fatalf("view %+v", v)
	}
}

func TestRolledBackPageAndRetryReport(t *testing.T) {
	g := newRig(t)
	r := g.requester(true)
	r.Request(context.Background(), ActionUpdate, false, "pi-web", "b1")
	g.writeStatus(Status{ID: "b1", Action: "update", State: StateRolledBack, FromVersion: "2.0.5", ToVersion: "2.0.6",
		Error: "not healthy in 120 s", StartedAt: g.now.Format(time.RFC3339), FinishedAt: g.now.Format(time.RFC3339)})
	g.failSend = true
	r.Tick(context.Background())
	if len(g.reports) != 0 || len(g.pages) != 1 {
		t.Fatalf("reports %v pages %v", g.reports, g.pages)
	}
	if g.pages[0] != "Lombardi's Pizza Pi update FAILED — rolled back to 2.0.5: not healthy in 120 s" {
		t.Fatalf("page %q", g.pages[0])
	}
	g.failSend = false
	r.Tick(context.Background())
	if len(g.reports) != 1 || g.reports[0]["state"] != StateRolledBack || g.reports[0]["requested_by"] != "pi-web" {
		t.Fatalf("reports %v", g.reports)
	}
	if len(g.pages) != 1 {
		t.Fatalf("pages %v", g.pages)
	}
}

func TestPickupTimeout(t *testing.T) {
	g := newRig(t)
	r := g.requester(true)
	r.Request(context.Background(), ActionUpdate, false, "pi-web", "p1")
	g.now = g.now.Add(11 * time.Minute)
	r.Tick(context.Background())
	if v := r.View(context.Background()); v.State != StateFailed || v.Last == nil || v.Last.ID != "p1" || !v.CanUpdateNow {
		t.Fatalf("view %+v", v)
	}
	if len(g.pages) != 1 {
		t.Fatalf("pages %v", g.pages)
	}
}

func TestOldResultNotPaged(t *testing.T) {
	g := newRig(t)
	g.writeStatus(Status{ID: "old", Action: "update", State: StateOK, FromVersion: "2.0.4", ToVersion: "2.0.5",
		FinishedAt: g.now.Add(-48 * time.Hour).Format(time.RFC3339)})
	r := g.requester(true)
	r.Tick(context.Background())
	if len(g.pages) != 0 || len(g.reports) != 1 {
		t.Fatalf("pages %v reports %v", g.pages, g.reports)
	}
}

// agent.update.runs counts each finished run once (across restarts) and
// agent.update.last_success keeps the last ok update through a later failure.
func TestFinishedOnceAndLastSuccess(t *testing.T) {
	g := newRig(t)
	ctx := context.Background()
	start := g.now
	g.writeStatus(Status{ID: "u1", Action: "update", State: StateOK, FromVersion: "2.1.0", ToVersion: "2.1.1",
		StartedAt: start.Format(time.RFC3339), FinishedAt: start.Add(time.Minute).Format(time.RFC3339)})
	g.now = start.Add(2 * time.Minute)
	r := g.requester(true)
	if at, ok := r.LastSuccess(); !ok || !at.Equal(start.Add(time.Minute)) {
		t.Fatalf("last success after load %v %v", at, ok)
	}
	r.Tick(ctx)
	r.Tick(ctx)
	r2 := g.requester(true) // the restart an update causes
	r2.Tick(ctx)
	if len(g.finished) != 1 || g.finished[0] != "update:ok" {
		t.Fatalf("finished %v", g.finished)
	}

	// A later update rolls back: counted, last success unchanged.
	g.writeStatus(Status{ID: "u2", Action: "update", State: StateRolledBack, FromVersion: "2.1.1", ToVersion: "2.1.2",
		StartedAt: g.now.Format(time.RFC3339), FinishedAt: g.now.Add(time.Minute).Format(time.RFC3339)})
	g.now = g.now.Add(3 * time.Minute)
	r3 := g.requester(true)
	r3.Tick(ctx)
	if len(g.finished) != 2 || g.finished[1] != "update:rolled_back" {
		t.Fatalf("finished %v", g.finished)
	}
	if at, ok := r3.LastSuccess(); !ok || !at.Equal(start.Add(time.Minute)) {
		t.Fatalf("last success after a rollback %v %v", at, ok)
	}
	if !g.store.lastOK.Equal(start.Add(time.Minute)) {
		t.Fatalf("persisted last success %v", g.store.lastOK)
	}

	// A successful manual rollback is a finished run, not an update success.
	g.writeStatus(Status{ID: "r1", Action: "rollback", State: StateOK, FromVersion: "2.1.1", ToVersion: "2.1.0",
		StartedAt: g.now.Format(time.RFC3339), FinishedAt: g.now.Add(time.Minute).Format(time.RFC3339)})
	g.now = g.now.Add(3 * time.Minute)
	r4 := g.requester(true)
	r4.Tick(ctx)
	if len(g.finished) != 3 || g.finished[2] != "rollback:ok" {
		t.Fatalf("finished %v", g.finished)
	}
	if at, _ := r4.LastSuccess(); !at.Equal(start.Add(time.Minute)) {
		t.Fatalf("rollback moved last success to %v", at)
	}
}

func TestMessages(t *testing.T) {
	cases := []struct {
		s    Status
		want string
	}{
		{Status{Action: "update", State: StateOK, FromVersion: "2.0.5", ToVersion: "2.0.6"}, "X Pi updated 2.0.5 → 2.0.6"},
		{Status{Action: "update", State: StateUpToDate, FromVersion: "2.0.6"}, "X Pi already up to date (2.0.6)"},
		{Status{Action: "update", State: StateRolledBack, FromVersion: "2.0.5", Error: "boom"}, "X Pi update FAILED — rolled back to 2.0.5: boom"},
		{Status{Action: "update", State: StateFailed, Error: "bad sig"}, "X Pi update FAILED: bad sig"},
		{Status{Action: "rollback", State: StateOK, FromVersion: "2.0.6", ToVersion: "2.0.5"}, "X Pi rolled back 2.0.6 → 2.0.5"},
	}
	for _, c := range cases {
		if got, _ := Message("X", &c.s); got != c.want {
			t.Errorf("got %q want %q", got, c.want)
		}
	}
}

const sampleManifest = `# quicc-manifest v1
# env=staging
# agent_version=2.0.6
1111111111111111111111111111111111111111111111111111111111111111  agent-install.sh
AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA  agent-linux-arm64
2222222222222222222222222222222222222222222222222222222222222222  tailscale_1.80.0_arm64.tgz
`

func TestParseManifest(t *testing.T) {
	m, err := ParseManifest([]byte(sampleManifest))
	if err != nil {
		t.Fatal(err)
	}
	if m.AgentVersion() != "2.0.6" || m.AgentSHA() != strings.Repeat("a", 64) || m.Header["env"] != "staging" {
		t.Fatalf("%+v", m)
	}
	if _, err := ParseManifest([]byte("# agent_version=1\n")); err == nil {
		t.Fatal("manifest without the agent line accepted")
	}
	if _, err := ParseManifest([]byte(strings.Repeat("a", 64) + "  agent-linux-arm64\n")); err == nil {
		t.Fatal("manifest without a version accepted")
	}
}

func TestLatest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/staging/manifest.txt" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(sampleManifest))
	}))
	defer srv.Close()
	exe := filepath.Join(t.TempDir(), "agent")
	_ = os.WriteFile(exe, []byte("binary"), 0o755)
	l := NewLatest(ManifestURL(srv.URL, "staging"), exe)
	if err := l.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	v, avail := l.Get(context.Background())
	if v != "2.0.6" || !avail {
		t.Fatalf("got %q %v", v, avail)
	}
	if ManifestURL("", "") != "" || ManifestURL("", "prod") != "https://utility.quiccpos.com/prod/manifest.txt" {
		t.Fatal("manifest url")
	}
	if ManifestEnv("dev") != "" || ManifestEnv("staging") != "staging" || ManifestEnv("prod") != "prod" {
		t.Fatal("manifest env")
	}
	if Supported("dev", DefaultDir, false) || Supported("staging", "/home/x/.quicc", false) || !Supported("dev", "/tmp", true) {
		t.Fatal("supported")
	}
}

// A forced update still waiting for idle is dropped once update.status shows
// an update that finished after it was asked for (one run by hand with its own
// id) — and never blocks a new request while it waits (prod 2026-10-27).
func TestWaitingForcedJobSupersededByManualUpdate(t *testing.T) {
	g := newRig(t)
	r := g.requester(true)
	g.total, g.active = 5, 2
	if rep := r.Request(context.Background(), ActionUpdate, true, "console", "f1"); rep.State != StateWaiting {
		t.Fatalf("got %+v", rep)
	}
	// An older finished update doesn't drop it.
	g.writeStatus(Status{ID: "old", Action: "update", State: StateOK, FromVersion: "2.0.4", ToVersion: "2.0.5",
		StartedAt: g.now.Add(-time.Hour).Format(time.RFC3339), FinishedAt: g.now.Add(-59 * time.Minute).Format(time.RFC3339)})
	r.Tick(context.Background())
	if v := r.View(context.Background()); v.State != StateWaiting {
		t.Fatalf("state %s", v.State)
	}
	// A manual update with another id finishes, the agent restarts on it.
	g.now = g.now.Add(20 * time.Minute)
	g.writeStatus(Status{ID: "manual-2-3-4", Action: "update", RequestedBy: "console", State: StateOK, FromVersion: "2.0.5", ToVersion: "2.0.6",
		StartedAt: g.now.Add(-2 * time.Minute).Format(time.RFC3339), FinishedAt: g.now.Add(-time.Minute).Format(time.RFC3339)})
	r2 := g.requester(true)
	if g.store.job != "" {
		t.Fatalf("job still persisted: %s", g.store.job)
	}
	v := r2.View(context.Background())
	if v.State != StateOK || v.Pending != nil || v.BlockedReason == CodeInProgress {
		t.Fatalf("after the manual update %+v", v)
	}
	// A new forced request is taken, not refused as already running.
	if rep := r2.Request(context.Background(), ActionUpdate, true, "console", "f2"); !rep.Accepted || rep.State != StateWaiting {
		t.Fatalf("new forced request %+v", rep)
	}
}

func TestNewRequestReplacesWaitingJob(t *testing.T) {
	g := newRig(t)
	r := g.requester(true)
	g.total, g.active = 5, 2
	r.Request(context.Background(), ActionUpdate, true, "console", "f1")
	if v := r.View(context.Background()); v.BlockedReason == CodeInProgress {
		t.Fatalf("a waiting job must not block: %+v", v)
	}
	// A plain update still needs Close batch: refused, the waiting job stays.
	if rep := r.Request(context.Background(), ActionUpdate, false, "pi-web", "p1"); rep.Code != CodeNotAfterClose {
		t.Fatalf("plain %+v", rep)
	}
	if v := r.View(context.Background()); v.Pending == nil || v.Pending.ID != "f1" {
		t.Fatalf("pending %+v", v.Pending)
	}
	// Another forced request replaces it.
	if rep := r.Request(context.Background(), ActionUpdate, true, "admin", "f2"); !rep.Accepted || rep.State != StateWaiting {
		t.Fatalf("replace %+v", rep)
	}
	if v := r.View(context.Background()); v.Pending == nil || v.Pending.ID != "f2" || v.Pending.RequestedBy != "admin" {
		t.Fatalf("pending %+v", v.Pending)
	}
	// Once written, the next request is refused while it runs.
	g.active = 0
	g.now = g.now.Add(31 * time.Second)
	r.Tick(context.Background())
	if req := g.request(); req == nil || req["id"] != "f2" {
		t.Fatalf("request %v", req)
	}
	if rep := r.Request(context.Background(), ActionUpdate, true, "admin", "f3"); rep.Code != CodeInProgress {
		t.Fatalf("while requested %+v", rep)
	}
}

// A finished job is retired: the persisted job is cleared, the final status
// stays the headline/Last, and a later update run by hand with its own id is
// shown as itself — never as "the Pi's updater didn't pick up the request"
// for the old job (2.3.6).
func TestFinishedJobClearedLaterUpdateShown(t *testing.T) {
	g := newRig(t)
	ctx := context.Background()
	r := g.requester(true)
	if rep := r.Request(ctx, ActionUpdate, false, "pi-web", "j1"); !rep.Accepted {
		t.Fatalf("got %+v", rep)
	}
	g.writeStatus(Status{ID: "j1", Action: "update", State: StateOK, FromVersion: "2.0.5", ToVersion: "2.0.6",
		StartedAt: g.now.Format(time.RFC3339), FinishedAt: g.now.Add(time.Minute).Format(time.RFC3339)})
	g.now = g.now.Add(2 * time.Minute)
	r.Tick(ctx)
	if g.store.job != "" {
		t.Fatalf("finished job still persisted: %s", g.store.job)
	}
	if v := r.View(ctx); v.State != StateOK || v.Last == nil || v.Last.ID != "j1" || v.Last.RequestedBy != "pi-web" || v.Pending != nil {
		t.Fatalf("view %+v", v)
	}
	// Hours later an update is run by hand: the loop sees it without a restart.
	g.now = g.now.Add(3 * time.Hour)
	g.writeStatus(Status{ID: "manual-1", Action: "update", RequestedBy: "console", State: StateOK, FromVersion: "2.0.6", ToVersion: "2.0.7",
		StartedAt: g.now.Add(-2 * time.Minute).Format(time.RFC3339), FinishedAt: g.now.Add(-time.Minute).Format(time.RFC3339)})
	g.now = g.now.Add(31 * time.Second)
	r.Tick(ctx)
	if last := g.reports[len(g.reports)-1]; last["id"] != "manual-1" || last["state"] != StateOK {
		t.Fatalf("reports %v", g.reports)
	}
	v := r.View(ctx)
	if v.State != StateOK || v.Last == nil || v.Last.ID != "manual-1" || !v.CanUpdateNow {
		t.Fatalf("view %+v", v)
	}
	for _, p := range g.pages {
		if strings.Contains(p, "FAILED") {
			t.Fatalf("stale failure paged: %v", g.pages)
		}
	}
	// A retried request with the finished id is answered, never run again.
	_ = os.Remove(filepath.Join(g.dir, RequestFile))
	if rep := r.Request(ctx, ActionUpdate, false, "pi-web", "j1"); !rep.Accepted || g.request() != nil {
		t.Fatalf("retry of a finished id %+v request %v", rep, g.request())
	}
}

// A request the updater never picked up fails once, is retired, and a later
// update replaces the failure (it used to stay the headline until restart).
func TestPickupTimeoutRetiresJob(t *testing.T) {
	g := newRig(t)
	ctx := context.Background()
	r := g.requester(true)
	r.Request(ctx, ActionUpdate, false, "pi-web", "p1")
	g.now = g.now.Add(11 * time.Minute)
	r.Tick(ctx)
	if g.store.job != "" {
		t.Fatalf("timed-out job still persisted: %s", g.store.job)
	}
	if v := r.View(ctx); v.State != StateFailed || v.Last == nil || v.Last.ID != "p1" || !v.CanUpdateNow {
		t.Fatalf("view %+v", v)
	}
	// The failure survives later ticks until something newer shows.
	g.now = g.now.Add(time.Minute)
	r.Tick(ctx)
	if v := r.View(ctx); v.Last == nil || v.Last.ID != "p1" {
		t.Fatalf("last %+v", v.Last)
	}
	// A new request is taken and runs.
	if rep := r.Request(ctx, ActionUpdate, false, "admin", "p2"); !rep.Accepted || rep.State != StateRequested {
		t.Fatalf("new request %+v", rep)
	}
	g.writeStatus(Status{ID: "p2", Action: "update", State: StateUpToDate, FromVersion: "2.0.5",
		StartedAt: g.now.Format(time.RFC3339), FinishedAt: g.now.Add(time.Minute).Format(time.RFC3339)})
	g.now = g.now.Add(2 * time.Minute)
	r.Tick(ctx)
	if v := r.View(ctx); v.State != StateUpToDate || v.Last == nil || v.Last.ID != "p2" || v.Last.RequestedBy != "admin" {
		t.Fatalf("view %+v", v)
	}
	if len(g.pages) != 2 {
		t.Fatalf("pages %v", g.pages)
	}
}

// A request already handed to the updater is dropped when another update
// finishes after it was asked for (its request file was replaced) — no
// pickup failure is reported or paged for it later.
func TestWrittenJobSupersededByOtherUpdate(t *testing.T) {
	g := newRig(t)
	ctx := context.Background()
	r := g.requester(true)
	if rep := r.Request(ctx, ActionUpdate, false, "pi-web", "w1"); rep.State != StateRequested {
		t.Fatalf("got %+v", rep)
	}
	g.now = g.now.Add(3 * time.Minute)
	g.writeStatus(Status{ID: "manual-2", Action: "update", RequestedBy: "console", State: StateOK, FromVersion: "2.0.5", ToVersion: "2.0.6",
		StartedAt: g.now.Add(-2 * time.Minute).Format(time.RFC3339), FinishedAt: g.now.Format(time.RFC3339)})
	r.Tick(ctx)
	if g.store.job != "" {
		t.Fatalf("superseded job still persisted: %s", g.store.job)
	}
	g.now = g.now.Add(15 * time.Minute)
	r.Tick(ctx)
	v := r.View(ctx)
	if v.Last == nil || v.Last.ID != "manual-2" || v.Pending != nil || !v.CanUpdateNow {
		t.Fatalf("view %+v", v)
	}
	for _, rep := range g.reports {
		if rep["id"] == "w1" && rep["state"] == StateFailed {
			t.Fatalf("pickup failure reported: %v", g.reports)
		}
	}
	for _, p := range g.pages {
		if strings.Contains(p, "FAILED") {
			t.Fatalf("pickup failure paged: %v", g.pages)
		}
	}
}

// A forced job's final report still says force:true after the job is retired.
func TestForcedFinishReportKeepsForce(t *testing.T) {
	g := newRig(t)
	ctx := context.Background()
	r := g.requester(true)
	g.total = 2
	if rep := r.Request(ctx, ActionUpdate, true, "admin", "f1"); rep.State != StateRequested {
		t.Fatalf("got %+v", rep)
	}
	g.writeStatus(Status{ID: "f1", Action: "update", State: StateOK, FromVersion: "2.0.5", ToVersion: "2.0.6",
		StartedAt: g.now.Format(time.RFC3339), FinishedAt: g.now.Add(time.Minute).Format(time.RFC3339)})
	g.now = g.now.Add(2 * time.Minute)
	r.Tick(ctx)
	last := g.reports[len(g.reports)-1]
	if last["id"] != "f1" || last["state"] != StateOK || last["force"] != true || last["requested_by"] != "admin" {
		t.Fatalf("reports %v", g.reports)
	}
}
