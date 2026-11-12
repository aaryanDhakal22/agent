-- +goose Up
-- setup_wizard (Centurion's first-time setup, contract/agent.md §2.10y):
-- admin_pair = the admin pairing code for counter terminals (JSON {code, off};
-- '' = the default code 1024, on); device_setups = each paired device's last
-- setup / full-check result from its report (JSON {device id: result}).
ALTER TABLE settings ADD COLUMN admin_pair text NOT NULL DEFAULT '';
ALTER TABLE settings ADD COLUMN device_setups text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE settings DROP COLUMN device_setups;
ALTER TABLE settings DROP COLUMN admin_pair;
