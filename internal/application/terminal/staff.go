package terminal

import (
	"context"
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

// One PIN check per terminal at a time, and one for the whole store (the
// security round, H3): the slot is reserved BEFORE bcrypt, so N parallel
// requests can't all pass the throttle check and then count their misses
// too late. A second check on the same terminal is refused at once; a
// check on another terminal waits up to pinSlotWait for the store's slot
// and re-reads the throttle once it has it.
const pinSlotWait = 3 * time.Second

// The daily ceiling (H3): a slow guesser (a few PINs every 15 minutes) never
// trips the short throttle. DefaultPinDailyCeiling wrong PINs in the store
// in pinDayWindow lock every PIN check until a manager unlocks them (the Pi
// page / Quibase, the store manager login) and page the owner. A Pi setting
// (the owner's "X means configurable").
const (
	DefaultPinDailyCeiling = 40
	MinPinDailyCeiling     = 10
	MaxPinDailyCeiling     = 1000
	pinDayWindow           = 24 * time.Hour
)

// Server-side idle expiry of a PIN session (M4): main's auto_lock_minutes
// (the terminal locks itself then) + sessionIdleMargin without any call
// from that terminal ends the session on the Pi too.
const (
	defaultAutoLockMinutes = 5
	sessionIdleMargin      = 2 * time.Minute
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
	// Day: the store's misses of the last 24 h; DayLocked: the daily
	// ceiling was reached then (every PIN check refused until a manager
	// unlocks); DailyCeiling: the setting (0 = the default).
	Day          []time.Time `json:"day,omitempty"`
	DayLocked    *time.Time  `json:"day_locked,omitempty"`
	DailyCeiling int         `json:"daily_ceiling,omitempty"`
}

func (p pinState) ceiling() int {
	if p.DailyCeiling < MinPinDailyCeiling || p.DailyCeiling > MaxPinDailyCeiling {
		return DefaultPinDailyCeiling
	}
	return p.DailyCeiling
}

type staffBook struct {
	mu       sync.Mutex
	sessions map[string]session // device id -> who is on it
	pins     pinState
	// cache: sha256(PIN) -> the staff member it matched (cleared with every
	// new config), so a login doesn't bcrypt every staff member each time.
	cache map[[32]byte]cached
	// checking: the terminals with a PIN check in flight; slot: the store's
	// one PIN check at a time (begin).
	checking map[string]bool
	slot     chan struct{}
	// idle: how long a session lives without a call from its terminal, for
	// the config idleFor was read from.
	idle    time.Duration
	idleFor *terminal.Config
}

func newStaffBook() *staffBook {
	return &staffBook{sessions: map[string]session{}, pins: pinState{Devices: map[string][]time.Time{}}, cache: map[[32]byte]cached{},
		checking: map[string]bool{}, slot: make(chan struct{}, 1)}
}

// begin reserves this terminal's single PIN check and the store's slot
// before the PIN is looked at; the caller calls release when the check is
// decided (its miss counted). A locked store answers *terminal.PinLocked, a
// paused or busy terminal *terminal.Throttled.
func (b *staffBook) begin(ctx context.Context, device string, now func() time.Time) (release func(), err error) {
	refuse := func() error { // under b.mu
		if b.pins.DayLocked != nil {
			return &terminal.PinLocked{At: *b.pins.DayLocked}
		}
		if w := b.waitLocked(device, now()); w > 0 {
			return &terminal.Throttled{Wait: w}
		}
		return nil
	}
	b.mu.Lock()
	if err := refuse(); err != nil {
		b.mu.Unlock()
		return nil, err
	}
	if b.checking[device] {
		b.mu.Unlock()
		return nil, &terminal.Throttled{Wait: time.Second}
	}
	b.checking[device] = true
	b.mu.Unlock()
	free := func() {
		b.mu.Lock()
		delete(b.checking, device)
		b.mu.Unlock()
	}
	t := time.NewTimer(pinSlotWait)
	defer t.Stop()
	select {
	case b.slot <- struct{}{}:
	case <-t.C:
		free()
		return nil, &terminal.Throttled{Wait: time.Second}
	case <-ctx.Done():
		free()
		return nil, &terminal.Throttled{Wait: time.Second}
	}
	// The store's count may have moved while this check waited its turn.
	b.mu.Lock()
	err = refuse()
	b.mu.Unlock()
	if err != nil {
		<-b.slot
		free()
		return nil, err
	}
	return func() {
		<-b.slot
		free()
	}, nil
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
	DayMisses    int  // the store's misses in 24 h
	DayLocked    bool // this miss reached the daily ceiling: locked until a manager unlocks
	Ceiling      int
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
	b.pins.Day = append(recent(b.pins.Day, now, pinDayWindow), now)
	r := missResult{DeviceMisses: len(b.pins.Devices[device]), StoreMisses: len(b.pins.Store), DayMisses: len(b.pins.Day), Ceiling: b.pins.ceiling()}
	if b.pins.DayLocked == nil && r.DayMisses >= r.Ceiling {
		t := now
		b.pins.DayLocked, r.DayLocked = &t, true
	}
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

// PinGuard is the daily ceiling's state (GET /api/pin-guard).
type PinGuard struct {
	Locked       bool       `json:"locked"`
	LockedAt     *time.Time `json:"locked_at,omitempty"`
	Misses24h    int        `json:"misses_24h"`
	DailyCeiling int        `json:"daily_ceiling"`
}

func (b *staffBook) guard(now time.Time) PinGuard {
	b.mu.Lock()
	defer b.mu.Unlock()
	g := PinGuard{Locked: b.pins.DayLocked != nil, Misses24h: len(recent(b.pins.Day, now, pinDayWindow)), DailyCeiling: b.pins.ceiling()}
	if b.pins.DayLocked != nil {
		t := b.pins.DayLocked.UTC()
		g.LockedAt = &t
	}
	return g
}

// unlock clears the daily lock and every counted miss (a manager did it):
// was = it was locked.
func (b *staffBook) unlock() (was bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	was = b.pins.DayLocked != nil
	b.pins.DayLocked, b.pins.Day, b.pins.Store, b.pins.Devices = nil, nil, nil, map[string][]time.Time{}
	return was
}

// setCeiling stores the daily ceiling (0 = back to the default).
func (b *staffBook) setCeiling(n int) {
	b.mu.Lock()
	b.pins.DailyCeiling = n
	b.mu.Unlock()
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

// match finds the active staff member whose PIN this is (never a
// call-center agent: their PINs don't work on a store terminal).
func (b *staffBook) match(cfg *terminal.Config, pin string) (terminal.Staff, bool) {
	if cfg == nil {
		return terminal.Staff{}, false
	}
	key := sha256.Sum256([]byte(pin))
	b.mu.Lock()
	c, ok := b.cache[key]
	b.mu.Unlock()
	if ok {
		if s, found := cfg.Find(c.staffID); found && s.PinHash == c.hash && s.Role != terminal.RoleCallcenter {
			return s, true
		}
	}
	for _, s := range cfg.Staff {
		// A call-center agent's PIN works only in the call-center app (main
		// checks it): on a store terminal it is a wrong PIN, and never a
		// manager's (contract §2.10ab).
		if !s.IsActive() || s.PinHash == "" || s.Role == terminal.RoleCallcenter {
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
	// seen: the terminal's last call (touch); a session idle past the
	// auto-lock + a margin is over on the Pi too (M4).
	seen time.Time
}

func (b *staffBook) login(device, staffID, pinHash string, now time.Time) {
	b.mu.Lock()
	b.sessions[device] = session{staffID: staffID, pinHash: pinHash, seen: now}
	b.mu.Unlock()
}

// idleTTL is how long a session lives without a call from its terminal:
// main's auto_lock_minutes (1-240, default 5) + sessionIdleMargin.
func idleTTL(cfg *terminal.Config) time.Duration {
	m := defaultAutoLockMinutes
	if cfg != nil && len(cfg.Settings) > 0 {
		var v struct {
			AutoLockMinutes *int `json:"auto_lock_minutes"`
		}
		if json.Unmarshal(cfg.Settings, &v) == nil && v.AutoLockMinutes != nil && *v.AutoLockMinutes >= 1 && *v.AutoLockMinutes <= 240 {
			m = *v.AutoLockMinutes
		}
	}
	return time.Duration(m)*time.Minute + sessionIdleMargin
}

// touch notes a call from the terminal: its session (if any) stays alive.
// An expired one is not brought back.
func (b *staffBook) touch(device string, cfg *terminal.Config, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.sessions[device]
	if !ok || b.expiredLocked(s, cfg, now) {
		return
	}
	s.seen = now
	b.sessions[device] = s
}

func (b *staffBook) expiredLocked(s session, cfg *terminal.Config, now time.Time) bool {
	if b.idleFor != cfg || b.idle == 0 {
		b.idle, b.idleFor = idleTTL(cfg), cfg
	}
	return !s.seen.IsZero() && now.Sub(s.seen) > b.idle
}

// live is the session on the terminal unless it idled out: expired true =
// it was dropped just now (the caller journals it once).
func (b *staffBook) live(device string, cfg *terminal.Config, now time.Time) (s session, ok, expired bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok = b.sessions[device]
	if !ok {
		return session{}, false, false
	}
	if b.expiredLocked(s, cfg, now) {
		delete(b.sessions, device)
		return s, false, true
	}
	return s, true, false
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
