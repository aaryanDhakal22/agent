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
	// GeoCheck: the address is ambiguous; staff pick one of Candidates (or
	// drop a pin). Not located: AUTO treats it like an unknown address.
	GeoCheck = "check"
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
	// Candidates (state check, up to 3) and the address staff confirmed by
	// choosing one (2.1.0).
	Candidates       []GeoCandidate    `json:"candidates,omitempty"`
	ConfirmedAddress *ConfirmedAddress `json:"confirmed_address,omitempty"`
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

// GeoCandidate is one possible match for an ambiguous address.
type GeoCandidate struct {
	Label      string  `json:"label"`
	Line1      string  `json:"line1"`
	City       string  `json:"city"`
	State      string  `json:"state"`
	PostalCode string  `json:"postal_code"`
	Lat        float64 `json:"lat"`
	Lng        float64 `json:"lng"`
}

// ConfirmedAddress is the address staff confirmed (a chosen candidate).
type ConfirmedAddress struct {
	Line1      string `json:"line1"`
	City       string `json:"city"`
	State      string `json:"state"`
	PostalCode string `json:"postal_code"`
}

// Confirmed reports whether this geo is a confirmed place (located, with a
// chosen candidate's address or pinned). It does NOT say staff just confirmed
// it — a geo_cache hit comes back pinned too; only GeoUpdate.Confirmed (main
// answering geo.choose / geo.pin directly) does.
func (g *OrderGeo) Confirmed() bool {
	return g.Located() && (g.ConfirmedAddress != nil || g.Quality == "pinned")
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
	// 2.1.0: ambiguous-address candidates and a chosen candidate's address.
	Candidates       []GeoCandidate    `json:"candidates"`
	ConfirmedAddress *ConfirmedAddress `json:"confirmed_address"`
	// Confirmed: this push is main's direct answer to a staff action
	// (geo.choose or geo.pin). Only then does the hub get address.confirmed.
	Confirmed bool `json:"confirmed"`
}

// GeoTime is the drive time between this order and another open delivery:
// ToS = this -> other, FromS = other -> this (seconds; nil = unknown).
type GeoTime struct {
	Other string `json:"other"`
	ToS   *int64 `json:"to_s"`
	FromS *int64 `json:"from_s"`
}
