package order

import (
	"testing"
	"time"
)

// OpenAgain (contract 2.10ak, batch B): reopened and nothing settled it since.
func TestStatusOpenAgain(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 11, 14, 17, 0, 0, 0, time.UTC)
	at := func(m int) *time.Time { v := t0.Add(time.Duration(m) * time.Minute); return &v }
	for _, c := range []struct {
		name string
		st   Status
		want bool
	}{
		{"never reopened", Status{PaidAt: at(-5)}, false},
		{"reopened after it was paid", Status{ReopenedAt: at(0), PaidAt: at(-5), FinishedAt: at(-2)}, true},
		{"paid again after the reopen", Status{ReopenedAt: at(0), PaidAt: at(3)}, false},
		{"picked up after the reopen", Status{ReopenedAt: at(0), FinishedAt: at(3)}, false},
		{"sent out after the reopen", Status{ReopenedAt: at(0), OutAt: at(3)}, false},
		{"delivered before the reopen", Status{ReopenedAt: at(0), OutAt: at(-20), DeliveredAt: at(-10)}, true},
		{"delivered after the reopen", Status{ReopenedAt: at(0), DeliveredAt: at(1)}, false},
	} {
		if got := c.st.OpenAgain(); got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
}
