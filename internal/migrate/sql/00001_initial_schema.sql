-- +goose Up
-- +goose StatementBegin

-- The agent stores main/'s canonical Order Snapshots verbatim as JSON text plus
-- a few denormalised columns the read paths need (id, scope, source identity,
-- lifecycle state, timestamps). Reprints + UI re-renders rebuild everything from
-- the snapshot column. Timestamps are RFC3339 text so the pure-Go SQLite driver
-- never has to guess a time format.
CREATE TABLE orders (
    id              text    PRIMARY KEY,
    tenant_id       text    NOT NULL,
    location_id     text    NOT NULL,
    source          text    NOT NULL,
    source_event_id text    NOT NULL,
    snapshot        text    NOT NULL,
    state           text    NOT NULL DEFAULT 'arrived',
    arrival_date    text    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    printed_date    text,
    CHECK (state IN ('arrived', 'accepted'))
);
CREATE INDEX idx_orders_arrival_date ON orders (arrival_date DESC);
CREATE INDEX idx_orders_state_pending ON orders (arrival_date DESC) WHERE state = 'arrived';
CREATE INDEX idx_orders_source_event ON orders (source, source_event_id);

-- Auto-accept toggle + the agent's STORE_API_KEY, both editable from the
-- in-store web UI. Singleton row.
CREATE TABLE settings (
    id            integer PRIMARY KEY DEFAULT 1,
    auto_accept   boolean NOT NULL DEFAULT false,
    store_api_key text    NOT NULL DEFAULT '',
    updated_at    text    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    CHECK (id = 1)
);
INSERT INTO settings (id) VALUES (1) ON CONFLICT DO NOTHING;

-- store_config persists the last starter pack main/ sent us over SSE. One row,
-- upserted on every SSE connect. Lets the agent restart and still know its
-- tenant + location + printers without a network call.
CREATE TABLE store_config (
    id         integer PRIMARY KEY DEFAULT 1,
    context    text    NOT NULL DEFAULT '{}',
    updated_at text    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    CHECK (id = 1)
);
INSERT INTO store_config (id) VALUES (1) ON CONFLICT DO NOTHING;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS store_config;
DROP TABLE IF EXISTS settings;
DROP INDEX IF EXISTS idx_orders_source_event;
DROP INDEX IF EXISTS idx_orders_state_pending;
DROP INDEX IF EXISTS idx_orders_arrival_date;
DROP TABLE IF EXISTS orders;

-- +goose StatementEnd
