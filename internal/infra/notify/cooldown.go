package notify

import (
	"sync"
	"time"
)

// AdminCooldown (2.5.2): the developer's alerts that could repeat for one
// condition (a printer flapping, every order rerouted off the same dead
// printer, an order still waiting) go out at most once per key in this long.
const AdminCooldown = 15 * time.Minute

// Cooldown remembers when each key last went out (in memory).
type Cooldown struct {
	mu   sync.Mutex
	gap  time.Duration
	last map[string]time.Time
}

// NewCooldown: a key passes at most once per gap.
func NewCooldown(gap time.Duration) *Cooldown {
	return &Cooldown{gap: gap, last: map[string]time.Time{}}
}

// Allow reports whether key may go out at now, and if so marks it sent.
// A nil Cooldown allows everything.
func (c *Cooldown) Allow(key string, now time.Time) bool {
	if c == nil {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if t, ok := c.last[key]; ok && now.Sub(t) < c.gap {
		return false
	}
	if len(c.last) > 1000 { // drop what is long past
		for k, t := range c.last {
			if now.Sub(t) >= c.gap {
				delete(c.last, k)
			}
		}
	}
	c.last[key] = now
	return true
}
