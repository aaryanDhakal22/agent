-- +goose Up
-- terminal_app_release: the counter terminal's APK this Pi has cached for its
-- terminals (JSON, like app_release for Quibase; contract/agent.md §2.10w).
ALTER TABLE settings ADD COLUMN terminal_app_release text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE settings DROP COLUMN terminal_app_release;
