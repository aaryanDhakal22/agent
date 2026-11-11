package terminal

import (
	"crypto/sha256"
	"regexp"
	"sync"
	"time"

	"quiccpos/agent/internal/domain/terminal"

	"golang.org/x/crypto/bcrypt"
)

// Staff login on the terminals: a 2-4 digit PIN checked against the bcrypt
// hashes in the cached terminal config (works offline). One staff member is
// logged in per terminal (device); the terminal locks itself (auto-lock is
// its own), the Pi only remembers who is on each one, in memory.

var pinRe = regexp.MustCompile(`^[0-9]{2,4}$`)

// Wrong PINs on one terminal: after pinFreeMisses misses in a row it waits
// pinFirstLock, doubling on each further miss up to pinMaxLock.
const (
	pinFreeMisses = 5
	pinFirstLock  = 30 * time.Second
	pinMaxLock    = 5 * time.Minute
)

type misses struct {
	n     int
	until time.Time
	lock  time.Duration
}

type cached struct {
	staffID string
	hash    string
}

type staffBook struct {
	mu       sync.Mutex
	sessions map[string]string // device id -> staff id
	misses   map[string]*misses
	// cache: sha256(PIN) -> the staff member it matched (cleared with every
	// new config), so a login doesn't bcrypt every staff member each time.
	cache map[[32]byte]cached
}

func newStaffBook() *staffBook {
	return &staffBook{sessions: map[string]string{}, misses: map[string]*misses{}, cache: map[[32]byte]cached{}}
}

// throttled is how long the device must wait before its next PIN (0 = none).
func (b *staffBook) throttled(device string, now time.Time) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	if m := b.misses[device]; m != nil && now.Before(m.until) {
		return m.until.Sub(now)
	}
	return 0
}

func (b *staffBook) miss(device string, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	m := b.misses[device]
	if m == nil {
		m = &misses{}
		b.misses[device] = m
	}
	m.n++
	if m.n < pinFreeMisses {
		return
	}
	switch {
	case m.lock == 0:
		m.lock = pinFirstLock
	case m.lock < pinMaxLock:
		m.lock = min(m.lock*2, pinMaxLock)
	}
	m.until = now.Add(m.lock)
}

func (b *staffBook) hit(device string) {
	b.mu.Lock()
	delete(b.misses, device)
	b.mu.Unlock()
}

// match finds the active staff member whose PIN this is.
func (b *staffBook) match(cfg *terminal.Config, pin string) (terminal.Staff, bool) {
	if cfg == nil {
		return terminal.Staff{}, false
	}
	key := sha256.Sum256([]byte(pin))
	b.mu.Lock()
	c, ok := b.cache[key]
	b.mu.Unlock()
	if ok {
		if s, found := cfg.Find(c.staffID); found && s.PinHash == c.hash {
			return s, true
		}
	}
	for _, s := range cfg.Staff {
		if !s.IsActive() || s.PinHash == "" {
			continue
		}
		if bcrypt.CompareHashAndPassword([]byte(s.PinHash), []byte(pin)) == nil {
			b.mu.Lock()
			b.cache[key] = cached{staffID: s.ID, hash: s.PinHash}
			b.mu.Unlock()
			return s, true
		}
	}
	return terminal.Staff{}, false
}

func (b *staffBook) login(device, staffID string) {
	b.mu.Lock()
	b.sessions[device] = staffID
	b.mu.Unlock()
}

func (b *staffBook) logout(device string) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id, ok := b.sessions[device]
	delete(b.sessions, device)
	return id, ok
}

func (b *staffBook) current(device string) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id, ok := b.sessions[device]
	return id, ok
}

// configChanged drops the PIN cache and the sessions of staff who are gone
// (removed or switched off in main).
func (b *staffBook) configChanged(cfg *terminal.Config) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cache = map[[32]byte]cached{}
	for dev, id := range b.sessions {
		if _, ok := cfg.Find(id); !ok {
			delete(b.sessions, dev)
		}
	}
}
