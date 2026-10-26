package repositories

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
	"quiccpos/agent/internal/infra/database/models"
)

// Pi updates + learned printer MACs (2.0.6).

// CountOpenBatchOrders returns how many orders the open batch holds, and how
// many of them are still in play (arrived / accepted / ready / on_delivery) —
// the update gate ("right after Close batch" = total 0; idle = active 0).
func (r *OrderRepository) CountOpenBatchOrders(ctx context.Context) (total, active int64, err error) {
	ctx, _, end := dbSpan(ctx, "orders.count_open_batch")
	defer end(&err)
	row, err := models.New(r.db).CountOpenBatchOrders(ctx)
	if err != nil {
		return 0, 0, err
	}
	return row.Total, row.Active, nil
}

// GetUpdateState returns the persisted update job (JSON, "" = none) and the
// "<id>:<state>" whose Pushover was already sent.
func (r *SettingsRepository) GetUpdateState(ctx context.Context) (job, notified string, err error) {
	row, err := models.New(r.db).GetUpdateState(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	return row.UpdateJob, row.UpdateNotified, err
}

func (r *SettingsRepository) SetUpdateJob(ctx context.Context, job string) error {
	return models.New(r.db).SetUpdateJob(ctx, job)
}

func (r *SettingsRepository) SetUpdateNotified(ctx context.Context, key string) error {
	return models.New(r.db).SetUpdateNotified(ctx, key)
}

// GetUpdateLastSuccess is when the last Pi update finished ok (zero = none
// known).
func (r *SettingsRepository) GetUpdateLastSuccess(ctx context.Context) (time.Time, error) {
	v, err := models.New(r.db).GetUpdateLastSuccess(ctx)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && v == "") {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339, v)
}

func (r *SettingsRepository) SetUpdateLastSuccess(ctx context.Context, at time.Time) error {
	return models.New(r.db).SetUpdateLastSuccess(ctx, at.UTC().Format(time.RFC3339))
}

// PrinterNetRepository persists learned printer MACs.
type PrinterNetRepository struct{ db *sql.DB }

func NewPrinterNetRepository(db *sql.DB) *PrinterNetRepository { return &PrinterNetRepository{db: db} }

// ListMACs returns every learned printer MAC keyed by printer id.
func (r *PrinterNetRepository) ListMACs(ctx context.Context) (out map[string]printer.NetAddress, err error) {
	ctx, _, end := dbSpan(ctx, "printer_macs.list")
	defer end(&err)
	rows, err := models.New(r.db).ListPrinterMACs(ctx)
	if err != nil {
		return nil, err
	}
	out = make(map[string]printer.NetAddress, len(rows))
	for _, row := range rows {
		at, _ := time.Parse(time.RFC3339Nano, row.SeenAt)
		out[row.PrinterID] = printer.NetAddress{PrinterID: row.PrinterID, MAC: row.Mac, IP: row.Ip, SeenAt: at}
	}
	return out, nil
}

// SaveMAC upserts one printer's MAC + IP (callers only call it on a change).
func (r *PrinterNetRepository) SaveMAC(ctx context.Context, m printer.NetAddress) (err error) {
	ctx, _, end := dbSpan(ctx, "printer_macs.upsert")
	defer end(&err)
	return models.New(r.db).UpsertPrinterMAC(ctx, models.UpsertPrinterMACParams{
		PrinterID: m.PrinterID, Mac: m.MAC, Ip: m.IP, SeenAt: m.SeenAt.UTC().Format(time.RFC3339Nano),
	})
}

// OpenFloorCount is one (state, online|POS) bucket of the open batch's
// in-play orders (the floor.orders.open gauge).
type OpenFloorCount struct {
	State order.State
	POS   bool
	N     int64
}

// CountOpenFloor counts the open batch's orders still in play (arrived,
// scheduled, accepted, ready, on_delivery) by state and online/POS. POS
// counter checks hidden from Kitchen are left out. No span: the metric
// reader calls it every export (15 s), which would be a root trace each time.
func (r *OrderRepository) CountOpenFloor(ctx context.Context) ([]OpenFloorCount, error) {
	rows, err := models.New(r.db).CountOpenFloorOrders(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]OpenFloorCount, 0, len(rows))
	for _, row := range rows {
		out = append(out, OpenFloorCount{State: order.State(row.State), POS: row.Pos != 0, N: row.N})
	}
	return out, nil
}
