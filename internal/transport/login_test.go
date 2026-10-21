package transport

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestLoginThrottle(t *testing.T) {
	var th loginThrottle
	now := time.Now()
	for i := 0; i < 4; i++ {
		th.fail("10.0.0.9", now)
	}
	if th.blockedFor("10.0.0.9", now) != 0 {
		t.Fatal("4 misses: not blocked yet")
	}
	th.fail("10.0.0.9", now)
	if w := th.blockedFor("10.0.0.9", now); w < 29*time.Second || w > 31*time.Second {
		t.Fatalf("5th miss blocks 30s, got %v", w)
	}
	if th.blockedFor("10.0.0.10", now) != 0 {
		t.Fatal("other addresses are unaffected")
	}
	th.fail("10.0.0.9", now)
	if w := th.blockedFor("10.0.0.9", now); w < 59*time.Second {
		t.Fatalf("6th miss doubles, got %v", w)
	}
	th.ok("10.0.0.9")
	if th.blockedFor("10.0.0.9", now) != 0 {
		t.Fatal("a correct password clears it")
	}
}

func TestSessionRotationSignsEveryoneOut(t *testing.T) {
	var saved string
	m := NewSessionManager([]byte("0123456789abcdef0123456789abcdef"), func(h string) error { saved = h; return nil })
	rec := httptest.NewRecorder()
	m.issue(rec)
	req := httptest.NewRequest("GET", "/", nil)
	for _, c := range rec.Result().Cookies() {
		req.AddCookie(c)
	}
	if !m.valid(req) {
		t.Fatal("fresh cookie is valid")
	}
	if err := m.rotate(); err != nil {
		t.Fatal(err)
	}
	if m.valid(req) {
		t.Fatal("after rotation the old cookie must be rejected")
	}
	if len(saved) != 64 {
		t.Fatalf("rotated secret must be persisted, got %q", saved)
	}
}

// Parallel attempts from one address used to all pass the block check before
// any failure was counted; only one may be in flight now.
func TestLoginThrottleOneAttemptInFlight(t *testing.T) {
	var th loginThrottle
	now := time.Now()
	release, wait := th.begin("10.0.0.5", now)
	if wait != 0 {
		t.Fatalf("first attempt refused: %v", wait)
	}
	if _, wait := th.begin("10.0.0.5", now); wait == 0 {
		t.Fatal("a second concurrent attempt from the same address was allowed")
	}
	if r2, wait := th.begin("10.0.0.6", now); wait != 0 {
		t.Fatal("another address was refused")
	} else {
		r2()
	}
	release()
	if r3, wait := th.begin("10.0.0.5", now); wait != 0 {
		t.Fatal("refused after the first attempt finished")
	} else {
		r3()
	}
	for i := 0; i < 5; i++ {
		th.fail("10.0.0.5", now)
	}
	if _, wait := th.begin("10.0.0.5", now); wait == 0 {
		t.Fatal("five failures should block")
	}
}
