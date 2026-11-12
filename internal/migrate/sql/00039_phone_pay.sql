-- +goose Up
-- phone_pay ("Send to phone", contract/agent.md §2.10z): a card charge a
-- terminal sends to a cashier's phone, which charges it with Square's Point
-- of Sale app (Tap to Pay on the phone) and reports Square's answer back.
-- One row per charge; the payment it records is an order_payments row
-- (tender square_app, client_key = the charge's client_key).
CREATE TABLE phone_charges (
    id                    text    PRIMARY KEY,
    client_key            text    NOT NULL,
    request_hash          text    NOT NULL DEFAULT '',
    order_id              text    NOT NULL,
    number                integer NOT NULL DEFAULT 0,
    phone_id              text    NOT NULL,
    phone_name            text    NOT NULL DEFAULT '',
    device_id             text    NOT NULL DEFAULT '',
    device_name           text    NOT NULL DEFAULT '',
    staff_id              text    NOT NULL DEFAULT '',
    staff_name            text    NOT NULL DEFAULT '',
    amount_cents          integer NOT NULL,
    tip_cents             integer NOT NULL DEFAULT 0,
    code                  text    NOT NULL,
    note                  text    NOT NULL DEFAULT '',
    status                text    NOT NULL,
    message               text    NOT NULL DEFAULT '',
    created_at            text    NOT NULL,
    updated_at            text    NOT NULL,
    expires_at            text    NOT NULL,
    opened_at             text,
    finished_at           text,
    transaction_id        text    NOT NULL DEFAULT '',
    client_transaction_id text    NOT NULL DEFAULT '',
    error_code            text    NOT NULL DEFAULT '',
    payment_id            text    NOT NULL DEFAULT '',
    square_payment_id     text    NOT NULL DEFAULT '',
    brand                 text    NOT NULL DEFAULT '',
    last4                 text    NOT NULL DEFAULT '',
    entry                 text    NOT NULL DEFAULT '',
    extra_tip_cents       integer NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX idx_phone_charges_key ON phone_charges (client_key);
CREATE INDEX idx_phone_charges_phone ON phone_charges (phone_id, status);
CREATE INDEX idx_phone_charges_order ON phone_charges (order_id);

-- paired_devices.kind gains 'phone' (a cashier phone: its key, in the page's
-- cookie, opens /api/phone/* only). SQLite can't change a CHECK: rebuilt as
-- 00025 did (nothing references it).
CREATE TABLE paired_devices_new (
    id           text PRIMARY KEY,
    name         text NOT NULL,
    key_hash     text NOT NULL UNIQUE,
    key_prefix   text NOT NULL,
    created_at   text NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    last_seen_at text,
    revoked_at   text,
    kind         text NOT NULL DEFAULT 'tablet' CHECK (kind IN ('tablet', 'integration', 'terminal', 'phone')),
    app_version         text NOT NULL DEFAULT '',
    app_version_seen_at text
);
INSERT INTO paired_devices_new (id, name, key_hash, key_prefix, created_at, last_seen_at, revoked_at, kind, app_version, app_version_seen_at)
SELECT id, name, key_hash, key_prefix, created_at, last_seen_at, revoked_at, kind, app_version, app_version_seen_at FROM paired_devices;
DROP TABLE paired_devices;
ALTER TABLE paired_devices_new RENAME TO paired_devices;

-- +goose Down
DROP TABLE phone_charges;
DELETE FROM paired_devices WHERE kind = 'phone';
CREATE TABLE paired_devices_old (
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
INSERT INTO paired_devices_old (id, name, key_hash, key_prefix, created_at, last_seen_at, revoked_at, kind, app_version, app_version_seen_at)
SELECT id, name, key_hash, key_prefix, created_at, last_seen_at, revoked_at, kind, app_version, app_version_seen_at FROM paired_devices;
DROP TABLE paired_devices;
ALTER TABLE paired_devices_old RENAME TO paired_devices;

