-- +goose Up
-- 2.2.0: an order accepted by auto-accept (the tablet shows a banner), the
-- Quibase version each paired tablet last reported (X-Quibase-Version), the
-- "nearby" rule for the next driver's run, and the Pi auto-update after
-- Close batch.
ALTER TABLE orders ADD COLUMN auto_accepted boolean NOT NULL DEFAULT false;
ALTER TABLE paired_devices ADD COLUMN app_version text NOT NULL DEFAULT '';
ALTER TABLE paired_devices ADD COLUMN app_version_seen_at text;
ALTER TABLE settings ADD COLUMN nearby_rule text NOT NULL DEFAULT 'detour';
ALTER TABLE settings ADD COLUMN nearby_detour_minutes integer NOT NULL DEFAULT 5;
ALTER TABLE settings ADD COLUMN nearby_distance_mi real NOT NULL DEFAULT 1.0;
ALTER TABLE settings ADD COLUMN auto_update boolean NOT NULL DEFAULT true;

-- +goose Down
ALTER TABLE settings DROP COLUMN auto_update;
ALTER TABLE settings DROP COLUMN nearby_distance_mi;
ALTER TABLE settings DROP COLUMN nearby_detour_minutes;
ALTER TABLE settings DROP COLUMN nearby_rule;
ALTER TABLE paired_devices DROP COLUMN app_version_seen_at;
ALTER TABLE paired_devices DROP COLUMN app_version;
ALTER TABLE orders DROP COLUMN auto_accepted;
