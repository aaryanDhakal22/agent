-- +goose Up
-- +goose StatementBegin
-- 2.1.0. POS orders pulled from the store's old POS by the store hub, future
-- (scheduled) online orders, kitchen-ticket switches and feature toggles.
--
-- The 'scheduled' state (a future order confirmed once, waiting to ring again
-- at ring_at) needs a wider CHECK, and SQLite can't widen one, so orders is
-- rebuilt once more (same columns as 00013 + the new ones):
--   ext_source / ext_ref / ext_hash   the POS ('softtouch'), its check number
--                                     and the hub's hash of the check
--   ext_driver_pos                    the last driver number the POS set
--   display_number                    the POS check number staff call
--   paid / cash_due_cents / tip_cents settled at the POS (or an inserted
--                                     online cash pickup settled); what the
--                                     driver collects; the tip on the check
--   board_hidden                      a counter check with no kitchen item
--   sent_at                           when the cashier pressed Send
--   local_only                        never synced to main (POS orders)
--   flag                              {text, severity} JSON, '' = none
--   pos_check_number                  an online order the hub inserted
--   pos_driver_note                   "POS says #12 (not clocked in)"
--   scheduled_lead_minutes / ring_at / rerung   future orders
CREATE TABLE orders_new (
    id                     text    PRIMARY KEY,
    tenant_id              text    NOT NULL,
    location_id            text    NOT NULL,
    source                 text    NOT NULL,
    source_event_id        text    NOT NULL,
    snapshot               text    NOT NULL,
    state                  text    NOT NULL DEFAULT 'arrived',
    arrival_date           text    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    printed_date           text,
    batch_id               integer NOT NULL,
    kitchen_number         integer,
    prep_minutes           integer,
    estimated_ready_at     text,
    accepted_at            text,
    ready_at               text,
    out_at                 text,
    delivered_at           text,
    driver_id              text,
    driver_name            text    NOT NULL DEFAULT '',
    last_alert_at          text,
    cancelled_at           text,
    accept_pending         integer NOT NULL DEFAULT 0,
    pool_driver_id         text,
    pooled_at              text,
    pool_seq               integer,
    pool_plan_id           text,
    ext_source             text    NOT NULL DEFAULT '',
    ext_ref                text    NOT NULL DEFAULT '',
    ext_hash               text    NOT NULL DEFAULT '',
    ext_driver_pos         text    NOT NULL DEFAULT '',
    display_number         integer,
    paid                   boolean NOT NULL DEFAULT false,
    cash_due_cents         integer,
    tip_cents              integer,
    board_hidden           boolean NOT NULL DEFAULT false,
    sent_at                text,
    local_only             boolean NOT NULL DEFAULT false,
    flag                   text    NOT NULL DEFAULT '',
    pos_check_number       integer,
    pos_driver_note        text    NOT NULL DEFAULT '',
    scheduled_lead_minutes integer,
    ring_at                text,
    rerung                 boolean NOT NULL DEFAULT false,
    CHECK (state IN ('arrived', 'scheduled', 'accepted', 'ready', 'on_delivery', 'delivered', 'cancelled'))
);
INSERT INTO orders_new (id, tenant_id, location_id, source, source_event_id, snapshot, state, arrival_date, printed_date, batch_id, kitchen_number, prep_minutes, estimated_ready_at, accepted_at, ready_at, out_at, delivered_at, driver_id, driver_name, last_alert_at, cancelled_at, accept_pending, pool_driver_id, pooled_at, pool_seq, pool_plan_id)
SELECT id, tenant_id, location_id, source, source_event_id, snapshot, state, arrival_date, printed_date, batch_id, kitchen_number, prep_minutes, estimated_ready_at, accepted_at, ready_at, out_at, delivered_at, driver_id, driver_name, last_alert_at, cancelled_at, accept_pending, pool_driver_id, pooled_at, pool_seq, pool_plan_id FROM orders;
DROP TABLE orders;
ALTER TABLE orders_new RENAME TO orders;
CREATE INDEX idx_orders_arrival_date ON orders (arrival_date DESC);
CREATE INDEX idx_orders_state_pending ON orders (arrival_date DESC) WHERE state = 'arrived';
CREATE INDEX idx_orders_source_event ON orders (source, source_event_id);
CREATE INDEX idx_orders_batch ON orders (batch_id, arrival_date DESC);
CREATE INDEX idx_orders_pool ON orders (pool_driver_id) WHERE pool_driver_id IS NOT NULL;
CREATE INDEX idx_orders_ext ON orders (ext_source, ext_ref) WHERE ext_ref <> '';
CREATE INDEX idx_orders_scheduled ON orders (ring_at) WHERE state = 'scheduled';

