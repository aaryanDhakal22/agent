package terminal

import (
	"crypto/sha256"
	"encoding/json"
	"regexp"
	"sort"
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

// Wrong PINs (HARDENING-PLAN X-2): counted per terminal AND for the whole
// store in a window that a right PIN never resets (so a cashier can't test
// manager PINs, log in with their own to clear the count, and go again).
// A terminal with pinFreeMisses misses in the window waits pinFirstLock after
// its latest miss, doubling with each further miss up to pinMaxLock; the
// store with pinStoreMisses misses in the window locks every terminal the
// same way. Misses older than pinWindow drop out. Kept in settings
// (pin_throttle) so a restart doesn't clear them; pinPageMisses in
// pinPageWindow page the owner (at most once an hour).
const (
	pinFreeMisses  = 5
	pinStoreMisses = 10
	pinFirstLock   = 30 * time.Second
	pinMaxLock     = 5 * time.Minute
	pinWindow      = 15 * time.Minute
	pinPageMisses  = 10
	pinPageWindow  = 10 * time.Minute
	pinPageEvery   = time.Hour
)

type cached struct {
	staffID string
	hash    string
}

// pinState is the persisted throttle (JSON in settings.pin_throttle).
type pinState struct {
	Devices  map[string][]time.Time `json:"devices"`
	Store    []time.Time            `json:"store"`
	PagedAt  *time.Time             `json:"paged_at,omitempty"`
	LockedAt *time.Time             `json:"locked_at,omitempty"` // the last lockout (the Pi page's pin_lockout)
}

type staffBook struct {
	mu       sync.Mutex
	sessions map[string]session // device id -> who is on it
	pins     pinState
	// cache: sha256(PIN) -> the staff member it matched (cleared with every
	// new config), so a login doesn't bcrypt every staff member each time.
	cache map[[32]byte]cached
}

func newStaffBook() *staffBook {
	return &staffBook{sessions: map[string]session{}, pins: pinState{Devices: map[string][]time.Time{}}, cache: map[[32]byte]cached{}}
}

func recent(ts []time.Time, now time.Time, window time.Duration) []time.Time {
	out := ts[:0:0]
	for _, t := range ts {
		if now.Sub(t) < window {
			out = append(out, t)
		}
	}
	return out
}

// lockFor: how long after its latest miss a list of n misses (free first)
// waits.
func lockFor(n, free int) time.Duration {
	if n < free {
		return 0
	}
	d := pinFirstLock
	for i := free; i < n && d < pinMaxLock; i++ {
		d *= 2
	}
	return min(d, pinMaxLock)
}

// throttled is how long the device must wait before its next PIN (0 = none).
func (b *staffBook) throttled(device string, now time.Time) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.waitLocked(device, now)
}

func (b *staffBook) waitLocked(device string, now time.Time) time.Duration {
	var wait time.Duration
	for _, l := range []struct {
		ts   []time.Time
		free int
	}{{recent(b.pins.Devices[device], now, pinWindow), pinFreeMisses}, {recent(b.pins.Store, now, pinWindow), pinStoreMisses}} {
		if len(l.ts) == 0 {
			continue
		}
		until := l.ts[len(l.ts)-1].Add(lockFor(len(l.ts), l.free))
		if w := until.Sub(now); w > wait {
			wait = w
		}
	}
	return wait
}

// missResult is what one wrong PIN did.
type missResult struct {
	DeviceMisses int  // in the window
	StoreMisses  int  // in the window
	Lockout      bool // the PIN pad is paused now
	Page         bool // page the owner (pinPageMisses in pinPageWindow, once an hour)
}

func (b *staffBook) miss(device string, now time.Time) missResult {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pins.Devices == nil {
		b.pins.Devices = map[string][]time.Time{}
	}
	b.pins.Devices[device] = append(recent(b.pins.Devices[device], now, pinWindow), now)
	b.pins.Store = append(recent(b.pins.Store, now, pinWindow), now)
	for d, ts := range b.pins.Devices {
		if ts = recent(ts, now, pinWindow); len(ts) == 0 {
			delete(b.pins.Devices, d)
		}
	}
	r := missResult{DeviceMisses: len(b.pins.Devices[device]), StoreMisses: len(b.pins.Store)}
	if b.waitLocked(device, now) > 0 {
		r.Lockout = true
		t := now
		b.pins.LockedAt = &t
	}
	if len(recent(b.pins.Store, now, pinPageWindow)) >= pinPageMisses && (b.pins.PagedAt == nil || now.Sub(*b.pins.PagedAt) >= pinPageEvery) {
		r.Page = true
		t := now
		b.pins.PagedAt = &t
	}
	return r
}

// hit: a right PIN. It never clears the misses (X-2): they age out.
func (b *staffBook) hit(string) {}

// lockedRecently: a PIN pad was paused in the last hour.
func (b *staffBook) lockedRecently(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.pins.LockedAt != nil && now.Sub(*b.pins.LockedAt) < time.Hour
}

// pinSnapshot is the throttle to persist.
func (b *staffBook) pinSnapshot() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	raw, _ := json.Marshal(b.pins)
	return raw
}

// restorePins loads a persisted throttle (bad JSON = start empty).
func (b *staffBook) restorePins(raw []byte) {
	var st pinState
	if len(raw) == 0 || json.Unmarshal(raw, &st) != nil {
		return
	}
	if st.Devices == nil {
		st.Devices = map[string][]time.Time{}
	}
	b.mu.Lock()
	b.pins = st
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

// session is one staff member logged in on a terminal: the PIN hash they
// logged in with (a new PIN in main ends the session).
type session struct {
	staffID string
	pinHash string
}

func (b *staffBook) login(device, staffID, pinHash string) {
	b.mu.Lock()
	b.sessions[device] = session{staffID: staffID, pinHash: pinHash}
	b.mu.Unlock()
}

func (b *staffBook) logout(device string) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.sessions[device]
	delete(b.sessions, device)
	return s.staffID, ok
}

func (b *staffBook) current(device string) (session, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.sessions[device]
	return s, ok
}

// endedSession is a session a new config ended.
type endedSession struct {
	DeviceID string `json:"device_id"`
	StaffID  string `json:"staff_id"`
	Reason   string `json:"reason"` // removed | deactivated | pin_changed
}

// endReason: why a config ends this session ("" = it goes on).
func endReason(cfg *terminal.Config, s session) string {
	var found *terminal.Staff
	if cfg != nil {
		for i := range cfg.Staff {
			if cfg.Staff[i].ID == s.staffID {
				found = &cfg.Staff[i]
			}
		}
	}
	switch {
	case found == nil:
		return terminal.SessionRemoved
	case !found.IsActive():
		return terminal.SessionDeactivated
	case found.PinHash != s.pinHash:
		return terminal.SessionPinChanged
	}
	return ""
}

// configChanged drops the PIN cache and ends the sessions of staff who are
// gone, switched off or have a new PIN in main (contract §2.10u): returned
// so the service tells the terminals at once.
func (b *staffBook) configChanged(cfg *terminal.Config) []endedSession {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cache = map[[32]byte]cached{}
	var out []endedSession
	for dev, s := range b.sessions {
		if r := endReason(cfg, s); r != "" {
			delete(b.sessions, dev)
			out = append(out, endedSession{DeviceID: dev, StaffID: s.staffID, Reason: r})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DeviceID < out[j].DeviceID })
	return out
}
