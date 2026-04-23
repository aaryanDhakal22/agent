-- +goose Up
-- Default auto-accept is OFF: orders sit in `arrived` until the mobile
-- interface explicitly accepts them. The field still exists so mobile can
-- flip it on for unattended operation; we just don't assume that's the
-- desired stance out of the box.
ALTER TABLE settings ALTER COLUMN auto_accept SET DEFAULT false;
UPDATE settings SET auto_accept = false WHERE id = 1;

-- +goose Down
ALTER TABLE settings ALTER COLUMN auto_accept SET DEFAULT true;
UPDATE settings SET auto_accept = true WHERE id = 1;