-- A driver's number on the POS (their login #), unique among active drivers.
ALTER TABLE drivers ADD COLUMN pos_number text NOT NULL DEFAULT '';
CREATE UNIQUE INDEX idx_drivers_pos_number ON drivers (pos_number) WHERE pos_number <> '' AND active;

-- Paired devices: a counter tablet, or an integration (the store hub), which
-- may call only /api/integration/* and the read-only floor routes.
ALTER TABLE paired_devices ADD COLUMN kind text NOT NULL DEFAULT 'tablet' CHECK (kind IN ('tablet', 'integration'));

-- The integration feed: what the hub must not miss (a cursor, unlike SSE).
-- Written in the same transaction as the change it describes; kept 3 days.
-- AUTOINCREMENT so a pruned seq is never handed out again.
CREATE TABLE integration_events (
    seq        integer PRIMARY KEY AUTOINCREMENT,
    type       text    NOT NULL,
    payload    text    NOT NULL,
    created_at text    NOT NULL
);
CREATE INDEX idx_integration_events_created ON integration_events (created_at);

-- Where main located a delivery: up to 3 candidates when the address is
-- ambiguous (state 'check'), and the address staff confirmed (JSON, '' = none).
ALTER TABLE order_geo ADD COLUMN candidates text NOT NULL DEFAULT '';
ALTER TABLE order_geo ADD COLUMN confirmed_address text NOT NULL DEFAULT '';

-- Settings: feature toggles + kitchen-ticket switches (JSON; missing keys =
-- defaults), the default pickup / delivery times (X / Y), and the hub's last
-- reported status (JSON).
ALTER TABLE settings ADD COLUMN features text NOT NULL DEFAULT '{}';
ALTER TABLE settings ADD COLUMN ticket_toggles text NOT NULL DEFAULT '{}';
ALTER TABLE settings ADD COLUMN default_pickup_minutes integer NOT NULL DEFAULT 15;
ALTER TABLE settings ADD COLUMN default_delivery_minutes integer NOT NULL DEFAULT 35;
ALTER TABLE settings ADD COLUMN integration_status text NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE settings DROP COLUMN integration_status;
ALTER TABLE settings DROP COLUMN default_delivery_minutes;
ALTER TABLE settings DROP COLUMN default_pickup_minutes;
ALTER TABLE settings DROP COLUMN ticket_toggles;
ALTER TABLE settings DROP COLUMN features;
ALTER TABLE order_geo DROP COLUMN confirmed_address;
ALTER TABLE order_geo DROP COLUMN candidates;
DROP TABLE integration_events;
ALTER TABLE paired_devices DROP COLUMN kind;
DROP INDEX idx_drivers_pos_number;
ALTER TABLE drivers DROP COLUMN pos_number;
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
    cancelled_at       text,
    accept_pending     integer NOT NULL DEFAULT 0,
    pool_driver_id     text,
    pooled_at          text,
    pool_seq           integer,
    pool_plan_id       text,
    CHECK (state IN ('arrived', 'accepted', 'ready', 'on_delivery', 'delivered', 'cancelled'))
);
INSERT INTO orders_old (id, tenant_id, location_id, source, source_event_id, snapshot, state, arrival_date, printed_date, batch_id, kitchen_number, prep_minutes, estimated_ready_at, accepted_at, ready_at, out_at, delivered_at, driver_id, driver_name, last_alert_at, cancelled_at, accept_pending, pool_driver_id, pooled_at, pool_seq, pool_plan_id)
SELECT id, tenant_id, location_id, source, source_event_id, snapshot, CASE WHEN state = 'scheduled' THEN 'arrived' ELSE state END, arrival_date, printed_date, batch_id, kitchen_number, prep_minutes, estimated_ready_at, accepted_at, ready_at, out_at, delivered_at, driver_id, driver_name, last_alert_at, cancelled_at, accept_pending, pool_driver_id, pooled_at, pool_seq, pool_plan_id FROM orders;
DROP TABLE orders;
ALTER TABLE orders_old RENAME TO orders;
CREATE INDEX idx_orders_arrival_date ON orders (arrival_date DESC);
CREATE INDEX idx_orders_state_pending ON orders (arrival_date DESC) WHERE state = 'arrived';
CREATE INDEX idx_orders_source_event ON orders (source, source_event_id);
CREATE INDEX idx_orders_batch ON orders (batch_id, arrival_date DESC);
CREATE INDEX idx_orders_pool ON orders (pool_driver_id) WHERE pool_driver_id IS NOT NULL;
-- +goose StatementEnd
