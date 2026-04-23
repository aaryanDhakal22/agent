-- Agent's schema. The agent stores a 7-day rolling cache of orders for the
-- mobile interface. Unlike main/, orders are not normalized — the full order
-- payload is persisted as JSONB and rehydrated to the domain type on read.
-- That keeps writes cheap, reads trivial, and avoids schema drift if main/'s
-- payload grows new fields.

CREATE TYPE order_state AS ENUM ('arrived', 'accepted');

CREATE TABLE IF NOT EXISTS orders (
    order_id      integer PRIMARY KEY,
    payload       jsonb       NOT NULL,
    state         order_state NOT NULL DEFAULT 'arrived',
    arrival_date  timestamptz NOT NULL DEFAULT now(),
    printed_date  timestamptz NULL
);

CREATE INDEX IF NOT EXISTS idx_orders_arrival_date ON orders (arrival_date DESC);
CREATE INDEX IF NOT EXISTS idx_orders_state_pending ON orders (arrival_date DESC) WHERE state = 'arrived';

-- Singleton settings row. The agent has exactly one "auto-accept" toggle that
-- the mobile interface flips. The CHECK constraint guarantees only one row.
CREATE TABLE IF NOT EXISTS settings (
    id          integer     PRIMARY KEY DEFAULT 1,
    auto_accept boolean     NOT NULL DEFAULT false,
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT settings_singleton CHECK (id = 1)
);

-- Printer network locations. Env vars seed rows on startup if-absent; mobile
-- updates via PUT /api/printers/{name}/ip take precedence thereafter and
-- persist across restarts.
CREATE TABLE IF NOT EXISTS printer_configs (
    name       text        PRIMARY KEY,
    ip         text        NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);
