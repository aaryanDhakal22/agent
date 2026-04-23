package order

import (
	"fmt"
	"time"
)

// State is the lifecycle of an order inside the agent.
//
//   - StateArrived   — persisted; awaiting mobile acceptance (or auto-accept).
//   - StateAccepted  — mobile (or auto-accept) has accepted it and the receipt
//     has been printed. PrintedDate is set.
type State string

const (
	StateArrived  State = "arrived"
	StateAccepted State = "accepted"
)

func (s State) Valid() bool {
	switch s {
	case StateArrived, StateAccepted:
		return true
	}
	return false
}

// Status captures the order's lifecycle metadata. It lives alongside the order
// payload in the DB and is what the mobile interface cares about.
type Status struct {
	State       State     `json:"state"`
	ArrivalDate time.Time `json:"arrival_date"`
	// PrintedDate is set the first time an order is printed (on accept) and is
	// never overwritten — subsequent reprints do not bump it.
	PrintedDate *time.Time `json:"printed_date,omitempty"`
}

// StoredOrder is the repository's view of an order: the original payload plus
// the status metadata the agent maintains.
type StoredOrder struct {
	Order  OrderRequest `json:"order"`
	Status Status       `json:"status"`
}

// ErrNotFound is returned by the repository when an order isn't present.
var ErrNotFound = fmt.Errorf("order not found")

// ErrAlreadyAccepted is returned when the accept flow is called on an order
// that's no longer in the arrived state (e.g. double-tap from mobile).
var ErrAlreadyAccepted = fmt.Errorf("order already accepted")
