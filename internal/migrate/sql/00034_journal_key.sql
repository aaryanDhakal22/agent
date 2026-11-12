-- +goose Up
-- A terminal's own journal rows are deduplicated by detail.client_key
-- (contract/agent.md §2.10s §9: the terminal's offline journal replays with
-- a key per entry).
CREATE INDEX idx_journal_client_key ON journal (device_id, action, json_extract(detail, '$.client_key'))
WHERE json_extract(detail, '$.client_key') IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_journal_client_key;
