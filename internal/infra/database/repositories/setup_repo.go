package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"

	"quiccpos/agent/internal/domain/device"
	"quiccpos/agent/internal/infra/database/models"
)

// SetupRepository keeps the setup wizard's settings (contract §2.10y): the
// admin pairing code and each device's last setup result, two JSON columns
// of the settings row (migration 00038_setup_wizard).
type SetupRepository struct {
	db *sql.DB
	mu sync.Mutex // read-modify-write of device_setups
}

var _ device.SetupStore = (*SetupRepository)(nil)

func NewSetupRepository(db *sql.DB) *SetupRepository { return &SetupRepository{db: db} }

func (r *SetupRepository) AdminPair(ctx context.Context) (a device.AdminPair, err error) {
	ctx, _, end := dbSpan(ctx, "settings.get_admin_pair")
	defer end(&err)
	raw, err := models.New(r.db).GetAdminPair(ctx)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && raw == "") {
		return device.AdminPair{}, nil
	}
	if err != nil {
		return a, fmt.Errorf("get admin_pair: %w", err)
	}
	if json.Unmarshal([]byte(raw), &a) != nil {
		return device.AdminPair{}, nil // unreadable = the default
	}
	return a, nil
}

func (r *SetupRepository) SetAdminPair(ctx context.Context, a device.AdminPair) (err error) {
	ctx, _, end := dbSpan(ctx, "settings.set_admin_pair")
	defer end(&err)
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	return models.New(r.db).SetAdminPair(ctx, string(b))
}

func (r *SetupRepository) DeviceSetups(ctx context.Context) (out map[string]device.SetupResult, err error) {
	ctx, _, end := dbSpan(ctx, "settings.get_device_setups")
	defer end(&err)
	return r.read(ctx)
}

func (r *SetupRepository) read(ctx context.Context) (map[string]device.SetupResult, error) {
	out := map[string]device.SetupResult{}
	raw, err := models.New(r.db).GetDeviceSetups(ctx)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && raw == "") {
		return out, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get device_setups: %w", err)
	}
	if json.Unmarshal([]byte(raw), &out) != nil {
		return map[string]device.SetupResult{}, nil
	}
	return out, nil
}

// SetDeviceSetup stores one device's result (the oldest dropped past
// device.MaxKeptSetups).
func (r *SetupRepository) SetDeviceSetup(ctx context.Context, deviceID string, res device.SetupResult) (err error) {
	ctx, _, end := dbSpan(ctx, "settings.set_device_setup")
	defer end(&err)
	r.mu.Lock()
	defer r.mu.Unlock()
	all, err := r.read(ctx)
	if err != nil {
		return err
	}
	all[deviceID] = res
	if len(all) > device.MaxKeptSetups {
		ids := make([]string, 0, len(all))
		for id := range all {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return all[ids[i]].At.Before(all[ids[j]].At) })
		for _, id := range ids[:len(all)-device.MaxKeptSetups] {
			delete(all, id)
		}
	}
	b, err := json.Marshal(all)
	if err != nil {
		return err
	}
	return models.New(r.db).SetDeviceSetups(ctx, string(b))
}
