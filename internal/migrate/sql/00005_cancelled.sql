-- +goose Up
-- +goose StatementBegin
-- Close batch cancels orders nobody ever accepted (owner decision 2026-10-15):
-- add the 'cancelled' state + cancelled_at. SQLite can't widen a CHECK, so the
-- orders table is rebuilt again (same columns as 00004 + cancelled_at).
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
    cancelled_at       text,
    CHECK (state IN ('arrived', 'accepted', 'ready', 'on_delivery', 'delivered', 'cancelled'))
);
INSERT INTO orders_new (id, tenant_id, location_id, source, source_event_id, snapshot, state, arrival_date, printed_date, batch_id, kitchen_number, prep_minutes, estimated_ready_at, accepted_at, ready_at, out_at, delivered_at, driver_id, driver_name, last_alert_at)
SELECT id, tenant_id, location_id, source, source_event_id, snapshot, state, arrival_date, printed_date, batch_id, kitchen_number, prep_minutes, estimated_ready_at, accepted_at, ready_at, out_at, delivered_at, driver_id, driver_name, last_alert_at FROM orders;
DROP TABLE orders;
ALTER TABLE orders_new RENAME TO orders;
CREATE INDEX idx_orders_arrival_date ON orders (arrival_date DESC);
CREATE INDEX idx_orders_state_pending ON orders (arrival_date DESC) WHERE state = 'arrived';
CREATE INDEX idx_orders_source_event ON orders (source, source_event_id);
CREATE INDEX idx_orders_batch ON orders (batch_id, arrival_date DESC);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE TABLE orders_old (
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
INSERT INTO orders_old (id, tenant_id, location_id, source, source_event_id, snapshot, state, arrival_date, printed_date, batch_id, kitchen_number, prep_minutes, estimated_ready_at, accepted_at, ready_at, out_at, delivered_at, driver_id, driver_name, last_alert_at)
SELECT id, tenant_id, location_id, source, source_event_id, snapshot, CASE WHEN state = 'cancelled' THEN 'delivered' ELSE state END, arrival_date, printed_date, batch_id, kitchen_number, prep_minutes, estimated_ready_at, accepted_at, ready_at, out_at, delivered_at, driver_id, driver_name, last_alert_at FROM orders;
DROP TABLE orders;
ALTER TABLE orders_old RENAME TO orders;
CREATE INDEX idx_orders_arrival_date ON orders (arrival_date DESC);
CREATE INDEX idx_orders_state_pending ON orders (arrival_date DESC) WHERE state = 'arrived';
CREATE INDEX idx_orders_source_event ON orders (source, source_event_id);
CREATE INDEX idx_orders_batch ON orders (batch_id, arrival_date DESC);
-- +goose StatementEnd
