package repositories

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/infra/database/models"

	"github.com/rs/zerolog"
)

var _ order.SettingsRepository = (*SettingsRepository)(nil)

type SettingsRepository struct {
	db     *sql.DB
	logger zerolog.Logger
}

func NewSettingsRepository(db *sql.DB, lg zerolog.Logger) *SettingsRepository {
	return &SettingsRepository{
		db:     db,
		logger: lg.With().Str("module", "settings-repo").Logger(),
	}
}

func (r *SettingsRepository) GetAutoAccept(ctx context.Context) (v bool, err error) {
	ctx, _, end := dbSpan(ctx, "settings.get_auto_accept")
	defer end(&err)

	v, err = models.New(r.db).GetAutoAccept(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("get auto_accept: %w", err)
	}
	return v, nil
}

func (r *SettingsRepository) SetAutoAccept(ctx context.Context, enabled bool) (err error) {
	ctx, _, end := dbSpan(ctx, "settings.set_auto_accept")
	defer end(&err)

	return models.New(r.db).SetAutoAccept(ctx, enabled)
}

// GetStoreAPIKey returns the agent's currently-configured key, or "" if none has
// been set yet. Empty is meaningful: it means the agent is in "unconfigured"
// mode and the SSE controller will not attempt to connect.
func (r *SettingsRepository) GetStoreAPIKey(ctx context.Context) (key string, err error) {
	ctx, _, end := dbSpan(ctx, "settings.get_store_api_key")
	defer end(&err)

	key, err = models.New(r.db).GetStoreAPIKey(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("get store_api_key: %w", err)
	}
	return key, nil
}

func (r *SettingsRepository) SetStoreAPIKey(ctx context.Context, key string) (err error) {
	ctx, _, end := dbSpan(ctx, "settings.set_store_api_key")
	defer end(&err)

	return models.New(r.db).SetStoreAPIKey(ctx, key)
}

// GetSessionSecret returns the persisted web-UI session-signing secret (hex), or
// "" if none has been generated yet.
func (r *SettingsRepository) GetSessionSecret(ctx context.Context) (secret string, err error) {
	ctx, _, end := dbSpan(ctx, "settings.get_session_secret")
	defer end(&err)

	secret, err = models.New(r.db).GetSessionSecret(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("get session_secret: %w", err)
	}
	return secret, nil
}

func (r *SettingsRepository) SetSessionSecret(ctx context.Context, secret string) (err error) {
	ctx, _, end := dbSpan(ctx, "settings.set_session_secret")
	defer end(&err)

	if err = models.New(r.db).SetSessionSecret(ctx, secret); err != nil {
		return fmt.Errorf("set session_secret: %w", err)
	}
	return nil
}

// GetUIPassword returns the local web-UI password's bcrypt hash + its optional
// plaintext hint. Empty hash means "no password set yet" (the agent is
// provisioned by token but the operator hasn't chosen a password).
func (r *SettingsRepository) GetUIPassword(ctx context.Context) (hash, hint string, err error) {
	ctx, _, end := dbSpan(ctx, "settings.get_ui_password")
	defer end(&err)

	row, err := models.New(r.db).GetUIPassword(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", nil
		}
		return "", "", fmt.Errorf("get ui_password: %w", err)
	}
	return row.UiPasswordHash, row.UiPasswordHint, nil
}

func (r *SettingsRepository) SetUIPassword(ctx context.Context, hash, hint string) (err error) {
	ctx, _, end := dbSpan(ctx, "settings.set_ui_password")
	defer end(&err)

	if err = models.New(r.db).SetUIPassword(ctx, models.SetUIPasswordParams{
		UiPasswordHash: hash,
		UiPasswordHint: hint,
	}); err != nil {
		return fmt.Errorf("set ui_password: %w", err)
	}
	return nil
}

// ClearProvisioning wipes the agent back to a blank, unprovisioned device: it
// drops the store key AND the local password + hint in one statement. Used on
// unlink (Settings) and on a definitive token revoke by main — both reset the UI
// to the "paste a token" setup screen.
func (r *SettingsRepository) ClearProvisioning(ctx context.Context) (err error) {
	ctx, _, end := dbSpan(ctx, "settings.clear_provisioning")
	defer end(&err)

	if err = models.New(r.db).ClearProvisioning(ctx); err != nil {
		return fmt.Errorf("clear provisioning: %w", err)
	}
	return nil
}

// GetLinkInfo — see order.SettingsRepository.
func (r *SettingsRepository) GetLinkInfo(ctx context.Context) (li order.LinkInfo, err error) {
	row, err := models.New(r.db).GetLinkInfo(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return li, nil
		}
		return li, err
	}
	return order.LinkInfo{
		Secrets: order.Secrets{PushoverAppToken: row.SecretPushoverAppToken, PushoverUserKey: row.SecretPushoverUserKey,
			PushoverBossKey: row.SecretPushoverBossKey, OTLPEndpoint: row.SecretOtlpEndpoint, OTLPHeaders: row.SecretOtlpHeaders},
		ManagerUsername: row.ManagerUsername, DeviceID: row.DeviceID,
	}, nil
}

// SetLinkInfo — see order.SettingsRepository.
func (r *SettingsRepository) SetLinkInfo(ctx context.Context, li order.LinkInfo, passwordHash, hint string) error {
	return models.New(r.db).SetLinkInfo(ctx, models.SetLinkInfoParams{
		SecretPushoverAppToken: li.Secrets.PushoverAppToken, SecretPushoverUserKey: li.Secrets.PushoverUserKey,
		SecretPushoverBossKey: li.Secrets.PushoverBossKey, SecretOtlpEndpoint: li.Secrets.OTLPEndpoint,
		SecretOtlpHeaders: li.Secrets.OTLPHeaders, ManagerUsername: li.ManagerUsername, DeviceID: li.DeviceID,
		UiPasswordHash: passwordHash, UiPasswordHint: hint,
	})
}

func (r *SettingsRepository) SetUIPasswordHash(ctx context.Context, hash string) error {
	return models.New(r.db).SetManagerPasswordHash(ctx, hash)
}

func (r *SettingsRepository) GetLastBackup(ctx context.Context) (string, error) {
	v, err := models.New(r.db).GetLastBackup(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (r *SettingsRepository) SetLastBackup(ctx context.Context, at string) error {
	return models.New(r.db).SetLastBackup(ctx, at)
}
