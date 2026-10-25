package repositories

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"quiccpos/agent/internal/domain/device"
	"quiccpos/agent/internal/infra/database/models"
)

var _ device.Repository = (*DeviceRepository)(nil)

// DeviceRepository persists paired counter tablets.
type DeviceRepository struct{ db *sql.DB }

func NewDeviceRepository(db *sql.DB) *DeviceRepository { return &DeviceRepository{db: db} }

func (r *DeviceRepository) Create(ctx context.Context, id, name, kind, keyHash, keyPrefix string) (err error) {
	ctx, _, end := dbSpan(ctx, "devices.create")
	defer end(&err)
	if kind == "" {
		kind = device.KindTablet
	}
	return models.New(r.db).CreatePairedDevice(ctx, models.CreatePairedDeviceParams{
		ID: id, Name: name, KeyHash: keyHash, KeyPrefix: keyPrefix, Kind: kind,
	})
}

func (r *DeviceRepository) FindActiveByHash(ctx context.Context, keyHash string) (d *device.Device, err error) {
	ctx, _, end := dbSpan(ctx, "devices.find_by_hash")
	defer end(&err)
	row, err := models.New(r.db).GetPairedDeviceByHash(ctx, keyHash)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, device.ErrNotFound
		}
		return nil, err
	}
	out, err := toDevice(row)
	return &out, err
}

func (r *DeviceRepository) Touch(ctx context.Context, id string, at time.Time) error {
	return models.New(r.db).TouchPairedDevice(ctx, models.TouchPairedDeviceParams{LastSeenAt: nullTS(at), ID: id})
}

func (r *DeviceRepository) List(ctx context.Context) (out []device.Device, err error) {
	ctx, _, end := dbSpan(ctx, "devices.list")
	defer end(&err)
	rows, err := models.New(r.db).ListPairedDevices(ctx)
	if err != nil {
		return nil, err
	}
	out = make([]device.Device, 0, len(rows))
	for _, row := range rows {
		d, err := toDevice(row)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

func (r *DeviceRepository) Revoke(ctx context.Context, id string, at time.Time) (err error) {
	ctx, _, end := dbSpan(ctx, "devices.revoke")
	defer end(&err)
	n, err := models.New(r.db).RevokePairedDevice(ctx, models.RevokePairedDeviceParams{RevokedAt: nullTS(at), ID: id})
	if err != nil {
		return err
	}
	if n == 0 {
		return device.ErrNotFound
	}
	return nil
}

func (r *DeviceRepository) Rename(ctx context.Context, id, name string) (err error) {
	ctx, _, end := dbSpan(ctx, "devices.rename")
	defer end(&err)
	n, err := models.New(r.db).RenamePairedDevice(ctx, models.RenamePairedDeviceParams{Name: name, ID: id})
	if err != nil {
		return err
	}
	if n == 0 {
		return device.ErrNotFound
	}
	return nil
}

func (r *DeviceRepository) RevokeAll(ctx context.Context, at time.Time) (err error) {
	ctx, _, end := dbSpan(ctx, "devices.revoke_all")
	defer end(&err)
	return models.New(r.db).RevokeAllPairedDevices(ctx, nullTS(at))
}

func toDevice(row models.PairedDevice) (device.Device, error) {
	created, err := parseTS(row.CreatedAt)
	if err != nil {
		return device.Device{}, err
	}
	seen, err := optTS(row.LastSeenAt)
	if err != nil {
		return device.Device{}, err
	}
	revoked, err := optTS(row.RevokedAt)
	if err != nil {
		return device.Device{}, err
	}
	return device.Device{ID: row.ID, Name: row.Name, Kind: row.Kind, KeyPrefix: row.KeyPrefix, CreatedAt: created,
		LastSeenAt: seen, RevokedAt: revoked}, nil
}
