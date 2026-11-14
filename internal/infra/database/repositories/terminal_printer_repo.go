package repositories

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"quiccpos/agent/internal/domain/device"
)

// The Pi's copy of each counter terminal's printer settings (contract
// §2.10ap, migration 00048_terminal_printers), on the setup store.

var _ device.TerminalPrinterStore = (*SetupRepository)(nil)

// TerminalPrinters lists the copies, newest first.
func (r *SetupRepository) TerminalPrinters(ctx context.Context) (out []device.TerminalPrinter, err error) {
	ctx, _, end := dbSpan(ctx, "terminal_printers.list")
	defer end(&err)
	rows, err := r.db.QueryContext(ctx, `SELECT printer_key, device_id, device_name, profile, route, updated_at
		FROM terminal_printers ORDER BY updated_at DESC, printer_key`)
	if err != nil {
		return nil, fmt.Errorf("list terminal_printers: %w", err)
	}
	defer rows.Close()
	out = []device.TerminalPrinter{}
	for rows.Next() {
		var p device.TerminalPrinter
		var profile, route, at string
		if err := rows.Scan(&p.PrinterKey, &p.DeviceID, &p.DeviceName, &profile, &route, &at); err != nil {
			return nil, fmt.Errorf("scan terminal_printers: %w", err)
		}
		p.Profile = json.RawMessage(profile)
		if route != "" {
			p.Route = json.RawMessage(route)
		}
		p.UpdatedAt, _ = time.Parse(time.RFC3339Nano, at)
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetTerminalPrinter keeps one printer's copy (the newest per printer key),
// dropping the oldest past device.MaxTerminalPrinters.
func (r *SetupRepository) SetTerminalPrinter(ctx context.Context, p device.TerminalPrinter) (err error) {
	ctx, _, end := dbSpan(ctx, "terminal_printers.set")
	defer end(&err)
	if err := device.CheckTerminalPrinter(p); err != nil {
		return err
	}
	at := p.UpdatedAt
	if at.IsZero() {
		at = time.Now()
	}
	route := ""
	if len(p.Route) > 0 && string(p.Route) != "null" {
		route = string(p.Route)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO terminal_printers (printer_key, device_id, device_name, profile, route, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (printer_key) DO UPDATE SET device_id = excluded.device_id, device_name = excluded.device_name,
			profile = excluded.profile, route = excluded.route, updated_at = excluded.updated_at`,
		p.PrinterKey, p.DeviceID, p.DeviceName, string(p.Profile), route, at.UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("set terminal_printers: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM terminal_printers WHERE printer_key NOT IN
		(SELECT printer_key FROM terminal_printers ORDER BY updated_at DESC, printer_key LIMIT ?)`, device.MaxTerminalPrinters); err != nil {
		return fmt.Errorf("trim terminal_printers: %w", err)
	}
	return tx.Commit()
}
