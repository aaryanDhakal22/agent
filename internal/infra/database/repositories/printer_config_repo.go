package repositories

import (
	"context"
	"errors"
	"fmt"

	domainprinter "quiccpos/agent/internal/domain/printer"
	"quiccpos/agent/internal/infra/database/models"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
)

var _ domainprinter.ConfigRepository = (*PrinterConfigRepository)(nil)

type PrinterConfigRepository struct {
	pool   *pgxpool.Pool
	logger zerolog.Logger
}

func NewPrinterConfigRepository(pool *pgxpool.Pool, lg zerolog.Logger) *PrinterConfigRepository {
	return &PrinterConfigRepository{
		pool:   pool,
		logger: lg.With().Str("module", "printer-config-repo").Logger(),
	}
}

func (r *PrinterConfigRepository) UpsertIfAbsent(ctx context.Context, name, ip string) error {
	q := models.New(r.pool)
	return q.UpsertPrinterConfigIfAbsent(ctx, models.UpsertPrinterConfigIfAbsentParams{
		Name: name,
		Ip:   ip,
	})
}

func (r *PrinterConfigRepository) SetIP(ctx context.Context, name, ip string) error {
	q := models.New(r.pool)
	return q.SetPrinterIP(ctx, models.SetPrinterIPParams{
		Name: name,
		Ip:   ip,
	})
}

func (r *PrinterConfigRepository) Get(ctx context.Context, name string) (domainprinter.PrinterConfig, bool, error) {
	q := models.New(r.pool)
	row, err := q.GetPrinterConfig(ctx, name)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domainprinter.PrinterConfig{}, false, nil
		}
		return domainprinter.PrinterConfig{}, false, fmt.Errorf("get printer config: %w", err)
	}
	return domainprinter.PrinterConfig{
		Name:      row.Name,
		IP:        row.Ip,
		UpdatedAt: row.UpdatedAt.Time,
	}, true, nil
}

func (r *PrinterConfigRepository) List(ctx context.Context) ([]domainprinter.PrinterConfig, error) {
	q := models.New(r.pool)
	rows, err := q.ListPrinterConfigs(ctx)
	if err != nil {
		return nil, fmt.Errorf("list printer configs: %w", err)
	}
	out := make([]domainprinter.PrinterConfig, 0, len(rows))
	for _, row := range rows {
		out = append(out, domainprinter.PrinterConfig{
			Name:      row.Name,
			IP:        row.Ip,
			UpdatedAt: row.UpdatedAt.Time,
		})
	}
	return out, nil
}
