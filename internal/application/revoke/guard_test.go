package revoke

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

type fakeMain struct {
	mu     sync.Mutex
	status int
	code   string
	asks   int
}

func (f *fakeMain) whoami(context.Context, string) (int, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asks++
	return f.status, f.code, nil
}

type rig struct {
	g      *Guard
	m      *fakeMain
	clock  time.Time
	wiped  chan string
	timers []func()
	pages  atomic.Int64
}

func (r *rig) Send(context.Context, string, string) error { r.pages.Add(1); return nil }

func newRig(status int, code string) *rig {
	r := &rig{m: &fakeMain{status: status, code: code}, clock: time.Now(), wiped: make(chan string, 1)}
	r.g = New(r.m.whoami, func(k string) { r.wiped <- k }, nil, zerolog.Nop())
	r.g.now = func() time.Time { return r.clock }
	r.g.after = func(_ time.Duration, f func()) { r.timers = append(r.timers, f) }
	return r
}

func (r *rig) wasWiped() bool {
	select {
	case <-r.wiped:
		return true
	case <-time.After(50 * time.Millisecond):
		return false
	}
}

// A bare 401 (whoami says the key is fine) never wipes.
func TestBare401KeepsTheLink(t *testing.T) {
	r := newRig(http.StatusOK, "")
	for i := 0; i < 5; i++ {
		r.g.Unauthorized("k1", "")
		r.clock = r.clock.Add(2 * time.Minute)
	}
	if r.wasWiped() || r.g.Failing() {
		t.Fatal("a 401 with a good key must not wipe")
	}
}

// unknown_key (a main restored from an older backup) never wipes: a sticky
// problem and one page an hour.
func TestUnknownKeyNeverWipes(t *testing.T) {
	r := newRig(http.StatusUnauthorized, CodeUnknownKey)
	r.g.pager = r
	for i := 0; i < 5; i++ {
		r.g.Unauthorized("k1", CodeUnknownKey)
		r.clock = r.clock.Add(5 * time.Minute)
	}
	if r.wasWiped() {
		t.Fatal("unknown_key must not wipe")
	}
	if !r.g.Failing() || len(r.g.Problems(context.Background())) != 1 {
		t.Fatal("main_auth_failing")
	}
	time.Sleep(20 * time.Millisecond)
	if n := r.pages.Load(); n != 1 {
		t.Fatalf("pages %d", n)
	}
	r.g.Accepted("k1")
	if r.g.Failing() {
		t.Fatal("cleared once main takes the key")
	}
}

// device_revoked twice >= 60 s apart wipes; inside the minute it waits.
func TestDeviceRevokedConfirmedTwice(t *testing.T) {
	r := newRig(http.StatusUnauthorized, CodeDeviceRevoked)
	r.g.Unauthorized("k1", CodeDeviceRevoked)
	r.clock = r.clock.Add(30 * time.Second)
	r.g.Unauthorized("k1", CodeDeviceRevoked)
	if r.wasWiped() {
		t.Fatal("wiped inside the confirmation minute")
	}
	if len(r.timers) != 1 {
		t.Fatalf("a re-check is scheduled: %d", len(r.timers))
	}
	r.clock = r.clock.Add(31 * time.Second)
	r.timers[0]()
	if !r.wasWiped() {
		t.Fatal("confirmed revoke must wipe")
	}
}

// A revoke that turns out fine at the re-check (main came back with the key)
// is dropped.
func TestRevokeNotConfirmed(t *testing.T) {
	r := newRig(http.StatusUnauthorized, CodeDeviceRevoked)
	r.g.Unauthorized("k1", CodeDeviceRevoked)
	r.m.mu.Lock()
	r.m.status, r.m.code = http.StatusOK, ""
	r.m.mu.Unlock()
	r.clock = r.clock.Add(61 * time.Second)
	r.timers[0]()
	if r.wasWiped() {
		t.Fatal("main took the key back: no wipe")
	}
}

// A main without whoami: the old rule, confirmed over a minute.
func TestLegacyMainWipesAfterTheMinute(t *testing.T) {
	r := newRig(http.StatusNotFound, "")
	r.g.Unauthorized("k1", "")
	if r.wasWiped() {
		t.Fatal("one 401 never wipes")
	}
	r.clock = r.clock.Add(61 * time.Second)
	r.g.Unauthorized("k1", "")
	if !r.wasWiped() {
		t.Fatal("old main: a 401 a minute later wipes")
	}
}
