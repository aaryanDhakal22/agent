package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// autoStore is memStore + the auto_update setting.
type autoStore struct {
	memStore
	amu  sync.Mutex
	auto bool
}

func (a *autoStore) GetAutoUpdate(context.Context) (bool, error) {
	a.amu.Lock()
	defer a.amu.Unlock()
	return a.auto, nil
}
func (a *autoStore) SetAutoUpdate(_ context.Context, on bool) error {
	a.amu.Lock()
	a.auto = on
	a.amu.Unlock()
	return nil
}

// autoRig: a requester whose manifest names version/sha, running "2.2.0"
// from a binary whose sha256 is own.
type autoRig struct {
	*rig
	store   *autoStore
	version string
	sha     string
	own     string
}

func newAutoRig(t *testing.T) *autoRig {
	g := &autoRig{rig: newRig(t), store: &autoStore{auto: true}, version: "2.2.1"}
	exe := filepath.Join(t.TempDir(), "agent")
	if err := os.WriteFile(exe, []byte("running binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("running binary"))
	g.own = hex.EncodeToString(sum[:])
	g.sha = strings.Repeat("b", 64)
	return g
}

func (g *autoRig) requester(supported bool) *Requester {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("# agent_version=" + g.version + "\n" + g.sha + "  agent-linux-arm64\n"))
	}))
	g.t.Cleanup(srv.Close)
	exe := filepath.Join(g.dir, "agent-bin")
	_ = os.WriteFile(exe, []byte("running binary"), 0o755)
	r := New(Config{Dir: g.dir, Supported: supported, Version: "2.2.0", Env: "staging", Now: func() time.Time { return g.now }},
		func(context.Context) (int64, int64, error) { return g.total, g.active, nil },
		g.store, func(context.Context, string, any) error { return nil }, nil,
		NewLatest(ManifestURL(srv.URL, "staging"), exe), zerolog.Nop())
	r.Load(context.Background())
	return r
}

func TestAutoUpdateAfterClose(t *testing.T) {
	ctx := context.Background()
	g := newAutoRig(t)
	r := g.requester(true)
	rep, reason := r.AfterClose(ctx)
	if reason != "" || rep == nil || !rep.Accepted || rep.State != StateRequested {
		t.Fatalf("got %+v %q", rep, reason)
	}
	req := g.request()
	if req["action"] != ActionUpdate || req["requested_by"] != RequestedByAuto || req["id"] != rep.ID {
		t.Fatalf("update.request: %v", req)
	}
	// The run's status, as the root updater would write it: the agent
	// reports requested_by auto even when an older update.sh says unknown.
	g.writeStatus(Status{ID: rep.ID, Action: ActionUpdate, RequestedBy: "unknown", State: StateRunning,
		StartedAt: g.now.Format(time.RFC3339)})
	if v := r.View(ctx); v.Pending == nil || v.Pending.RequestedBy != RequestedByAuto {
		t.Fatalf("view: %+v", v)
	}
	if eff := r.effectiveLocked(g.now); eff == nil || eff.RequestedBy != RequestedByAuto {
		t.Fatalf("effective: %+v", eff)
	}
}

func TestAutoUpdateSkips(t *testing.T) {
	ctx := context.Background()

	g := newAutoRig(t)
	g.store.auto = false
	if rep, reason := g.requester(true).AfterClose(ctx); rep != nil || reason != AutoOff {
		t.Fatalf("off: %+v %q", rep, reason)
	}

	g = newAutoRig(t)
	if _, reason := g.requester(false).AfterClose(ctx); reason != CodeNotSupported {
		t.Fatalf("not supported: %q", reason)
	}

	g = newAutoRig(t)
	g.sha = g.own // the manifest names the running binary
	if _, reason := g.requester(true).AfterClose(ctx); reason != AutoUpToDate {
		t.Fatalf("same sha: %q", reason)
	}

	g = newAutoRig(t)
	g.version = "2.1.9" // an older release published (a rollback)
	if _, reason := g.requester(true).AfterClose(ctx); reason != AutoNotNewer {
		t.Fatalf("older: %q", reason)
	}

	g = newAutoRig(t)
	g.total = 1 // a scheduled order moved into the new batch: the normal gate
	rep, reason := g.requester(true).AfterClose(ctx)
	if reason != CodeNotAfterClose || rep == nil || rep.Accepted || g.request() != nil {
		t.Fatalf("orders open: %+v %q", rep, reason)
	}

	g = newAutoRig(t)
	g.writeStatus(Status{ID: "x", Action: ActionUpdate, RequestedBy: RequestedByAuto, State: StateRolledBack,
		ToSHA: g.sha, StartedAt: g.now.Format(time.RFC3339), FinishedAt: g.now.Format(time.RFC3339)})
	if _, reason := g.requester(true).AfterClose(ctx); reason != AutoFailedBefore {
		t.Fatalf("failed before: %q", reason)
	}
}

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		m, r string
		want bool
	}{
		{"2.2.1", "2.2.0", true}, {"2.2.0", "2.2.0", false}, {"2.1.9", "2.2.0", false}, {"v3.0.0", "2.9.9", true},
		{"2.10.0", "2.9.0", true}, {"staging-latest-abcdef012345", "2.2.0", true}, {"", "2.2.0", false},
	} {
		if got := Newer(c.m, c.r); got != c.want {
			t.Errorf("Newer(%q, %q) = %v", c.m, c.r, got)
		}
	}
}
