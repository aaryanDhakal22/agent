-- +goose Up
-- 2.0.9. Driver pools + delivery maps + AUTO plans.
--
-- pool_driver_id / pooled_at: a delivery order (accepted or ready) waiting in
-- an available driver's pool for "Send Out". Cleared when it goes out, and
-- when the driver clocks out, is removed, or the batch closes.
ALTER TABLE orders ADD COLUMN pool_driver_id text;
ALTER TABLE orders ADD COLUMN pooled_at text;
CREATE INDEX idx_orders_pool ON orders (pool_driver_id) WHERE pool_driver_id IS NOT NULL;

-- order_geo: where main located the delivery address (RPC order.geo) and the
-- driving route from the store. state located|not_found|pending; quality
-- exact|approx|pinned ('' when not found). store_to_s / to_store_s are the
-- driving times store -> address and back (AUTO). No foreign key: retention
-- drops rows whose order is gone.
CREATE TABLE order_geo (
    order_id     text    PRIMARY KEY,
    state        text    NOT NULL,
    lat          real,
    lng          real,
    quality      text    NOT NULL DEFAULT '',
    distance_m   integer NOT NULL DEFAULT 0,
    duration_s   integer NOT NULL DEFAULT 0,
    polyline     text    NOT NULL DEFAULT '',
    outside_area boolean NOT NULL DEFAULT false,
    store_to_s   integer,
    to_store_s   integer,
    updated_at   text    NOT NULL
);

-- order_times: driving seconds from order a's address to order b's (main
-- sends both directions when an order is located).
CREATE TABLE order_times (
    a       text    NOT NULL,
    b       text    NOT NULL,
    seconds integer NOT NULL,
    PRIMARY KEY (a, b)
);

-- auto_plans: every AUTO suggestion (input + output JSON) and when it was
-- confirmed, kept for tuning.
CREATE TABLE auto_plans (
    id           text PRIMARY KEY,
    created_at   text NOT NULL,
    plan_json    text NOT NULL,
    confirmed_at text
);

-- Delivery settings (AUTO) and the store map file this Pi downloaded (JSON
-- {version, sha256, url, bbox}; '' = none).
ALTER TABLE settings ADD COLUMN soon_ready_minutes integer NOT NULL DEFAULT 5;
ALTER TABLE settings ADD COLUMN delivery_target_minutes integer NOT NULL DEFAULT 45;
ALTER TABLE settings ADD COLUMN map_file text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE settings DROP COLUMN map_file;
ALTER TABLE settings DROP COLUMN delivery_target_minutes;
ALTER TABLE settings DROP COLUMN soon_ready_minutes;
DROP TABLE auto_plans;
DROP TABLE order_times;
DROP TABLE order_geo;
DROP INDEX idx_orders_pool;
ALTER TABLE orders DROP COLUMN pooled_at;
ALTER TABLE orders DROP COLUMN pool_driver_id;
