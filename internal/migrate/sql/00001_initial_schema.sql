-- +goose Up

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

CREATE TABLE IF NOT EXISTS settings (
    id          integer     PRIMARY KEY DEFAULT 1,
    auto_accept boolean     NOT NULL DEFAULT true,
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT settings_singleton CHECK (id = 1)
);

INSERT INTO settings (id, auto_accept) VALUES (1, true) ON CONFLICT (id) DO NOTHING;

-- +goose Down
DROP TABLE IF EXISTS settings;
DROP TABLE IF EXISTS orders;
DROP TYPE IF EXISTS order_state;
