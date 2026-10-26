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
    -- 00012_pools_geo: the available driver whose pool holds this delivery.
    pool_driver_id     text,
    pooled_at          text,
    -- 00013_pool_seq: stop order inside the pool (1..n).
    pool_seq           integer,
    pool_plan_id       text,
    -- 00014_integration: POS orders from the store hub + future orders.
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
    -- 00016_v220: accepted by auto-accept.
    auto_accepted          boolean NOT NULL DEFAULT false,
    CHECK (state IN ('arrived', 'scheduled', 'accepted', 'ready', 'on_delivery', 'delivered', 'cancelled'))
);
CREATE INDEX idx_orders_arrival_date ON orders (arrival_date DESC);
CREATE INDEX idx_orders_state_pending ON orders (arrival_date DESC) WHERE state = 'arrived';
CREATE INDEX idx_orders_source_event ON orders (source, source_event_id);
CREATE INDEX idx_orders_batch ON orders (batch_id, arrival_date DESC);
CREATE INDEX idx_orders_pool ON orders (pool_driver_id) WHERE pool_driver_id IS NOT NULL;
CREATE INDEX idx_orders_ext ON orders (ext_source, ext_ref) WHERE ext_ref <> '';
CREATE INDEX idx_orders_scheduled ON orders (ring_at) WHERE state = 'scheduled';

CREATE TABLE drivers (
    id               text    PRIMARY KEY,
    name             text    NOT NULL,
    active           boolean NOT NULL DEFAULT true,
    position         integer NOT NULL DEFAULT 0,
    -- 00006_driver_shifts: 'off' = clocked out (not in the queue).
    status           text    NOT NULL DEFAULT 'off',
    last_returned_at text,
    created_at       text    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    -- 00014_integration: the driver's POS login number (unique when set).
    pos_number       text    NOT NULL DEFAULT '',
    CHECK (status IN ('off', 'available', 'out'))
);
CREATE UNIQUE INDEX idx_drivers_pos_number ON drivers (pos_number) WHERE pos_number <> '' AND active;

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
    revoked_at   text,
    -- 00014_integration: tablet | integration (the store hub).
    kind         text NOT NULL DEFAULT 'tablet' CHECK (kind IN ('tablet', 'integration')),
    -- 00016_v220: the Quibase version the tablet last sent (X-Quibase-Version).
    app_version         text NOT NULL DEFAULT '',
    app_version_seen_at text
);

-- 00014_integration: the hub's cursor feed (kept 3 days).
CREATE TABLE integration_events (
    seq        integer PRIMARY KEY AUTOINCREMENT,
    type       text    NOT NULL,
    payload    text    NOT NULL,
    created_at text    NOT NULL
);
CREATE INDEX idx_integration_events_created ON integration_events (created_at);

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
    -- 00012_pools_geo: AUTO delivery settings + the downloaded store map (JSON).
    soon_ready_minutes        integer NOT NULL DEFAULT 5,
    delivery_target_minutes   integer NOT NULL DEFAULT 45,
    map_file                  text NOT NULL DEFAULT '',
    -- 00014_integration: feature toggles + kitchen-ticket switches (JSON),
    -- default pickup / delivery minutes, the hub's last status (JSON).
    features                  text NOT NULL DEFAULT '{}',
    ticket_toggles            text NOT NULL DEFAULT '{}',
    default_pickup_minutes    integer NOT NULL DEFAULT 15,
    default_delivery_minutes  integer NOT NULL DEFAULT 35,
    integration_status        text NOT NULL DEFAULT '',
    -- 00015_update_last_success: when the last Pi update finished ok (RFC 3339).
    update_last_success       text NOT NULL DEFAULT '',
    -- 00016_v220: the nearby rule for the next driver's run + Pi auto-update
    -- after Close batch.
    nearby_rule               text    NOT NULL DEFAULT 'detour',
    nearby_detour_minutes     integer NOT NULL DEFAULT 5,
    nearby_distance_mi        real    NOT NULL DEFAULT 1.0,
    auto_update               boolean NOT NULL DEFAULT true,
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

-- 00012_pools_geo: where main located each delivery + drive times + AUTO plans.
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
    updated_at   text    NOT NULL,
    -- 00014_integration: candidates (state check) + the confirmed address (JSON).
    candidates        text NOT NULL DEFAULT '',
    confirmed_address text NOT NULL DEFAULT ''
);

CREATE TABLE order_times (
    a       text    NOT NULL,
    b       text    NOT NULL,
    seconds integer NOT NULL,
    PRIMARY KEY (a, b)
);

CREATE TABLE auto_plans (
    id           text PRIMARY KEY,
    created_at   text NOT NULL,
    plan_json    text NOT NULL,
    confirmed_at text
);
