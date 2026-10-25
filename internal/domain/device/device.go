// Package device models the counter tablets paired to this agent. A tablet
// pairs once with a 6-digit code shown in the web UI and gets a device key it
// sends as X-Agent-Device-Key on every request; the key is stored hashed.
// Since 2.1.0 a device has a kind: a counter tablet, or an integration (the
// store hub), whose key opens only /api/integration/* and the read-only
// floor routes.
package device

import (
	"context"
	"errors"
	"time"
)

// Device kinds.
const (
	KindTablet      = "tablet"
	KindIntegration = "integration"
)

// Device is one paired tablet (the key itself is never stored or returned).
type Device struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Kind       string     `json:"kind"`
	KeyPrefix  string     `json:"key_prefix"`
	CreatedAt  time.Time  `json:"created_at"`
	LastSeenAt *time.Time `json:"last_seen_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

var ErrNotFound = errors.New("device not found")

type Repository interface {
	Create(ctx context.Context, id, name, kind, keyHash, keyPrefix string) error
	// FindActiveByHash returns the non-revoked device with that key hash.
	FindActiveByHash(ctx context.Context, keyHash string) (*Device, error)
	Touch(ctx context.Context, id string, at time.Time) error
	List(ctx context.Context) ([]Device, error)
	Revoke(ctx context.Context, id string, at time.Time) error
	// Rename changes an active device's name (ErrNotFound if revoked/unknown).
	Rename(ctx context.Context, id, name string) error
	RevokeAll(ctx context.Context, at time.Time) error
}
