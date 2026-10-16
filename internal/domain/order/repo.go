package order

import (
	"context"

	"github.com/google/uuid"
)

// Repository is the persistence port for orders. Kept in the domain layer so
// the application service depends only on the interface; the pgx-backed
// implementation lives in infra.
type Repository interface {
	// UpsertArrived inserts a freshly-arrived order. Duplicates (same canonical
	// id) silently no-op — the existing row + state are preserved.
	// inserted=false means the order was already stored (SSE redelivery).
	UpsertArrived(ctx context.Context, s Snapshot) (inserted bool, err error)

	// GetByID returns the stored order with its current status.
	GetByID(ctx context.Context, id uuid.UUID) (*StoredOrder, error)

	// ListPage returns orders newest-first across all kept batches.
	ListPage(ctx context.Context, offset, limit int) ([]StoredOrder, error)

	// ListArrived returns every order currently awaiting acceptance, oldest
	// first (the queue order mobile should display).
	ListArrived(ctx context.Context) ([]StoredOrder, error)

	// ClaimAccept transitions arrived→accepted and returns the order, assigning
	// its kitchen number (kept across a revert + re-accept) and the prep
	// estimate. ErrAlreadyAccepted if it isn't arrived; ErrNotFound if missing.
	// The caller prints afterwards and calls RevertAccept if nothing printed,
	// so a printer outage never causes a silently lost order.
	ClaimAccept(ctx context.Context, id uuid.UUID, prepMinutes int) (*StoredOrder, error)

	// RevertAccept puts a claimed order back to arrived.
	RevertAccept(ctx context.Context, id uuid.UUID) error

	FloorRepository
}

// SettingsRepository owns the singleton settings row.
type SettingsRepository interface {
	GetAutoAccept(ctx context.Context) (bool, error)
	SetAutoAccept(ctx context.Context, enabled bool) error

	// StoreAPIKey is the agent's authentication credential to main/. It's
	// editable from the in-store web UI so operators can pair a freshly-
	// built agent with a freshly-minted device on main/'s /admin without
	// redeploying. Empty value means "agent unconfigured" — the SSE
	// controller will refuse to connect.
	GetStoreAPIKey(ctx context.Context) (string, error)
	SetStoreAPIKey(ctx context.Context, key string) error

	// SessionSecret is the persisted HMAC key that signs the web-UI session
	// cookie. Persisting it (vs a per-boot random) is what lets manager sessions
	// survive agent restarts. Empty means "not generated yet".
	GetSessionSecret(ctx context.Context) (string, error)
	SetSessionSecret(ctx context.Context, secret string) error

	// UIPassword is the local web-UI password gate: a bcrypt hash + an optional
	// plaintext hint the operator sets right after provisioning the agent by
	// token. Empty hash means "provisioned but no password chosen yet".
	GetUIPassword(ctx context.Context) (hash, hint string, err error)
	SetUIPassword(ctx context.Context, hash, hint string) error

	// ClearProvisioning wipes the store key + local password + hint together,
	// resetting the agent to a blank, unprovisioned device (unlink / revoke).
	// The secrets and manager login from a link are wiped with them.
	ClearProvisioning(ctx context.Context) error

	// LinkInfo is what a manager-login link stored (secrets from main, the
	// manager username, this agent's device id).
	GetLinkInfo(ctx context.Context) (LinkInfo, error)
	// SetLinkInfo stores a link; the manager's bcrypt hash becomes the web-UI
	// password (offline unlock with the manager login).
	SetLinkInfo(ctx context.Context, li LinkInfo, passwordHash, hint string) error
	// SetUIPasswordHash refreshes the manager hash (password changed in main).
	SetUIPasswordHash(ctx context.Context, hash string) error
	GetLastBackup(ctx context.Context) (string, error)
	SetLastBackup(ctx context.Context, at string) error
}

// Secrets are the runtime secrets main hands a linked agent.
type Secrets struct {
	PushoverAppToken string `json:"pushover_app_token,omitempty"`
	PushoverUserKey  string `json:"pushover_user_key,omitempty"`
	PushoverBossKey  string `json:"pushover_boss_key,omitempty"`
	OTLPEndpoint     string `json:"otlp_endpoint,omitempty"`
	OTLPHeaders      string `json:"otlp_headers,omitempty"`
	TailscaleAuthKey string `json:"tailscale_authkey,omitempty"`
}

// LinkInfo is a stored manager-login link.
type LinkInfo struct {
	Secrets         Secrets
	ManagerUsername string
	DeviceID        string
}
