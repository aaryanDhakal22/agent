-- +goose Up
-- +goose StatementBegin
-- Floor flow: the agent owns an order's life on the restaurant floor
-- (accept -> ready -> on delivery -> delivered), kitchen numbers, drivers and
-- batches, and syncs every change to main through the floor_events outbox.

-- Batches: a shift/day of orders. Exactly one is open (closed_at IS NULL);
-- "Close batch" closes it and opens the next. Retention keeps the open batch
-- plus the newest few closed ones.
CREATE TABLE batches (
    id        integer PRIMARY KEY AUTOINCREMENT,
    opened_at text    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    closed_at text
);
INSERT INTO batches (opened_at) VALUES (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'));

-- orders is rebuilt to widen the state CHECK (SQLite can't alter a CHECK).
CREATE TABLE orders_new (
    id                 text    PRIMARY KEY,
    tenant_id          text    NOT NULL,
    location_id        text    NOT NULL,
    source             text    NOT NULL,
    source_event_id    text    NOT NULL,
    snapshot           text    NOT NULL,
    state              text    NOT NULL DEFAULT 'arrived',
    arrival_date       text    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    printed_date       text,
    batch_id           integer NOT NULL,
    kitchen_number     integer,
    prep_minutes       integer,
    estimated_ready_at text,
    accepted_at        text,
    ready_at           text,
    out_at             text,
    delivered_at       text,
    driver_id          text,
    driver_name        text    NOT NULL DEFAULT '',
    last_alert_at      text,
    CHECK (state IN ('arrived', 'accepted', 'ready', 'on_delivery', 'delivered'))
);
INSERT INTO orders_new (id, tenant_id, location_id, source, source_event_id, snapshot, state,
                        arrival_date, printed_date, batch_id, accepted_at)
SELECT id, tenant_id, location_id, source, source_event_id, snapshot, state,
       arrival_date, printed_date, (SELECT max(id) FROM batches),
       CASE WHEN state = 'accepted' THEN printed_date END
FROM orders;
DROP TABLE orders;
ALTER TABLE orders_new RENAME TO orders;
CREATE INDEX idx_orders_arrival_date ON orders (arrival_date DESC);
CREATE INDEX idx_orders_state_pending ON orders (arrival_date DESC) WHERE state = 'arrived';
CREATE INDEX idx_orders_source_event ON orders (source, source_event_id);
CREATE INDEX idx_orders_batch ON orders (batch_id, arrival_date DESC);

-- Delivery drivers + their queue. position orders the queue (lowest = next
-- up); a driver who returns goes to the back. Removal is soft (active=false)
-- so past orders keep resolving the name.
CREATE TABLE drivers (
    id               text    PRIMARY KEY,
    name             text    NOT NULL,
    active           boolean NOT NULL DEFAULT true,
    position         integer NOT NULL DEFAULT 0,
    status           text    NOT NULL DEFAULT 'available',
    last_returned_at text,
    created_at       text    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    CHECK (status IN ('available', 'out'))
);

-- Sync outbox to main: one row per floor change, written in the same
-- transaction as the change; floorsync pushes unsynced rows (oldest first)
-- and stamps synced_at. id doubles as main's idempotency key.
CREATE TABLE floor_events (
    id         text    PRIMARY KEY,
    order_id   text,
    type       text    NOT NULL,
    payload    text    NOT NULL,
    at         text    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    synced_at  text,
    attempts   integer NOT NULL DEFAULT 0,
    last_error text    NOT NULL DEFAULT ''
);
CREATE INDEX idx_floor_events_unsynced ON floor_events (at) WHERE synced_at IS NULL;

-- Counter tablets paired to this agent (6-digit code from the web UI). The
-- device key is only ever stored hashed (sha256 hex); key_prefix is for display.
CREATE TABLE paired_devices (
    id           text PRIMARY KEY,
    name         text NOT NULL,
    key_hash     text NOT NULL UNIQUE,
    key_prefix   text NOT NULL,
    created_at   text NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    last_seen_at text,
    revoked_at   text
);

-- Kitchen numbers start at 1100 and reset on "Close batch"; the last prep
-- estimate staff picked is the next accept's default.
ALTER TABLE settings ADD COLUMN kitchen_next integer NOT NULL DEFAULT 1100;
ALTER TABLE settings ADD COLUMN last_prep_minutes integer NOT NULL DEFAULT 20;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE settings DROP COLUMN last_prep_minutes;
ALTER TABLE settings DROP COLUMN kitchen_next;
DROP TABLE paired_devices;
DROP TABLE floor_events;
DROP TABLE drivers;
CREATE TABLE orders_old (
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
INSERT INTO orders_old (id, tenant_id, location_id, source, source_event_id, snapshot, state, arrival_date, printed_date)
SELECT id, tenant_id, location_id, source, source_event_id, snapshot,
       CASE WHEN state = 'arrived' THEN 'arrived' ELSE 'accepted' END, arrival_date, printed_date
FROM orders;
DROP TABLE orders;
ALTER TABLE orders_old RENAME TO orders;
CREATE INDEX idx_orders_arrival_date ON orders (arrival_date DESC);
CREATE INDEX idx_orders_state_pending ON orders (arrival_date DESC) WHERE state = 'arrived';
CREATE INDEX idx_orders_source_event ON orders (source, source_event_id);
DROP TABLE batches;
-- +goose StatementEnd
