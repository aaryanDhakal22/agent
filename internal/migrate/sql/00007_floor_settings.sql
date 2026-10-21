-- +goose Up
-- +goose StatementBegin
-- Store settings edited in the agent web UI (owner decision 2026-10-16):
--   green_till_minutes: kitchen tickets stay green this long after arrival,
--                       then shade to red at twice that.
--   tip_tax_bps:        taken off drivers' tips on the driver report (600 = 6%).
-- The last driver return is remembered so a mistaken "returned" tap can be
-- undone from the tablet (driver back to out, previous queue position).
-- app_release: the Quibase APK this agent has cached for its tablets (JSON).
ALTER TABLE settings ADD COLUMN green_till_minutes integer NOT NULL DEFAULT 20;
ALTER TABLE settings ADD COLUMN tip_tax_bps integer NOT NULL DEFAULT 600;
ALTER TABLE settings ADD COLUMN last_return_driver_id text NOT NULL DEFAULT '';
ALTER TABLE settings ADD COLUMN last_return_position integer NOT NULL DEFAULT 0;
ALTER TABLE settings ADD COLUMN last_return_at text NOT NULL DEFAULT '';
ALTER TABLE settings ADD COLUMN app_release text NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE settings DROP COLUMN app_release;
ALTER TABLE settings DROP COLUMN last_return_at;
ALTER TABLE settings DROP COLUMN last_return_position;
ALTER TABLE settings DROP COLUMN last_return_driver_id;
ALTER TABLE settings DROP COLUMN tip_tax_bps;
ALTER TABLE settings DROP COLUMN green_till_minutes;
-- +goose StatementEnd
