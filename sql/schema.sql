-- SQLite schema (pure-Go modernc.org/sqlite). Mirrors the goose migrations in
-- internal/migrate/sql/. Timestamps are stored as RFC3339 text so the driver
-- never has to guess a time format; the repos parse them back into time.Time.
-- 00004_floor: batches (exactly one open), orders rebuilt with floor columns.
CREATE TABLE batches (
    id        integer PRIMARY KEY AUTOINCREMENT,
    opened_at text    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    closed_at text
);

CREATE TABLE orders (
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
    -- 00005_cancelled: never-accepted orders cancelled by Close batch.
    cancelled_at       text,
    -- 00009_accept_pending: claimed for accept, not yet recorded.
    accept_pending     integer NOT NULL DEFAULT 0,
    CHECK (state IN ('arrived', 'accepted', 'ready', 'on_delivery', 'delivered', 'cancelled'))
);
CREATE INDEX idx_orders_arrival_date ON orders (arrival_date DESC);
CREATE INDEX idx_orders_state_pending ON orders (arrival_date DESC) WHERE state = 'arrived';
CREATE INDEX idx_orders_source_event ON orders (source, source_event_id);
CREATE INDEX idx_orders_batch ON orders (batch_id, arrival_date DESC);

CREATE TABLE drivers (
    id               text    PRIMARY KEY,
    name             text    NOT NULL,
    active           boolean NOT NULL DEFAULT true,
    position         integer NOT NULL DEFAULT 0,
    -- 00006_driver_shifts: 'off' = clocked out (not in the queue).
    status           text    NOT NULL DEFAULT 'off',
    last_returned_at text,
    created_at       text    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    CHECK (status IN ('off', 'available', 'out'))
);

CREATE TABLE driver_shifts (
    id          text    PRIMARY KEY,
    driver_id   text    NOT NULL,
    driver_name text    NOT NULL,
    batch_id    integer NOT NULL,
    clock_in    text    NOT NULL,
    clock_out   text,
    auto_out    boolean NOT NULL DEFAULT false
);
CREATE INDEX idx_driver_shifts_open ON driver_shifts (driver_id) WHERE clock_out IS NULL;

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

CREATE TABLE paired_devices (
    id           text PRIMARY KEY,
    name         text NOT NULL,
    key_hash     text NOT NULL UNIQUE,
    key_prefix   text NOT NULL,
    created_at   text NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    last_seen_at text,
    revoked_at   text
);

CREATE TABLE settings (
    id            integer PRIMARY KEY DEFAULT 1,
    auto_accept   boolean NOT NULL DEFAULT false,
    store_api_key text    NOT NULL DEFAULT '',
    updated_at    text    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    -- 00002_session_secret: persisted web-UI session-signing secret (hex).
    session_secret   text NOT NULL DEFAULT '',
    -- 00003_ui_password: local web-UI password (bcrypt) + optional hint.
    ui_password_hash text NOT NULL DEFAULT '',
    ui_password_hint text NOT NULL DEFAULT '',
    -- 00004_floor: kitchen-number counter + the last prep estimate picked.
    kitchen_next      integer NOT NULL DEFAULT 1100,
    last_prep_minutes integer NOT NULL DEFAULT 20,
    -- 00006_driver_shifts: daily driver clock-out time (location-local HH:MM).
    driver_reset_time    text NOT NULL DEFAULT '10:00',
    last_driver_reset_at text NOT NULL DEFAULT '',
    -- 00007_floor_settings: ticket lateness + driver-report tip tax, the last
    -- driver return (undo), and the cached tablet app release (JSON).
    green_till_minutes    integer NOT NULL DEFAULT 20,
    tip_tax_bps           integer NOT NULL DEFAULT 600,
    last_return_driver_id text    NOT NULL DEFAULT '',
    last_return_position  integer NOT NULL DEFAULT 0,
    last_return_at        text    NOT NULL DEFAULT '',
    app_release           text    NOT NULL DEFAULT '',
    -- 00008_link_restore: secrets from main (manager-login link), the manager
    -- username, this agent's device id, the last nightly backup upload.
    secret_pushover_app_token text NOT NULL DEFAULT '',
    secret_pushover_user_key  text NOT NULL DEFAULT '',
    secret_pushover_boss_key  text NOT NULL DEFAULT '',
    secret_otlp_endpoint      text NOT NULL DEFAULT '',
    secret_otlp_headers       text NOT NULL DEFAULT '',
    manager_username          text NOT NULL DEFAULT '',
    device_id                 text NOT NULL DEFAULT '',
    last_backup_at            text NOT NULL DEFAULT '',
    -- 00010_receipt_prefs: receipt settings (JSON; missing keys = defaults).
    receipt_prefs             text NOT NULL DEFAULT '{}',
    -- 00011_updates_printer_macs: the latest Pi update request (JSON) + the
    -- "<id>:<state>" already announced by Pushover.
    update_job                text NOT NULL DEFAULT '',
    update_notified           text NOT NULL DEFAULT '',
    CHECK (id = 1)
);

CREATE TABLE store_config (
    id         integer PRIMARY KEY DEFAULT 1,
    context    text    NOT NULL DEFAULT '{}',
    updated_at text    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    CHECK (id = 1)
);

-- 00011_updates_printer_macs: learned printer hardware addresses.
CREATE TABLE printer_macs (
    printer_id text PRIMARY KEY,
    mac        text NOT NULL,
    ip         text NOT NULL,
    seen_at    text NOT NULL
);
