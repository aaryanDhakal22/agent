package printer

import (
	"context"
	"time"
)

// PrinterConfig is the persisted address of one printer. The env var is the
// seed; mobile can overwrite it at runtime via ConfigRepository.SetIP.
type PrinterConfig struct {
	Name      string    `json:"name"`
	IP        string    `json:"ip"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ConfigRepository is the persistence port for printer network addresses.
type ConfigRepository interface {
	// UpsertIfAbsent inserts (name, ip) only if no row exists for `name`.
	// Used to seed env-var values on startup without stomping on mobile-set
	// overrides.
	UpsertIfAbsent(ctx context.Context, name, ip string) error

	// SetIP is the mobile-update path — always writes.
	SetIP(ctx context.Context, name, ip string) error

	// Get returns the stored config, or (_, false) if no row for `name`.
	Get(ctx context.Context, name string) (PrinterConfig, bool, error)

	// List returns all configs, sorted by name.
	List(ctx context.Context) ([]PrinterConfig, error)
}
