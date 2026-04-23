-- +goose Up
-- Printer network locations, mutable at runtime via the mobile interface.
-- Env vars seed rows on startup if-absent; mobile updates win thereafter and
-- survive restarts. The primary key is the printer's human name (e.g. "Pizza",
-- "Online") — matches the names the agent already uses in metrics + logs.
CREATE TABLE IF NOT EXISTS printer_configs (
    name       text        PRIMARY KEY,
    ip         text        NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE IF EXISTS printer_configs;
