-- +goose Up
-- The store's customers on the Pi (TERMINAL-PLAN 1d, contract/agent.md
-- 2.10j): a full copy of main's customers for this store, kept by main's
-- change feed (GET /store/v1/agent/customers?since=), so the terminals look
-- a caller up by phone with no internet. Customers added or changed at a
-- terminal are saved here at once and go to main as the floor event
-- customer.upsert.
-- phone: the 10 digits; phone_rev: reversed, so "the last 4" is an indexed
-- prefix search too. updated_at: main's; local_at: when a terminal last
-- changed it ('' = as main has it; the feed's older copy never overwrites
-- a newer local change). last_order_at: the newest order the Pi saw for it.
CREATE TABLE customers (
    id            text NOT NULL PRIMARY KEY,
    phone         text NOT NULL,
    phone_rev     text NOT NULL,
    first_name    text NOT NULL DEFAULT '',
    last_name     text NOT NULL DEFAULT '',
    name          text NOT NULL DEFAULT '',
    notes         text NOT NULL DEFAULT '',
    source        text NOT NULL DEFAULT '',
    updated_at    text NOT NULL DEFAULT '',
    local_at      text NOT NULL DEFAULT '',
    last_order_at text NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX idx_customers_phone ON customers (phone);
CREATE INDEX idx_customers_phone_rev ON customers (phone_rev);

-- lat / lng: where an order to this address was located (main's geo), for
-- the in / out of area check; kept across feed updates.
CREATE TABLE customer_addresses (
    id          text    NOT NULL PRIMARY KEY,
    customer_id text    NOT NULL,
    position    integer NOT NULL DEFAULT 0,
    label       text    NOT NULL DEFAULT '',
    line1       text    NOT NULL,
    line2       text    NOT NULL DEFAULT '',
    city        text    NOT NULL DEFAULT '',
    state       text    NOT NULL DEFAULT '',
    zip         text    NOT NULL DEFAULT '',
    notes       text    NOT NULL DEFAULT '',
    is_default  boolean NOT NULL DEFAULT false,
    created_at  text    NOT NULL DEFAULT '',
    lat         real,
    lng         real
);
CREATE INDEX idx_customer_addresses_customer ON customer_addresses (customer_id);

-- The feed's state: the store it belongs to (a re-linked Pi starts over),
-- the last cursor, the last good pull.
CREATE TABLE customer_sync (
    id          integer NOT NULL PRIMARY KEY CHECK (id = 1),
    location_id text    NOT NULL DEFAULT '',
    cursor      text    NOT NULL DEFAULT '',
    synced_at   text    NOT NULL DEFAULT ''
);

-- +goose Down
DROP TABLE customer_sync;
DROP TABLE customer_addresses;
DROP TABLE customers;
