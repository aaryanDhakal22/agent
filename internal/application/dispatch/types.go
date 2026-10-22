// Package dispatch is the AUTO-assign optimizer: given the waiting delivery
// orders, the available drivers (queue order, with their locked pools) and the
// drive-time matrix main pre-sends, it suggests one run per driver.
//
// It is the Meal Delivery Routing Problem solved exactly by bundle
// enumeration (Reyes et al. 2018; Yildiz & Savelsbergh 2019): every feasible
// run of <= MaxStops orders (detour limit + one 90-degree sector) is
// enumerated with its best stop order, then a
// branch & bound picks the cheapest disjoint set of runs (one per driver,
// locked pools kept). Pure functions, no I/O: the same Input always gives the
// same Output (except SolveMs, and except when the time budget is hit, which
// sets Fallback).
//
// Wire contract: contract/agent.md "AUTO plan" (Input = the autoplan CLI's
// stdin, Output = the runs/unassigned part of POST /api/deliveries/auto-plan).
package dispatch

import "time"

// Input is one planning request. Times are RFC3339; durations are seconds.
type Input struct {
	// Now is the planning instant (the floor clock).
	Now time.Time `json:"now"`
	// TargetMinutes is the placed-to-door target (setting
	// delivery_target_minutes, 45 for Lombardi's). <= 0 means 45.
	TargetMinutes int `json:"target_minutes"`
	// SoonReadyMinutes admits still-cooking orders whose ReadyAt is within
	// this many minutes of Now (setting soon_ready_minutes). < 0 means 0.
	SoonReadyMinutes int `json:"soon_ready_minutes"`
	// Store is the store pin; nil when unknown (then only the matrix is used).
	Store *LatLng `json:"store"`
	// Orders are the open delivery orders (accepted|ready), unassigned or pooled.
	Orders []Order `json:"orders"`
	// Drivers are the AVAILABLE drivers in queue order (next first).
	Drivers []Driver `json:"drivers"`
	// OutDrivers are the drivers on a run, with their estimated return; only
	// used to estimate when a left-over order would leave.
	OutDrivers []OutDriver `json:"out_drivers"`
	// Times is the road drive-time matrix (seconds). Missing pairs are
	// estimated from straight-line distance (Output.Estimated).
	Times Times `json:"times"`
	// BudgetMs bounds the exact search; <= 0 means 200.
	BudgetMs int `json:"budget_ms"`
	// Params overrides the tuning constants (the simulator's knob). Omitted
	// fields keep DefaultParams.
	Params *Params `json:"params,omitempty"`
}

// LatLng is a WGS84 point.
type LatLng struct {
	Lat float64 `json:"lat"`
	Lng float64 `json:"lng"`
}

// Order is one open delivery order.
type Order struct {
	ID       string    `json:"id"`
	PlacedAt time.Time `json:"placed_at"`
	// ReadyAt is the estimated (or actual) ready time. Ignored when Ready.
	// Omit (or null) when unknown.
	ReadyAt time.Time `json:"ready_at,omitzero"`
	Ready   bool      `json:"ready"`
	Lat     float64   `json:"lat"`
	Lng     float64   `json:"lng"`
	// Located is false while main hasn't geocoded it (or couldn't): such an
	// order is never auto-placed.
	Located bool `json:"located"`
	// PoolDriverID is the driver whose pool holds it ("" = unassigned).
	PoolDriverID string `json:"pool_driver_id"`
}

// Driver is an available driver. Pool lists the orders already pooled to it
// (union'd with the orders whose PoolDriverID is this driver).
type Driver struct {
	ID   string   `json:"id"`
	Name string   `json:"name"`
	Pool []string `json:"pool"`
}

// OutDriver is a driver currently on a run.
type OutDriver struct {
	ID       string    `json:"id"`
	ReturnAt time.Time `json:"return_at"`
}

// Times is the drive-time matrix in seconds.
type Times struct {
	Store StoreTimes `json:"store"`
	// Pairs[a][b] = seconds driving from order a to order b.
	Pairs map[string]map[string]int `json:"pairs"`
}

// StoreTimes: To[id] = store -> id, From[id] = id -> store (seconds).
type StoreTimes struct {
	To   map[string]int `json:"to"`
	From map[string]int `json:"from"`
}

// Output is the plan. Runs are in driver queue order.
type Output struct {
	Runs       []Run        `json:"runs"`
	Unassigned []Unassigned `json:"unassigned"`
	// Estimated is true when a leg of a suggested run used a straight-line
	// estimate instead of a road time.
	Estimated bool `json:"estimated"`
	// Fallback is true when the time budget was hit and the plan is the best
	// found so far (or the greedy plan), not the proven optimum.
	Fallback bool  `json:"fallback"`
	SolveMs  int64 `json:"solve_ms"`
	// Cost is the plan objective (cost units ~ "minutes of badness", see Params).
	Cost float64 `json:"cost"`
}

// Run is one driver's suggested trip.
type Run struct {
	DriverID string `json:"driver_id"`
	// OrderIDs in driving order (first stop first), pooled + suggested.
	OrderIDs []string `json:"order_ids"`
	// SuggestedIDs are the OrderIDs AUTO adds (not already pooled to this
	// driver), in driving order. Empty for a pool AUTO left as is.
	SuggestedIDs []string  `json:"suggested_ids"`
	DepartAt     time.Time `json:"depart_at"`
	ReturnAt     time.Time `json:"return_at"`
	// DriveS is the pure driving time (out, between stops, back), no service.
	DriveS int     `json:"drive_s"`
	Stops  []Stop  `json:"stops"`
	Cost   float64 `json:"cost"`
}

// Stop is one delivery of a run.
type Stop struct {
	OrderID       string    `json:"order_id"`
	ETA           time.Time `json:"eta"`
	PlacedToDoorS int       `json:"placed_to_door_s"`
	// Late is true when PlacedToDoorS is past the target.
	Late bool `json:"late"`
}

// Unassigned is a candidate-or-not order AUTO did not place.
type Unassigned struct {
	OrderID string `json:"order_id"`
	Reason  string `json:"reason"`
}

// Unassigned reasons (contract).
const (
	ReasonNoLocation    = "no_location"     // not geocoded (or no road time can be estimated)
	ReasonNotReadySoon  = "not_ready_soon"  // cooking, ready later than now+soon_ready_minutes
	ReasonNoDriver      = "no_driver"       // drivers all used (or none available)
	ReasonNoFeasibleRun = "no_feasible_run" // no driver can take it (only locked pools, none fit)
	ReasonOverLimit     = "over_limit"      // beyond the MaxCandidates oldest
)
