package order

import (
	"encoding/json"
	"time"
)

// Geo states (main's geo worker): located = lat/lng known; not_found = no
// geocoder matched (staff can drop a pin); pending = main is still trying.
const (
	GeoLocated  = "located"
	GeoNotFound = "not_found"
	GeoPending  = "pending"
	// GeoUnavailable: the agent has no geo from main for a delivery that
	// arrived GeoPendingWindow ago or more (geo off on main, an older order,
	// or main unreachable). Terminal on the agent; never sent by main.
	GeoUnavailable = "unavailable"
)

// GeoPendingWindow: a delivery without geo from main reads as pending this
// long after it arrived, then as unavailable.
const GeoPendingWindow = 15 * time.Minute

// FillMissingGeo gives a delivery order main never located an honest geo
// state (pending, then unavailable); pickups and orders with geo stay as is.
func (so *StoredOrder) FillMissingGeo(now time.Time) {
	if so.Geo != nil || !so.IsDelivery() {
		return
	}
	st := GeoUnavailable
	if now.Sub(so.Status.ArrivalDate) < GeoPendingWindow {
		st = GeoPending
	}
	so.Geo = &OrderGeo{State: st}
}

// OrderGeo is where main located a delivery address and the driving route to
// it from the store (StoredOrder.geo; contract/agent.md).
type OrderGeo struct {
	State string   `json:"state"`
	Lat   *float64 `json:"lat,omitempty"`
	Lng   *float64 `json:"lng,omitempty"`
	// Quality exact|approx|pinned; empty when not found.
	Quality   string `json:"quality,omitempty"`
	DistanceM int64  `json:"distance_m"`
	DurationS int64  `json:"duration_s"`
	// Polyline: store -> address route, Google encoded polyline (precision 5).
	Polyline    string    `json:"polyline"`
	OutsideArea bool      `json:"outside_area"`
	UpdatedAt   time.Time `json:"updated_at,omitzero"` // absent on an agent-made state
	// Drive seconds store -> address and back (AUTO; not on the wire).
	StoreToS *int64 `json:"-"`
	ToStoreS *int64 `json:"-"`
}

// MarshalJSON writes an agent-made state (FillMissingGeo: no row from main,
// zero UpdatedAt) as just {"state"}; main's rows keep every field.
func (g OrderGeo) MarshalJSON() ([]byte, error) {
	if g.UpdatedAt.IsZero() && g.Lat == nil && (g.State == GeoPending || g.State == GeoUnavailable) {
		return json.Marshal(struct {
			State string `json:"state"`
		}{g.State})
	}
	type plain OrderGeo
	return json.Marshal(plain(g))
}

// Located reports whether the address has coordinates.
func (g *OrderGeo) Located() bool {
	return g != nil && g.State == GeoLocated && g.Lat != nil && g.Lng != nil
}

// GeoUpdate is main's order.geo RPC payload.
type GeoUpdate struct {
	OrderID     string    `json:"order_id"`
	State       string    `json:"state"`
	Lat         *float64  `json:"lat"`
	Lng         *float64  `json:"lng"`
	Quality     string    `json:"quality"`
	DistanceM   int64     `json:"distance_m"`
	DurationS   int64     `json:"duration_s"`
	Polyline    string    `json:"polyline"`
	OutsideArea bool      `json:"outside_area"`
	StoreToS    *int64    `json:"store_to_s"`
	ToStoreS    *int64    `json:"to_store_s"`
	Times       []GeoTime `json:"times"`
}

// GeoTime is the drive time between this order and another open delivery:
// ToS = this -> other, FromS = other -> this (seconds; nil = unknown).
type GeoTime struct {
	Other string `json:"other"`
	ToS   *int64 `json:"to_s"`
	FromS *int64 `json:"from_s"`
}
