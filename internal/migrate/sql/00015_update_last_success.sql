-- +goose Up
-- update_last_success: when the last Pi update finished ok (RFC 3339, '' =
-- none known). update.status only holds the newest run, so a later failed
-- run would hide it; the agent.update.last_success gauge reads this.
ALTER TABLE settings ADD COLUMN update_last_success text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE settings DROP COLUMN update_last_success;
