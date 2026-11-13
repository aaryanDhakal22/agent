package printer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"quiccpos/agent/internal/infra/notify"
	"quiccpos/agent/internal/observability"
)

// 2.5.2: a flapping printer pages the developer's "down" at most once per
// 15 min, and a "back online" only follows a "down" that went out.
func TestFlappingPrinterAdminCooldown(t *testing.T) {
	var mu sync.Mutex
	var msgs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		mu.Lock()
		msgs = append(msgs, r.Form.Get("message"))
		mu.Unlock()
	}))
	defer srv.Close()
	t.Setenv("PUSHOVER_API_URL", srv.URL)
	n := notify.NewNotifier("app", "user", "")

	meters, _ := observability.NewMeters()
	bp := &busyPrinter{release: make(chan struct{})}
	svc := NewService(bp, NewRegistry(), zerolog.Nop(), meters)
	now := time.Unix(1_700_000_000, 0)
	svc.now = func() time.Time { return now }
	svc.downAfter = func() time.Duration { return 30 * time.Second }
	svc.adminCool = notify.NewCooldown(notify.AdminCooldown)
	var st probeState
	cycle := func() { // 40 s dead, 8 s up
		bp.mu.Lock()
		bp.dead = true
		bp.mu.Unlock()
		for i := 0; i < 10; i++ {
			now = now.Add(4 * time.Second)
			svc.probeStep(context.Background(), &st, n)
		}
		bp.mu.Lock()
		bp.dead = false
		bp.mu.Unlock()
		for i := 0; i < 2; i++ {
			now = now.Add(4 * time.Second)
			svc.probeStep(context.Background(), &st, n)
		}
	}
	for i := 0; i < 10; i++ { // 8 minutes of flapping
		cycle()
	}
	mu.Lock()
	got := len(msgs)
	mu.Unlock()
	if got != 2 {
		t.Fatalf("flapping paged %d times: %v", got, msgs)
	}
	now = now.Add(15 * time.Minute)
	cycle()
	mu.Lock()
	defer mu.Unlock()
	if len(msgs) != 4 {
		t.Fatalf("after the cooldown: %v", msgs)
	}
}
