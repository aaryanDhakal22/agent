package printer

import (
	"sort"
	"sync"
	"time"
)

// StatusSnapshot is what the mobile interface sees when it polls. Timestamps
// are pointers so "never probed" vs. "probed at t=0" is distinguishable.
type StatusSnapshot struct {
	Name string `json:"name"`
	// IP is the address that was used for the most recent probe. Empty string
	// means the printer has no IP configured yet (mobile hasn't set one and
	// no env-var seed exists) — in that case Up is false and LastError
	// explains it.
	IP          string     `json:"ip"`
	Up          bool       `json:"up"`
	LastChecked *time.Time `json:"last_checked,omitempty"`
	// LastError carries the most recent failure string. Cleared the first time
	// the printer comes back up so mobile doesn't show stale errors.
	LastError string `json:"last_error,omitempty"`
	// UpSince / DownSince mark the most recent state transition. Both can be
	// nil before the first probe completes. On a transition only the new one
	// is updated; the old one is left untouched as historical context.
	UpSince   *time.Time `json:"up_since,omitempty"`
	DownSince *time.Time `json:"down_since,omitempty"`
}

// Registry is a small in-memory cache of the last probe result per printer.
// KeepCheck writes to it on every iteration; the HTTP handlers read it. No
// I/O is performed at read time — mobile can poll this at whatever frequency
// it likes without adding load to the printer network.
type Registry struct {
	mu      sync.RWMutex
	entries map[string]StatusSnapshot
}

func NewRegistry() *Registry {
	return &Registry{entries: make(map[string]StatusSnapshot)}
}

// Register pre-populates a printer so GET /api/printers lists it even before
// the first probe completes. Called from the printer Service's constructor.
func (r *Registry) Register(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.entries[name]; !ok {
		r.entries[name] = StatusSnapshot{Name: name}
	}
}

// Record updates the snapshot after a probe. `at` is when the probe finished;
// `ip` is the address that was just probed (so mobile sees the IP in use
// without needing a separate read). Transition edges are inferred by
// comparing `up` against the last known state, so UpSince/DownSince only move
// when the status actually flips.
func (r *Registry) Record(name, ip string, up bool, probeErr error, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()

	prev, existed := r.entries[name]
	snap := prev
	snap.Name = name
	snap.IP = ip
	snap.Up = up
	t := at
	snap.LastChecked = &t

	if up {
		snap.LastError = ""
		if !existed || !prev.Up {
			snap.UpSince = &t
		}
	} else {
		if probeErr != nil {
			snap.LastError = probeErr.Error()
		}
		if !existed || prev.Up || prev.LastChecked == nil {
			snap.DownSince = &t
		}
	}

	r.entries[name] = snap
}

// Get returns the snapshot for a named printer, or false if it was never
// registered.
func (r *Registry) Get(name string) (StatusSnapshot, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.entries[name]
	return s, ok
}

// All returns every registered printer's snapshot, sorted by name so the
// mobile UI has a stable order.
func (r *Registry) All() []StatusSnapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]StatusSnapshot, 0, len(r.entries))
	for _, s := range r.entries {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
