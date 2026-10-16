-- +goose Up
-- +goose StatementBegin
-- Headless Pi recovery (2026-10-16): an agent linked with the store manager
-- login keeps the runtime secrets main handed it (no secrets on the SD card,
-- no Doppler on the Pi), the manager username (its web UI unlocks with the
-- manager login; the bcrypt hash lives in ui_password_hash), its device id,
-- and when it last uploaded its nightly backup.
ALTER TABLE settings ADD COLUMN secret_pushover_app_token text NOT NULL DEFAULT '';
ALTER TABLE settings ADD COLUMN secret_pushover_user_key  text NOT NULL DEFAULT '';
ALTER TABLE settings ADD COLUMN secret_pushover_boss_key  text NOT NULL DEFAULT '';
ALTER TABLE settings ADD COLUMN secret_otlp_endpoint      text NOT NULL DEFAULT '';
ALTER TABLE settings ADD COLUMN secret_otlp_headers       text NOT NULL DEFAULT '';
ALTER TABLE settings ADD COLUMN manager_username          text NOT NULL DEFAULT '';
ALTER TABLE settings ADD COLUMN device_id                 text NOT NULL DEFAULT '';
ALTER TABLE settings ADD COLUMN last_backup_at            text NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE settings DROP COLUMN last_backup_at;
ALTER TABLE settings DROP COLUMN device_id;
ALTER TABLE settings DROP COLUMN manager_username;
ALTER TABLE settings DROP COLUMN secret_otlp_headers;
ALTER TABLE settings DROP COLUMN secret_otlp_endpoint;
ALTER TABLE settings DROP COLUMN secret_pushover_boss_key;
ALTER TABLE settings DROP COLUMN secret_pushover_user_key;
ALTER TABLE settings DROP COLUMN secret_pushover_app_token;
-- +goose StatementEnd
