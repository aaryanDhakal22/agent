-- +goose Up
-- +goose StatementBegin
-- Persist the web-UI session-signing secret so browser sessions survive agent
-- restarts (previously random per-boot → every restart logged managers out).
ALTER TABLE settings ADD COLUMN session_secret text NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE settings DROP COLUMN session_secret;
-- +goose StatementEnd
