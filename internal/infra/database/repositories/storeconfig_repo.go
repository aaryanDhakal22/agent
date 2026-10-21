package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"quiccpos/agent/internal/domain/storeconfig"
	"quiccpos/agent/internal/infra/database/models"

	"github.com/rs/zerolog"
)

var _ storeconfig.Repository = (*StoreConfigRepository)(nil)

type StoreConfigRepository struct {
	db     *sql.DB
	logger zerolog.Logger
}

func NewStoreConfigRepository(db *sql.DB, lg zerolog.Logger) *StoreConfigRepository {
	return &StoreConfigRepository{
		db:     db,
		logger: lg.With().Str("module", "storeconfig-repo").Logger(),
	}
}

func (r *StoreConfigRepository) Get(ctx context.Context) (sc *storeconfig.StoreContext, err error) {
	ctx, _, end := dbSpan(ctx, "store_config.get")
	defer end(&err)

	row, err := models.New(r.db).GetStoreConfig(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return &storeconfig.StoreContext{}, nil
		}
		return nil, fmt.Errorf("get store_config: %w", err)
	}
	if row.Context == "" || row.Context == "{}" {
		return &storeconfig.StoreContext{}, nil
	}
	var out storeconfig.StoreContext
	if err := json.Unmarshal([]byte(row.Context), &out); err != nil {
		return nil, fmt.Errorf("unmarshal store_config: %w", err)
	}
	return &out, nil
}

func (r *StoreConfigRepository) Save(ctx context.Context, sc storeconfig.StoreContext) (err error) {
	ctx, _, end := dbSpan(ctx, "store_config.save")
	defer end(&err)

	body, err := json.Marshal(sc)
	if err != nil {
		return fmt.Errorf("marshal store_config: %w", err)
	}
	if err := models.New(r.db).UpsertStoreConfig(ctx, string(body)); err != nil {
		return fmt.Errorf("upsert store_config: %w", err)
	}
	return nil
}
