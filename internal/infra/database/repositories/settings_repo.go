package repositories

import (
	"context"
	"errors"
	"fmt"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/infra/database/models"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
)

var _ order.SettingsRepository = (*SettingsRepository)(nil)

type SettingsRepository struct {
	pool   *pgxpool.Pool
	logger zerolog.Logger
}

func NewSettingsRepository(pool *pgxpool.Pool, lg zerolog.Logger) *SettingsRepository {
	return &SettingsRepository{
		pool:   pool,
		logger: lg.With().Str("module", "settings-repo").Logger(),
	}
}

func (r *SettingsRepository) GetAutoAccept(ctx context.Context) (bool, error) {
	q := models.New(r.pool)
	v, err := q.GetAutoAccept(ctx)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Initial migration seeds a row; this shouldn't happen, but if it
			// does, fall back to OFF so the agent never surprises the kitchen
			// with an unexpected auto-print.
			return false, nil
		}
		return false, fmt.Errorf("get auto_accept: %w", err)
	}
	return v, nil
}

func (r *SettingsRepository) SetAutoAccept(ctx context.Context, enabled bool) error {
	q := models.New(r.pool)
	return q.SetAutoAccept(ctx, enabled)
}
