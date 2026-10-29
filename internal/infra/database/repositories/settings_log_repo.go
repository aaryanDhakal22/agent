package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"quiccpos/agent/internal/domain/settingslog"
	"quiccpos/agent/internal/infra/database/models"
)

var _ settingslog.Repository = (*SettingsLogRepository)(nil)

// SettingsLogRepository stores the settings history (settings_log, 00021).
type SettingsLogRepository struct {
	db  *sql.DB
	now func() time.Time
}

func NewSettingsLogRepository(db *sql.DB) *SettingsLogRepository {
	return &SettingsLogRepository{db: db, now: time.Now}
}

// Add stores e (At zero = now) and drops entries older than settingslog.Keep.
func (r *SettingsLogRepository) Add(ctx context.Context, e settingslog.Entry) (err error) {
	ctx, _, end := dbSpan(ctx, "settings_log.add")
	defer end(&err)

	if e.At.IsZero() {
		e.At = r.now()
	}
	changes, err := json.Marshal(e.Changes)
	if err != nil {
		return fmt.Errorf("encode settings changes: %w", err)
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := models.New(tx)
	if err := q.InsertSettingsChange(ctx, models.InsertSettingsChangeParams{
		At: fmtTS(e.At), Section: e.Section, ByKind: e.By.Kind, ByName: e.By.Name,
		DeviceID: e.By.DeviceID, Changes: string(changes),
	}); err != nil {
		return fmt.Errorf("insert settings change: %w", err)
	}
	if err := q.PruneSettingsLog(ctx, fmtTS(r.now().Add(-settingslog.Keep))); err != nil {
		return fmt.Errorf("prune settings log: %w", err)
	}
	return tx.Commit()
}

// List returns the entries since a time, newest first, at most limit.
func (r *SettingsLogRepository) List(ctx context.Context, since time.Time, limit int) (out []settingslog.Entry, err error) {
	ctx, _, end := dbSpan(ctx, "settings_log.list")
	defer end(&err)

	rows, err := models.New(r.db).ListSettingsLog(ctx, models.ListSettingsLogParams{Since: fmtTS(since), Lim: int64(limit)})
	if err != nil {
		return nil, fmt.Errorf("list settings log: %w", err)
	}
	out = make([]settingslog.Entry, 0, len(rows))
	for _, row := range rows {
		e := settingslog.Entry{ID: row.ID, Section: row.Section,
			By: settingslog.By{Kind: row.ByKind, Name: row.ByName, DeviceID: row.DeviceID}}
		if e.At, err = parseTS(row.At); err != nil {
			return nil, fmt.Errorf("settings log %d time: %w", row.ID, err)
		}
		if err := json.Unmarshal([]byte(row.Changes), &e.Changes); err != nil {
			return nil, fmt.Errorf("settings log %d changes: %w", row.ID, err)
		}
		out = append(out, e)
	}
	return out, nil
}
