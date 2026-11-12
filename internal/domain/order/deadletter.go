package order

import "time"

// Floor sync dead letters (contract/agent.md §2.4e). An event main refuses
// for good that the Pi keeps (floorsync.KeepRefused) is parked after
// SyncPark.Refusals refusals or SyncPark.Hours since the first refusal: it
// leaves the queue (keeping its place for a retry), and later events about
// the same subject wait behind it.

// Default parking thresholds (Pi settings, PUT /api/settings/sync).
const (
	DefaultSyncParkRefusals = 8
	DefaultSyncParkHours    = 2
)

// SyncPark is the parking thresholds (Pi settings) and the last parked-
// events Pushover.
type SyncPark struct {
	Refusals  int        `json:"refusals"`
	Hours     int        `json:"hours"`
	AlertedAt *time.Time `json:"alerted_at,omitempty"`
}

// Due reports whether an event refused n times since first is parked now.
func (p SyncPark) Due(n int64, first, now time.Time) bool {
	refusals, hours := p.Refusals, p.Hours
	if refusals <= 0 {
		refusals = DefaultSyncParkRefusals
	}
	if hours <= 0 {
		hours = DefaultSyncParkHours
	}
	return n >= int64(refusals) || (!first.IsZero() && now.Sub(first) >= time.Duration(hours)*time.Hour)
}

// Refusal is an event's refusal count after main refused it once more.
type Refusal struct {
	EventID   string
	Type      string
	Subject   string
	Refusals  int64
	FirstAt   time.Time
	LastError string
}

// DeadLetter is a parked floor event as the web page and the route show it
// (never the payload: it may carry a customer's details).
type DeadLetter struct {
	EventID        string    `json:"event_id"`
	Type           string    `json:"type"`
	Subject        string    `json:"subject"`
	Result         string    `json:"result"`
	Refusals       int64     `json:"refusals"`
	FirstRefusedAt time.Time `json:"first_refused_at"`
	ParkedAt       time.Time `json:"parked_at"`
	LastError      string    `json:"last_error"`
	At             time.Time `json:"at"` // when the event was written
}

// ParkedView is GET /api/sync/parked.
type ParkedView struct {
	Parked   []DeadLetter `json:"parked"`
	Count    int64        `json:"count"`
	Held     int64        `json:"held"`    // events waiting behind a parked one
	Backlog  int64        `json:"backlog"` // unsynced and not parked
	Settings SyncPark     `json:"settings"`
}
