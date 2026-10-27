-- +goose Up
-- Driver routes (2.3.7): pushover_device = the driver's phone in Pushover
-- (Send Out pushes them the route; '' = none). secret_pushover_admin_devices
-- = the admin's phones (comma list, handed over by main with the other
-- Pushover secrets): system alerts go only there once it is set.
ALTER TABLE drivers ADD COLUMN pushover_device text NOT NULL DEFAULT '';
ALTER TABLE settings ADD COLUMN secret_pushover_admin_devices text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE settings DROP COLUMN secret_pushover_admin_devices;
ALTER TABLE drivers DROP COLUMN pushover_device;
