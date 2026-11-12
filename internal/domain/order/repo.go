package order

import (
	"context"
	"time"

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
	// estimate (auto: accepted by auto-accept, Status.AutoAccepted).
	// ErrAlreadyAccepted if it isn't arrived; ErrNotFound if missing.
	// The caller prints afterwards and calls RevertAccept if nothing printed,
	// so a printer outage never causes a silently lost order.
	ClaimAccept(ctx context.Context, id uuid.UUID, prepMinutes int, auto bool) (*StoredOrder, error)

	// RevertAccept puts a claimed order back to arrived.
	RevertAccept(ctx context.Context, id uuid.UUID) error

	FloorRepository
	PoolRepository
	IntegrationRepository
	DriverViewRepository
}

// IntegrationRepository persists the store-hub surface (the callout box):
// settings, the hub feed and future orders. Every change the hub must see
// writes its feed event in the same transaction.
type IntegrationRepository interface {
	IntegrationSettings(ctx context.Context) (IntegrationSettings, error)
	// SetIntegrationSettings stores all three and writes settings.changed.
	SetIntegrationSettings(ctx context.Context, set IntegrationSettings) error
	IntegrationStatus(ctx context.Context) (IntegrationStatus, error)
	SetIntegrationStatus(ctx context.Context, st IntegrationStatus) error

	// AppendFeed writes a feed event that goes with no other change.
	AppendFeed(ctx context.Context, typ string, at time.Time, data any) error
	// ListFeed returns up to limit feed events after seq, and the newest seq.
	ListFeed(ctx context.Context, after int64, limit int) ([]FeedEvent, int64, error)

	// Schedule puts a future order (arrived or scheduled) into scheduled
	// with its lead time and ring time (ErrInvalidState otherwise).
	Schedule(ctx context.Context, id string, leadMinutes int64, ringAt time.Time) (*StoredOrder, error)
	// RaiseDue puts every scheduled order whose ring time passed back to
	// arrived (rerung) and returns them.
	RaiseDue(ctx context.Context, now time.Time) ([]StoredOrder, error)
	// SetEarlyPrinted records a future order's early tickets (2.4.0).
	SetEarlyPrinted(ctx context.Context, id string, kitchen, receipt bool) error
	// PrepareMakeNow (2.4.3) marks a waiting future order (arrived or
	// scheduled) made now and leaves it arrived for the accept that follows
	// (ErrInvalidState otherwise).
	PrepareMakeNow(ctx context.Context, id string) error
	// SetDueOverride (2.4.3) stores the Pi's own due time of a waiting order
	// and, when ringAt is set, its new ring time; no floor event.
	SetDueOverride(ctx context.Context, id string, due time.Time, ringAt *time.Time) (*StoredOrder, error)
	// CancelWaiting (2.4.3) cancels a waiting order (arrived or scheduled,
	// not mid-accept) with its cancelled floor event (ErrInvalidState).
	CancelWaiting(ctx context.Context, id string) (*StoredOrder, error)

	// 00019. FinishDueOnlinePickups finishes online pickups / dine-in ready since
	// before cutoff (finished_by auto) and returns them.
	FinishDueOnlinePickups(ctx context.Context, cutoff, now time.Time) ([]StoredOrder, error)
	// InsertCalloutTest writes the feed's callout.test.
	InsertCalloutTest(ctx context.Context, t CalloutTestData) error
	// InsertCalloutRender writes the feed's callout.render (2.3.2).
	InsertCalloutRender(ctx context.Context, t CalloutRenderData) error
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
	// SetPushoverSecrets stores the Pushover part of the secrets (2.3.7).
	SetPushoverSecrets(ctx context.Context, sec Secrets) error
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
	// PushoverAdminDevices (2.3.7): the admin's phones, a comma list.
	PushoverAdminDevices string `json:"pushover_admin_devices,omitempty"`
	OTLPEndpoint         string `json:"otlp_endpoint,omitempty"`
	OTLPHeaders          string `json:"otlp_headers,omitempty"`
	TailscaleAuthKey     string `json:"tailscale_authkey,omitempty"`
}

// LinkInfo is a stored manager-login link.
type LinkInfo struct {
	Secrets         Secrets
	ManagerUsername string
	DeviceID        string
}

// DriverViewRepository is the terminal's driver view (contract/agent.md
// §2.10b) and the batch auto-close (§2.10c).
type DriverViewRepository interface {
	// DeliverOne is a driver's "close" on one of his out orders: delivered
	// (order.status to main); returned = it was his last stop and he is back
	// in the queue. ErrDriverNotFound; ErrInvalidState when it isn't one of
	// his out orders.
	DeliverOne(ctx context.Context, driverID, orderID string) (so *StoredOrder, returned bool, err error)
	// ClaimDelivered is a driver claiming deliveries nobody took (the
	// Dispatch section): each must be ClaimableLate(rule) in the open batch
	// (one already his and delivered is left as it is), else *ErrTaken with
	// the ones that failed, all or nothing; an unknown id is ErrNotFound. Each
	// becomes his and delivered (delivered now when it wasn't), order.status
	// to main; he does not go out. claimed = the orders that changed.
	ClaimDelivered(ctx context.Context, driverID string, ids []string, rule ClaimRule, by MarkedBy) (claimed []StoredOrder, err error)
	// AutoCloseLast / SetAutoCloseLast: the last auto-close slot handled
	// (zero = none yet).
	AutoCloseLast(ctx context.Context) (time.Time, error)
	SetAutoCloseLast(ctx context.Context, slot time.Time) error
	// CountOpenBatchOrders: the open batch's orders (total) and the ones
	// still on the floor (active).
	CountOpenBatchOrders(ctx context.Context) (total, active int64, err error)
}
