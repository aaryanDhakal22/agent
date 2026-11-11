-- +goose Up
-- Terminal stage 0d: the Pi is the server for the store's own POS terminals.
--
-- paired_devices.kind gains 'terminal' (a counter POS terminal; its key opens
-- /api/terminal/* and /api/events). SQLite can't change a CHECK, so the
-- table is rebuilt (nothing references it).
CREATE TABLE paired_devices_new (
    id           text PRIMARY KEY,
    name         text NOT NULL,
    key_hash     text NOT NULL UNIQUE,
    key_prefix   text NOT NULL,
    created_at   text NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    last_seen_at text,
    revoked_at   text,
    kind         text NOT NULL DEFAULT 'tablet' CHECK (kind IN ('tablet', 'integration', 'terminal')),
    app_version         text NOT NULL DEFAULT '',
    app_version_seen_at text
);
INSERT INTO paired_devices_new (id, name, key_hash, key_prefix, created_at, last_seen_at, revoked_at, kind, app_version, app_version_seen_at)
SELECT id, name, key_hash, key_prefix, created_at, last_seen_at, revoked_at, kind, app_version, app_version_seen_at FROM paired_devices;
DROP TABLE paired_devices;
ALTER TABLE paired_devices_new RENAME TO paired_devices;

-- The counter sequence (counter orders' numbers): the next number to hand
-- out, 0 = start again at the terminal config's counter_start. Close batch
-- sets it back to 0, like kitchen_next.
ALTER TABLE settings ADD COLUMN counter_next integer NOT NULL DEFAULT 0;

-- A counter order placed from a terminal: the terminal's idempotency key
-- (one order per key, '' = not a terminal order).
ALTER TABLE orders ADD COLUMN client_key text NOT NULL DEFAULT '';
CREATE UNIQUE INDEX idx_orders_client_key ON orders (client_key) WHERE client_key <> '';

-- The snapshots main sends for the terminals (kind counter_menu |
-- terminal_config): the last good body, kept so both terminals get the same
-- one, also offline.
CREATE TABLE terminal_snapshots (
    kind       text    PRIMARY KEY CHECK (kind IN ('counter_menu', 'terminal_config')),
    version    integer NOT NULL,
    etag       text    NOT NULL,
    body       text    NOT NULL,
    fetched_at text    NOT NULL
);

-- Money taken (or given back) on a counter order at a terminal.
CREATE TABLE order_payments (
    id             text    PRIMARY KEY,
    order_id       text    NOT NULL,
    client_key     text    NOT NULL DEFAULT '',
    at             text    NOT NULL,
    tender         text    NOT NULL,
    amount_cents   integer NOT NULL,
    tip_cents      integer NOT NULL DEFAULT 0,
    tendered_cents integer NOT NULL DEFAULT 0,
    change_cents   integer NOT NULL DEFAULT 0,
    refund         boolean NOT NULL DEFAULT false,
    reason         text    NOT NULL DEFAULT '',
    ref            text    NOT NULL DEFAULT '',
    last4          text    NOT NULL DEFAULT '',
    staff_id       text    NOT NULL DEFAULT '',
    device_id      text    NOT NULL DEFAULT '',
    approved_by    text    NOT NULL DEFAULT ''
);
CREATE INDEX idx_order_payments_order ON order_payments (order_id, at);
CREATE UNIQUE INDEX idx_order_payments_key ON order_payments (order_id, client_key) WHERE client_key <> '';

-- Every terminal action (login, lock, order, payment, void, no sale, ...):
-- one row each, synced to main as the floor event journal.entries.
CREATE TABLE journal (
    id        text PRIMARY KEY,
    at        text NOT NULL,
    staff_id  text NOT NULL DEFAULT '',
    device_id text NOT NULL DEFAULT '',
    action    text NOT NULL,
    order_id  text NOT NULL DEFAULT '',
    detail    text NOT NULL DEFAULT '{}'
);
CREATE INDEX idx_journal_at ON journal (at);

-- +goose Down
DROP INDEX idx_journal_at;
DROP TABLE journal;
DROP INDEX idx_order_payments_key;
DROP INDEX idx_order_payments_order;
DROP TABLE order_payments;
DROP TABLE terminal_snapshots;
DROP INDEX idx_orders_client_key;
ALTER TABLE orders DROP COLUMN client_key;
ALTER TABLE settings DROP COLUMN counter_next;
DELETE FROM paired_devices WHERE kind = 'terminal';
CREATE TABLE paired_devices_old (
    id           text PRIMARY KEY,
    name         text NOT NULL,
    key_hash     text NOT NULL UNIQUE,
    key_prefix   text NOT NULL,
    created_at   text NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    last_seen_at text,
    revoked_at   text,
    kind         text NOT NULL DEFAULT 'tablet' CHECK (kind IN ('tablet', 'integration')),
    app_version         text NOT NULL DEFAULT '',
    app_version_seen_at text
);
INSERT INTO paired_devices_old (id, name, key_hash, key_prefix, created_at, last_seen_at, revoked_at, kind, app_version, app_version_seen_at)
SELECT id, name, key_hash, key_prefix, created_at, last_seen_at, revoked_at, kind, app_version, app_version_seen_at FROM paired_devices;
DROP TABLE paired_devices;
ALTER TABLE paired_devices_old RENAME TO paired_devices;
