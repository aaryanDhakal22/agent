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
}

// ExternalMeta is what the service adds to a POS upsert: the id for a new
// order (UUIDv7, made on the Pi), the store it belongs to, its prep time
// (the store default for its type) and the clock.
type ExternalMeta struct {
	OrderID     string
	TenantID    string
	LocationID  string
	PrepMinutes int64
	Now         time.Time
	// UpdateOnly (2.3.6, PUT ?if_exists=1): a check the Pi doesn't hold is
	// not created — the upsert answers ErrNotFound (the hub's final PUT for
	// a check the Pi already dropped must never bring it back).
	UpdateOnly bool
}

// ExternalResult is a POS upsert's outcome. AddressChanged: an open check is
// a delivery now with a new address (or newly a delivery) — main must locate
// it; DeliveryEnded: an open check stopped being a delivery — main must
// forget its point. PrevDriverPos is the driver number the POS had set
// before (to see a change).
type ExternalResult struct {
	Order          StoredOrder
	Created        bool
	Changed        bool
	AddressChanged bool
	DeliveryEnded  bool
	PrevDriverPos  string
	// 00019: Shown = a hidden check now qualifies for Kitchen (published as
	// a new order, with the chime); Change = a terminal change to a check
	// already on the tablet (rings until OK; nil = none or quiet);
	// Reopened = a ready order went back to cooking (left its pool);
	// Finished = the POS closed it and it is done with; VoidPrint = the
	// kitchen lines removed (for a void ticket where the POS switch prints).
	Shown     bool
	Change    *PosChange
	Reopened  bool
	Finished  bool
	VoidPrint []LineItem
}

// OnlinePatch is PATCH /api/integration/online-orders/{id}. SetFlag with a
// nil Flag clears it.
type OnlinePatch struct {
	Paid           *bool
	PosCheckNumber *int64
	SetFlag        bool
	Flag           *Flag
}

// IntegrationRepository persists the 2.1.0 store-hub surface: settings, POS
// orders, the hub feed and future orders. Every change the hub must see
// writes its feed event in the same transaction.
type IntegrationRepository interface {
	IntegrationSettings(ctx context.Context) (IntegrationSettings, error)
	// SetIntegrationSettings stores all three and writes settings.changed.
	SetIntegrationSettings(ctx context.Context, set IntegrationSettings) error
	IntegrationStatus(ctx context.Context) (IntegrationStatus, error)
	SetIntegrationStatus(ctx context.Context, st IntegrationStatus) error

	// UpsertExternal inserts a new POS check (accepted, no kitchen number,
	// local only) or updates a changed one (same hash = no-op; a finished
	// order only takes the money and paid).
	UpsertExternal(ctx context.Context, extRef string, in ExternalOrder, meta ExternalMeta) (ExternalResult, error)
	SetExtDriver(ctx context.Context, id, posNumber, note string) (*StoredOrder, error)
	// VoidExternal cancels a POS order (pool cleared, order.cancelled on the
	// feed); changed=false when it was already cancelled or delivered.
	VoidExternal(ctx context.Context, source, extRef string) (so *StoredOrder, changed bool, err error)
	PatchOnline(ctx context.Context, id string, p OnlinePatch) (*StoredOrder, error)
	DriverByPosNumber(ctx context.Context, pos string) (*Driver, error)
	// MirrorSendOut sends one delivery out alone with an available driver
	// (marked ready first if cooking; run.sent on the feed); the driver's
	// other pooled orders go back to unassigned.
	MirrorSendOut(ctx context.Context, driverID, orderID string) (sent, readied, unpooled []StoredOrder, err error)
	// MirrorJoinRun puts a delivery on the run of a driver already out.
	MirrorJoinRun(ctx context.Context, driverID, orderID string) (so *StoredOrder, readied []StoredOrder, err error)
	// AppendFeed writes a feed event that goes with no other change (2.3.0:
	// driver_ledger.requested).
	AppendFeed(ctx context.Context, typ string, at time.Time, data any) error
	// ListFeed returns up to limit feed events after seq, and the newest seq.
	ListFeed(ctx context.Context, after int64, limit int) ([]FeedEvent, int64, error)

	// Schedule puts a future order (arrived or scheduled) into scheduled
	// with its lead time and ring time (ErrInvalidState otherwise).
	Schedule(ctx context.Context, id string, leadMinutes int64, ringAt time.Time) (*StoredOrder, error)
	// RaiseDue puts every scheduled order whose ring time passed back to
	// arrived (rerung) and returns them.
	RaiseDue(ctx context.Context, now time.Time) ([]StoredOrder, error)

	// 00019. AckPosChange marks an order's terminal change OK'd (acked_at;
	// it stops ringing, the ticket keeps CHANGED). changeID "" = whatever is
	// pending; acked=false when nothing is pending or a newer one came in.
	AckPosChange(ctx context.Context, id, changeID string) (so *StoredOrder, acked bool, err error)
	// FinishDueOnlinePickups finishes online pickups / dine-in ready since
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
